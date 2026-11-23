package prune

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
	"golang.org/x/sys/unix"
)

func makeOutside(t *testing.T, c Config, name string, mtime time.Time) string {
	t.Helper()
	p := filepath.Join(filepath.Dir(c.Root), name)
	if err := os.MkdirAll(p, 0700); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(p, "data")
	if err := os.WriteFile(f, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(f, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return p
}

func emptyProcRoot(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "proc")
	if err := os.MkdirAll(filepath.Join(p, "net"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(p, "self"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "net", "unix"), []byte("Num RefCount Protocol Flags Type St Inode Path\n"), 0600); err != nil {
		t.Fatal(err)
	}
	mount, err := statxInode(p)
	if err != nil {
		t.Fatal(err)
	}
	mountLine := fmt.Sprintf("%d 0 %d:%d / / rw - tmpfs tmpfs rw\n", mount.MountID, unix.Major(mount.Dev), unix.Minor(mount.Dev))
	if err := os.WriteFile(filepath.Join(p, "self", "mountinfo"), []byte(mountLine), 0600); err != nil {
		t.Fatal(err)
	}
	oldReadSelf := readSelfFDInfo
	readSelfFDInfo = func(fd int) ([]byte, error) {
		var st unix.Statx_t
		if err := unix.Statx(fd, "", unix.AT_EMPTY_PATH, unix.STATX_INO|unix.STATX_MNT_ID, &st); err != nil {
			return nil, err
		}
		return fmt.Appendf(nil, "mnt_id:\t%d\nino:\t%d\n", st.Mnt_id, st.Ino), nil
	}
	t.Cleanup(func() { readSelfFDInfo = oldReadSelf })
	return p
}

func addTestProc(t *testing.T, root, pid string, uid int) string {
	t.Helper()
	base := filepath.Join(root, pid)
	if err := os.MkdirAll(filepath.Join(base, "fd"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "fdinfo"), 0700); err != nil {
		t.Fatal(err)
	}
	status := fmt.Sprintf("Name:\ttest\nUid:\t%d\t%d\t%d\t%d\n", uid, uid, uid, uid)
	if err := os.WriteFile(filepath.Join(base, "status"), []byte(status), 0600); err != nil {
		t.Fatal(err)
	}
	mountInfo, err := os.ReadFile(filepath.Join(root, "self", "mountinfo"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "mountinfo"), mountInfo, 0600); err != nil {
		t.Fatal(err)
	}
	return base
}

func addTestFDInfo(t *testing.T, base, fdName, target string) {
	t.Helper()
	id, err := statxInode(target)
	if err != nil {
		t.Fatal(err)
	}
	data := fmt.Sprintf("pos:\t0\nflags:\t0100000\nmnt_id:\t%d\nino:\t%d\n", id.MountID, id.Ino)
	if err := os.WriteFile(filepath.Join(base, "fdinfo", fdName), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func outsideHooks(h Hooks, now time.Time) Hooks {
	h.Now = func() time.Time { return now.Add(8*24*time.Hour + time.Second) }
	return h
}

func probeTickets(t *testing.T, c Config, body string) {
	t.Helper()
	dir := filepath.Join(c.Home, "docs", "tasks")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "open.md"), []byte("Статус: открыт\n"+body), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestOutsideModeAgeTicketAndExactName(t *testing.T) {
	c, now := world(t)
	c.OutsideMode = ModeAct
	old := now.Add(-10 * 24 * time.Hour)
	linked := makeOutside(t, c, "rev", old)
	neighbor := makeOutside(t, c, "rev-fin2", old)
	fresh := makeOutside(t, c, "fresh", now.Add(2*24*time.Hour))
	oldFile := filepath.Join(filepath.Dir(c.Root), "old-file")
	if err := os.WriteFile(oldFile, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(oldFile, old, old); err != nil {
		t.Fatal(err)
	}
	freshChild := makeOutside(t, c, "fresh-child", old)
	if err := os.Chtimes(filepath.Join(freshChild, "data"), now.Add(2*24*time.Hour), now.Add(2*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	tickets := filepath.Join(c.Home, "docs", "tasks")
	if err := os.MkdirAll(tickets, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tickets, "open.md"), []byte("Статус: открыт\nclaude-tmp/rev\n"), 0600); err != nil {
		t.Fatal(err)
	}
	h := outsideHooks(baseHooks(t, now), now)
	h.ProcRoot = emptyProcRoot(t)
	h.References = func(ctx context.Context, since time.Time, y func(context.Context) error) (*References, error) {
		return NewReferences(ctx, c.Home, c.Projects, since, y)
	}
	r := Run(context.Background(), c, h)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if !exists(t, linked) || exists(t, neighbor) || !exists(t, fresh) || exists(t, oldFile) || !exists(t, freshChild) {
		t.Fatalf("outside eligibility mismatch: %+v", r.Outside)
	}
	got := map[string]string{}
	for _, item := range r.Outside {
		got[filepath.Base(item.Path)] = item.Reason
	}
	if got["rev"] != "ссылка из открытого тикета" || got["fresh"] != "свежее порога" || got["fresh-child"] != "свежее порога" || !strings.Contains(got["rev-fin2"], "удалил") {
		t.Fatalf("outside reasons: %#v", got)
	}
	if got["old-file"] != "удалил" {
		t.Fatalf("top-level file should use its own mtime and be removed: %#v", got)
	}
}

func TestOutsideTicketPunctuationAndWildcardProtectNames(t *testing.T) {
	c, now := world(t)
	c.Mode, c.OutsideMode = ModeLog, ModeAct
	old := now.Add(-10 * 24 * time.Hour)
	names := []string{"rev", "kgx", "acc", "pipe", "paren", "colon", "single", "dot-fin2", "aguard-test-one", "aguard-test-two"}
	paths := make(map[string]string, len(names))
	for _, name := range names {
		paths[name] = makeOutside(t, c, name, old)
	}
	neighbor := makeOutside(t, c, "aguard-tests-other", old)
	probeTickets(t, c, "`claude-tmp/rev` **claude-tmp/kgx** «claude-tmp/acc» |claude-tmp/pipe| )claude-tmp/paren): claude-tmp/colon: *claude-tmp/single* См. claude-tmp/dot-fin2. claude-tmp/aguard-test-*\n")
	h := outsideHooks(baseHooks(t, now), now)
	h.ProcRoot = emptyProcRoot(t)
	h.References = func(ctx context.Context, since time.Time, y func(context.Context) error) (*References, error) {
		return NewReferences(ctx, c.Home, c.Projects, since, y)
	}
	r := Run(context.Background(), c, h)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	for _, name := range names {
		if !exists(t, paths[name]) {
			t.Errorf("ticket syntax did not protect %q: %+v", name, r.Outside)
		}
	}
	if exists(t, neighbor) {
		t.Fatal("wildcard reference protected a non-matching name")
	}
}

func TestOutsideLogReportsTicketAndProcessProtections(t *testing.T) {
	c, now := world(t)
	linked := makeOutside(t, c, "ticket-log", now.Add(-10*24*time.Hour))
	busy := makeOutside(t, c, "busy-log", now.Add(-10*24*time.Hour))
	probeTickets(t, c, "См. claude-tmp/ticket-log\n")
	proc := emptyProcRoot(t)
	owner := addTestProc(t, proc, "780", os.Getuid())
	if err := os.Symlink(busy, filepath.Join(owner, "cwd")); err != nil {
		t.Fatal(err)
	}
	h := outsideHooks(baseHooks(t, now), now)
	h.ProcRoot = proc
	h.References = func(ctx context.Context, since time.Time, yield func(context.Context) error) (*References, error) {
		return NewReferences(ctx, c.Home, c.Projects, since, yield)
	}
	c.Mode, c.OutsideMode = ModeAct, ModeLog
	r := Run(context.Background(), c, h)
	if r.Err != nil || !exists(t, linked) || !exists(t, busy) {
		t.Fatalf("log must preserve both records: %+v", r)
	}
	if got := outsideReasonFor(r.Outside, linked); got != "ссылка из открытого тикета" {
		t.Fatalf("ticket log reason=%q", got)
	}
	if got := outsideReasonFor(r.Outside, busy); got != "открытый файл или cwd живого процесса" {
		t.Fatalf("busy log reason=%q", got)
	}
}

func TestOutsideTaskCwdAndMapsProtectInLog(t *testing.T) {
	c, now := world(t)
	taskRecord := makeOutside(t, c, "task-owner", now.Add(-10*24*time.Hour))
	mapRecord := makeOutside(t, c, "map-owner", now.Add(-10*24*time.Hour))
	proc := emptyProcRoot(t)
	taskProc := addTestProc(t, proc, "781", os.Getuid())
	taskCwd := filepath.Join(taskProc, "task", "781", "cwd")
	if err := os.MkdirAll(filepath.Dir(taskCwd), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(taskRecord, taskCwd); err != nil {
		t.Fatal(err)
	}
	mapProc := addTestProc(t, proc, "782", os.Getuid())
	mapFile := filepath.Join(mapRecord, "data")
	mapLine := "00400000-00401000 r--p 00000000 08:01 12345 " + mapFile + "\n"
	if err := os.WriteFile(filepath.Join(mapProc, "maps"), []byte(strings.Repeat(mapLine, 40)), 0600); err != nil {
		t.Fatal(err)
	}
	h := outsideHooks(baseHooks(t, now), now)
	h.ProcRoot = proc
	c.Mode, c.OutsideMode = ModeAct, ModeLog
	r := Run(context.Background(), c, h)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	for _, path := range []string{taskRecord, mapRecord} {
		if !exists(t, path) {
			t.Fatalf("proc target did not protect %s: %+v", path, r.Outside)
		}
		if got := outsideReasonFor(r.Outside, path); got != "открытый файл или cwd живого процесса" {
			t.Errorf("%s reason=%q", filepath.Base(path), got)
		}
	}
}

func TestOutsideRootAndExeProtectInLog(t *testing.T) {
	c, now := world(t)
	rootRecord := makeOutside(t, c, "root-owner", now.Add(-10*24*time.Hour))
	exeRecord := makeOutside(t, c, "exe-owner", now.Add(-10*24*time.Hour))
	proc := emptyProcRoot(t)
	rootProc := addTestProc(t, proc, "783", os.Getuid())
	if err := os.Symlink(rootRecord, filepath.Join(rootProc, "root")); err != nil {
		t.Fatal(err)
	}
	exeProc := addTestProc(t, proc, "784", os.Getuid())
	if err := os.Symlink(filepath.Join(exeRecord, "data"), filepath.Join(exeProc, "exe")); err != nil {
		t.Fatal(err)
	}
	h := outsideHooks(baseHooks(t, now), now)
	h.ProcRoot = proc
	c.Mode, c.OutsideMode = ModeAct, ModeLog
	r := Run(context.Background(), c, h)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	for _, path := range []string{rootRecord, exeRecord} {
		if !exists(t, path) || outsideReasonFor(r.Outside, path) != "открытый файл или cwd живого процесса" {
			t.Fatalf("root/exe did not protect %s: %+v", path, r.Outside)
		}
	}
}

func TestOutsideProcPathScopeAndDedupBeforeStat(t *testing.T) {
	c, now := world(t)
	unmapped := makeOutside(t, c, "unmapped", now.Add(-10*24*time.Hour))
	mapRecord := makeOutside(t, c, "mapped", now.Add(-10*24*time.Hour))
	proc := emptyProcRoot(t)
	base := addTestProc(t, proc, "785", os.Getuid())
	insidePath := filepath.Join(mapRecord, "data")
	foreignPath := "/run/containerd/s/not-under-root"
	line := "00400000-00401000 r--p 00000000 08:01 12345 " + insidePath + "\n"
	maps := strings.Repeat(line, 80) + "00401000-00402000 r--p 00000000 08:01 12346 " + foreignPath + "\n"
	if err := os.WriteFile(filepath.Join(base, "maps"), []byte(maps), 0600); err != nil {
		t.Fatal(err)
	}
	unixTable := "Num RefCount Protocol Flags Type St Inode Path\n0000000000000000: 00000002 00000000 00010000 0001 01 12345 " + foreignPath + "\n"
	if err := os.WriteFile(filepath.Join(proc, "net", "unix"), []byte(unixTable), 0600); err != nil {
		t.Fatal(err)
	}
	oldStat := openProcTarget
	mapStats, foreignStats := 0, 0
	openProcTarget = func(path string) (Inode, error) {
		switch path {
		case insidePath:
			mapStats++
		case foreignPath:
			foreignStats++
			return Inode{}, syscall.EACCES
		}
		return oldStat(path)
	}
	t.Cleanup(func() { openProcTarget = oldStat })
	h := outsideHooks(baseHooks(t, now), now)
	h.ProcRoot = proc
	c.Mode, c.OutsideMode = ModeAct, ModeAct
	r := Run(context.Background(), c, h)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if exists(t, unmapped) {
		t.Fatalf("out-of-scope EACCES path created doubt: %+v", r.Outside)
	}
	if !exists(t, mapRecord) {
		t.Fatal("in-scope maps path did not protect mapped record")
	}
	if mapStats != 2 || foreignStats != 0 {
		t.Fatalf("map path stats=%d foreign path stats=%d; want one per snapshot and 0", mapStats, foreignStats)
	}
}

func TestProcScanCostSynthetic(t *testing.T) {
	if os.Getenv("AGUARD_PROC_SCAN_COST") != "1" {
		t.Skip("set AGUARD_PROC_SCAN_COST=1 for the one-shot synthetic proc scan measurement")
	}
	const processCount, fdPerProcess, uniquePaths, mapLinesPerProcess = 128, 8, 40, 64
	scopeRoot := filepath.Join(t.TempDir(), "claude-tmp")
	if err := os.MkdirAll(scopeRoot, 0700); err != nil {
		t.Fatal(err)
	}
	mapFiles := make([]string, uniquePaths)
	for i := range mapFiles {
		mapFiles[i] = filepath.Join(scopeRoot, fmt.Sprintf("mapped-%03d", i))
		if err := os.WriteFile(mapFiles[i], []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	procRoot := emptyProcRoot(t)
	for i := range processCount {
		pid := strconv.Itoa(1000 + i)
		base := addTestProc(t, procRoot, pid, os.Getuid())
		for fd := range fdPerProcess {
			target := mapFiles[(i*fdPerProcess+fd)%uniquePaths]
			fdName := strconv.Itoa(fd)
			if err := os.Symlink(target, filepath.Join(base, "fd", fdName)); err != nil {
				t.Fatal(err)
			}
			addTestFDInfo(t, base, fdName, target)
		}
		var maps strings.Builder
		for line := range mapLinesPerProcess {
			fmt.Fprintf(&maps, "00400000-00401000 r--p 00000000 08:01 12345 %s\n", mapFiles[line%uniquePaths])
		}
		if err := os.WriteFile(filepath.Join(base, "maps"), []byte(maps.String()), 0600); err != nil {
			t.Fatal(err)
		}
	}
	stats := 0
	oldStat := openProcTarget
	openProcTarget = func(path string) (Inode, error) {
		within, err := pathWithinRoot(scopeRoot, path)
		if err == nil && within {
			stats++
		}
		return oldStat(path)
	}
	t.Cleanup(func() { openProcTarget = oldStat })
	started := time.Now()
	targets, err := collectProcTargets(context.Background(), procRoot, scopeRoot, os.Getuid(), nil)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("synthetic proc scan: elapsed=%s processes=%d fd=%d unique_paths=%d map_lines=%d target_inodes=%d", elapsed, processCount, processCount*fdPerProcess, stats, processCount*mapLinesPerProcess, len(targets.Inodes))
	if stats != uniquePaths {
		t.Fatalf("map path stat count=%d, want deduplicated %d", stats, uniquePaths)
	}
}

func TestOutsideOldMtimeWithFreshCtimeIsPreserved(t *testing.T) {
	c, now := world(t)
	c.Mode, c.OutsideMode = ModeLog, ModeAct
	path := makeOutside(t, c, "rev-copy", now.Add(-10*24*time.Hour))
	h := baseHooks(t, now)
	h.ProcRoot = emptyProcRoot(t)
	r := Run(context.Background(), c, h)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if !exists(t, path) {
		t.Fatal("copy with fresh ctime and preserved old mtime was removed")
	}
	for _, item := range r.Outside {
		if item.Path == path && item.Reason != "свежее порога" {
			t.Fatalf("reason=%q", item.Reason)
		}
	}
}

func TestOutsideForeignUIDDoesNotCauseUncertainty(t *testing.T) {
	c, now := world(t)
	c.Mode, c.OutsideMode = ModeLog, ModeLog
	path := makeOutside(t, c, "foreign-cwd", now.Add(-10*24*time.Hour))
	proc := emptyProcRoot(t)
	foreign := addTestProc(t, proc, "9001", os.Getuid()+1)
	if err := os.Symlink(path, filepath.Join(foreign, "cwd")); err != nil {
		t.Fatal(err)
	}
	h := outsideHooks(baseHooks(t, now), now)
	h.ProcRoot = proc
	r := Run(context.Background(), c, h)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	for _, item := range r.Outside {
		if item.Path == path && item.Reason != "удалил бы" {
			t.Fatalf("foreign UID caused uncertainty: %q", item.Reason)
		}
	}
}

func TestOutsideUnixSocketInodeProtectsRecord(t *testing.T) {
	c, now := world(t)
	c.Mode, c.OutsideMode = ModeLog, ModeAct
	old := now.Add(-10 * 24 * time.Hour)
	path := makeOutside(t, c, "srv", old)
	aliasDir, err := os.MkdirTemp("", "sock-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(aliasDir) })
	aliasRecord := filepath.Join(aliasDir, "record")
	if err := os.Symlink(path, aliasRecord); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(aliasRecord, "s")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chtimes(socketPath, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	proc := emptyProcRoot(t)
	unixTable := "Num RefCount Protocol Flags Type St Inode Path\n0000000000000000: 00000002 00000000 00010000 0001 01 12345 " + filepath.Join(path, "s") + "\n"
	if err := os.WriteFile(filepath.Join(proc, "net", "unix"), []byte(unixTable), 0600); err != nil {
		t.Fatal(err)
	}
	h := outsideHooks(baseHooks(t, now), now)
	h.ProcRoot = proc
	r := Run(context.Background(), c, h)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if !exists(t, path) {
		t.Fatal("live unix socket did not protect its containing record")
	}
	for _, item := range r.Outside {
		if item.Path == path && item.Reason != "открытый файл или cwd живого процесса" {
			t.Fatalf("socket reason=%q", item.Reason)
		}
	}
}

func TestOutsidePreflightRefreshesProcSnapshot(t *testing.T) {
	c, now := world(t)
	c.Mode, c.OutsideMode = ModeLog, ModeAct
	path := makeOutside(t, c, "late-cwd", now.Add(-10*24*time.Hour))
	procRoot := emptyProcRoot(t)
	armed, planted := false, false
	h := outsideHooks(baseHooks(t, now), now)
	h.ProcRoot = procRoot
	h.Measure = func(ctx context.Context, measured string) (int64, time.Time, error) {
		bytes, changed, err := measure(measured, nil, ctx)
		if measured == path {
			armed = true
		}
		return bytes, changed, err
	}
	h.Yield = func(context.Context) error {
		if armed && !planted {
			planted = true
			proc := addTestProc(t, procRoot, "300", os.Getuid())
			if err := os.Symlink(path, filepath.Join(proc, "cwd")); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}
	r := Run(context.Background(), c, h)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if !planted || !exists(t, path) {
		t.Fatalf("proc change during preflight was missed: %+v", r.Outside)
	}
	for _, item := range r.Outside {
		if item.Path == path && item.Reason != "открытый файл или cwd живого процесса" {
			t.Fatalf("reason=%q", item.Reason)
		}
	}
}

func TestOutsideFreshProcSnapshotFindsLateUnixSocket(t *testing.T) {
	c, now := world(t)
	path := makeOutside(t, c, "late-socket", now.Add(-10*24*time.Hour))
	proc := emptyProcRoot(t)
	aliasDir, err := os.MkdirTemp("", "late-sock-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(aliasDir) })
	aliasRecord := filepath.Join(aliasDir, "record")
	if err := os.Symlink(path, aliasRecord); err != nil {
		t.Fatal(err)
	}
	listenerPath := filepath.Join(aliasRecord, "s")
	var listener net.Listener
	armed, planted := false, false
	h := outsideHooks(baseHooks(t, now), now)
	h.ProcRoot = proc
	h.Measure = func(ctx context.Context, measured string) (int64, time.Time, error) {
		bytes, changed, err := measure(measured, nil, ctx)
		if measured == path {
			armed = true
		}
		return bytes, changed, err
	}
	h.Yield = func(context.Context) error {
		if armed && !planted {
			planted = true
			listener, err = net.Listen("unix", listenerPath)
			if err != nil {
				return err
			}
			unixPath := filepath.Join(path, "s")
			table := "Num RefCount Protocol Flags Type St Inode Path\n0000000000000000: 00000002 00000000 00010000 0001 01 12345 " + unixPath + "\n"
			if err := os.WriteFile(filepath.Join(proc, "net", "unix"), []byte(table), 0600); err != nil {
				return err
			}
		}
		return nil
	}
	defer func() {
		if listener != nil {
			_ = listener.Close()
		}
	}()
	c.Mode, c.OutsideMode = ModeLog, ModeAct
	r := Run(context.Background(), c, h)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if !planted || !exists(t, path) {
		t.Fatalf("fresh /proc/net/unix snapshot missed late socket: %+v", r.Outside)
	}
	if got := outsideReasonFor(r.Outside, path); got != "открытый файл или cwd живого процесса" {
		t.Fatalf("late socket reason=%q", got)
	}
}

func TestOutsideSameNamespaceFDUsesMountIDAndInode(t *testing.T) {
	c, now := world(t)
	path := makeOutside(t, c, "fd-mount-id", now.Add(-10*24*time.Hour))
	proc := emptyProcRoot(t)
	// Simulate btrfs subvolume st_dev differing from the mountinfo device.
	id, err := statxInode(filepath.Join(path, "data"))
	if err != nil {
		t.Fatal(err)
	}
	fakeDev := fmt.Sprintf("%d:%d", unix.Major(id.Dev), unix.Minor(id.Dev)+1)
	if err := os.WriteFile(filepath.Join(proc, "self", "mountinfo"), fmt.Appendf(nil, "%d 0 %s / / rw - btrfs /dev/test rw\n", id.MountID, fakeDev), 0600); err != nil {
		t.Fatal(err)
	}
	base := addTestProc(t, proc, "7911", os.Getuid())
	fdTarget := filepath.Join(path, "data")
	if err := os.Symlink(fdTarget, filepath.Join(base, "fd", "3")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "fdinfo", "3"), fmt.Appendf(nil, "mnt_id:\t%d\nino:\t%d\n", id.MountID, id.Ino), 0600); err != nil {
		t.Fatal(err)
	}
	targets, err := collectProcTargets(context.Background(), proc, filepath.Dir(c.Root), os.Getuid(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := measureOutside(path, targets, nil, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !tree.Busy {
		t.Fatal("fdinfo (mnt_id, ino) did not protect target when mountinfo dev differed")
	}
}

func TestOutsideDeletedSuffixDirectoryIsProtected(t *testing.T) {
	c, now := world(t)
	c.Mode, c.OutsideMode = ModeLog, ModeLog
	path := makeOutside(t, c, "literal (deleted)", now.Add(-10*24*time.Hour))
	proc := emptyProcRoot(t)
	base := addTestProc(t, proc, "7920", os.Getuid())
	selfMountInfo, err := os.ReadFile(filepath.Join(proc, "self", "mountinfo"))
	if err != nil {
		t.Fatal(err)
	}
	mountFields := strings.Fields(string(selfMountInfo))
	if len(mountFields) < 3 {
		t.Fatalf("bad self mountinfo fixture: %q", selfMountInfo)
	}
	pathID, err := statxInode(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "mountinfo"), fmt.Appendf(nil, "%d 0 %s / / rw - tmpfs tmpfs rw\n", pathID.MountID+1, mountFields[2]), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, filepath.Join(base, "cwd")); err != nil {
		t.Fatal(err)
	}
	h := outsideHooks(baseHooks(t, now), now)
	h.ProcRoot = proc
	r := Run(context.Background(), c, h)
	if r.Err != nil || !exists(t, path) {
		t.Fatalf("cwd in directory ending with ' (deleted)' was not protected: %+v", r)
	}
	if got := outsideReasonFor(r.Outside, path); got != "открытый файл или cwd живого процесса" {
		t.Fatalf("reason=%q", got)
	}
}

const bindAliasChildEnv = "AGUARD_PRUNE_BIND_ALIAS_CHILD"

func TestOutsideProcBindAliasProtection(t *testing.T) {
	if role := os.Getenv(bindAliasChildEnv); role != "" {
		testOutsideProcBindAliasChild(t, role)
		return
	}
	unshare, err := exec.LookPath("unshare")
	if err != nil {
		t.Skipf("bind alias probe requires unshare: %v", err)
	}
	for _, role := range []string{"cwd", "fd"} {
		t.Run(role, func(t *testing.T) {
			cmd := exec.Command(unshare, "-rm", "--fork", "--", os.Args[0], "-test.run=^TestOutsideProcBindAliasProtection$")
			cmd.Env = withEnv(os.Environ(), bindAliasChildEnv+"="+role)
			output, err := cmd.CombinedOutput()
			if err != nil {
				if unsharePermissionDenied(string(output)) {
					t.Skipf("unshare refused private user/mount namespace: %s", strings.TrimSpace(string(output)))
				}
				t.Fatalf("bind alias child failed: %v\n%s", err, output)
			}
			t.Logf("isolated bind alias child: %s", strings.TrimSpace(string(output)))
		})
	}
}

func testOutsideProcBindAliasChild(t *testing.T, role string) {
	c, now := world(t)
	c.Mode, c.OutsideMode = ModeLog, ModeLog
	path := makeOutside(t, c, "bind-source", now.Add(-10*24*time.Hour))
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Mkdir(alias, 0700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mount(path, alias, "", unix.MS_BIND, ""); err != nil {
		t.Fatalf("bind mount failed: %v", err)
	}
	defer unix.Unmount(alias, unix.MNT_DETACH)
	var held *os.File
	if role == "cwd" {
		if err := os.Chdir(alias); err != nil {
			t.Fatal(err)
		}
	} else {
		var err error
		held, err = os.Open(filepath.Join(alias, "data"))
		if err != nil {
			t.Fatal(err)
		}
		defer held.Close()
	}
	proc := emptyProcRoot(t)
	base := addTestProc(t, proc, "7921", os.Getuid())
	aliasInfo, err := statxInode(alias)
	if err != nil {
		t.Fatal(err)
	}
	selfMountPath := filepath.Join(proc, "self", "mountinfo")
	selfMountData, err := os.ReadFile(selfMountPath)
	if err != nil {
		t.Fatal(err)
	}
	mountFields := strings.Fields(string(selfMountData))
	if len(mountFields) < 3 {
		t.Fatalf("bad self mountinfo fixture: %q", selfMountData)
	}
	aliasMountLine := fmt.Appendf(nil, "%d 0 %s / %s rw - none none rw\n", aliasInfo.MountID, mountFields[2], alias)
	if err := os.WriteFile(selfMountPath, append(selfMountData, aliasMountLine...), 0600); err != nil {
		t.Fatal(err)
	}
	processMountPath := filepath.Join(base, "mountinfo")
	processMountData, err := os.ReadFile(processMountPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(processMountPath, append(processMountData, aliasMountLine...), 0600); err != nil {
		t.Fatal(err)
	}
	if role == "cwd" {
		if err := os.Symlink(alias, filepath.Join(base, "cwd")); err != nil {
			t.Fatal(err)
		}
	} else {
		fdTarget := filepath.Join(alias, "data")
		if err := os.Symlink(fdTarget, filepath.Join(base, "fd", "3")); err != nil {
			t.Fatal(err)
		}
		addTestFDInfo(t, base, "3", fdTarget)
	}
	h := outsideHooks(baseHooks(t, now), now)
	h.ProcRoot = proc
	r := Run(context.Background(), c, h)
	if r.Err != nil || !exists(t, path) {
		t.Fatalf("%s through bind alias did not protect source record: %+v", role, r)
	}
	if got := outsideReasonFor(r.Outside, path); got != "открытый файл или cwd живого процесса" {
		t.Fatalf("%s alias reason=%q", role, got)
	}
}

func TestOutsideBtrfsChildOpenFDProtectsRecord(t *testing.T) {
	base := t.TempDir()
	var fsStat unix.Statfs_t
	if err := unix.Statfs(base, &fsStat); err != nil {
		t.Fatal(err)
	}
	if uint64(fsStat.Type) != 0x9123683e {
		t.Skipf("btrfs fd regression requires btrfs temp filesystem, got type %#x", uint64(fsStat.Type))
	}
	path := filepath.Join(base, "candidate")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	fdTarget := filepath.Join(path, "held")
	if err := os.WriteFile(fdTarget, []byte("held"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", `cd /; exec 3<"$1"; echo ready; read line`, "sh", fdTarget)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = stdin.Write([]byte("stop\n")); _ = stdin.Close(); _ = cmd.Wait() }()
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || strings.TrimSpace(line) != "ready" {
		t.Fatalf("btrfs fd child not ready: %q %v", line, err)
	}
	id, err := statxInode(fdTarget)
	if err != nil {
		t.Fatal(err)
	}
	proc := emptyProcRoot(t)
	if err := os.WriteFile(filepath.Join(proc, "self", "mountinfo"), fmt.Appendf(nil, "%d 0 0:37 / / rw - btrfs /dev/test rw\n", id.MountID), 0600); err != nil {
		t.Fatal(err)
	}
	process := addTestProc(t, proc, "7914", os.Getuid())
	if err := os.Symlink(fdTarget, filepath.Join(process, "fd", "3")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(process, "fdinfo", "3"), fmt.Appendf(nil, "mnt_id:\t%d\nino:\t%d\n", id.MountID, id.Ino), 0600); err != nil {
		t.Fatal(err)
	}
	targets, err := collectProcTargets(context.Background(), proc, base, os.Getuid(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := measureOutside(path, targets, nil, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !tree.Busy {
		t.Fatal("real btrfs child open fd did not protect candidate")
	}
}

func TestOutsideUnmappedFDMountIDsAreIgnored(t *testing.T) {
	c, now := world(t)
	path := makeOutside(t, c, "pipe-socket-fds", now.Add(-10*24*time.Hour))
	proc := emptyProcRoot(t)
	base := addTestProc(t, proc, "7912", os.Getuid())
	for _, name := range []string{"pipe:[123]", "socket:[456]", "anon_inode:[eventpoll]"} {
		fd := filepath.Join(base, "fd", strconv.Itoa(len(name)))
		if err := os.Symlink(name, fd); err != nil {
			t.Fatal(err)
		}
		data := fmt.Sprintf("mnt_id:\t999\nino:\t%d\n", len(name))
		if err := os.WriteFile(filepath.Join(base, "fdinfo", strconv.Itoa(len(name))), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	targets, err := collectProcTargets(context.Background(), proc, filepath.Dir(c.Root), os.Getuid(), nil)
	if err != nil {
		t.Fatalf("unmapped pipe/socket mount IDs caused uncertainty: %v", err)
	}
	tree, err := measureOutside(path, targets, nil, context.Background())
	if err != nil || tree.Busy {
		t.Fatalf("unmapped fd targets should not protect or fail scan: tree=%+v err=%v", tree, err)
	}
}

func TestOutsideUnknownFDMountIDNeedsScopedLink(t *testing.T) {
	t.Run("non-tree fd links and unreadable link are ignored", func(t *testing.T) {
		c, now := world(t)
		c.Mode, c.OutsideMode = ModeLog, ModeLog
		path := makeOutside(t, c, "unknown-fd-nontree", now.Add(-10*24*time.Hour))
		proc := emptyProcRoot(t)
		base := addTestProc(t, proc, "7919", os.Getuid())
		links := []string{"/memfd:x (deleted)", "/dmabuf:", "/run/systemd/inhibit/7.ref"}
		for i, link := range links {
			fd := strconv.Itoa(i + 3)
			if err := os.Symlink(link, filepath.Join(base, "fd", fd)); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(base, "fdinfo", fd), []byte("mnt_id:\t0\nino:\t123\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink(filepath.Join(path, "data"), filepath.Join(base, "fd", "6")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, "fdinfo", "6"), []byte("mnt_id:\t0\nino:\t123\n"), 0600); err != nil {
			t.Fatal(err)
		}
		oldReadlink := readProcLink
		readProcLink = func(name string) (string, error) {
			if strings.HasSuffix(name, "/fd/6") {
				return "", syscall.EIO
			}
			return oldReadlink(name)
		}
		t.Cleanup(func() { readProcLink = oldReadlink })
		h := outsideHooks(baseHooks(t, now), now)
		h.ProcRoot = proc
		r := Run(context.Background(), c, h)
		if r.Err != nil {
			t.Fatal(r.Err)
		}
		if got := outsideReasonFor(r.Outside, path); got != "удалил бы" {
			t.Fatalf("unmapped non-tree fd links caused doubt: reason=%q", got)
		}
	})

	t.Run("deleted path in prune root causes doubt", func(t *testing.T) {
		c, now := world(t)
		c.Mode, c.OutsideMode = ModeAct, ModeLog
		path := makeOutside(t, c, "unknown-fd-tree", now.Add(-10*24*time.Hour))
		proc := emptyProcRoot(t)
		base := addTestProc(t, proc, "7922", os.Getuid())
		if err := os.Symlink(filepath.Join(path, "data")+" (deleted)", filepath.Join(base, "fd", "3")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, "fdinfo", "3"), []byte("mnt_id:\t999999\nino:\t123\n"), 0600); err != nil {
			t.Fatal(err)
		}
		h := outsideHooks(baseHooks(t, now), now)
		h.ProcRoot = proc
		r := Run(context.Background(), c, h)
		if r.Err != nil {
			t.Fatal(r.Err)
		}
		if !exists(t, path) {
			t.Fatal("scoped fd with unknown mount ID was deleted")
		}
		if got := outsideReasonFor(r.Outside, path); !strings.Contains(got, "сомнение: ошибка чтения /proc") {
			t.Fatalf("scoped fd reason=%q, want /proc uncertainty", got)
		}
	})
}

func TestOutsideEmptyMountDevicesAreUncertain(t *testing.T) {
	c, now := world(t)
	path := makeOutside(t, c, "empty-mount-devices", now.Add(-10*24*time.Hour))
	_, err := measureOutside(path, procTargets{Inodes: map[inodeKey]struct{}{}, MountDevices: map[uint64]uint64{}}, nil, context.Background())
	if err == nil || !strings.Contains(err.Error(), "absent from self mountinfo") {
		t.Fatalf("empty mount device map must make tree measurement uncertain, got %v", err)
	}
}

func TestOutsideProcFailureBranchesFailClosed(t *testing.T) {
	for _, name := range []string{"unknown-cwd-mount", "mountinfo-eio", "self-fdinfo-eio", "scoped-path-unknown-mount", "malformed-fdinfo", "malformed-mountinfo"} {
		t.Run(name, func(t *testing.T) {
			c, now := world(t)
			c.Mode, c.OutsideMode = ModeAct, ModeAct
			path := makeOutside(t, c, "failure-"+name, now.Add(-10*24*time.Hour))
			proc := emptyProcRoot(t)
			base := addTestProc(t, proc, "7916", os.Getuid())
			target := filepath.Join(path, "data")
			restore := func() {}
			switch name {
			case "unknown-cwd-mount":
				if err := os.Symlink(path, filepath.Join(base, "cwd")); err != nil {
					t.Fatal(err)
				}
				old := openProcTarget
				openProcTarget = func(p string) (Inode, error) {
					if strings.HasSuffix(p, "/cwd") {
						return Inode{Ino: 99, MountID: 999999}, nil
					}
					return old(p)
				}
				restore = func() { openProcTarget = old }
			case "mountinfo-eio":
				old := openMountInfo
				openMountInfo = func(p string) (*os.File, error) {
					if p == filepath.Join(base, "mountinfo") {
						return nil, syscall.EIO
					}
					return old(p)
				}
				restore = func() { openMountInfo = old }
			case "self-fdinfo-eio":
				if err := os.Symlink(path, filepath.Join(base, "cwd")); err != nil {
					t.Fatal(err)
				}
				old := readSelfFDInfo
				calls := 0
				readSelfFDInfo = func(int) ([]byte, error) { calls++; return nil, syscall.EIO }
				restore = func() { readSelfFDInfo = old }
				_, directErr := openProcTarget(filepath.Join(base, "cwd"))
				if !errors.Is(directErr, syscall.EIO) || calls == 0 {
					t.Fatalf("O_PATH target fdinfo EIO was not surfaced: err=%v calls=%d", directErr, calls)
				}
				restore()
				return
			case "scoped-path-unknown-mount":
				if err := os.WriteFile(filepath.Join(base, "maps"), []byte("1000-2000 r--p 0 08:01 1 "+target+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
				old := openProcTarget
				openProcTarget = func(p string) (Inode, error) {
					if p == target {
						return Inode{Ino: 77, MountID: 999999}, nil
					}
					return old(p)
				}
				restore = func() { openProcTarget = old }
			case "malformed-fdinfo":
				if err := os.Symlink(target, filepath.Join(base, "fd", "3")); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(base, "fdinfo", "3"), []byte("mnt_id: nope\nino: 1\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "malformed-mountinfo":
				if err := os.WriteFile(filepath.Join(base, "mountinfo"), []byte("not-a-mountinfo-line\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			defer restore()
			h := outsideHooks(baseHooks(t, now), now)
			h.ProcRoot = proc
			r := Run(context.Background(), c, h)
			if r.Err != nil {
				t.Fatal(r.Err)
			}
			if !exists(t, path) {
				t.Fatalf("%s uncertainty allowed deletion", name)
			}
			got := outsideReasonFor(r.Outside, path)
			if !strings.Contains(got, "сомнение: ошибка чтения /proc") {
				t.Fatalf("reason=%q, want /proc uncertainty", got)
			}
		})
	}
}

func TestOutsideFDInfoEIOIsUncertain(t *testing.T) {
	c, now := world(t)
	path := makeOutside(t, c, "fdinfo-eio", now.Add(-10*24*time.Hour))
	proc := emptyProcRoot(t)
	base := addTestProc(t, proc, "7913", os.Getuid())
	if err := os.Symlink(filepath.Join(path, "data"), filepath.Join(base, "fd", "3")); err != nil {
		t.Fatal(err)
	}
	oldRead := readProcFile
	readProcFile = func(name string) ([]byte, error) {
		if strings.HasSuffix(name, "/fdinfo/3") {
			return nil, syscall.EIO
		}
		return oldRead(name)
	}
	t.Cleanup(func() { readProcFile = oldRead })
	_, err := collectProcTargets(context.Background(), proc, filepath.Dir(c.Root), os.Getuid(), nil)
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("fdinfo EIO must produce uncertainty, got %v", err)
	}
}

func TestOutsideSkipsAllClaudeNumberRoots(t *testing.T) {
	c, now := world(t)
	c.Mode, c.OutsideMode = ModeLog, ModeAct
	path := makeOutside(t, c, "claude-1001", now.Add(-10*24*time.Hour))
	h := outsideHooks(baseHooks(t, now), now)
	h.ProcRoot = emptyProcRoot(t)
	r := Run(context.Background(), c, h)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if !exists(t, path) {
		t.Fatal("claude-1001 must be excluded like claude-1000")
	}
	for _, item := range r.Outside {
		if item.Path == path {
			t.Fatal("claude-1001 appeared as an outside candidate")
		}
	}
}

func TestOutsideModeDefaultsToLogIndependently(t *testing.T) {
	c, now := world(t)
	c.Mode, c.OutsideMode = ModeAct, ModeLog
	path := makeOutside(t, c, "old-agent", now.Add(-10*24*time.Hour))
	h := outsideHooks(baseHooks(t, now), now)
	h.ProcRoot = emptyProcRoot(t)
	r := Run(context.Background(), c, h)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if !exists(t, path) {
		t.Fatal("outside_mode=log removed a record while session mode=act")
	}
	for _, item := range r.Outside {
		if item.Path == path && item.Reason != "удалил бы" {
			t.Fatalf("reason=%q", item.Reason)
		}
	}
}

func TestOutsideTicketLinkIsRecheckedBeforeDeletion(t *testing.T) {
	c, now := world(t)
	c.Mode = ModeLog
	c.OutsideMode = ModeAct
	path := makeOutside(t, c, "ticket-arrived", now.Add(-10*24*time.Hour))
	calls := 0
	h := outsideHooks(baseHooks(t, now), now)
	h.ProcRoot = emptyProcRoot(t)
	h.Tickets = func(context.Context) (map[string]bool, error) {
		calls++
		if calls < 3 {
			return map[string]bool{}, nil
		}
		return map[string]bool{"outside:ticket-arrived": true}, nil
	}
	r := Run(context.Background(), c, h)
	if r.Err != nil || !exists(t, path) || calls != 3 {
		t.Fatalf("late ticket reference did not protect entry: %+v", r)
	}
	if len(r.Outside) != 1 || r.Outside[0].Reason != "ссылка из открытого тикета" {
		t.Fatalf("late ticket reason: %+v", r.Outside)
	}
}

func TestOutsideActUnlinksSymlinkWithoutFollowingIt(t *testing.T) {
	c, now := world(t)
	c.OutsideMode = ModeAct
	old := now.Add(-10 * 24 * time.Hour)
	target := filepath.Join(t.TempDir(), "precious")
	if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(c.Root), "old-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, link, []unix.Timespec{unix.NsecToTimespec(old.UnixNano()), unix.NsecToTimespec(old.UnixNano())}, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		t.Fatal(err)
	}
	h := outsideHooks(baseHooks(t, now), now)
	h.ProcRoot = emptyProcRoot(t)
	r := Run(context.Background(), c, h)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if exists(t, link) || !exists(t, target) {
		t.Fatalf("symlink cleanup followed target: report=%+v", r.Outside)
	}
}

func TestOutsideProcLinksProtectCwdAndOpenFDAndErrorsFailClosed(t *testing.T) {
	c, now := world(t)
	old := now.Add(-10 * 24 * time.Hour)
	cwdPath := makeOutside(t, c, "cwd-owner", old)
	fdPath := makeOutside(t, c, "fd-owner", old)
	proc := emptyProcRoot(t)
	for pid, target := range map[string]string{"101": cwdPath, "102": filepath.Join(fdPath, "data")} {
		base := addTestProc(t, proc, pid, os.Getuid())
		if err := os.Symlink(target, filepath.Join(base, "cwd")); err != nil {
			t.Fatal(err)
		}
		if pid == "102" {
			if err := os.Symlink(target, filepath.Join(base, "fd", "7")); err != nil {
				t.Fatal(err)
			}
			addTestFDInfo(t, base, "7", target)
		}
	}
	c.OutsideMode = ModeAct
	h := outsideHooks(baseHooks(t, now), now)
	h.ProcRoot = proc
	oldStat := openProcTarget
	fdTargetStats := 0
	openProcTarget = func(path string) (Inode, error) {
		if strings.Contains(path, "/fd/") {
			fdTargetStats++
			return Inode{}, syscall.EIO
		}
		return oldStat(path)
	}
	t.Cleanup(func() { openProcTarget = oldStat })
	r := Run(context.Background(), c, h)
	if r.Err != nil || !exists(t, cwdPath) || !exists(t, fdPath) {
		t.Fatalf("proc protections failed: %+v", r)
	}
	if fdTargetStats != 0 {
		t.Fatalf("collector statted fd targets %d times instead of using fdinfo", fdTargetStats)
	}

	failed := makeOutside(t, c, "proc-error", old)
	h.ProcRoot = filepath.Join(t.TempDir(), "missing-proc")
	r = Run(context.Background(), c, h)
	if r.Err != nil || !exists(t, failed) {
		t.Fatalf("proc read error did not fail closed: %+v", r)
	}
	for _, item := range r.Outside {
		if item.Path == failed && !strings.Contains(item.Reason, "ошибка чтения /proc") {
			t.Fatalf("proc error reason=%q", item.Reason)
		}
	}
}

func TestOutsideOwnUIDPermissionDeniedIsInvisible(t *testing.T) {
	c, now := world(t)
	actPath := makeOutside(t, c, "own-proc-eacces-act", now.Add(-10*24*time.Hour))
	proc := emptyProcRoot(t)
	base := addTestProc(t, proc, "777", os.Getuid())
	if err := os.Symlink(actPath, filepath.Join(base, "cwd")); err != nil {
		t.Fatal(err)
	}
	oldStat := openProcTarget
	openProcTarget = func(path string) (Inode, error) {
		if strings.HasSuffix(path, "/cwd") {
			return Inode{}, syscall.EACCES
		}
		return oldStat(path)
	}
	t.Cleanup(func() { openProcTarget = oldStat })
	h := outsideHooks(baseHooks(t, now), now)
	h.ProcRoot = proc
	c.OutsideMode = ModeAct
	r := Run(context.Background(), c, h)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if exists(t, actPath) {
		t.Fatalf("EACCES should mean invisible, not uncertain: %+v", r.Outside)
	}
	logPath := makeOutside(t, c, "own-proc-eacces-log", now.Add(-10*24*time.Hour))
	c.OutsideMode = ModeLog
	r = Run(context.Background(), c, h)
	if r.Err != nil || !exists(t, logPath) {
		t.Fatalf("log run unexpectedly removed candidate: %+v", r)
	}
	if got := outsideReasonFor(r.Outside, logPath); got != "удалил бы" {
		t.Fatalf("EACCES log reason=%q, want удалил бы", got)
	}
}

func TestOutsideUnexpectedProcStatErrorIsUncertain(t *testing.T) {
	c, now := world(t)
	path := makeOutside(t, c, "proc-eio", now.Add(-10*24*time.Hour))
	proc := emptyProcRoot(t)
	base := addTestProc(t, proc, "778", os.Getuid())
	if err := os.Symlink(path, filepath.Join(base, "cwd")); err != nil {
		t.Fatal(err)
	}
	oldStat := openProcTarget
	openProcTarget = func(path string) (Inode, error) {
		if strings.HasSuffix(path, "/cwd") {
			return Inode{}, syscall.EIO
		}
		return oldStat(path)
	}
	t.Cleanup(func() { openProcTarget = oldStat })
	h := outsideHooks(baseHooks(t, now), now)
	h.ProcRoot = proc
	c.OutsideMode = ModeAct
	r := Run(context.Background(), c, h)
	if r.Err != nil || !exists(t, path) {
		t.Fatalf("EIO should fail closed: %+v", r)
	}
	if got := outsideReasonFor(r.Outside, path); !strings.Contains(got, "сомнение: ошибка чтения /proc") {
		t.Fatalf("EIO reason=%q", got)
	}
}

func TestOutsideStatusReadErrorIsUncertain(t *testing.T) {
	c, now := world(t)
	path := makeOutside(t, c, "status-error", now.Add(-10*24*time.Hour))
	proc := emptyProcRoot(t)
	base := addTestProc(t, proc, "779", os.Getuid())
	status := filepath.Join(base, "status")
	if err := os.Remove(status); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(base, status); err != nil {
		t.Fatal(err)
	}
	h := outsideHooks(baseHooks(t, now), now)
	h.ProcRoot = proc
	c.OutsideMode = ModeAct
	r := Run(context.Background(), c, h)
	if r.Err != nil || !exists(t, path) {
		t.Fatalf("status read error should fail closed: %+v", r)
	}
	if got := outsideReasonFor(r.Outside, path); !strings.Contains(got, "сомнение: ошибка чтения /proc") {
		t.Fatalf("status error reason=%q", got)
	}
}

func TestOutsideOwnUIDFDDirectoryEIOIsUncertain(t *testing.T) {
	c, now := world(t)
	path := makeOutside(t, c, "fd-dir-eio", now.Add(-10*24*time.Hour))
	proc := emptyProcRoot(t)
	base := addTestProc(t, proc, "7801", os.Getuid())
	oldReadDir := readProcDir
	readProcDir = func(path string) ([]os.DirEntry, error) {
		if path == filepath.Join(base, "fd") {
			return nil, syscall.EIO
		}
		return oldReadDir(path)
	}
	t.Cleanup(func() { readProcDir = oldReadDir })
	h := outsideHooks(baseHooks(t, now), now)
	h.ProcRoot = proc
	c.OutsideMode = ModeLog
	r := Run(context.Background(), c, h)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if !exists(t, path) {
		t.Fatal("fd directory EIO did not preserve candidate")
	}
	if got := outsideReasonFor(r.Outside, path); !strings.Contains(got, "сомнение: ошибка чтения /proc") {
		t.Fatalf("fd directory EIO reason=%q", got)
	}
}

func outsideReasonFor(items []Candidate, path string) string {
	for _, item := range items {
		if item.Path == path {
			return item.Reason
		}
	}
	return "candidate absent"
}

func TestOutsideProcessChildCwdAndOpenFile(t *testing.T) {
	if os.Getenv("AGUARD_PRUNE_CHILD") == "1" {
		f, err := os.Open(os.Getenv("AGUARD_PRUNE_FILE"))
		if err != nil {
			os.Exit(2)
		}
		defer f.Close()
		fmt.Fprintln(os.Stdout, "ready")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		os.Exit(0)
	}
	c, now := world(t)
	target := makeOutside(t, c, "child-live", now.Add(-10*24*time.Hour))
	cmd := exec.Command(os.Args[0], "-test.run=^TestOutsideProcessChildCwdAndOpenFile$")
	cmd.Dir = target
	cmd.Env = append(os.Environ(), "AGUARD_PRUNE_CHILD=1", "AGUARD_PRUNE_FILE="+filepath.Join(target, "data"))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = stdin.Write([]byte("stop\n")); _ = stdin.Close(); _ = cmd.Wait() }()
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || strings.TrimSpace(line) != "ready" {
		t.Fatalf("child not ready: %q %v", line, err)
	}
	fdTarget := filepath.Join(target, "data")
	// Feed synthetic proc links for the child. Tests never scan the host's live /proc.
	procRoot := emptyProcRoot(t)
	child := addTestProc(t, procRoot, "123", os.Getuid())
	if err := os.Symlink(target, filepath.Join(child, "cwd")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fdTarget, filepath.Join(child, "fd", "3")); err != nil {
		t.Fatal(err)
	}
	addTestFDInfo(t, child, "3", fdTarget)
	targets, err := collectProcTargets(context.Background(), procRoot, filepath.Dir(c.Root), os.Getuid(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := measureOutside(target, targets, nil, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !tree.Busy {
		t.Fatal("child cwd/open fd did not protect outside record")
	}
}

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
func baseHooks(t *testing.T, now time.Time) Hooks {
	return Hooks{Now: func() time.Time { return now }, SetIdle: func() (func() error, error) { return func() error { return nil }, nil }, Live: func(context.Context) (map[string]bool, error) { return map[string]bool{}, nil }, Tickets: func(context.Context) (map[string]bool, error) { return map[string]bool{}, nil }, ProcRoot: emptyProcRoot(t)}
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
	h := baseHooks(t, now)
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
	h := baseHooks(t, now)
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
	h := baseHooks(t, now)
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
	h := baseHooks(t, now)
	h.ProcRoot = emptyProcRoot(t)
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
	r := Run(context.Background(), c, outsideHooks(h, now))
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
	r := Run(context.Background(), c, baseHooks(t, now))
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
	r := Run(context.Background(), c, baseHooks(t, now))
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if !exists(t, filepath.Join(c.Root, "-project", oldID)) || r.Items[0].Reason != "удалил бы" {
		t.Fatalf("log mode changed or misreported candidate: %+v", r)
	}
}

func TestPruneTranscriptOldIDAfterClearAndReadOnlyFreshness(t *testing.T) {
	c, now := world(t)
	h := baseHooks(t, now)
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
	h := baseHooks(t, now)
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
	h := baseHooks(t, now)
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
	h := baseHooks(t, now)
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
	h := baseHooks(t, now)
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
	h := baseHooks(t, now)
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

// EINVAL на mountinfo — задача без пространств имён (завершается). Запись не держит, только если все потоки
// зомби или исчезли; живой поток (главный после pthread_exit — зомби) или живое состояние — сомнение, как прежде.
// Случай 23.11 02:16: один завершающийся pid сделал «сомнением» все 20 созревших записей вне claude-1000.
func TestOutsideMountinfoEINVALOfExitedProcess(t *testing.T) {
	cases := []struct {
		name    string
		threads map[string]string // tid → State; nil — каталога task нет (процесс убран)
		deleted bool
	}{
		{"all-threads-zombie", map[string]string{"7916": "Z (zombie)", "7917": "X (dead)"}, true},
		{"reaped-no-task-dir", nil, true},
		{"zombie-leader-live-thread", map[string]string{"7916": "Z (zombie)", "7917": "S (sleeping)"}, false},
		{"running", map[string]string{"7916": "R (running)"}, false},
		{"state-missing", map[string]string{"7916": ""}, false},
		{"task-dir-unreadable", map[string]string{"7916": "Z (zombie)"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, now := world(t)
			c.Mode, c.OutsideMode = ModeAct, ModeAct
			path := makeOutside(t, c, "einval-"+tc.name, now.Add(-10*24*time.Hour))
			proc := emptyProcRoot(t)
			base := addTestProc(t, proc, "7916", os.Getuid())
			for tid, state := range tc.threads {
				dir := filepath.Join(base, "task", tid)
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				body := "Name:\ttest\n"
				if state != "" {
					body += "State:\t" + state + "\n"
				}
				if err := os.WriteFile(filepath.Join(dir, "status"), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "task-dir-unreadable" {
				oldDir := readProcDir
				readProcDir = func(p string) ([]os.DirEntry, error) {
					if p == filepath.Join(base, "task") {
						return nil, syscall.EIO
					}
					return oldDir(p)
				}
				defer func() { readProcDir = oldDir }()
			}
			old := openMountInfo
			openMountInfo = func(p string) (*os.File, error) {
				if p == filepath.Join(base, "mountinfo") {
					return nil, syscall.EINVAL
				}
				return old(p)
			}
			defer func() { openMountInfo = old }()
			h := outsideHooks(baseHooks(t, now), now)
			h.ProcRoot = proc
			r := Run(context.Background(), c, h)
			if r.Err != nil {
				t.Fatal(r.Err)
			}
			if exists(t, path) == tc.deleted {
				t.Fatalf("удалена=%v, хотим %v; причина %q", !exists(t, path), tc.deleted, outsideReasonFor(r.Outside, path))
			}
			if tc.name == "task-dir-unreadable" && !strings.Contains(outsideReasonFor(r.Outside, path), "read threads") {
				t.Fatalf("причина %q, хотим ошибку чтения task/", outsideReasonFor(r.Outside, path))
			}
			if !tc.deleted && !strings.Contains(outsideReasonFor(r.Outside, path), "сомнение: ошибка чтения /proc") {
				t.Fatalf("причина %q, хотим сомнение /proc", outsideReasonFor(r.Outside, path))
			}
		})
	}
}

// Гонка exec: первый обход task/ видит зомби-главного и уже ушедший поток (de_thread поменял tid), второй —
// живой процесс под тем же pid. Запись остаётся под сомнением.
func TestOutsideMountinfoEINVALExecRace(t *testing.T) {
	c, now := world(t)
	c.Mode, c.OutsideMode = ModeAct, ModeAct
	path := makeOutside(t, c, "einval-exec-race", now.Add(-10*24*time.Hour))
	proc := emptyProcRoot(t)
	base := addTestProc(t, proc, "7916", os.Getuid())
	if err := os.MkdirAll(filepath.Join(base, "task", "7916"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "task", "7916", "status"), []byte("State:\tZ (zombie)\n"), 0600); err != nil {
		t.Fatal(err)
	}
	reads, raceAt := 0, 1 // с какого чтения status под pid живёт бывший t2
	oldFile := readProcFile
	readProcFile = func(p string) ([]byte, error) {
		if p == filepath.Join(base, "task", "7916", "status") {
			reads++
			if reads > raceAt {
				return []byte("State:\tS (sleeping)\n"), nil // после exec под pid живёт бывший t2
			}
		}
		return oldFile(p)
	}
	defer func() { readProcFile = oldFile }()
	old := openMountInfo
	openMountInfo = func(p string) (*os.File, error) {
		if p == filepath.Join(base, "mountinfo") {
			return nil, syscall.EINVAL
		}
		return old(p)
	}
	defer func() { openMountInfo = old }()
	if exited, err := processSurelyExited(base); err != nil || exited {
		t.Fatal("гонка exec: второй проход видит живой поток, процесс не завершён")
	}
	// В Run решение принимает повторная проверка перед удалением (scanOutside — чтения 1–2, beforeRemoveOutside — 3–4):
	// гонка во втором проходе повторной проверки. Один проход вместо двух прочёл бы там Z и удалил запись.
	reads, raceAt = 0, 3
	h := outsideHooks(baseHooks(t, now), now)
	h.ProcRoot = proc
	r := Run(context.Background(), c, h)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if !exists(t, path) {
		t.Fatal("гонка exec: запись живого процесса удалена")
	}
	if got := outsideReasonFor(r.Outside, path); !strings.Contains(got, "сомнение: ошибка чтения /proc") {
		t.Fatalf("причина %q, хотим сомнение /proc", got)
	}
}
