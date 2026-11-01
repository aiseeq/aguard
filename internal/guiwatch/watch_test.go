package guiwatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
)

func TestFreshUserJournalFiles(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 11, 1, 12, 0, 0, 0, time.Local)
	files := []struct {
		name  string
		mtime time.Time
	}{
		{"user-1000-old.journal", start.Add(-time.Second)},
		{"user-1000-fresh.journal", start},
		{"user-1000@abcdef.journal", start.Add(time.Second)},
		{"system.journal", start.Add(time.Second)},
		{"user-no-uid.journal", start.Add(time.Second)},
	}
	for _, file := range files {
		path := filepath.Join(dir, file.name)
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, file.mtime, file.mtime); err != nil {
			t.Fatal(err)
		}
	}
	got, err := freshJournalFiles(dir, start, userJournalFile)
	if err != nil {
		t.Fatalf("select fresh journal files: %v", err)
	}
	want := []string{filepath.Join(dir, "user-1000-fresh.journal"), filepath.Join(dir, "user-1000@abcdef.journal")}
	if !slices.Equal(got, want) {
		t.Fatalf("fresh journal files = %v, want %v", got, want)
	}
}

func fixture(t *testing.T) (string, string, int, time.Time) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "out")
	pid := 4321
	base := filepath.Join(root, fmt.Sprint(pid))
	task := filepath.Join(base, "task", fmt.Sprint(pid))
	if err := os.MkdirAll(task, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(base, "stat"), statLine("S", 100))
	write(t, filepath.Join(base, "cmdline"), "/fake/victim\x00")
	write(t, filepath.Join(task, "stat"), statLine("S", 100))
	write(t, filepath.Join(task, "schedstat"), "0 0 0\n")
	write(t, filepath.Join(task, "wchan"), "ep_poll\n")
	write(t, filepath.Join(root, "thread-self", "schedstat"), "0 0 0\n")
	return root, dir, pid, time.Now()
}

func write(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}

func rewrite(t *testing.T, path, value string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString(value); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
}

func statLine(state string, start int) string {
	f := make([]string, 20)
	f[0], f[1], f[9], f[19] = state, "1", "0", fmt.Sprint(start)
	for i := range f {
		if f[i] == "" {
			f[i] = "0"
		}
	}
	return fmt.Sprintf("4321 (victim) %s\n", strings.Join(f, " "))
}

func setupState(t *testing.T, root, dir string, pid int) (*Watcher, *targetState) {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	watcher := New(root, dir, Default(), []Target{{Name: "victim", PID: pid, Main: true}})
	state, err := watcher.newState(Target{Name: "victim", PID: pid, Start: 100, Main: true})
	if err != nil {
		t.Fatal(err)
	}
	watcher.states[pid] = state
	t.Setenv("PATH", filepath.Dir(os.Args[0]))
	return watcher, state
}

func TestDStallProducesSlice(t *testing.T) {
	root, dir, pid, at := fixture(t)
	w, s := setupState(t, root, dir, pid)
	defer w.closeAll()
	statPath := filepath.Join(root, fmt.Sprint(pid), "task", fmt.Sprint(pid), "stat")
	if err := w.sample(s, at); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 31; i++ {
		rewrite(t, statPath, statLine("D", 100))
		if err := w.sample(s, at.Add(time.Duration(i)*50*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}
	if len(w.document.Slices) != 1 || w.document.Slices[0].Kind != "D" || w.document.Slices[0].DurationMS < 1000 {
		t.Fatalf("expected D slice >=1s, got %#v", w.document.Slices)
	}
}

func TestSleepingFutexAndEpollNeverTrigger(t *testing.T) {
	for _, channel := range []string{"ep_poll", "futex_wait_queue"} {
		t.Run(channel, func(t *testing.T) {
			root, dir, pid, at := fixture(t)
			w, s := setupState(t, root, dir, pid)
			defer w.closeAll()
			base := filepath.Join(root, fmt.Sprint(pid), "task", fmt.Sprint(pid))
			rewrite(t, filepath.Join(base, "wchan"), channel+"\n")
			if err := w.sample(s, at); err != nil {
				t.Fatal(err)
			}
			for i := 1; i <= 200; i++ {
				if err := w.sample(s, at.Add(time.Duration(i)*50*time.Millisecond)); err != nil {
					t.Fatal(err)
				}
			}
			if len(w.document.Slices) != 0 {
				t.Fatalf("sleeping S triggered a slice: %#v", w.document.Slices)
			}
			if got := s.ring[len(s.ring)-1].Wchan; got != channel {
				t.Fatalf("wchan=%q, want %q", got, channel)
			}
		})
	}
}

func TestPIDReuseCreatesFreshHistory(t *testing.T) {
	root, dir, pid, _ := fixture(t)
	w, _ := setupState(t, root, dir, pid)
	defer w.closeAll()
	old := w.states[pid]
	old.ring = []Sample{{At: time.Now()}}
	rewrite(t, filepath.Join(root, fmt.Sprint(pid), "stat"), statLine("S", 101))
	if err := w.discover(); err != nil {
		t.Fatal(err)
	}
	if w.states[pid] == old || w.states[pid].Start != 101 || len(w.states[pid].ring) != 0 {
		t.Fatalf("PID reuse inherited state: %#v", w.states[pid])
	}
}

func TestSuspendClosesEpisodeAtBoundary(t *testing.T) {
	root, dir, pid, at := fixture(t)
	w, s := setupState(t, root, dir, pid)
	defer w.closeAll()
	s.episode = &episode{start: at, end: at.Add(time.Second), kind: "D", detected: true}
	w.boottime, w.monotonic = time.Second, time.Second
	w.lastAny = at.Add(time.Second)
	w.clock = func() (time.Duration, time.Duration, error) { return 5 * time.Second, 2 * time.Second, nil }
	if err := w.step(at.Add(4 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(w.document.Slices) != 1 || !w.document.Slices[0].Suspend || w.document.Slices[0].DurationMS != 1000 {
		t.Fatalf("suspend slice boundary wrong: %#v", w.document.Slices)
	}
}

func TestSamplerGapAndThrottle(t *testing.T) {
	root, dir, _, at := fixture(t)
	w := New(root, dir, Default(), nil)
	w.now = func() time.Time { return at.Add(time.Second) }
	w.recordSamplerGap(at.Add(time.Second), 400*time.Millisecond)
	if got := w.document.Slices[0].Kind; got != "sampler_gap" {
		t.Fatalf("kind=%s", got)
	}
	w.Config.MaxPerHour = 1
	s := &targetState{Target: Target{Name: "x", PID: 9}}
	w.now = func() time.Time { return at.Add(time.Second) }
	e := &episode{start: at, end: at.Add(time.Second), kind: "stopped"}
	w.writeSlice(s, e)
	e2 := &episode{start: at.Add(2 * time.Second), end: at.Add(3 * time.Second), kind: "stopped"}
	w.writeSlice(&targetState{Target: Target{Name: "y", PID: 10}}, e2)
	if w.document.Suppressed != 2 {
		t.Fatalf("suppressed=%d, want 2 including sampler_gap", w.document.Suppressed)
	}
}

func TestGapJoinsStallIntervals(t *testing.T) {
	root, dir, pid, at := fixture(t)
	w, s := setupState(t, root, dir, pid)
	defer w.closeAll()
	w.judge(s, "stopped", at)
	w.judge(s, "stopped", at.Add(750*time.Millisecond))
	w.judge(s, "", at.Add(800*time.Millisecond))
	w.judge(s, "stopped", at.Add(1*time.Second))
	w.judge(s, "stopped", at.Add(1*time.Second+750*time.Millisecond))
	if s.episode == nil || !s.episode.detected {
		t.Fatal("two stop intervals with a short gap did not form an episode")
	}
	w.judge(s, "", at.Add(2100*time.Millisecond))
	w.judge(s, "", at.Add(7*time.Second))
	if len(w.document.Slices) != 1 || w.document.Slices[0].DurationMS < 1700 {
		t.Fatalf("gap should produce one joined slice: %#v", w.document.Slices)
	}
}

func TestSleepingSIsNeverBusy(t *testing.T) {
	if got := sampleKind("S", 50_000_000, 0, 50*time.Millisecond); got != "" {
		t.Fatalf("S classified as %q", got)
	}
}

func TestBusyAndQueueStallsProduceSlices(t *testing.T) {
	for _, tc := range []struct {
		name      string
		runDelta  uint64
		waitDelta uint64
		want      string
	}{
		{name: "busy", runDelta: 50_000_000, waitDelta: 0, want: "busy"},
		{name: "queue", runDelta: 5_000_000, waitDelta: 45_000_000, want: "queue"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, dir, pid, at := fixture(t)
			w, s := setupState(t, root, dir, pid)
			defer w.closeAll()
			base := filepath.Join(root, fmt.Sprint(pid), "task", fmt.Sprint(pid))
			rewrite(t, filepath.Join(base, "stat"), statLine("R", 100))
			if err := w.sample(s, at); err != nil {
				t.Fatal(err)
			}
			var run, wait uint64
			for i := 1; i <= 30; i++ {
				run += tc.runDelta
				wait += tc.waitDelta
				rewrite(t, filepath.Join(base, "schedstat"), fmt.Sprintf("%d %d 0\n", run, wait))
				if err := w.sample(s, at.Add(time.Duration(i)*50*time.Millisecond)); err != nil {
					t.Fatal(err)
				}
			}
			if len(w.document.Slices) != 1 {
				t.Fatalf("slices=%#v, want one", w.document.Slices)
			}
			got := w.document.Slices[0]
			if got.Kind != tc.want || got.DurationMS < 1400 || got.DurationMS > 1600 {
				t.Fatalf("slice=%#v, want %s around 1.5s", got, tc.want)
			}
		})
	}
}

func TestWatchUsesCurrentTimeInsteadOfTickerTimestamp(t *testing.T) {
	root, dir, _, at := fixture(t)
	w := New(root, dir, Default(), []Target{{Name: "victim", PID: 4321, Main: true}})
	ticks := make(chan time.Time, 2)
	w.newTicker = func(time.Duration) watchTick { return watchTick{C: ticks} }
	clockCalls := make(chan struct{}, 2)
	w.clock = func() (time.Duration, time.Duration, error) {
		clockCalls <- struct{}{}
		return 0, 0, nil
	}
	w.now = func() time.Time { return at.Add(2 * time.Second) }
	ticks <- at
	ticks <- at.Add(time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Watch(ctx) }()
	for range 2 {
		select {
		case <-clockCalls:
		case err := <-done:
			cancel()
			t.Fatalf("Watch exited early: %v", err)
		case <-time.After(time.Second):
			cancel()
			t.Fatal("Watch did not process ticker events")
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, s := range w.document.Slices {
		if s.Kind == "sampler_gap" {
			t.Fatalf("ticker timestamp produced a false sampler gap: %#v", s)
		}
	}
}

func TestClosingEpisodeDoesNotBlockNewOne(t *testing.T) {
	root, dir, pid, at := fixture(t)
	w, s := setupState(t, root, dir, pid)
	defer w.closeAll()
	w.Config.MinStall = policy.Duration(50 * time.Millisecond)
	w.judge(s, "D", at)
	w.judge(s, "D", at.Add(time.Second))
	w.judge(s, "", at.Add(time.Second+time.Millisecond))
	w.judge(s, "", at.Add(1400*time.Millisecond))
	if s.episode != nil {
		t.Fatal("first episode should be closing")
	}
	w.judge(s, "D", at.Add(1500*time.Millisecond))
	if s.episode == nil {
		t.Fatal("second episode was ignored during context")
	}
}

func TestQuietGapDoesNotExtendConfirmedEnd(t *testing.T) {
	root, dir, pid, at := fixture(t)
	w, s := setupState(t, root, dir, pid)
	defer w.closeAll()
	w.judge(s, "D", at)
	w.judge(s, "D", at.Add(time.Second))
	w.judge(s, "", at.Add(1100*time.Millisecond))
	w.judge(s, "", at.Add(1450*time.Millisecond))
	if len(s.closing) != 1 || !s.closing[0].end.Equal(at.Add(time.Second)) {
		t.Fatalf("end extended through gap: %#v", s.closing)
	}
}

func TestSamplerGapClosesAndResetsEpisode(t *testing.T) {
	root, dir, pid, at := fixture(t)
	w, s := setupState(t, root, dir, pid)
	defer w.closeAll()
	s.sampled = at.Add(time.Second)
	s.episode = &episode{start: at, end: at.Add(time.Second), kind: "D", detected: true}
	if err := w.recordSamplerGap(at.Add(2*time.Second), 400*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if s.episode != nil || !s.sampled.IsZero() {
		t.Fatalf("gap retained episode/baseline: %#v", s)
	}
}

func TestSleepStatisticsPersistWithoutSlices(t *testing.T) {
	root, dir, pid, at := fixture(t)
	w, s := setupState(t, root, dir, pid)
	defer w.closeAll()
	for i := 0; i <= 100; i++ {
		w.trackSleep(s, Sample{State: "S", Wchan: "hrtimer_nanosleep"}, at.Add(time.Duration(i)*50*time.Millisecond))
	}
	r := w.sleepCurrent[pid]
	w.finishSleep(r, at.Add(5*time.Second))
	if err := w.saveSleeps(); err != nil {
		t.Fatal(err)
	}
	d, err := ReadReport(dir, at.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Slices) != 0 || len(d.Sleeps) != 1 || d.Sleeps[0].Date != at.Format("2006-01-02") || d.Sleeps[0].LongestMS < 4900 || d.Sleeps[0].Over5SMS != 1 {
		t.Fatalf("sleep report: %#v", d)
	}
}

func TestPruneKeepsOpenEpisodeSliceByStableID(t *testing.T) {
	root, dir, pid, at := fixture(t)
	w, s := setupState(t, root, dir, pid)
	defer w.closeAll()
	now := at.Add(16 * 24 * time.Hour)
	w.now = func() time.Time { return now }
	w.document.Slices = []Slice{
		{ID: "expired", Target: "old", Start: at, End: at.Add(time.Hour)},
		{ID: "open", Target: "victim", PID: pid, Starttime: 100, Start: now.Add(-time.Hour), End: now.Add(-time.Second), Samples: []Sample{{AtMS: -5000, State: "D"}}},
	}
	e := &episode{start: now.Add(-time.Hour), end: now, kind: "D", detected: true, sliceID: "open"}
	s.ring = []Sample{{At: now, AtMS: 0, State: "D"}}
	if err := w.prune(); err != nil {
		t.Fatal(err)
	}
	if len(w.document.Slices) != 1 || w.document.Slices[0].ID != e.sliceID {
		t.Fatalf("prune invalidated open slice ID: %#v, episode=%q", w.document.Slices, e.sliceID)
	}
	if err := w.writeSlice(s, e); err != nil {
		t.Fatal(err)
	}
	if len(w.document.Slices) != 1 || w.document.Slices[0].ID != "open" || w.document.Slices[0].End.Before(now) {
		t.Fatalf("update missed the open slice after prune: %#v", w.document.Slices)
	}
}

func TestStormMergeRequiresSameStarttime(t *testing.T) {
	root, dir, _, at := fixture(t)
	w := New(root, dir, Default(), nil)
	w.now = func() time.Time { return at.Add(time.Second) }
	for _, st := range []uint64{100, 101} {
		e := &episode{start: at, end: at.Add(time.Second), kind: "D"}
		s := &targetState{Target: Target{Name: "victim", PID: 4321, Start: st}}
		if err := w.writeSlice(s, e); err != nil {
			t.Fatal(err)
		}
	}
	if len(w.document.Slices) != 2 {
		t.Fatalf("different process generations merged: %#v", w.document.Slices)
	}
}

func TestSuppressedCountedOncePerEpisode(t *testing.T) {
	root, dir, _, at := fixture(t)
	w := New(root, dir, Default(), nil)
	w.Config.MaxPerHour = 1
	w.now = func() time.Time { return at.Add(3 * time.Second) }
	for i := range 2 {
		w.document.Slices = append(w.document.Slices, Slice{Target: fmt.Sprint(i), Start: at, End: at.Add(time.Second)})
	}
	e := &episode{start: at, end: at.Add(time.Second), kind: "D"}
	s := &targetState{Target: Target{Name: "x", PID: 5, Start: 1}}
	_ = w.writeSlice(s, e)
	_ = w.writeSlice(s, e)
	if w.document.Suppressed != 1 {
		t.Fatalf("suppressed=%d", w.document.Suppressed)
	}
}

func TestSamplerGapUsesHourlyLimit(t *testing.T) {
	root, dir, _, at := fixture(t)
	w := New(root, dir, Default(), nil)
	w.Config.MaxPerHour = 1
	w.now = func() time.Time { return at }
	w.document.Slices = append(w.document.Slices, Slice{Start: at, End: at})
	if err := w.recordSamplerGap(at, 400*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if len(w.document.Slices) != 1 || w.document.Suppressed != 1 {
		t.Fatalf("gap bypassed cap: %#v", w.document)
	}
}

func TestSuspendKeepsClosingEndAndResetsBasis(t *testing.T) {
	root, dir, pid, at := fixture(t)
	w, s := setupState(t, root, dir, pid)
	defer w.closeAll()
	e := &episode{start: at, end: at.Add(time.Second), kind: "D", detected: true, closing: true}
	s.closing = []*episode{e}
	s.sampled = at.Add(3 * time.Second)
	if err := w.finishAll(at.Add(20*time.Second), true); err != nil {
		t.Fatal(err)
	}
	if !w.document.Slices[0].End.Equal(e.end) || w.document.Slices[0].Suspend || !s.sampled.IsZero() {
		t.Fatalf("closing episode changed at suspend: %#v", w.document.Slices[0])
	}
}

func TestLongEpisodeUpdateRetainsSamplesPastRing(t *testing.T) {
	root, dir, _, at := fixture(t)
	w := New(root, dir, Default(), nil)
	w.now = func() time.Time { return at.Add(40 * time.Second) }
	old := Slice{ID: "stable", Target: "v", PID: 1, Starttime: 8, Start: at, End: at.Add(time.Second), Samples: []Sample{{AtMS: -5000, State: "D"}}}
	w.document.Slices = []Slice{old}
	e := &episode{start: at, end: at.Add(40 * time.Second), kind: "D", detected: true, sliceID: "stable"}
	s := &targetState{Target: Target{Name: "v", PID: 1, Start: 8}, ring: []Sample{{At: at.Add(39 * time.Second), AtMS: 39000, State: "D"}}}
	if err := w.writeSlice(s, e); err != nil {
		t.Fatal(err)
	}
	if len(w.document.Slices[0].Samples) != 2 || w.document.Slices[0].Samples[0].AtMS != -5000 {
		t.Fatalf("old context lost: %#v", w.document.Slices[0].Samples)
	}
}

func TestMaxBytesCountsOnDiskFilesAndPrettyJSON(t *testing.T) {
	t.Run("other files count", func(t *testing.T) {
		root, dir, _, at := fixture(t)
		w := New(root, dir, Default(), nil)
		w.now = func() time.Time { return at }
		w.document.Slices = []Slice{{Target: "v", Kind: "D", Start: at, End: at}}
		w.document.Slices[0].ID = "bounded"
		pretty, err := json.MarshalIndent(w.document.Slices[0], "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		journal := strings.Repeat("j", 300)
		if err := os.MkdirAll(filepath.Join(dir, "slices"), 0700); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(dir, "slices", "bounded.json"), string(pretty))
		write(t, filepath.Join(dir, "journal-extra.json"), journal)
		w.Config.MaxBytes = int64(len(pretty) + len(journal) - 1)
		if err := w.prune(); err != nil {
			t.Fatal(err)
		}
		if len(w.document.Slices) != 0 {
			t.Fatalf("slice retained when pretty JSON plus journal exceeds cap: %#v", w.document.Slices)
		}
	})

	t.Run("pretty JSON counts", func(t *testing.T) {
		root, dir, _, at := fixture(t)
		w := New(root, dir, Default(), nil)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		w.now = func() time.Time { return at }
		w.document.Slices = []Slice{{ID: "pretty", Target: "v", Kind: "D", Start: at, End: at}}
		pretty, err := json.MarshalIndent(w.document.Slices[0], "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		compact, err := json.Marshal(w.document.Slices[0])
		if err != nil {
			t.Fatal(err)
		}
		if len(pretty) <= len(compact)+10 {
			t.Fatalf("fixture lacks useful pretty/compact boundary: %d/%d", len(pretty), len(compact))
		}
		if err := os.MkdirAll(filepath.Join(dir, "slices"), 0700); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(dir, "slices", "pretty.json"), string(pretty))
		w.Config.MaxBytes = int64(len(compact) + (len(pretty)-len(compact))/2)
		if err := w.prune(); err != nil {
			t.Fatal(err)
		}
		if len(w.document.Slices) != 0 {
			t.Fatalf("slice retained when pretty JSON exceeds cap: %#v", w.document.Slices)
		}
	})
}

func TestMaxBytesDoesNotCountReplacedSlicesJSONTwice(t *testing.T) {
	_, dir, _, at := fixture(t)
	w := New("/proc", dir, Default(), nil)
	w.now = func() time.Time { return at }
	w.document.Slices = []Slice{{ID: "current", Target: "v", Kind: "D", Start: at, End: at}}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	pretty, err := json.MarshalIndent(w.document.Slices[0], "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	journal := strings.Repeat("j", 200)
	oldSlices := strings.Repeat("o", len(pretty)+100)
	write(t, filepath.Join(dir, "slices.json"), oldSlices)
	if err := os.MkdirAll(filepath.Join(dir, "slices"), 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "slices", "current.json"), string(pretty))
	write(t, filepath.Join(dir, "journal-current.json"), journal)
	w.Config.MaxBytes = int64(len(pretty) + len(journal))
	if w.Config.MaxBytes != int64(len(pretty)+len(journal)) {
		t.Fatal("test fixture cap must equal stored slice and journal size")
	}
	if err := w.save(); err != nil {
		t.Fatal(err)
	}
	if len(w.document.Slices) != 1 {
		t.Fatalf("new document was pruned using replaced slices.json size: %#v", w.document.Slices)
	}
}

func TestSnapshotCapturesPSIErrors(t *testing.T) {
	root, _, _, at := fixture(t)
	w := New(root, filepath.Join(root, "out"), Default(), nil)
	s := &targetState{Target: Target{Name: "v", PID: 4321, Start: 100}}
	item := w.makeSlice(s, &episode{start: at, end: at.Add(time.Second), kind: "D"}, false)
	if len(item.Errors) < 3 {
		t.Fatalf("PSI errors hidden: %#v", item.Errors)
	}
}

func TestTopBaselineRejectsReusedPID(t *testing.T) {
	root, dir, _, at := fixture(t)
	w := New(root, dir, Default(), nil)
	w.now = func() time.Time { return at }
	if err := w.updateTop(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "4321", "stat")
	rewrite(t, p, statLine("S", 101))
	w.now = func() time.Time { return at.Add(time.Second) }
	if err := w.updateTop(); err != nil {
		t.Fatal(err)
	}
	if _, ok := w.topDelta[4321]; ok {
		t.Fatal("reused PID contributed CPU delta")
	}
}

func TestConcurrentWritersMergeSlicesUnderLock(t *testing.T) {
	root, dir, _, at := fixture(t)
	w1 := New(root, dir, Default(), nil)
	w2 := New(root, dir, Default(), nil)
	w1.now = func() time.Time { return at }
	w2.now = w1.now
	for i, w := range []*Watcher{w1, w2} {
		w.document.Slices = []Slice{{ID: fmt.Sprint("id", i), Target: fmt.Sprint(i), Start: at, End: at}}
		if err := w.save(); err != nil {
			t.Fatal(err)
		}
	}
	d, err := ReadReport(dir, at.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Slices) != 2 {
		t.Fatalf("concurrent write lost slice: %#v", d.Slices)
	}
}

func TestMaxBytesRemovesOldestSliceFromOtherWatcher(t *testing.T) {
	_, dir, _, at := fixture(t)
	first := New("/proc", dir, Default(), nil)
	second := New("/proc", dir, Default(), nil)
	first.now = func() time.Time { return at.Add(time.Minute) }
	second.now = func() time.Time { return at }
	second.document.Slices = []Slice{{ID: "other-old", Target: "other", PID: 2, Kind: "D", Start: at.Add(-time.Hour), End: at.Add(-time.Minute)}}
	if err := second.save(); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "journal-other-old.json"), strings.Repeat("j", 128))
	first.document.Slices = []Slice{{ID: "first-new", Target: "first", PID: 1, Kind: "D", Start: at, End: at}}
	if err := first.save(); err != nil {
		t.Fatal(err)
	}
	first.document.Slices = append(first.document.Slices, second.document.Slices[0])
	total, files, err := first.storedFileSizes()
	if err != nil {
		t.Fatal(err)
	}
	oldSize := files["other-old"]
	info, err := os.Stat(filepath.Join(dir, "journal-other-old.json"))
	if err != nil {
		t.Fatal(err)
	}
	first.Config.MaxBytes = total - oldSize - info.Size()
	first.lastMaxBytesCheck = time.Time{}
	if err := first.prune(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(slicePath(dir, "other-old")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old slice from second watcher remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "journal-other-old.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old slice journal remains: %v", err)
	}
	if len(first.document.Slices) != 1 || first.document.Slices[0].ID != "first-new" {
		t.Fatalf("memory did not drop the removed foreign slice: %#v", first.document.Slices)
	}
}

func TestConfigReloadRequestsRestart(t *testing.T) {
	root, dir, _, at := fixture(t)
	w := New(root, dir, Default(), nil)
	w.clock = func() (time.Duration, time.Duration, error) { return time.Second, time.Second, nil }
	changed := Default()
	changed.Interval = policy.Duration(100 * time.Millisecond)
	w.ReloadConfig = func() (Config, error) { return changed, nil }
	if err := w.checkClock(at); !errors.Is(err, ErrConfigChanged) {
		t.Fatalf("config change error=%v", err)
	}
}

func TestReportReturnsCorruptFileError(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "slices"), 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "slices", "broken.json"), "{")
	if _, err := ReadReport(dir, time.Time{}); err == nil {
		t.Fatal("corrupt report silently accepted")
	}
}

func TestLoadMigratesLegacySlicesAndSuppressed(t *testing.T) {
	_, dir, _, at := fixture(t)
	legacy := Document{Version: 1, Suppressed: 4, Slices: []Slice{{ID: "legacy", Target: "old", Start: at, End: at.Add(time.Second)}}}
	b, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "slices.json"), string(b))
	w := New("/proc", dir, Default(), nil)
	if err := w.load(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "slices.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy file remains: %v", err)
	}
	d, err := ReadReport(dir, at.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Slices) != 1 || d.Slices[0].ID != "legacy" || d.Suppressed != 4 {
		t.Fatalf("migrated report = %#v", d)
	}
	if err := New("/proc", dir, Default(), nil).load(); err != nil {
		t.Fatalf("second startup: %v", err)
	}
}

func TestInterruptedLegacyMigrationIsIdempotentAndReportDeduplicates(t *testing.T) {
	_, dir, _, at := fixture(t)
	legacy := Document{Version: 1, Slices: []Slice{
		{Target: "old", PID: 42, Kind: "sampler_gap", Start: at, End: at.Add(time.Second)},
		{Target: "old", PID: 42, Kind: "sampler_gap", Start: at.Add(time.Minute), End: at.Add(time.Minute + time.Second)},
	}}
	b, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "slices.json"), string(b))
	if err := os.MkdirAll(filepath.Join(dir, "slices"), DirectoryMode); err != nil {
		t.Fatal(err)
	}
	first := legacy.Slices[0]
	first.ID = stableSliceID(first)
	if err := writeJSONAtomic(slicePath(dir, first.ID), first); err != nil {
		t.Fatal(err)
	}
	before, err := ReadReport(dir, at.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Slices) != len(legacy.Slices) {
		t.Fatalf("report before retry has %d slices, want %d", len(before.Slices), len(legacy.Slices))
	}
	if err := New("/proc", dir, Default(), nil).load(); err != nil {
		t.Fatal(err)
	}
	after, err := ReadReport(dir, at.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Slices) != len(legacy.Slices) {
		t.Fatalf("report after retry has %d slices, want %d", len(after.Slices), len(legacy.Slices))
	}
	if after.Slices[0].ID != stableSliceID(legacy.Slices[0]) || after.Slices[1].ID != stableSliceID(legacy.Slices[1]) {
		t.Fatalf("migration IDs are not deterministic: %#v", after.Slices)
	}
}

func TestPruneRemovesExpiredSliceAndJournalFiles(t *testing.T) {
	_, dir, _, at := fixture(t)
	w := New("/proc", dir, Default(), nil)
	w.now = func() time.Time { return at.Add(15 * 24 * time.Hour) }
	w.document.Slices = []Slice{{ID: "expired", Target: "old", Start: at, End: at.Add(time.Hour)}}
	if err := os.MkdirAll(filepath.Join(dir, "slices"), 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "slices", "expired.json"), `{}`)
	write(t, filepath.Join(dir, "journal-expired.json"), `[]`)
	if err := w.prune(); err != nil {
		t.Fatal(err)
	}
	if len(w.document.Slices) != 0 {
		t.Fatalf("expired slice retained: %#v", w.document.Slices)
	}
	for _, p := range []string{filepath.Join(dir, "slices", "expired.json"), filepath.Join(dir, "journal-expired.json")} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s remains: %v", p, err)
		}
	}
}

func TestPruneChecksMaxBytesAtMostOncePerMinute(t *testing.T) {
	_, dir, _, at := fixture(t)
	w := New("/proc", dir, Default(), nil)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	now := at
	w.now = func() time.Time { return now }
	if err := w.save(); err != nil {
		t.Fatal(err)
	}
	if !w.lastMaxBytesCheck.Equal(at) {
		t.Fatalf("first max_bytes check at %s, want %s", w.lastMaxBytesCheck, at)
	}

	now = at.Add(30 * time.Second)
	if err := w.save(); err != nil {
		t.Fatal(err)
	}
	if !w.lastMaxBytesCheck.Equal(at) {
		t.Fatalf("max_bytes check repeated before a minute: %s", w.lastMaxBytesCheck)
	}

	now = at.Add(time.Minute)
	if err := w.save(); err != nil {
		t.Fatal(err)
	}
	if !w.lastMaxBytesCheck.Equal(now) {
		t.Fatalf("max_bytes check not repeated after a minute: %s", w.lastMaxBytesCheck)
	}
}

func TestSamplerGapUsesThreadSelfWaitDelta(t *testing.T) {
	root, dir, _, at := fixture(t)
	w := New(root, dir, Default(), nil)
	w.clock = func() (time.Duration, time.Duration, error) { return time.Second, time.Second, nil }
	path := filepath.Join(root, "thread-self", "schedstat")
	rewrite(t, path, "0 1000000 0\n")
	if err := w.checkClock(at); err != nil {
		t.Fatal(err)
	}
	rewrite(t, path, "0 3000000 0\n")
	w.clock = func() (time.Duration, time.Duration, error) { return 2 * time.Second, 2 * time.Second, nil }
	if err := w.checkClock(at.Add(400 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if len(w.document.Slices) != 1 || w.document.Slices[0].SelfWaitMS != 2 {
		t.Fatalf("self wait delta=%#v", w.document.Slices)
	}
	rewrite(t, path, "malformed\n")
	w.lastAny = at.Add(time.Second)
	if err := w.checkClock(at.Add(time.Second)); err == nil {
		t.Fatal("schedstat read error was hidden")
	}
}

func TestJournalCollectionDoesNotBlockCaller(t *testing.T) {
	root, dir, _, at := fixture(t)
	w := New(root, dir, Default(), nil)
	fake := filepath.Join(root, "bin")
	if err := os.MkdirAll(fake, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(fake, "journalctl"), "#!/bin/sh\n/bin/sleep 5\n")
	if err := os.Chmod(filepath.Join(fake, "journalctl"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fake)
	started := time.Now()
	w.queueJournal("async", at, at.Add(time.Second))
	if time.Since(started) > 100*time.Millisecond {
		t.Fatal("journal command blocked sampler")
	}
}

func TestMergedEpisodeKeepsCounterAndSamples(t *testing.T) {
	root, dir, _, at := fixture(t)
	w := New(root, dir, Default(), nil)
	w.now = func() time.Time { return at.Add(10 * time.Second) }
	old := Slice{ID: "original", Target: "victim", PID: 4321, Starttime: 100, Start: at, End: at.Add(time.Second), Samples: []Sample{{AtMS: -1000, State: "D"}}}
	w.document.Slices = []Slice{old}
	s := &targetState{Target: Target{Name: "victim", PID: 4321, Start: 100}, ring: []Sample{{At: at.Add(2 * time.Second), State: "D"}}}
	e := &episode{start: at.Add(2 * time.Second), end: at.Add(3 * time.Second), kind: "D"}
	if err := w.writeSlice(s, e); err != nil {
		t.Fatal(err)
	}
	got := w.document.Slices[0]
	if got.Merged != 1 || len(got.Samples) != 2 || got.ID != "original" {
		t.Fatalf("merge lost history or count: %#v", got)
	}
}

func TestMergedCountSurvivesLaterSliceUpdateOnDisk(t *testing.T) {
	root, dir, _, at := fixture(t)
	w := New(root, dir, Default(), nil)
	w.now = func() time.Time { return at.Add(10 * time.Second) }
	fakeBin := filepath.Join(root, "bin")
	if err := os.MkdirAll(fakeBin, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(fakeBin, "journalctl"), "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(filepath.Join(fakeBin, "journalctl"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin)
	old := Slice{ID: "original", Target: "victim", PID: 4321, Starttime: 100, Start: at, End: at.Add(time.Second)}
	w.document.Slices = []Slice{old}
	s := &targetState{Target: Target{Name: "victim", PID: 4321, Start: 100}, ring: []Sample{{At: at.Add(2 * time.Second), State: "D"}}}
	merged := &episode{start: at.Add(2 * time.Second), end: at.Add(3 * time.Second), kind: "D"}
	if err := w.writeSlice(s, merged); err != nil {
		t.Fatal(err)
	}
	update := &episode{start: at.Add(2 * time.Second), end: at.Add(4 * time.Second), kind: "D", detected: true, sliceID: merged.sliceID}
	s.ring = append(s.ring, Sample{At: at.Add(4 * time.Second), AtMS: 4000, State: "D"})
	if err := w.writeSlice(s, update); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "slices", "original.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved Slice
	if err := json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Merged != 1 {
		t.Fatalf("later update lost merged counter on disk: %#v", saved)
	}
}

func TestTargetSummaryReportedOnDiscovery(t *testing.T) {
	root, dir, pid, _ := fixture(t)
	w, s := setupState(t, root, dir, pid)
	s.close()
	w.states = map[int]*targetState{}
	var line string
	var missing []string
	w.OnTargets = func(summary string, m []string) { line, missing = summary, m }
	if err := w.discover(); err != nil {
		t.Fatal(err)
	}
	// заданные -pid цели: строка о них, свои цели (kwin…) не «отсутствуют»
	if line != fmt.Sprintf("цели: victim pid %d", pid) || len(missing) != 0 {
		t.Fatalf("заданные цели: %q, отсутствуют %v", line, missing)
	}
	own := New(root, dir, Default(), nil)
	defer own.closeAll()
	own.OnTargets = func(summary string, m []string) { line, missing = summary, m }
	if err := own.discover(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "цели:") || !strings.Contains(line, "рендереров 0") || !strings.Contains(line, "kwin отсутствует") || len(missing) != 4 {
		t.Fatalf("свои цели: %q, отсутствуют %v", line, missing)
	}
}

func TestTargetSummaryGivenPIDGone(t *testing.T) {
	root, dir, _, _ := fixture(t)
	w := New(root, dir, Default(), []Target{{Name: "gone", PID: 999999, Main: true}})
	defer w.closeAll()
	var line string
	var missing []string
	w.OnTargets = func(summary string, m []string) { line, missing = summary, m }
	if err := w.discover(); err != nil {
		t.Fatal(err)
	}
	if line != "цели: gone отсутствует" || len(missing) != 1 || missing[0] != "gone" {
		t.Fatalf("исчезнувшая заданная цель: %q, отсутствуют %v", line, missing)
	}
}

func TestTopScanAlsoDiscoversTargets(t *testing.T) {
	root, dir, pid, _ := fixture(t)
	rewrite(t, filepath.Join(root, fmt.Sprint(pid), "stat"), strings.Replace(statLine("S", 100), "(victim)", "(kwin_wayland)", 1))
	w := New(root, dir, Default(), nil)
	defer w.closeAll()
	_, _, _, targets, err := w.readTopProcesses(true)
	if err != nil {
		t.Fatal(err)
	}
	if got := targets[pid]; got.Name != "kwin" || got.Start != 100 {
		t.Fatalf("top scan target=%+v, want kwin with starttime 100", got)
	}
}

func procFixtureStat(pid, ppid, start int, comm string) []byte {
	fields := make([]string, 20)
	fields[0], fields[1], fields[19] = "S", fmt.Sprint(ppid), fmt.Sprint(start)
	for i := range fields {
		if fields[i] == "" {
			fields[i] = "0"
		}
	}
	return fmt.Appendf(nil, "%d (%s) %s\n", pid, comm, strings.Join(fields, " "))
}

func addChromeFixture(t *testing.T, root string, pid, ppid, start int, args ...string) {
	t.Helper()
	base := filepath.Join(root, fmt.Sprint(pid))
	write(t, filepath.Join(base, "stat"), string(procFixtureStat(pid, ppid, start, "chrome")))
	write(t, filepath.Join(base, "cmdline"), strings.Join(append([]string{"/opt/google/chrome/chrome"}, args...), "\x00")+"\x00")
	task := filepath.Join(base, "task", fmt.Sprint(pid))
	write(t, filepath.Join(task, "stat"), string(procFixtureStat(pid, ppid, start, "chrome")))
	write(t, filepath.Join(task, "schedstat"), "0 0 0\n")
	write(t, filepath.Join(task, "wchan"), "0\n")
}

func TestChromeTargetsExcludeHeadlessAndRequireBrowserAncestor(t *testing.T) {
	root, dir, _, _ := fixture(t)
	addChromeFixture(t, root, 100, 1, 10, "--profile-directory=Default") // аргументы через NUL, как у не переписавшего cmdline
	addChromeFixture(t, root, 101, 100, 11, "--type=zygote")
	addChromeFixture(t, root, 102, 101, 12, "--type=gpu-process")
	addChromeFixture(t, root, 103, 101, 13, "--type=renderer")
	addChromeFixture(t, root, 200, 1, 20, "--headless")
	addChromeFixture(t, root, 201, 200, 21, "--type=zygote", "--headless")
	addChromeFixture(t, root, 202, 201, 22, "--type=gpu-process", "--headless")
	addChromeFixture(t, root, 203, 201, 23, "--type=renderer")
	addChromeFixture(t, root, 300, 999, 30, "--type=renderer")
	w := New(root, dir, Default(), nil)
	defer w.closeAll()
	wanted, err := w.findTargets()
	if err != nil {
		t.Fatal(err)
	}
	for _, pid := range []int{100, 102, 103} {
		if _, ok := wanted[pid]; !ok {
			t.Errorf("user Chrome process %d missing from targets", pid)
		}
	}
	for _, pid := range []int{101, 200, 201, 202, 203, 300} {
		if _, ok := wanted[pid]; ok {
			t.Errorf("non-user Chrome process %d unexpectedly targeted", pid)
		}
	}
}

func TestChromeTargetSummaryStableAndRendererCountDoesNotAnnounce(t *testing.T) {
	w := New("", "", Default(), nil)
	var calls int
	var last string
	w.OnTargets = func(summary string, _ []string) { calls++; last = summary }
	w.states = map[int]*targetState{
		20: {Target: Target{Name: "chrome", PID: 20, Start: 2, Main: true}},
		10: {Target: Target{Name: "chrome", PID: 10, Start: 1, Main: true}},
		30: {Target: Target{Name: "chrome-gpu", PID: 30, Start: 3, Main: true}},
		40: {Target: Target{Name: "chrome-renderer", PID: 40}},
	}
	for i := range 100 {
		w.reportTargets()
		if !strings.Contains(w.targetSummary, "chrome pid 10, chrome pid 20") {
			t.Fatalf("call %d: unstable target order: %q", i, w.targetSummary)
		}
	}
	if calls != 1 || !strings.Contains(last, "chrome pid 10, chrome pid 20") {
		t.Fatalf("unstable target summary: calls=%d summary=%q", calls, last)
	}
	w.states[41] = &targetState{Target: Target{Name: "chrome-renderer", PID: 41}}
	w.reportTargets()
	if calls != 1 || !strings.Contains(w.targetSummary, "рендереров 2") {
		t.Fatalf("renderer count change announced or omitted: calls=%d summary=%q", calls, w.targetSummary)
	}
	w.states[10] = &targetState{Target: Target{Name: "chrome", PID: 11, Start: 1, Main: true}}
	w.reportTargets()
	if calls != 2 {
		t.Fatalf("main target change did not announce: calls=%d", calls)
	}
}

func TestThreadReadErrorsAreKeptForSlice(t *testing.T) {
	root, _, pid, at := fixture(t)
	w, s := setupState(t, root, filepath.Join(root, "out"), pid)
	defer w.closeAll()
	rewrite(t, filepath.Join(root, fmt.Sprint(pid), "task", fmt.Sprint(pid), "schedstat"), "broken\n")
	if err := w.sample(s, at); err != nil {
		t.Fatal(err)
	}
	item := w.makeSlice(s, &episode{start: at, end: at, kind: "D"}, false)
	if len(item.Errors) == 0 || !strings.Contains(item.Errors[len(item.Errors)-1], "schedstat") {
		t.Fatalf("schedstat error missing from slice: %#v", item.Errors)
	}
}

// Процесс умер между открытием и чтением /proc/<pid>/stat: чтение даёт ESRCH, это «исчез», а не сбой сторожа.
func TestProcGoneTreatsESRCHAsVanished(t *testing.T) {
	esrch := &fs.PathError{Op: "read", Path: "/proc/1/stat", Err: syscall.ESRCH}
	if !procGone(esrch) || !procUnreadable(esrch) {
		t.Fatalf("ESRCH not treated as vanished process")
	}
	if !procUnreadable(&fs.PathError{Op: "open", Path: "/proc/1/stat", Err: fs.ErrPermission}) {
		t.Fatalf("permission denied not treated as unreadable")
	}
	if procGone(&fs.PathError{Op: "open", Path: "/proc/1/stat", Err: fs.ErrPermission}) || procUnreadable(errors.New("io")) {
		t.Fatalf("other errors treated as vanished")
	}
}

// Отчёт читает срезы без блокировки: чистка демона может удалить файл между чтением каталога и чтением файла.
// Пропавший файл — удалённый срез, не ошибка отчёта (висячая ссылка воспроизводит это окно детерминированно).
func TestReportSkipsSliceRemovedDuringRead(t *testing.T) {
	dir := t.TempDir()
	at := time.Now()
	if err := os.MkdirAll(filepath.Join(dir, "slices"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(slicePath(dir, "kept"), Slice{ID: "kept", Target: "v", Start: at, End: at}); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "gone"), slicePath(dir, "removed")); err != nil {
		t.Fatal(err)
	}
	d, err := ReadReport(dir, at.Add(-time.Hour))
	if err != nil {
		t.Fatalf("removed slice broke the report: %v", err)
	}
	if len(d.Slices) != 1 || d.Slices[0].ID != "kept" {
		t.Fatalf("slices = %#v, want only kept", d.Slices)
	}
}

// Слияние сэмплов среза (обновление в памяти и слияние с файлом на диске) склеивает пересекающиеся окна:
// дубли не соседние. На машине 01.11 файл среза вырос до 14,6 МБ — 105 817 сэмплов, из них уникальных 129.
func TestDedupeSamplesRemovesOverlapOfMergedWindows(t *testing.T) {
	merged := []Sample{{AtMS: 0}, {AtMS: 50}, {AtMS: 100}}
	for range 10 { // десять сохранений: каждое склеивает старое с новым окном
		merged = dedupeSamples(append(merged, Sample{AtMS: 50}, Sample{AtMS: 100}, Sample{AtMS: 150}))
		merged = dedupeSamples(append(slices.Clone(merged), merged...))
	}
	got := make([]int64, 0, len(merged))
	for _, s := range merged {
		got = append(got, s.AtMS)
	}
	if want := []int64{0, 50, 100, 150}; !slices.Equal(got, want) {
		t.Fatalf("samples after merges = %v, want %v", got, want)
	}
}
