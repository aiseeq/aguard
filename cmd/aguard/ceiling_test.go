package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/activity"
	"github.com/aiseeq/aguard/internal/ceiling"
	"github.com/aiseeq/akit/session"
)

func TestParseCeilingOOM(t *testing.T) {
	log := strings.Join([]string{
		`{"MESSAGE":"oom-kill:constraint=CONSTRAINT_MEMCG,oom_memcg=/user.slice/agents.slice/agent-demo-42.scope,task_memcg=/x","__REALTIME_TIMESTAMP":"1000000000"}`,
		`{"MESSAGE":"Memory cgroup out of memory: Killed process 99 (cc1) total-vm:100kB, anon-rss: 2048kB, file-rss:0kB","__REALTIME_TIMESTAMP":"1001000000"}`,
	}, "\n")
	got, _, err := parseCeilingOOMJSON(log, "agent-demo-42.scope", time.Time{})
	if err != nil || len(got) != 1 || got[0].PID != 99 || got[0].Name != "cc1" || got[0].RSS != 2<<20 {
		t.Fatalf("victims: %+v err=%v", got, err)
	}
	other, _, err := parseCeilingOOMJSON(log, "agent-other-3.scope", time.Time{})
	if err != nil || len(other) != 0 {
		t.Fatalf("wrong scope victims: %+v err=%v", other, err)
	}
}

func TestCeilingJournalTimestampFiltering(t *testing.T) {
	log := strings.Join([]string{
		`{"MESSAGE":"oom-kill: oom_memcg=/agents.slice/agent-demo-1.scope","__REALTIME_TIMESTAMP":"999000000"}`,
		`{"MESSAGE":"Memory cgroup out of memory: Killed process 10 (old) total-vm:1kB, anon-rss: 1kB","__REALTIME_TIMESTAMP":"999100000"}`,
		`{"MESSAGE":"oom-kill: oom_memcg=/agents.slice/agent-demo-1.scope","__REALTIME_TIMESTAMP":"1001000000"}`,
		`{"MESSAGE":"Memory cgroup out of memory: Killed process 11 (new) total-vm:1kB, anon-rss: 2kB","__REALTIME_TIMESTAMP":"1002000000"}`,
	}, "\n")
	victims, latest, err := parseCeilingOOMJSON(log, "agent-demo-1.scope", time.Unix(1000, 0))
	if err != nil || len(victims) != 1 || victims[0].Name != "new" || !latest.Equal(time.Unix(1002, 0)) {
		t.Fatalf("victims=%+v latest=%v err=%v", victims, latest, err)
	}
}

func reviewCeilingScope(t *testing.T, slice, unit, events string) {
	t.Helper()
	dir := filepath.Join(slice, unit)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"memory.stat":    "file 0\nshmem 0\n",
		"memory.current": "0\n",
		"memory.max":     "1073741824\n",
		"memory.events":  events,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCeilingNoticePersistedBeforePropertyFailure(t *testing.T) {
	home := t.TempDir()
	slice, unit := filepath.Join(home, "cg"), "agent-demo-1.scope"
	reviewCeilingScope(t, slice, unit, "oom_kill 1\n")
	statePath := ceiling.StatePath(home, unit)
	if err := ceiling.Write(statePath, ceiling.State{Unit: unit, Base: 2 << 30, By: "cl", OOMSeen: true}); err != nil {
		t.Fatal(err)
	}
	old := ceilingSetProperty
	ceilingSetProperty = func(string, int64) error { return errors.New("unit not loaded") }
	t.Cleanup(func() { ceilingSetProperty = old })
	sent := 0
	a := &app{home: home, log: slog.New(slog.DiscardHandler), act: activity.New(filepath.Join(home, "act"), time.Now()),
		ceilingAlive: map[string]session.Info{}, ceilingAliveAt: time.Now().Add(time.Hour),
		ceilingJournalSource: func(context.Context, string, time.Time) ([]oomVictim, time.Time, error) {
			return []oomVictim{{PID: 5, Name: "grow", RSS: 1 << 30}}, time.Now(), nil
		},
		ceilingNoticeSend: func(_ ceiling.State, _ []oomVictim, count, _, _ int64, _ map[string]session.Info, _ error) error {
			sent += int(count)
			return nil
		},
	}
	for range 3 {
		if err := a.ceilingRound(context.Background(), slice); err != nil {
			t.Fatal(err)
		}
	}
	if sent != 1 {
		t.Fatalf("one OOM counted %d times", sent)
	}
}

func TestCeilingGoneBoundsFailedNoticeAndUsesEventCount(t *testing.T) {
	home := t.TempDir()
	unit := "agent-demo-3.scope"
	path := ceiling.StatePath(home, unit)
	state := ceiling.State{Unit: unit, Base: 1 << 30, By: "cl", OOM: 7, OOMNotified: 4, MissingAt: time.Now().Add(-time.Hour)}
	if err := ceiling.Write(path, state); err != nil {
		t.Fatal(err)
	}
	queries, notices := 0, 0
	a := &app{home: home, log: slog.New(slog.DiscardHandler), act: activity.New(filepath.Join(home, "act"), time.Now()),
		ceilingAlive: map[string]session.Info{}, ceilingAliveAt: time.Now().Add(time.Hour),
		ceilingJournalSource: func(context.Context, string, time.Time) ([]oomVictim, time.Time, error) {
			queries++
			return []oomVictim{{PID: 5, Name: "grow", RSS: 1 << 30}}, time.Now(), nil
		},
		ceilingNoticeSend: func(_ ceiling.State, _ []oomVictim, count, _, _ int64, _ map[string]session.Info, _ error) error {
			notices += int(count)
			return errors.New("session unavailable")
		},
	}
	a.ceilingGone(context.Background(), path, state)
	if queries != 1 || notices != 3 {
		t.Fatalf("queries=%d notified count=%d", queries, notices)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old failed state retained: %v", err)
	}
}

func TestCeilingMissingAtClearedWhenScopeAppears(t *testing.T) {
	home := t.TempDir()
	unit := "agent-demo-2.scope"
	slice := filepath.Join(home, "cg")
	path := ceiling.StatePath(home, unit)
	state := ceiling.State{Unit: unit, Base: 1 << 30, By: "cl"}
	if err := ceiling.Write(path, state); err != nil {
		t.Fatal(err)
	}
	a := &app{home: home, log: slog.New(slog.DiscardHandler), act: activity.New(filepath.Join(home, "act"), time.Now()),
		ceilingAlive: map[string]session.Info{}, ceilingAliveAt: time.Now().Add(time.Hour)}
	if err := a.ceilingRound(context.Background(), slice); err != nil {
		t.Fatal(err)
	}
	reviewCeilingScope(t, slice, unit, "oom_kill 0\n")
	if err := a.ceilingRound(context.Background(), slice); err != nil {
		t.Fatal(err)
	}
	got, err := ceiling.Read(path)
	if err != nil || !got.MissingAt.IsZero() {
		t.Fatalf("MissingAt=%v err=%v", got.MissingAt, err)
	}
}

func TestParseCeilingOOMUsesScopePIDWithoutHeader(t *testing.T) {
	raw := `{"MESSAGE":"Memory cgroup out of memory: Killed process 42 (worker) total-vm:1kB, anon-rss: 4096kB","__REALTIME_TIMESTAMP":"2000000000"}`
	victims, _, err := parseCeilingOOMJSONForPIDs(raw, "agent-demo-1.scope", time.Time{}, map[int]bool{42: true})
	if err != nil || len(victims) != 1 || victims[0].Name != "worker" || victims[0].RSS != 4<<20 {
		t.Fatalf("victims=%+v err=%v", victims, err)
	}
	if _, _, err := parseCeilingOOMJSON(`{"MESSAGE":"Killed process bad (x) anon-rss: 1kB","__REALTIME_TIMESTAMP":"2000000000"}`, "agent-demo-1.scope", time.Time{}); err == nil {
		t.Fatal("malformed pid was silently ignored")
	}
}

func TestCeilingEventCountSendsWithoutJournalVictims(t *testing.T) {
	home := t.TempDir()
	slice, unit := filepath.Join(home, "cg"), "agent-count-1.scope"
	reviewCeilingScope(t, slice, unit, "oom_kill 4\n")
	path := ceiling.StatePath(home, unit)
	if err := ceiling.Write(path, ceiling.State{Unit: unit, Base: 100 << 20, By: "cl", OOMSeen: true, OOM: 3, OOMNotified: 3}); err != nil {
		t.Fatal(err)
	}
	count, victims := int64(0), -1
	a := &app{home: home, log: slog.New(slog.DiscardHandler), act: activity.New(filepath.Join(home, "act"), time.Now()),
		ceilingAlive: map[string]session.Info{}, ceilingAliveAt: time.Now().Add(time.Hour),
		ceilingJournalSource: func(context.Context, string, time.Time) ([]oomVictim, time.Time, error) { return nil, time.Time{}, nil },
		ceilingNoticeSend: func(_ ceiling.State, got []oomVictim, n, _, _ int64, _ map[string]session.Info, _ error) error {
			victims, count = len(got), n
			return nil
		},
	}
	if err := a.ceilingRound(context.Background(), slice); err != nil {
		t.Fatal(err)
	}
	state, err := ceiling.Update(path, func(state *ceiling.State) error {
		state.OOMPendingAt = time.Now().Add(-3 * time.Second)
		return nil
	})
	if err != nil || state.OOMPendingAt.IsZero() {
		t.Fatalf("set elapsed journal grace: %+v %v", state, err)
	}
	if err := a.ceilingRound(context.Background(), slice); err != nil {
		t.Fatal(err)
	}
	if count != 1 || victims != 0 {
		t.Fatalf("count=%d victims=%d", count, victims)
	}
}

func TestCeilingStateNotRewrittenWhenUnchanged(t *testing.T) {
	home := t.TempDir()
	unit := "agent-stable-8.scope"
	slice := filepath.Join(home, "cg")
	root := filepath.Join(slice, unit)
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"memory.stat":    "file 0\nshmem 0\n",
		"memory.current": "0\n",
		"memory.max":     "134217728\n",
		"memory.events":  "oom_kill 0\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	path := ceiling.StatePath(home, unit)
	if err := ceiling.Write(path, ceiling.State{Unit: unit, Base: 128 << 20, By: "cl"}); err != nil {
		t.Fatal(err)
	}
	a := &app{home: home, log: slog.New(slog.DiscardHandler), act: activity.New(filepath.Join(home, "act"), time.Now()),
		ceilingAlive: map[string]session.Info{}, ceilingAliveAt: time.Now().Add(time.Hour)}
	if err := a.ceilingRound(context.Background(), slice); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := a.ceilingRound(context.Background(), slice); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("unchanged state rewritten: before=%v after=%v err=%v", before.ModTime(), after.ModTime(), err)
	}
}

func TestCeilingRoundContinuesAfterScopeError(t *testing.T) {
	home := t.TempDir()
	stateDir := filepath.Join(home, ".local", "state", "aguard", "ceiling")
	slice := filepath.Join(home, "cg", "agents.slice")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, unit := range []string{"agent-a-1.scope", "agent-b-2.scope"} {
		if err := ceiling.Write(ceiling.StatePath(home, unit), ceiling.State{Unit: unit, Base: 200 << 20, Swap: ceilingState{}.Swap, By: "cl"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(slice, "agent-a-1.scope"), 0700); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(slice, "agent-b-2.scope")
	if err := os.MkdirAll(good, 0700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"memory.stat": "file 0\nshmem 0\n", "memory.current": "0\n", "memory.max": "100\n", "memory.events": "oom_kill 0\n"} {
		if err := os.WriteFile(filepath.Join(good, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var set []string
	old := ceilingSetProperty
	ceilingSetProperty = func(unit string, n int64) error { set = append(set, unit); return nil }
	t.Cleanup(func() { ceilingSetProperty = old })
	a := &app{home: home, stateDir: filepath.Join(home, ".local", "state", "aguard"), cgroupDir: filepath.Join(home, "cg", "agents.slice"), log: slog.Default(), act: activity.New(filepath.Join(home, "activity"), time.Now())}
	if err := a.ceilingRound(context.Background(), slice); err != nil {
		t.Fatal(err)
	}
	if len(set) != 1 || set[0] != "agent-b-2.scope" {
		t.Fatalf("scope failure interrupted pass: %v", set)
	}
}

func TestCeilingGoneSendsJournalNoticeBeforeRemovingState(t *testing.T) {
	home := t.TempDir()
	path := ceiling.StatePath(home, "agent-race-123.scope")
	st := ceiling.State{Unit: "agent-race-123.scope", Base: 1 << 30, Swap: 256 << 20, By: "cl", OOM: 1, OOMNotified: 0, MissingAt: time.Now().Add(-3 * time.Minute)}
	if err := ceiling.Write(path, st); err != nil {
		t.Fatal(err)
	}
	notified := false
	a := &app{
		home:           home,
		log:            slog.Default(),
		ceilingLimits:  map[string]int64{st.Unit: 2 << 30},
		ceilingShmem:   map[string]int64{st.Unit: 64 << 20},
		ceilingAlive:   map[string]session.Info{},
		ceilingAliveAt: time.Now(),
		ceilingJournalSource: func(_ context.Context, unit string, after time.Time) ([]oomVictim, time.Time, error) {
			if unit != st.Unit || !after.Before(time.Now()) {
				t.Fatalf("journal query unit=%s after=%v", unit, after)
			}
			return []oomVictim{{PID: 900, Name: "cc1", RSS: 8 << 20}}, time.Now(), nil
		},
		ceilingNoticeSend: func(got ceiling.State, victims []oomVictim, count, max, shmem int64, _ map[string]session.Info, _ error) error {
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("state disappeared before notice: %v", err)
			}
			if got.Unit != st.Unit || len(victims) != 1 || count != 1 || max != 2<<30 || shmem != 64<<20 {
				t.Fatalf("notice state=%+v victims=%+v count=%d max=%d shmem=%d", got, victims, count, max, shmem)
			}
			notified = true
			return nil
		},
	}
	a.ceilingGone(context.Background(), path, st)
	if !notified {
		t.Fatal("journal OOM notice was not delivered")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("state was not removed after notice: %v", err)
	}
}

func TestCeilingOOMClaudePIDRoutesToLead(t *testing.T) {
	to, killed := ceilingOOMRoute("demo", []oomVictim{{PID: 77, Name: "cc1"}}, map[string]session.Info{"demo": {Pid: 77}}, 77)
	if to != "lead" || !killed {
		t.Fatalf("route %q killed=%v", to, killed)
	}
	to, killed = ceilingOOMRoute("demo", []oomVictim{{PID: 78, Name: "claude"}}, map[string]session.Info{"demo": {Pid: 77}}, 77)
	if to != "demo" || killed {
		t.Fatalf("name-based false match: %q %v", to, killed)
	}
}

func TestCeilingPassNoteAtMostEveryTenMinutes(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	due, next := ceilingPassNext(start, time.Time{})
	if !due {
		t.Fatal("initial note not due")
	}
	for i := 1; i < 3000; i++ {
		now := start.Add(time.Duration(i) * 200 * time.Millisecond)
		due, next = ceilingPassNext(now, next)
		if due {
			t.Fatalf("extra pass note after %s", now.Sub(start))
		}
	}
	due, _ = ceilingPassNext(start.Add(10*time.Minute), next)
	if !due {
		t.Fatal("10 minute pass note missing")
	}
}

func TestCeilingUnlimitedScopeIsNotManaged(t *testing.T) {
	home := t.TempDir()
	unit := "agent-demo-55.scope"
	slice := filepath.Join(home, "cg", "agents.slice")
	root := filepath.Join(slice, unit)
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"memory.stat":    "file 0\nshmem 0\n",
		"memory.current": "0\n",
		"memory.max":     "max\n",
		"memory.events":  "oom_kill 0\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := ceiling.Write(ceiling.StatePath(home, unit), ceiling.State{Unit: unit, Base: 1 << 30, By: "cl"}); err != nil {
		t.Fatal(err)
	}
	procRoot := filepath.Join(home, "proc")
	if err := os.MkdirAll(filepath.Join(procRoot, "self"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(procRoot, "self/cgroup"), []byte("0::/agents.slice/"+unit+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var set int
	old := ceilingSetProperty
	ceilingSetProperty = func(string, int64) error { set++; return nil }
	t.Cleanup(func() { ceilingSetProperty = old })
	var out bytes.Buffer
	a := &app{home: home, procDir: procRoot, cgroupDir: filepath.Join(home, "cg"), out: &out,
		log: slog.New(slog.DiscardHandler), act: activity.New(filepath.Join(home, "act"), time.Now()),
		ceilingAlive: map[string]session.Info{}, ceilingAliveAt: time.Now().Add(time.Hour)}
	if err := a.ceilingRound(context.Background(), slice); err != nil {
		t.Fatal(err)
	}
	if set != 0 {
		t.Fatalf("unlimited scope got a ceiling: %d", set)
	}
	if err := a.ceilingCmd(nil); err != nil || strings.TrimSpace(out.String()) != "потолка нет" {
		t.Fatalf("output=%q err=%v", out.String(), err)
	}
}

func TestCeilingCommandNoScope(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, ".local", "state", "aguard")
	proc := filepath.Join(dir, "proc")
	if err := os.MkdirAll(filepath.Join(proc, "self"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proc, "self/cgroup"), []byte("0::/user.slice\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a := &app{out: &out, home: dir, stateDir: stateDir, procDir: proc, cgroupDir: filepath.Join(dir, "cg")}
	if err := a.ceilingCmd([]string{"8G"}); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "потолка нет" {
		t.Fatalf("output %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(a.stateDir, "ceiling")); !os.IsNotExist(err) {
		t.Fatalf("unexpected state: %v", err)
	}
}

func TestCeilingCommandRaisesAndDefersLowering(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, ".local", "state", "aguard", "ceiling")
	proc := filepath.Join(dir, "proc")
	scope := "agent-demo-123.scope"
	rel := "user.slice/agents.slice/" + scope
	if err := os.MkdirAll(filepath.Join(proc, "self"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proc, "self/cgroup"), []byte("0::/"+rel+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cg := filepath.Join(dir, "cg", rel)
	if err := os.MkdirAll(cg, 0700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"memory.max": "104857600\n", "memory.stat": "file 0\nshmem 0\n", "memory.current": "200000000\n"} {
		if err := os.WriteFile(filepath.Join(cg, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	statePath := ceiling.StatePath(dir, scope)
	seed := ceilingState{Unit: scope, Base: 80 << 20, Swap: 256 << 20, By: "cl"}
	if err := ceiling.Write(statePath, seed); err != nil {
		t.Fatal(err)
	}
	var calls []string
	old := ceilingSetProperty
	ceilingSetProperty = func(unit string, n int64) error { calls = append(calls, unit+":"+strconv.FormatInt(n, 10)); return nil }
	t.Cleanup(func() { ceilingSetProperty = old })
	var out bytes.Buffer
	a := &app{out: &out, home: dir, stateDir: filepath.Join(dir, ".local", "state", "aguard"), procDir: proc, cgroupDir: filepath.Join(dir, "cg")}
	if err := a.ceilingCmd([]string{"200M"}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] != scope+":"+strconv.FormatInt(ceiling.Target(200<<20, 0), 10) {
		t.Fatalf("raise calls %v", calls)
	}
	calls = nil
	if err := a.ceilingCmd([]string{"50M"}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Fatalf("lowering must be deferred, calls %v", calls)
	}
	got, err := ceiling.Read(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if got.Base != 50<<20 || got.By != "demo" || got.At.IsZero() {
		t.Fatalf("updated ceiling state %+v", got)
	}
}
