package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/ceiling"
)

// a.ceilingPIDs растёт на каждом проходе и не чистится.
func TestReview2PIDHistoryGrows(t *testing.T) {
	home := t.TempDir()
	slice := filepath.Join(home, "cg")
	unit := "agent-demo-7.scope"
	reviewScope(t, slice, unit, "oom_kill 0\n")
	if err := os.WriteFile(filepath.Join(slice, unit, "cgroup.procs"), []byte("11\n12\n13\n14\n15\n16\n17\n18\n19\n20\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ceiling.Write(ceiling.StatePath(home, unit), ceiling.State{Unit: unit, Base: 1 << 30, By: "cl"}); err != nil {
		t.Fatal(err)
	}
	a := reviewApp(home)
	a.ceilingJournalSource = func(context.Context, string, time.Time) ([]oomVictim, time.Time, error) { return nil, time.Time{}, nil }
	for range 1000 {
		if err := a.ceilingRound(context.Background(), slice); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("after 1000 passes (200 s at 200 ms) with 10 pids: len(ceilingPIDs)=%d", len(a.ceilingPIDs[unit]))
	if err := os.RemoveAll(filepath.Join(slice, unit)); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		cur, err := ceiling.Read(ceiling.StatePath(home, unit))
		if err != nil {
			break
		}
		cur.MissingCheckAt = cur.MissingCheckAt.Add(-2 * time.Second)
		a.ceilingGone(context.Background(), ceiling.StatePath(home, unit), cur)
	}
	_, gone := os.Stat(ceiling.StatePath(home, unit))
	t.Logf("state removed=%v; maps after scope gone: pids=%d baseline=%v cursors=%v", gone != nil, len(a.ceilingPIDs[unit]), len(a.ceilingBaseline), len(a.ceilingCursors))
	if len(a.ceilingPIDs[unit]) > 10 {
		t.Errorf("pid history unbounded: %d", len(a.ceilingPIDs[unit]))
	}
	if gone == nil || len(a.ceilingPIDs) != 0 || len(a.ceilingBaseline) != 0 || len(a.ceilingCursors) != 0 || len(a.ceilingLimits) != 0 || len(a.ceilingShmem) != 0 || len(a.ceilingNotified) != 0 || len(a.ceilingUnpersisted) != 0 || len(a.ceilingSeedRetries) != 0 {
		t.Errorf("scope maps survived removal: pids=%v baseline=%v cursors=%v limits=%v shmem=%v notified=%v unpersisted=%v retries=%v", a.ceilingPIDs, a.ceilingBaseline, a.ceilingCursors, a.ceilingLimits, a.ceilingShmem, a.ceilingNotified, a.ceilingUnpersisted, a.ceilingSeedRetries)
	}
}

func TestReview2SeedJournalRetriesAreRateLimited(t *testing.T) {
	home := t.TempDir()
	slice, unit := filepath.Join(home, "cg"), "agent-demo-8.scope"
	reviewScope(t, slice, unit, "oom_kill 0\n")
	if err := ceiling.Write(ceiling.StatePath(home, unit), ceiling.State{Unit: unit, Base: 1 << 30, By: "cl"}); err != nil {
		t.Fatal(err)
	}
	a := reviewApp(home)
	queries := 0
	a.ceilingJournalSource = func(context.Context, string, time.Time) ([]oomVictim, time.Time, error) {
		queries++
		return nil, time.Time{}, errors.New("journal unavailable")
	}
	for range 100 {
		if err := a.ceilingRound(context.Background(), slice); err != nil {
			t.Fatal(err)
		}
	}
	if queries != 1 || a.ceilingSeedRetries[unit].Attempts != 1 {
		t.Fatalf("seed journal retries were not delayed: queries=%d retries=%+v", queries, a.ceilingSeedRetries[unit])
	}
}
