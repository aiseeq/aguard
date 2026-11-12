package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/activity"
	"github.com/aiseeq/aguard/internal/ceiling"
	"github.com/aiseeq/aguard/internal/cpuwatch"
	"github.com/aiseeq/aguard/internal/iowatch"
	"github.com/aiseeq/aguard/internal/sigguard"
	"github.com/aiseeq/akit/session"
)

// Оба аварийных письма sigguard — об отказе сигнала и о неразобранных строках — в тишине доходят до лида.
func TestQuietSigguardLettersGoThrough(t *testing.T) {
	a, _ := quietObservedApp(t)
	a.cfgPath = filepath.Join(a.home, "missing.toml")
	writeQuietState(t, a, time.Now().Add(time.Hour))
	got := liveLead(t, a)
	a.sigJobs = &cpuwatch.Jobs{Dir: filepath.Join(a.home, "no-workers")}
	cfg := sigguard.Default()
	cfg.Lead = "lead"

	a.sigguardSend(&cfg, sigguard.Group{Count: 2, Lines: 1, Started: time.Now(), Denies: []sigguard.Deny{{
		Kind: "signal", Arg: "15", Count: 2, Unit: "run-test.service", Root: "999", PID: 2, Comm: "bash", PPID: 1,
		Pcomm: "systemd", Target: 4, TargetComm: "sleep", TargetUnit: "x.service", Mode: "enforce"}}})
	if msg := letter(t, got, "отказ сигнала"); !strings.Contains(msg, "aguard: авария") {
		t.Fatalf("письмо об отказе сигнала: %q", msg)
	}
	a.sigguardSend(&cfg, sigguard.Group{Unparsed: true, Raw: "что-то непонятное", Count: 1, Lines: 1, Started: time.Now()})
	if msg := letter(t, got, "неразобранные строки"); !strings.Contains(msg, "aguard: авария") {
		t.Fatalf("письмо о неразобранных строках: %q", msg)
	}
}

// ceilingQuietApp — app потолка в тишине: живые сессии заданы, журнал отдаёт одну жертву, письмо идёт настоящей отправкой.
func ceilingQuietApp(t *testing.T, alive map[string]session.Info) *app {
	t.Helper()
	a := quietTestApp(t)
	writeQuietState(t, a, time.Now().Add(time.Hour))
	a.cfgPath = filepath.Join(a.home, "missing.toml")
	a.log = slog.New(slog.DiscardHandler)
	a.act = activity.New(filepath.Join(a.home, "act"), time.Now())
	a.ceilingAlive, a.ceilingAliveAt = alive, time.Now().Add(time.Hour)
	a.ceilingJournalSource = func(context.Context, string, time.Time) ([]oomVictim, time.Time, error) {
		return []oomVictim{{PID: 1, Name: "grow", RSS: 1 << 30}}, time.Now(), nil
	}
	return a
}

// Письмо о снятии потолком в тишине доходит: сессия жива — ей самой.
func TestQuietCeilingLiveSessionLetterGoesThrough(t *testing.T) {
	sock, got := quietListener(t)
	a := ceilingQuietApp(t, map[string]session.Info{"demo": {Pid: os.Getpid(), Name: "demo", SocketPath: sock}})
	old := ceilingSetProperty
	ceilingSetProperty = func(string, int64) error { return nil }
	t.Cleanup(func() { ceilingSetProperty = old })
	slice, unit := filepath.Join(a.home, "cg"), "agent-demo-1.scope"
	reviewCeilingScope(t, slice, unit, "oom_kill 1\n")
	if err := ceiling.Write(ceiling.StatePath(a.home, unit), ceiling.State{Unit: unit, Base: 2 << 30, By: "cl", OOMSeen: true}); err != nil {
		t.Fatal(err)
	}
	for range 2 { // первый проход ставит событие в ожидание, второй шлёт
		if err := a.ceilingRound(context.Background(), slice); err != nil {
			t.Fatal(err)
		}
	}
	if msg := letter(t, got, "снятие потолком, сессия жива"); !strings.Contains(msg, "потолок памяти сессии demo") {
		t.Fatalf("письмо о снятии: %q", msg)
	}
}

// Письмо о снятии потолком в тишине доходит: scope исчез, сессии нет — лиду.
func TestQuietCeilingGoneLetterGoesThrough(t *testing.T) {
	sock, got := quietListener(t)
	a := ceilingQuietApp(t, map[string]session.Info{"lead": {Pid: os.Getpid(), Name: "lead", SocketPath: sock}})
	unit := "agent-demo-3.scope"
	path := ceiling.StatePath(a.home, unit)
	st := ceiling.State{Unit: unit, Base: 1 << 30, By: "cl", OOM: 2, OOMNotified: 1, MissingAt: time.Now().Add(-time.Hour)}
	if err := ceiling.Write(path, st); err != nil {
		t.Fatal(err)
	}
	a.ceilingGone(context.Background(), path, st)
	msg := letter(t, got, "снятие потолком, scope исчез")
	if !strings.Contains(msg, "потолок памяти сессии demo") || !strings.Contains(msg, "уже завершилась") {
		t.Fatalf("письмо о снятии лиду: %q", msg)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("состояние исчезнувшего scope не убрано после письма: %v", err)
	}
}

// Письмо о перегрузке в тишине не уходит и не считается отправленным: после её конца уходит обычным повтором.
func TestQuietCpuLetterWaitsForEnd(t *testing.T) {
	var logs strings.Builder
	a := cpuApp(t, &logs)
	writeQuietState(t, a, time.Now().Add(time.Hour))
	got := liveLead(t, a)
	cfg := cpuwatch.Default(a.home)
	cfg.Mode, cfg.Lead = cpuwatch.ModeAlert, "lead"
	h := cpuwatch.Host{Name: cpuwatch.HostLocal, Every: time.Minute}
	start := time.Now().Add(-time.Hour)
	act := func(i int) {
		at := start.Add(time.Duration(i) * time.Minute)
		v := h.Judge(cpuSample(at, 30, 30, map[string]int{"lead": 20}), cpuOwner)
		a.cpuAct(&cfg, &h, &v, at)
	}
	for i := range 12 {
		act(i)
	}
	noLetter(t, got, "перегрузка в тишине")
	if err := os.Remove(a.quietPath()); err != nil {
		t.Fatal(err)
	}
	act(12) // до repeat: письмо уходит только потому, что в тишине не отмечено отправленным
	if msg := letter(t, got, "перегрузка после тишины"); !strings.Contains(msg, "aguard: ноут перегружен") {
		t.Fatalf("письмо о перегрузке: %q", msg)
	}
}

// Письмо io в тишине не уходит.
func TestQuietIOLetterDropped(t *testing.T) {
	a, _ := quietObservedApp(t)
	writeQuietState(t, a, time.Now().Add(time.Hour))
	got := liveLead(t, a)
	cfg := iowatch.Default()
	cfg.Mode, cfg.Lead = iowatch.ModeAlert, "lead"
	a.ioSend(cfg, "lead", "срочно", "aguard io: фриз", 2500)
	noLetter(t, got, "io в тишине")
}

// aguard quiet -h — справка с тремя группами писем и строкой о busy --quiet, без ошибки.
func TestQuietHelp(t *testing.T) {
	a := quietTestApp(t)
	var out strings.Builder
	a.out = &out
	for _, arg := range []string{"-h", "--help"} {
		out.Reset()
		if err := a.quietCmd([]string{arg}); err != nil {
			t.Fatalf("%s: %v", arg, err)
		}
		for _, want := range []string{"сквозь неё (аварии): sigguard", "ceiling", "ждут её конца и уходят обычным повтором: queue, night, cpu, tmp, codex, disk, said",
			"пропадают: io, bgtask, remote, orphans", "busy --quiet — другое"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("%s: в справке нет %q:\n%s", arg, want, out.String())
			}
		}
	}
}

// Сломанный файл тишины аварийное письмо не держит: sigguard всё равно доходит.
func TestQuietBrokenStateDoesNotHoldUrgent(t *testing.T) {
	a, _ := quietObservedApp(t)
	a.cfgPath = filepath.Join(a.home, "missing.toml")
	if err := os.MkdirAll(a.dir("quiet"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.quietPath(), []byte("{сломано"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := liveLead(t, a)
	a.sigJobs = &cpuwatch.Jobs{Dir: filepath.Join(a.home, "no-workers")}
	cfg := sigguard.Default()
	cfg.Lead = "lead"
	a.sigguardSend(&cfg, sigguard.Group{Unparsed: true, Raw: "что-то непонятное", Count: 1, Lines: 1, Started: time.Now()})
	if msg := letter(t, got, "аварийное при сломанной тишине"); !strings.Contains(msg, "aguard: авария") {
		t.Fatalf("письмо: %q", msg)
	}
}
