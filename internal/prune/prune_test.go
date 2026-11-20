package prune

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
	"golang.org/x/sys/unix"
)

const oldID = "12345678-1234-4234-8234-123456789abc"
const otherID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"

func world(t *testing.T) (Config, time.Time) {
	t.Helper()
	base := t.TempDir()
	now := time.Now().In(time.Local)
	root := filepath.Join(base, ".cache", "claude-tmp", "claude-1000")
	projects := filepath.Join(base, ".claude", "projects")
	for _, id := range []string{oldID, otherID} {
		p := filepath.Join(root, "-project", id)
		if e := os.MkdirAll(p, 0700); e != nil {
			t.Fatal(e)
		}
		f := filepath.Join(p, "scratch.txt")
		if e := os.WriteFile(f, []byte("payload"), 0600); e != nil {
			t.Fatal(e)
		}
		if e := os.Chtimes(f, now.Add(-10*24*time.Hour), now.Add(-10*24*time.Hour)); e != nil {
			t.Fatal(e)
		}
		if e := os.Chtimes(p, now.Add(-10*24*time.Hour), now.Add(-10*24*time.Hour)); e != nil {
			t.Fatal(e)
		}
	}
	if e := os.MkdirAll(projects, 0700); e != nil {
		t.Fatal(e)
	}
	return Config{Mode: ModeAct, Age: policy.Duration(7 * 24 * time.Hour), Window: windowAround(now), Root: root, Projects: projects, Home: base}, now
}

func windowAround(now time.Time) string {
	minute := now.Hour()*60 + now.Minute()
	start, end := (minute+1439)%1440, (minute+2)%1440
	return fmt.Sprintf("%02d:%02d-%02d:%02d", start/60, start%60, end/60, end%60)
}
func baseHooks(now time.Time) Hooks {
	return Hooks{Now: func() time.Time { return now }, SetIdle: func() (func() error, error) { return func() error { return nil }, nil }, Live: func(context.Context) (map[string]bool, error) { return map[string]bool{}, nil }, Tickets: func(context.Context) (map[string]bool, error) { return map[string]bool{}, nil }}
}
func exists(t *testing.T, p string) bool { t.Helper(); _, e := os.Stat(p); return e == nil }

// Main acceptance fixture: session liveness, freshness, ticket UUID matching and strict scope.
func TestPruneSyntheticTreeSafety(t *testing.T) {
	c, now := world(t)
	c.Mode = ModeLog
	transcript := filepath.Join(c.Projects, "-project", "new-session.jsonl")
	if e := os.MkdirAll(filepath.Dir(transcript), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(transcript, []byte(`{"scratch":"claude-tmp/claude-1000/-project/`+oldID+`"}`), 0600); e != nil {
		t.Fatal(e)
	}
	_ = os.Chtimes(transcript, now, now)
	linked := filepath.Join(c.Root, "-project", otherID)
	h := baseHooks(now)
	h.Live = func(context.Context) (map[string]bool, error) { return map[string]bool{}, nil }
	h.Tickets = func(context.Context) (map[string]bool, error) { return map[string]bool{otherID: true}, nil }
	r := Run(context.Background(), c, h)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if len(r.Items) != 2 || !exists(t, filepath.Join(c.Root, "-project", oldID)) || !exists(t, linked) {
		t.Fatalf("log changed fixture or missed candidates: %+v", r)
	}
	if r.Items[0].Reason != "живая сессия" {
		t.Errorf("transcript liveness=%q", r.Items[0].Reason)
	}
	c.Mode = ModeAct
	h.Live = func(context.Context) (map[string]bool, error) { return map[string]bool{oldID: true}, nil }
	h.Tickets = func(context.Context) (map[string]bool, error) { return map[string]bool{otherID: true}, nil }
	// Remove fresh transcript so session status for the act pass comes from the test hook.
	if e := os.Remove(transcript); e != nil {
		t.Fatal(e)
	}
	// One unrelated old UUID is deleted, and entries outside session UUID slots remain.
	extra := filepath.Join(c.Root, "bundled-skills")
	if e := os.MkdirAll(extra, 0700); e != nil {
		t.Fatal(e)
	}
	third := filepath.Join(c.Root, "-project", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	if e := os.MkdirAll(third, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.Chtimes(third, now.Add(-10*24*time.Hour), now.Add(-10*24*time.Hour)); e != nil {
		t.Fatal(e)
	}
	r = Run(context.Background(), c, h)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if exists(t, third) || !exists(t, extra) || !exists(t, filepath.Join(c.Root, "-project", oldID)) || !exists(t, linked) {
		t.Fatalf("act scope/live/link incorrect: %+v", r)
	}
}

func TestPruneWindowAndIdle(t *testing.T) {
	c, now := world(t)
	h := baseHooks(now)
	idle := false
	h.SetIdle = func() (func() error, error) { idle = true; return func() error { return nil }, nil }
	inside := c.Window
	c.Window = windowAround(now.Add(12 * time.Hour))
	r := Run(context.Background(), c, h)
	if r.Err != nil || len(r.Items) != 0 || idle {
		t.Fatalf("outside window ran: %+v idle=%v", r, idle)
	}
	c.Window = inside
	_ = Run(context.Background(), c, h)
	if !idle {
		t.Fatal("idle IO priority not requested")
	}
}

func TestThreadIOPriorityIsIdle(t *testing.T) {
	restore, err := SetThreadIdle()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := restore(); err != nil {
			t.Errorf("restore ioprio: %v", err)
		}
	}()
	priority, _, errno := unix.Syscall(unix.SYS_IOPRIO_GET, 1, uintptr(unix.Gettid()), 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	if priority>>13 != 3 {
		t.Fatalf("ioprio class=%d, want idle (3)", priority>>13)
	}
}

func TestRunMeasuresAndDeletesOnPinnedIdleThread(t *testing.T) {
	c, now := world(t)
	h := baseHooks(now)
	h.Live = func(context.Context) (map[string]bool, error) { return map[string]bool{}, nil }
	h.Tickets = func(context.Context) (map[string]bool, error) { return map[string]bool{}, nil }
	setThread := 0
	h.SetIdle = func() (func() error, error) {
		restore, err := SetThreadIdle()
		if err != nil {
			return nil, err
		}
		setThread = unix.Gettid()
		return restore, nil
	}
	var measureThreads []int
	var measureErr error
	h.Measure = func(context.Context, string) (int64, time.Time, error) {
		measureThreads = append(measureThreads, unix.Gettid())
		priority, _, errno := unix.Syscall(unix.SYS_IOPRIO_GET, 1, uintptr(unix.Gettid()), 0)
		if errno != 0 {
			measureErr = errno
			return 0, time.Time{}, nil
		}
		if priority>>13 != 3 && measureErr == nil {
			measureErr = fmt.Errorf("measure ran at ioprio class %d, want idle", priority>>13)
		}
		return 1, now.Add(-10 * 24 * time.Hour), nil
	}
	r := Run(context.Background(), c, h)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if measureErr != nil {
		t.Fatal(measureErr)
	}
	if len(measureThreads) == 0 {
		t.Fatal("measure hook did not run")
	}
	for _, tid := range measureThreads {
		if tid != setThread {
			t.Fatalf("measure moved from IO-idle thread %d to %d", setThread, tid)
		}
	}
	if exists(t, filepath.Join(c.Root, "-project", oldID)) {
		t.Fatal("act pass did not remove its synthetic candidate")
	}
}

func TestRestoreIOPriorityFailureKeepsThreadPinned(t *testing.T) {
	unlocked := false
	restore := restoreIOPriority(func() error { return os.ErrPermission }, func() { unlocked = true })
	if err := restore(); err == nil {
		t.Fatal("restore error was hidden")
	}
	if unlocked {
		t.Fatal("thread was unlocked after failed priority restoration")
	}
}

func TestOvernightWindowKeyUsesStartDate(t *testing.T) {
	now := time.Date(2026, 11, 21, 1, 0, 0, 0, time.Local)
	if got := WindowKey("22:00-03:00", now); got != "2026-11-20" {
		t.Fatalf("overnight key=%q", got)
	}
}

func TestPruneDeletesOldDeadSessionAndReportsOutsideClaude1000(t *testing.T) {
	c, now := world(t)
	// Keep one old session eligible and make all its descendants old as well.
	h := baseHooks(now)
	h.Live = func(context.Context) (map[string]bool, error) { return map[string]bool{otherID: true}, nil }
	outside := filepath.Join(filepath.Dir(c.Root), "tmp.agent-review")
	if e := os.MkdirAll(outside, 0700); e != nil {
		t.Fatal(e)
	}
	f := filepath.Join(outside, "artifact")
	if e := os.WriteFile(f, []byte("outside"), 0600); e != nil {
		t.Fatal(e)
	}
	old := now.Add(-9 * 24 * time.Hour)
	_ = os.Chtimes(f, old, old)
	_ = os.Chtimes(outside, old, old)
	linkedOutside := filepath.Join(c.Root, "-project", oldID, "external-link")
	if e := os.Symlink(outside, linkedOutside); e != nil {
		t.Fatal(e)
	}
	times := []unix.Timespec{unix.NsecToTimespec(old.UnixNano()), unix.NsecToTimespec(old.UnixNano())}
	if e := unix.UtimesNanoAt(unix.AT_FDCWD, linkedOutside, times, unix.AT_SYMLINK_NOFOLLOW); e != nil {
		t.Fatal(e)
	}
	if e := os.Chtimes(filepath.Dir(linkedOutside), old, old); e != nil {
		t.Fatal(e)
	}
	r := Run(context.Background(), c, h)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if exists(t, filepath.Join(c.Root, "-project", oldID)) || !exists(t, outside) {
		t.Fatalf("old dead / out-of-scope handling wrong: %+v", r)
	}
	if !exists(t, f) {
		t.Fatal("deleting a session followed its symlink outside the allowed tree")
	}
	if len(r.Outside) != 1 || r.Outside[0].Bytes != 7 || r.Outside[0].Age < 8*24*time.Hour {
		t.Fatalf("outside entry missing size/age: %+v", r.Outside)
	}
}

func TestPruneFreshCatalogDoesNotDelete(t *testing.T) {
	c, now := world(t)
	p := filepath.Join(c.Root, "-project", oldID, "scratch.txt")
	if e := os.Chtimes(p, now, now); e != nil {
		t.Fatal(e)
	}
	r := Run(context.Background(), c, baseHooks(now))
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if !exists(t, filepath.Join(c.Root, "-project", oldID)) || r.Items[0].Reason != "свежее порога" {
		t.Fatalf("fresh entry not protected: %+v", r)
	}
}

func TestPruneLogModeOnlyReportsOldCandidate(t *testing.T) {
	c, now := world(t)
	c.Mode = ModeLog
	r := Run(context.Background(), c, baseHooks(now))
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if !exists(t, filepath.Join(c.Root, "-project", oldID)) || r.Items[0].Reason != "удалил бы" {
		t.Fatalf("log mode changed or misreported candidate: %+v", r)
	}
}

func TestPruneTranscriptOldIDAfterClearAndReadOnlyFreshness(t *testing.T) {
	c, now := world(t)
	h := baseHooks(now)
	h.Live = func(context.Context) (map[string]bool, error) { return map[string]bool{}, nil }
	p := filepath.Join(c.Projects, "p", "resumed.jsonl")
	if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(p, []byte("resume wrote claude-tmp/claude-1000/-project/"+oldID+"/scratchpad"), 0600); e != nil {
		t.Fatal(e)
	}
	_ = os.Chtimes(p, now, now)
	beforeInfo, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	before := beforeInfo.ModTime()
	r := Run(context.Background(), c, h)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if r.Items[0].Reason != "живая сессия" || !exists(t, filepath.Join(c.Root, "-project", oldID)) {
		t.Fatalf("scratchpad old ID not protected: %+v", r)
	}
	afterInfo, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !afterInfo.ModTime().Equal(before) {
		t.Fatal("read changed transcript mtime")
	}
}

func TestPruneTicketMatchesUUIDAndNotNeighbor(t *testing.T) {
	c, now := world(t)
	tasks := filepath.Join(c.Home, "docs", "tasks")
	if e := os.MkdirAll(tasks, 0700); e != nil {
		t.Fatal(e)
	}
	body := "Статус: на проверке\nСм. ~/cache/" + oldID + "/scratch.txt\n"
	if e := os.WriteFile(filepath.Join(tasks, "ticket.md"), []byte(body), 0600); e != nil {
		t.Fatal(e)
	}
	h := baseHooks(now)
	h.Live = func(context.Context) (map[string]bool, error) { return map[string]bool{}, nil }
	h.Tickets = func(ctx context.Context) (map[string]bool, error) { return openTicketIDs(ctx, c.Home, nil) }
	r := Run(context.Background(), c, h)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if r.Items[0].Reason != "ссылка из открытого тикета" || exists(t, filepath.Join(c.Root, "-project", otherID)) {
		t.Fatalf("UUID link matching failed: %+v", r)
	}
}

func TestPruneRechecksImmediatelyBeforeDelete(t *testing.T) {
	c, now := world(t)
	h := baseHooks(now)
	calls := 0
	h.Live = func(context.Context) (map[string]bool, error) {
		calls++
		if calls > 1 {
			return map[string]bool{oldID: true}, nil
		}
		return map[string]bool{}, nil
	}
	r := Run(context.Background(), c, h)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	for _, i := range r.Items {
		if i.ID == oldID && (i.Reason != "живая сессия" || !exists(t, i.Path)) {
			t.Fatalf("resume race not caught: %+v", i)
		}
	}
}

func TestPruneMeasurementFailureSkips(t *testing.T) {
	c, now := world(t)
	h := baseHooks(now)
	h.Measure = func(_ context.Context, p string) (int64, time.Time, error) {
		if strings.Contains(p, oldID) {
			return 0, time.Time{}, os.ErrPermission
		}
		return 1, now.Add(-10 * 24 * time.Hour), nil
	}
	r := Run(context.Background(), c, h)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if !exists(t, filepath.Join(c.Root, "-project", oldID)) {
		t.Fatal("measurement error led to deletion")
	}
}

func TestPruneTicketRecheck(t *testing.T) {
	c, now := world(t)
	h := baseHooks(now)
	calls := 0
	h.Tickets = func(context.Context) (map[string]bool, error) {
		calls++
		if calls > 1 {
			return map[string]bool{oldID: true}, nil
		}
		return map[string]bool{}, nil
	}
	r := Run(context.Background(), c, h)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if !exists(t, filepath.Join(c.Root, "-project", oldID)) {
		t.Fatal("ticket race should skip before removal")
	}
}

func TestPruneStopsAtWindowEndAndYieldErrorPauses(t *testing.T) {
	c, now := world(t)
	h := baseHooks(now)
	n := 0
	h.Yield = func(context.Context) error {
		n++
		if n > 4 {
			return context.Canceled
		}
		return nil
	}
	// Simulate clock reaching window end while traversing candidate files.
	current := now
	h.Now = func() time.Time { return current }
	h.Yield = func(context.Context) error {
		n++
		if n > 4 {
			current = now.Add(3 * time.Hour)
		}
		return nil
	}
	r := Run(context.Background(), c, h)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	for _, i := range r.Items {
		if i.Removed {
			t.Fatalf("continued deletion after the window ended: %+v yields=%d", r, n)
		}
	}
	current = now
	h.Yield = func(context.Context) error { return context.DeadlineExceeded }
	r = Run(context.Background(), c, h)
	if r.Err == nil || !exists(t, filepath.Join(c.Root, "-project", oldID)) {
		t.Fatalf("yield error did not pause safely: %+v", r)
	}
}
