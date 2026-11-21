package prune

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
)

// TestProbe3LiveMirrorLog runs the reviewer probe against a caller-supplied
// synthetic mirror. MIRROR must never point at the host's live claude-tmp.
func TestProbe3LiveMirrorLog(t *testing.T) {
	base := os.Getenv("MIRROR")
	if base == "" {
		base = makeSyntheticReviewerMirror(t)
	}
	home := filepath.Join(base, "home")
	c := Config{Mode: ModeLog, OutsideMode: os.Getenv("OUTSIDE"), Age: policy.Duration(168 * time.Hour), Window: "00:00-23:59",
		Root: filepath.Join(home, ".cache", "claude-tmp", "claude-1000"), Projects: filepath.Join(home, ".claude", "projects"), Home: home}
	now := time.Now().Add(30 * 24 * time.Hour)
	start := time.Now()
	targets, err := collectProcTargets(context.Background(), filepath.Join(base, "proc"), filepath.Dir(c.Root), os.Getuid(), nil)
	t.Logf("collect synthetic mirror: %v targets=%d err=%v", time.Since(start), len(targets.Inodes), err)
	h := Hooks{Now: func() time.Time { return now }, SetIdle: func() (func() error, error) { return func() error { return nil }, nil },
		Live:    func(context.Context) (map[string]bool, error) { return map[string]bool{}, nil },
		Tickets: func(context.Context) (map[string]bool, error) { return map[string]bool{}, nil }, ProcRoot: filepath.Join(base, "proc")}
	start = time.Now()
	r := Run(context.Background(), c, h)
	t.Logf("synthetic mirror run: %v err=%v", time.Since(start), r.Err)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	sort.Slice(r.Outside, func(i, j int) bool { return r.Outside[i].ID < r.Outside[j].ID })
	for _, it := range r.Outside {
		t.Logf("%s: %q removed=%v", it.ID, it.Reason, it.Removed)
	}
	if len(r.Outside) == 0 {
		t.Fatal("synthetic mirror produced no outside records")
	}
	busyPaths := map[string]bool{}
	for _, item := range r.Outside {
		tree, measureErr := measureOutside(item.Path, targets, nil, context.Background())
		if measureErr != nil {
			t.Errorf("measure mirror candidate %s: %v", item.ID, measureErr)
			continue
		}
		busyPaths[item.Path] = tree.Busy || item.ID == "busy-entry" || item.ID == "fd-entry" || item.ID == "sock-entry"
	}
	for _, it := range r.Outside {
		want := "удалил бы"
		if busyPaths[it.Path] {
			want = "открытый файл или cwd живого процесса"
		}
		if it.Reason != want {
			t.Errorf("%s: reason=%q, want %q", it.ID, it.Reason, want)
		}
	}
}

func makeSyntheticReviewerMirror(t *testing.T) string {
	t.Helper()
	proc := emptyProcRoot(t)
	base := filepath.Dir(proc)
	home := filepath.Join(base, "home")
	tree := filepath.Join(home, ".cache", "claude-tmp")
	if err := os.MkdirAll(filepath.Join(home, ".claude", "projects"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"claude-1000/-project", "plain-old", "busy-entry", "fd-entry", "sock-entry"} {
		dir := filepath.Join(tree, name)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if name == "claude-1000/-project" {
			continue
		}
		file := filepath.Join(dir, "data")
		if err := os.WriteFile(file, []byte("old"), 0600); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-30 * 24 * time.Hour)
		if err := os.Chtimes(file, old, old); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(dir, old, old); err != nil {
			t.Fatal(err)
		}
	}
	cwdProc := addTestProc(t, proc, "7940", os.Getuid())
	if err := os.Symlink(filepath.Join(tree, "busy-entry"), filepath.Join(cwdProc, "cwd")); err != nil {
		t.Fatal(err)
	}
	fdProc := addTestProc(t, proc, "7941", os.Getuid())
	fdTarget := filepath.Join(tree, "fd-entry", "data")
	if err := os.Symlink(fdTarget, filepath.Join(fdProc, "fd", "3")); err != nil {
		t.Fatal(err)
	}
	addTestFDInfo(t, fdProc, "3", fdTarget)
	listener, socketPath := listenShortUnixSocket(t, filepath.Join(tree, "sock-entry"))
	t.Cleanup(func() { _ = listener.Close() })
	unixTable := "Num RefCount Protocol Flags Type St Inode Path\n0000000000000000: 00000002 00000000 00010000 0001 01 12345 " + socketPath + "\n"
	if err := os.WriteFile(filepath.Join(proc, "net", "unix"), []byte(unixTable), 0600); err != nil {
		t.Fatal(err)
	}
	return base
}

func TestOutsideSyntheticMirrorHolders(t *testing.T) {
	c, now := world(t)
	c.Mode, c.OutsideMode = ModeLog, ModeLog
	old := now.Add(-10 * 24 * time.Hour)
	plain := makeOutside(t, c, "plain-old", old)
	cwdRecord := makeOutside(t, c, "busy-entry", old)
	fdRecord := makeOutside(t, c, "fd-entry", old)
	socketRecord := makeOutside(t, c, "sock-entry", old)
	listener, socketPath := listenShortUnixSocket(t, socketRecord)
	defer listener.Close()

	proc := emptyProcRoot(t)
	cwdProc := addTestProc(t, proc, "7930", os.Getuid())
	if err := os.Symlink(cwdRecord, filepath.Join(cwdProc, "cwd")); err != nil {
		t.Fatal(err)
	}
	fdProc := addTestProc(t, proc, "7931", os.Getuid())
	fdTarget := filepath.Join(fdRecord, "data")
	if err := os.Symlink(fdTarget, filepath.Join(fdProc, "fd", "3")); err != nil {
		t.Fatal(err)
	}
	addTestFDInfo(t, fdProc, "3", fdTarget)
	unixTable := "Num RefCount Protocol Flags Type St Inode Path\n0000000000000000: 00000002 00000000 00010000 0001 01 12345 " + socketPath + "\n"
	if err := os.WriteFile(filepath.Join(proc, "net", "unix"), []byte(unixTable), 0600); err != nil {
		t.Fatal(err)
	}
	h := outsideHooks(baseHooks(t, now), now)
	h.ProcRoot = proc
	started := time.Now()
	r := Run(context.Background(), c, h)
	elapsed := time.Since(started)
	t.Logf("synthetic mirror: elapsed=%s records=%d", elapsed, len(r.Outside))
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	for path, want := range map[string]string{
		plain:        "удалил бы",
		cwdRecord:    "открытый файл или cwd живого процесса",
		fdRecord:     "открытый файл или cwd живого процесса",
		socketRecord: "открытый файл или cwd живого процесса",
	} {
		if got := outsideReasonFor(r.Outside, path); got != want {
			t.Errorf("%s: reason=%q, want %q", filepath.Base(path), got, want)
		}
	}
}

func listenShortUnixSocket(t *testing.T, dir string) (net.Listener, string) {
	t.Helper()
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	listener, listenErr := net.Listen("unix", "s.sock")
	restoreErr := os.Chdir(oldDir)
	if listenErr != nil {
		t.Fatal(listenErr)
	}
	if restoreErr != nil {
		listener.Close()
		t.Fatal(restoreErr)
	}
	return listener, filepath.Join(dir, "s.sock")
}
