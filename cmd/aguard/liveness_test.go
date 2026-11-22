package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/liveness"
	"github.com/aiseeq/aguard/internal/policy"
)

func TestLivenessRoundLifecycle(t *testing.T) {
	oldPIDAlive := liveness.PIDAlive
	liveness.PIDAlive = func(int) bool { return true }
	t.Cleanup(func() { liveness.PIDAlive = oldPIDAlive })
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	base := t.TempDir()
	statePath := filepath.Join(base, "producer.json")
	var sent []string
	makeApp := func() *app {
		return &app{home: base, stateDir: filepath.Join(base, "aguard"), now: func() time.Time { return now }, livenessSend: func(to, msg, kind string) error { sent = append(sent, kind+":"+to); return nil }}
	}
	cfg := liveness.Config{Mode: liveness.ModeAlert, Interval: policy.Duration(60 * time.Second), Watch: []liveness.Watch{{Name: "tlfeed", Path: statePath, MaxAge: policy.Duration(10 * time.Minute), Grace: policy.Duration(2 * time.Minute), To: []string{"aguard-master", "akit-master"}}}}
	write := func(heartbeat time.Time, lag int64) {
		t.Helper()
		b, _ := json.Marshal(liveness.State{Heartbeat: heartbeat, LastRowAt: heartbeat, LagBytes: lag, PID: 1})
		if err := os.WriteFile(statePath, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	a := makeApp()
	if err := a.livenessRound(&cfg, false); err != nil {
		t.Fatal(err)
	} // not yet seen
	if len(sent) != 0 {
		t.Fatalf("absent file sent %v", sent)
	}
	write(now, 10)
	if err := a.livenessRound(&cfg, false); err != nil {
		t.Fatal(err)
	} // seed
	if len(sent) != 0 {
		t.Fatalf("fresh file sent %v", sent)
	}
	now = now.Add(9 * time.Minute)
	if err := a.livenessRound(&cfg, false); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 0 {
		t.Fatalf("early alert: %v", sent)
	}
	now = now.Add(time.Minute + time.Second)
	if err := a.livenessRound(&cfg, false); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 {
		t.Fatalf("start alert count = %d, want 2: %v", len(sent), sent)
	}
	// Reload from the persisted state: no duplicate before the hourly repeat.
	a = makeApp()
	now = now.Add(59 * time.Minute)
	if err := a.livenessRound(&cfg, false); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 {
		t.Fatalf("restart duplicated alert: %v", sent)
	}
	now = now.Add(time.Minute + time.Second)
	if err := a.livenessRound(&cfg, false); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 4 {
		t.Fatalf("hourly repeat count = %d, want 4", len(sent))
	}
	write(now, 0)
	if err := a.livenessRound(&cfg, false); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 6 || sent[4] != "liveness.recovered:aguard-master" {
		t.Fatalf("recovery messages: %v", sent)
	}
	if err := a.livenessRound(&cfg, false); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 6 {
		t.Fatalf("extra tick repeated recovery email: %v", sent)
	}
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if err := a.livenessRound(&cfg, false); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 6 {
		t.Fatalf("missing file must wait grace before alerting: %v", sent)
	}
	now = now.Add(2 * time.Minute)
	if err := a.livenessRound(&cfg, false); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 8 {
		t.Fatalf("missing file after seen should alert: %v", sent)
	}
}

func TestLivenessLogModeDedupeAndRecovery(t *testing.T) {
	oldPIDAlive := liveness.PIDAlive
	liveness.PIDAlive = func(int) bool { return true }
	t.Cleanup(func() { liveness.PIDAlive = oldPIDAlive })
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	base, statePath := t.TempDir(), ""
	statePath = filepath.Join(base, "producer.json")
	var logbuf bytes.Buffer
	writes := 0
	newApp := func() *app {
		return &app{home: base, stateDir: filepath.Join(base, "aguard"), now: func() time.Time { return now }, out: io.Discard,
			log: slog.New(slog.NewTextHandler(&logbuf, nil)), livenessSend: func(string, string, string) error { writes++; return nil }}
	}
	cfg := liveness.Config{Mode: liveness.ModeLog, Watch: []liveness.Watch{{Name: "quiet-worker", Path: statePath, MaxAge: policy.Duration(10 * time.Minute), Grace: policy.Duration(2 * time.Minute), To: []string{"master"}}}}
	write := func(at time.Time) {
		t.Helper()
		b, _ := json.Marshal(liveness.State{Heartbeat: at, LastRowAt: at, PID: 1})
		if err := os.WriteFile(statePath, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(now.Add(-11 * time.Minute))
	a := newApp()
	if err := a.livenessRound(&cfg, false); err != nil {
		t.Fatal(err)
	}
	if err := a.livenessRound(&cfg, false); err != nil {
		t.Fatal(err)
	}
	if strings.Count(logbuf.String(), "написал бы") != 1 || writes != 0 {
		t.Fatalf("log mode output=%q emails=%d", logbuf.String(), writes)
	}
	a = newApp() // demon restart
	if err := a.livenessRound(&cfg, false); err != nil {
		t.Fatal(err)
	}
	if strings.Count(logbuf.String(), "написал бы") != 1 {
		t.Fatalf("stuck line repeated after restart: %q", logbuf.String())
	}
	write(now)
	if err := a.livenessRound(&cfg, false); err != nil {
		t.Fatal(err)
	}
	if err := a.livenessRound(&cfg, false); err != nil {
		t.Fatal(err)
	}
	if strings.Count(logbuf.String(), "восстановился") != 1 {
		t.Fatalf("recovery log count: %q", logbuf.String())
	}
	repeated := livenessRecord{Stuck: true, LoggedRecovery: true}
	if a.livenessRecovered(&cfg, cfg.Watch[0], &repeated, now, nil, nil) != true {
		t.Fatal("repeat recovery should complete without delivery in log mode")
	}
	if strings.Count(logbuf.String(), "восстановился") != 1 {
		t.Fatalf("recovery line repeated for an already logged episode: %q", logbuf.String())
	}
	if writes != 0 {
		t.Fatalf("log mode sent %d emails", writes)
	}
}

func TestLivenessDeliveryRetriesAndStopsAfterRecovery(t *testing.T) {
	oldPIDAlive := liveness.PIDAlive
	liveness.PIDAlive = func(int) bool { return true }
	t.Cleanup(func() { liveness.PIDAlive = oldPIDAlive })
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	base := t.TempDir()
	statePath := filepath.Join(base, "producer.json")
	counts := map[string]int{}
	a := &app{home: base, stateDir: filepath.Join(base, "aguard"), now: func() time.Time { return now }, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		livenessSend: func(_ string, _ string, kind string) error {
			counts[kind]++
			if counts[kind] == 1 {
				return errors.New("temporary delivery error")
			}
			return nil
		}}
	cfg := liveness.Config{Mode: liveness.ModeAlert, Watch: []liveness.Watch{{Name: "worker", Path: statePath, MaxAge: policy.Duration(10 * time.Minute), Grace: policy.Duration(2 * time.Minute), To: []string{"master"}}}}
	write := func(at time.Time) {
		t.Helper()
		b, _ := json.Marshal(liveness.State{Heartbeat: at, LastRowAt: at, PID: 1})
		if err := os.WriteFile(statePath, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(now.Add(-11 * time.Minute))
	for range 2 {
		if err := a.livenessRound(&cfg, false); err != nil {
			t.Fatal(err)
		}
	}
	if counts["liveness.stuck"] != 2 {
		t.Fatalf("stuck delivery attempts=%d, want retry after failed send", counts["liveness.stuck"])
	}
	write(now)
	for range 2 {
		if err := a.livenessRound(&cfg, false); err != nil {
			t.Fatal(err)
		}
	}
	if counts["liveness.recovered"] != 2 {
		t.Fatalf("recovery delivery attempts=%d, want retry after failed send", counts["liveness.recovered"])
	}
	if err := a.livenessRound(&cfg, false); err != nil {
		t.Fatal(err)
	}
	if counts["liveness.recovered"] != 2 {
		t.Fatalf("email repeated after recovery: %v", counts)
	}
}

func TestLivenessPIDGraceAndPersistedStart(t *testing.T) {
	alive := true
	oldPIDAlive := liveness.PIDAlive
	liveness.PIDAlive = func(int) bool { return alive }
	t.Cleanup(func() { liveness.PIDAlive = oldPIDAlive })
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	base, statePath := t.TempDir(), ""
	statePath = filepath.Join(base, "producer.json")
	sent := 0
	newApp := func() *app {
		return &app{home: base, stateDir: filepath.Join(base, "aguard"), now: func() time.Time { return now }, log: slog.New(slog.NewTextHandler(io.Discard, nil)), livenessSend: func(string, string, string) error { sent++; return nil }}
	}
	cfg := liveness.Config{Mode: liveness.ModeAlert, Watch: []liveness.Watch{{Name: "worker", Path: statePath, MaxAge: policy.Duration(10 * time.Minute), Grace: policy.Duration(2 * time.Minute), To: []string{"master"}}}}
	write := func() {
		t.Helper()
		b, _ := json.Marshal(liveness.State{Heartbeat: now, LastRowAt: now, PID: 42})
		if err := os.WriteFile(statePath, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	a := newApp()
	write()
	if err := a.livenessRound(&cfg, false); err != nil {
		t.Fatal(err)
	}
	alive = false
	now = now.Add(time.Minute)
	write()
	if err := a.livenessRound(&cfg, false); err != nil {
		t.Fatal(err)
	}
	alive = true
	now = now.Add(time.Minute)
	write()
	if err := a.livenessRound(&cfg, false); err != nil {
		t.Fatal(err)
	}
	if sent != 0 {
		t.Fatalf("transient dead pid sent email: %d", sent)
	}
	alive = false
	now = now.Add(time.Minute)
	write()
	if err := a.livenessRound(&cfg, false); err != nil {
		t.Fatal(err)
	}
	a = newApp() // transient start timestamp must be reloaded
	now = now.Add(time.Minute)
	write()
	if err := a.livenessRound(&cfg, false); err != nil {
		t.Fatal(err)
	}
	if sent != 0 {
		t.Fatalf("pid alert before grace: %d", sent)
	}
	now = now.Add(time.Minute)
	write()
	if err := a.livenessRound(&cfg, false); err != nil {
		t.Fatal(err)
	}
	if sent != 1 {
		t.Fatalf("pid alert at grace=%d, want 1", sent)
	}
}

func TestLivenessDryRunReportsNeverSeenFile(t *testing.T) {
	base := t.TempDir()
	var out bytes.Buffer
	a := &app{home: base, stateDir: filepath.Join(base, "aguard"), out: &out, now: time.Now}
	cfg := liveness.Config{Mode: liveness.ModeAlert, Watch: []liveness.Watch{{Name: "worker", Path: filepath.Join(base, "missing.json"), MaxAge: policy.Duration(time.Minute), Grace: policy.Duration(2 * time.Minute)}}}
	if err := a.livenessRound(&cfg, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "файла нет, ещё не появлялся") {
		t.Fatalf("dry-run output: %q", out.String())
	}
}

func TestLivenessFutureHeartbeat(t *testing.T) {
	oldPIDAlive := liveness.PIDAlive
	liveness.PIDAlive = func(int) bool { return true }
	t.Cleanup(func() { liveness.PIDAlive = oldPIDAlive })
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	state, _ := json.Marshal(liveness.State{Heartbeat: now.Add(11 * time.Minute), PID: 1})
	stuck, reason := liveness.Check(state, now, 10*time.Minute)
	if !stuck || reason != "heartbeat в будущем" {
		t.Fatalf("Check = (%t, %q)", stuck, reason)
	}
}

func TestLivenessInstantKindIgnoresPIDAndError(t *testing.T) {
	if livenessInstantKind("процесс pid 12 отсутствует") != livenessInstantKind("процесс pid 345 отсутствует") {
		t.Fatal("different pids must be one kind: a restart loop must not restart grace")
	}
	if livenessInstantKind("битый JSON состояния: unexpected EOF") != livenessInstantKind("битый JSON состояния: invalid character") {
		t.Fatal("different JSON errors must be one kind")
	}
	if livenessInstantKind("процесс pid 12 отсутствует") == livenessInstantKind("файл состояния пропал") {
		t.Fatal("pid and missing file are different kinds")
	}
}

// Загрузчик падает по кругу: каждый тик в файле новый мёртвый pid — отсрочка не начинается заново, тревога через grace.
func TestLivenessRestartLoopKeepsGrace(t *testing.T) {
	oldPIDAlive := liveness.PIDAlive
	liveness.PIDAlive = func(int) bool { return false }
	t.Cleanup(func() { liveness.PIDAlive = oldPIDAlive })
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	base := t.TempDir()
	statePath := filepath.Join(base, "producer.json")
	sent := 0
	a := &app{home: base, stateDir: filepath.Join(base, "aguard"), now: func() time.Time { return now }, log: slog.New(slog.NewTextHandler(io.Discard, nil)), livenessSend: func(string, string, string) error { sent++; return nil }}
	cfg := liveness.Config{Mode: liveness.ModeAlert, Watch: []liveness.Watch{{Name: "worker", Path: statePath, MaxAge: policy.Duration(10 * time.Minute), Grace: policy.Duration(2 * time.Minute), To: []string{"master"}}}}
	for i := range 3 {
		b, _ := json.Marshal(liveness.State{Heartbeat: now, LastRowAt: now, PID: 100 + i})
		if err := os.WriteFile(statePath, b, 0600); err != nil {
			t.Fatal(err)
		}
		if err := a.livenessRound(&cfg, false); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
	}
	if sent != 1 {
		t.Fatalf("restart loop with new pids must alert after grace once: sent %d", sent)
	}
}
