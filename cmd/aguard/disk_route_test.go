package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/disk"
	"github.com/aiseeq/akit/proc"
	"github.com/aiseeq/akit/session"
)

// liveNamed — живые сессии с сокетами: первая — pid теста, вторая — его родителя (реестр — по файлу на pid).
func liveNamed(t *testing.T, a *app, names ...string) map[string]<-chan string {
	t.Helper()
	a.reg = session.Registry{Dir: filepath.Join(t.TempDir(), "sessions"), ProcRoot: "/proc"}
	if err := os.MkdirAll(a.reg.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	out := map[string]<-chan string{}
	for i, name := range names {
		pid := []int{os.Getpid(), os.Getppid()}[i]
		st, err := proc.ReadStat("/proc", pid)
		if err != nil {
			t.Fatal(err)
		}
		sock := fmt.Sprintf("@aguard-test-%s-%d-%d", name, pid, time.Now().UnixNano())
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		info, err := json.Marshal(session.Info{Pid: pid, ProcStart: st.Start, Name: name, SocketPath: sock})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(a.reg.Dir, strconv.Itoa(pid)+".json"), info, 0o600); err != nil {
			t.Fatal(err)
		}
		got := make(chan string, 32)
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				b, _ := io.ReadAll(conn)
				got <- string(b)
				_ = conn.Close()
			}
		}()
		out[name] = got
	}
	return out
}

func diskRouteApp(t *testing.T) (*app, *bytes.Buffer) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir()) // нет тихого режима
	a, _ := testIOApp(t)
	var logged bytes.Buffer
	a.log = slog.New(slog.NewTextHandler(&logged, nil))
	if err := os.MkdirAll(a.dir("disk"), 0o700); err != nil {
		t.Fatal(err)
	}
	return a, &logged
}

func alertDiskConfig() disk.Config {
	cfg := disk.Default()
	cfg.Lead, cfg.Mode, cfg.FreeTell = "lead", disk.ModeAlert, []string{"git-admin"}
	return cfg
}

func letter(t *testing.T, ch <-chan string, who string) string {
	t.Helper()
	select {
	case m := <-ch:
		return m
	case <-time.After(5 * time.Second):
		t.Fatalf("письма %s нет", who)
	}
	return ""
}

func noLetter(t *testing.T, ch <-chan string, who string) {
	t.Helper()
	select {
	case m := <-ch:
		t.Fatalf("лишнее письмо %s: %s", who, m)
	case <-time.After(100 * time.Millisecond):
	}
}

// Приёмка (1) тикета и сценарий 2: запись выросла на 12 ГБ за 23 мин, хозяин жив — письмо ему (режим alert); через
// 10 мин ещё +100 ГБ, свободно 151 ГБ — второе письмо, несмотря на паузу пары, с нынешним остатком.
func TestDiskRouteFastLetterToLiveOwner(t *testing.T) {
	a, _ := diskRouteApp(t)
	got := liveNamed(t, a, "life-master", "lead")
	cfg := alertDiskConfig()
	st := &disk.State{}
	t0 := time.Now()
	p := filepath.Join(a.home, ".cache/claude-tmp/claude-1000/-home-u-git-life/8e689919")
	r := disk.Record{At: t0, Kind: disk.KindFast, Path: p, Bytes: 20 << 30, Delta: 12 << 30, Since: t0.Add(-23 * time.Minute),
		Owner: "life-master", Why: "session", Free: 250 << 30}
	if err := a.diskRoute(t.Context(), &cfg, st, []disk.Record{r}, t0); err != nil {
		t.Fatal(err)
	}
	if m := letter(t, got["life-master"], "хозяину"); !strings.Contains(m, "~/.cache/claude-tmp/claude-1000/-home-u-git-life/8e689919: +12 ГБ") {
		t.Fatalf("письмо хозяину: %s", m)
	}
	noLetter(t, got["lead"], "лиду")
	r.At, r.Bytes, r.Delta, r.Since, r.Free = t0.Add(10*time.Minute), 120<<30, 100<<30, t0, 151<<30
	if err := a.diskRoute(t.Context(), &cfg, st, []disk.Record{r}, r.At); err != nil {
		t.Fatal(err)
	}
	if m := letter(t, got["life-master"], "хозяину второе"); !strings.Contains(m, "свободно 151 ГБ") {
		t.Fatalf("второе письмо: %s", m)
	}
	r.At, r.Bytes, r.Delta, r.Free = t0.Add(20*time.Minute), 123<<30, 3<<30, 149<<30
	if err := a.diskRoute(t.Context(), &cfg, st, []disk.Record{r}, r.At); err != nil {
		t.Fatal(err)
	}
	noLetter(t, got["life-master"], "в паузе при малом росте")
}

// Сценарий 3 и хозяин не в сети: убыль без виновной записи — лиду, без выдуманной записи; хозяина нет среди живых —
// лиду с его именем; mode = log — ни одного письма, только журнал «написал бы».
func TestDiskRouteLeadLettersAndLogMode(t *testing.T) {
	a, logged := diskRouteApp(t)
	got := liveNamed(t, a, "lead")
	cfg := alertDiskConfig()
	st := &disk.State{}
	now := time.Now()
	recs := []disk.Record{
		{At: now, Kind: disk.KindFast, Delta: 6 << 30, Since: now.Add(-10 * time.Minute), Owner: "lead", Why: "lead", Free: 174 << 30},
		{At: now, Kind: disk.KindFast, Path: "/x/w", Bytes: 9 << 30, Delta: 6 << 30, Since: now.Add(-10 * time.Minute), Owner: "life-master", Free: 174 << 30},
	}
	if err := a.diskRoute(t.Context(), &cfg, st, recs, now); err != nil {
		t.Fatal(err)
	}
	first, second := letter(t, got["lead"], "лиду"), letter(t, got["lead"], "лиду о хозяине")
	if !strings.Contains(first, "убыло на 6.0 ГБ") || !strings.Contains(first, "виновника обход не нашёл") || strings.Contains(first, "\n- ") {
		t.Fatalf("письмо без виновника: %s", first)
	}
	if !strings.Contains(second, "Хозяин life-master не в сети — пишу лиду") || !strings.Contains(second, "/x/w: +6.0 ГБ") {
		t.Fatalf("письмо о хозяине не в сети: %s", second)
	}
	cfg.Mode = disk.ModeLog
	if err := a.diskRoute(t.Context(), &cfg, &disk.State{}, recs, now); err != nil {
		t.Fatal(err)
	}
	noLetter(t, got["lead"], "в режиме log")
	if !strings.Contains(logged.String(), "написал бы") {
		t.Fatalf("журнал log:\n%s", logged.String())
	}
}

// Сценарий 4: 20 stale одного хозяина в одном обходе — один тикет со всеми 20; при открытом тикете — одна заметка со
// всеми 20; до конца суток новые находки класса тикета не дают.
func TestDiskRouteStaleOneTicket(t *testing.T) {
	a, _ := diskRouteApp(t)
	proj := filepath.Join(a.home, "git", "dream")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	var recs []disk.Record
	for i := range 20 {
		recs = append(recs, disk.Record{At: now, Kind: disk.KindStale, Path: filepath.Join(a.home, ".cache", fmt.Sprintf("s%02d", i)),
			Bytes: 2 << 30, Since: now.Add(-72 * time.Hour), Owner: "dream-master", Why: "names"})
	}
	for _, tc := range []struct{ ls, want string }{
		{`[]`, proj + "|new disk-stale --prio MED"},
		{`[{"slug":"disk-stale","status":"открыт"}]`, proj + "|note disk-stale aguard disk: ещё 20:"},
	} {
		if err := os.Remove(filepath.Join(a.dir("disk"), "tickets.json")); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		cfg := alertDiskConfig()
		var calls string
		cfg.Tk, calls = fakeTk(t, t.TempDir(), tc.ls)
		if err := a.diskRoute(t.Context(), &cfg, &disk.State{}, recs, now); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(calls)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(string(b), proj+"|") != 1 || !strings.Contains(string(b), tc.want) {
			t.Fatalf("вызовы tk:\n%s\nждал один с %q", b, tc.want)
		}
		for i := range 20 {
			if !strings.Contains(string(b), fmt.Sprintf("~/.cache/s%02d (2.0 ГБ) не менялась с", i)) {
				t.Fatalf("нет s%02d:\n%s", i, b)
			}
		}
		more := []disk.Record{{At: now, Kind: disk.KindStale, Path: "/x/s99", Bytes: 2 << 30, Owner: "dream-master"}}
		if err := a.diskRoute(t.Context(), &cfg, &disk.State{}, more, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if b2, _ := os.ReadFile(calls); string(b2) != string(b) {
			t.Fatalf("второй тикет за сутки:\n%s", b2)
		}
	}
}

// Сценарий 5: след 3 ГБ мёртвой сессии, проект не определён — тикет в задачах лида с путём и размером; хозяин — лид —
// тикет в его домашней папке.
func TestDiskRouteTraceNoProjectToLead(t *testing.T) {
	a, _ := diskRouteApp(t)
	cfg := alertDiskConfig()
	var calls string
	cfg.Tk, calls = fakeTk(t, t.TempDir(), `[]`)
	now := time.Now()
	path := filepath.Join(a.home, ".cache/claude-tmp/claude-1000/-tmp-gone/0b9b0f0e-1111-2222-3333-444455556666")
	tr := disk.Trace{Path: path, Class: disk.ClassScratchpad, Kind: disk.TraceLeft, Owner: "gone-master", Bytes: 3 << 30, Born: now.Add(-50 * time.Hour)}
	recs := []disk.Record{
		{At: now, Kind: disk.KindTrace, Path: path, Bytes: 3 << 30, Since: tr.Born, Owner: "gone-master", Why: "names", Trace: &tr},
		{At: now, Kind: disk.KindGrew, Path: filepath.Join(a.home, ".cache/go-build"), Bytes: 165 << 30, Delta: 44 << 30, Since: now.Add(-24 * time.Hour), Owner: "lead", Why: "lead"},
	}
	if err := a.diskRoute(t.Context(), &cfg, &disk.State{}, recs, now); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, a.home+"|new disk-trace") || !strings.Contains(s, "тикет лиду: lead (за gone-master)") ||
		!strings.Contains(s, "~/.cache/claude-tmp/claude-1000/-tmp-gone/0b9b0f0e-1111-2222-3333-444455556666, 3.0 ГБ") {
		t.Fatalf("тикет следа лиду:\n%s", s)
	}
	if !strings.Contains(s, a.home+"|new disk-grew") || !strings.Contains(s, "~/.cache/go-build выросла на 44 ГБ") {
		t.Fatalf("тикет роста лиду:\n%s", s)
	}
}

// Сценарий 6 в демоне: ряд переживает перезапуск (forecast.json), письмо лиду и free_tell «~11 ч до 150 ГБ» — раз в
// сутки; строка журнала — только при смене вывода.
func TestDiskForecastLetterDailyAndLineOnChange(t *testing.T) {
	a, _ := diskRouteApp(t)
	got := liveNamed(t, a, "lead", "git-admin")
	cfg := alertDiskConfig()
	now := time.Now()
	st := &disk.ForecastState{Told: disk.FreeTold{}}
	for age := 6 * time.Hour; age > 0; age -= 5 * time.Minute {
		st.Series = append(st.Series, disk.Point{At: now.Add(-age), Bytes: int64((320 - 10*(6-age.Hours())) * (1 << 30))})
	}
	path := filepath.Join(a.dir("disk"), "forecast.json")
	if err := st.Save(path); err != nil {
		t.Fatal(err)
	}
	if err := a.diskForecast(&cfg, now, 260<<30); err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"lead", "git-admin"} {
		if m := letter(t, got[who], who); !strings.Contains(m, "~11 ч до 150 ГБ") {
			t.Fatalf("письмо %s: %s", who, m)
		}
	}
	if err := a.diskForecast(&cfg, now.Add(time.Minute), 260<<30-160<<20); err != nil {
		t.Fatal(err)
	}
	noLetter(t, got["lead"], "второе за сутки")
	var lines []string
	if _, err := disk.NewStore(a.dir("disk")).Read(now.Add(-time.Hour), func(r *disk.Record) {
		if r.Kind == disk.KindForecast {
			lines = append(lines, r.Text)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "~11 ч до 150 ГБ") {
		t.Fatalf("строки прогноза: %q", lines)
	}
	// Убыль встала: вывод сменился — строка «прогноза нет».
	if err := a.diskForecast(&cfg, now.Add(2*time.Hour), 260<<30); err != nil {
		t.Fatal(err)
	}
	lines = nil
	if _, err := disk.NewStore(a.dir("disk")).Read(now.Add(-time.Hour), func(r *disk.Record) {
		if r.Kind == disk.KindForecast {
			lines = append(lines, r.Text)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || !strings.HasPrefix(lines[1], "прогноза нет") {
		t.Fatalf("строки прогноза после остановки убыли: %q", lines)
	}
}

// mode = log: тикетов нет, в журнале «завёл бы» с классом и числом находок.
func TestDiskRouteLogModeTicketWould(t *testing.T) {
	a, logged := diskRouteApp(t)
	cfg := alertDiskConfig()
	cfg.Mode = disk.ModeLog
	var calls string
	cfg.Tk, calls = fakeTk(t, t.TempDir(), `[]`)
	now := time.Now()
	recs := []disk.Record{{At: now, Kind: disk.KindStale, Path: "/x/s", Bytes: 2 << 30, Owner: "lead", Why: "lead"}}
	if err := a.diskRoute(t.Context(), &cfg, &disk.State{}, recs, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		b, _ := os.ReadFile(calls)
		t.Fatalf("в режиме log tk вызван:\n%s", b)
	}
	if !strings.Contains(logged.String(), "aguard disk: завёл бы тикет") || !strings.Contains(logged.String(), "slug=disk-stale") {
		t.Fatalf("журнал:\n%s", logged.String())
	}
}
