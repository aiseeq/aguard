package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/cpuwatch"
)

const strayPC = "##procs\n 7 1 7200 3600 bash\n##strays\n7\t555\t/user.slice/user-1000.slice/session-1.scope\t/home/u/git/graft\t\t\tbash -c x\n##end\n"

// strayApp — машина pc с одним процессом без родителя в проекте graft; ноут без них; лид есть, мастера graft — по live.
func strayApp(t *testing.T) (*app, *strings.Builder, *cpuwatch.Config) {
	t.Helper()
	dir := t.TempDir()
	logs := &strings.Builder{}
	a := &app{home: dir, stateDir: filepath.Join(dir, "state"), uid: 1000, log: slog.New(slog.NewTextHandler(logs, nil))}
	if err := os.MkdirAll(a.dir("cpu"), 0o700); err != nil {
		t.Fatal(err)
	}
	liveLead(t, a)
	a.strayRead = func(_ context.Context, h *cpuHost) ([]byte, error) {
		if h.ssh == "" {
			return []byte("##procs\n##strays\n##end\n"), nil
		}
		return []byte(strayPC), nil
	}
	cfg := cpuwatch.Default(dir)
	cfg.Mode, cfg.Lead = cpuwatch.ModeLog, "lead"
	if err := cfg.Validate(dir); err != nil {
		t.Fatal(err)
	}
	return a, logs, &cfg
}

func strayPass(a *app, cfg *cpuwatch.Config) {
	for _, h := range a.cpuHosts(cfg) {
		h.strayAt = time.Time{} // срок между проходами проверяется отдельно
	}
	a.strayRound(context.Background(), cfg, a.cpuResolver(cfg))
}

// Письмо один раз: мастера нет — лиду с его именем; рестарт демона (новое окно, отметка с диска) не повторяет.
func TestStrayLetterOnceAcrossRestart(t *testing.T) {
	a, logs, cfg := strayApp(t)
	strayPass(a, cfg)
	strayPass(a, cfg)
	a.cpu = nil // рестарт демона
	strayPass(a, cfg)
	out := logs.String()
	if n := strings.Count(out, "написал бы"); n != 1 {
		t.Fatalf("писем %d, журнал:\n%s", n, out)
	}
	for _, want := range []string{"session=lead", "graft-master нет в сессиях", "pid 7 bash", "cwd /home/u/git/graft", "Ничего не снимаю"} {
		if !strings.Contains(out, want) {
			t.Fatalf("нет %q:\n%s", want, out)
		}
	}
}

// Срок между проходами: второй проход сразу — замера нет.
func TestStrayEveryRespected(t *testing.T) {
	a, _, cfg := strayApp(t)
	reads := 0
	inner := a.strayRead
	a.strayRead = func(ctx context.Context, h *cpuHost) ([]byte, error) {
		reads++
		return inner(ctx, h)
	}
	r := a.cpuResolver(cfg)
	a.strayRound(context.Background(), cfg, r)
	a.strayRound(context.Background(), cfg, r)
	if reads != 2 { // ноут и pc, по разу
		t.Fatalf("замеров %d", reads)
	}
}

// Повтор через repeat, снятый забывается: появится снова — новое письмо, а не повтор.
func TestStrayRepeatAndForget(t *testing.T) {
	a, logs, cfg := strayApp(t)
	strayPass(a, cfg)
	path := filepath.Join(a.dir("cpu"), "strays.json")
	st, err := cpuwatch.LoadStrayState(path)
	if err != nil {
		t.Fatal(err)
	}
	for k := range st.Told {
		st.Told[k] = time.Now().Add(-cfg.Strays.Repeat.D() - time.Minute)
	}
	if err := st.Save(path); err != nil {
		t.Fatal(err)
	}
	strayPass(a, cfg)
	if !strings.Contains(logs.String(), "пишу повторно") {
		t.Fatalf("повтора нет:\n%s", logs.String())
	}
	inner := a.strayRead
	a.strayRead = func(context.Context, *cpuHost) ([]byte, error) { return []byte("##procs\n##strays\n##end\n"), nil }
	strayPass(a, cfg)
	if st, err = cpuwatch.LoadStrayState(path); err != nil || len(st.Told) != 0 {
		t.Fatalf("отметка не снята: %+v, %v", st, err)
	}
	a.strayRead = inner
	before := strings.Count(logs.String(), "пишу повторно")
	strayPass(a, cfg)
	if strings.Count(logs.String(), "пишу повторно") != before {
		t.Fatalf("после снятия — повтор:\n%s", logs.String())
	}
}

// Письмо не ушло: отметки нет, следующий проход пробует снова. Адресат — живой мастер: письмо ему, лиду не копия.
func TestStrayUnsentRetried(t *testing.T) {
	a, logs, cfg := strayApp(t)
	cfg.Mode = cpuwatch.ModeAlert
	cfg.Lead = "нет такого"
	strayPass(a, cfg)
	strayPass(a, cfg)
	if n := strings.Count(logs.String(), "письмо о перегрузке"); n != 2 {
		t.Fatalf("попыток %d, журнал:\n%s", n, logs.String())
	}
	st, err := cpuwatch.LoadStrayState(filepath.Join(a.dir("cpu"), "strays.json"))
	if err != nil || len(st.Told) != 0 {
		t.Fatalf("%+v, %v", st, err)
	}
}

// Сбой замера одной машины: другой идёт, отметки упавшей не теряются, о сбое — одна строка журнала.
func TestStrayHostFailureIsolated(t *testing.T) {
	a, logs, cfg := strayApp(t)
	strayPass(a, cfg)
	path := filepath.Join(a.dir("cpu"), "strays.json")
	a.strayRead = func(_ context.Context, h *cpuHost) ([]byte, error) {
		if h.ssh != "" {
			return nil, errors.New("ssh упал")
		}
		return []byte("##procs\n##strays\n##end\n"), nil
	}
	for range 2 {
		strayPass(a, cfg)
	}
	if n := strings.Count(logs.String(), "замер не удался"); n != 1 {
		t.Fatalf("строк о сбое %d:\n%s", n, logs.String())
	}
	st, err := cpuwatch.LoadStrayState(path)
	if err != nil || len(st.Told) != 1 {
		t.Fatalf("отметка при сбое ssh: %+v, %v", st, err)
	}
}

func TestStrayAddress(t *testing.T) {
	a, _, _ := strayApp(t)
	alive, err := a.reg.Alive()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ master, to, absent string }{{"", "lead", ""}, {"lead", "lead", ""}, {"нет-master", "lead", "нет-master"}} {
		to, absent := strayAddress(c.master, "lead", alive)
		if to != c.to || absent != c.absent {
			t.Errorf("%s: %s %s", fmt.Sprint(c), to, absent)
		}
	}
}
