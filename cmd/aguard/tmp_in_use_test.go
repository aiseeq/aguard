package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
	"github.com/aiseeq/aguard/internal/record"
	"github.com/aiseeq/akit/proc"
	"github.com/aiseeq/akit/session"
)

func TestTmpInUseProcessHelper(t *testing.T) {
	file := os.Getenv("AGUARD_TMP_HOLDER_FILE")
	if file == "" {
		return
	}
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	listener, err := net.Listen("unix", os.Getenv("AGUARD_TMP_HOLDER_SOCKET"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.WriteFile(os.Getenv("AGUARD_TMP_HOLDER_READY"), []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func TestTmpInUseFindsOpenFileAndUnixListenerThenReleases(t *testing.T) {
	root := t.TempDir()
	fileEntry := t.TempDir()
	socketEntry := root
	for _, path := range []string{fileEntry, socketEntry} {
		if err := os.Chmod(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(fileEntry, "held")
	if err := os.WriteFile(file, []byte("held"), 0o600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(socketEntry, "s")
	ready := filepath.Join(root, "ready")
	child := exec.Command(os.Args[0], "-test.run=^TestTmpInUseProcessHelper$")
	child.Env = append(os.Environ(), "AGUARD_TMP_HOLDER_FILE="+file,
		"AGUARD_TMP_HOLDER_SOCKET="+socket, "AGUARD_TMP_HOLDER_READY="+ready)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			if _, err := proc.StopChild(child.Process.Pid, 300*time.Millisecond, func() error {
				return proc.SignalChild(child.Process.Pid, syscall.SIGTERM)
			}); err != nil {
				t.Errorf("остановить дочерний процесс: %v", err)
			}
			_ = child.Wait()
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("дочерний процесс не готов")
		}
		time.Sleep(10 * time.Millisecond)
	}

	items := []policy.Item{
		{Entry: record.Entry{Path: fileEntry}},
		{Entry: record.Entry{Path: socketEntry}},
	}
	scanStarted := time.Now()
	holders, err := findTmpHolders("/proc", items)
	t.Logf("findTmpHolders (/proc live): %s", time.Since(scanStarted))
	if err != nil {
		t.Fatal(err)
	}
	if holders[fileEntry].pid != child.Process.Pid || holders[socketEntry].pid != child.Process.Pid {
		t.Fatalf("не найдены fd и unix LISTEN (pid %d): %+v; file=%s socket=%s",
			child.Process.Pid, holders, fileEntry, socketEntry)
	}
	item := policy.Item{Entry: record.Entry{Path: fileEntry, Born: time.Now().Add(-time.Hour), Bytes: 1 << 30,
		Owner: &record.Owner{Proc: record.Proc{Pid: child.Process.Pid}, Session: &record.Session{Pid: child.Process.Pid, Name: "creator", ProcStart: "1"}}},
		Class: policy.ClassSession, OwnerKey: "owner"}
	groups := []policy.Group{{Key: "owner", Items: []policy.Item{item}}}
	markTmpHolders(groups, holders)
	c := policy.Default(t.TempDir())
	c.Lead = "lead"
	c.Escalation.After = 1
	c.Escalation.TopRepeat = policy.Duration(time.Hour)
	live := tmpInUseLive{creator: policy.Target{Name: "creator", Pid: child.Process.Pid}, lead: policy.Target{Name: "lead", Pid: 1}}
	state := &policy.UserState{}
	send, _ := c.Plan(time.Now(), groups, live, state, 1)
	if state.Alerts[policy.EntryKey(&groups[0].Items[0])].Sent != 0 {
		t.Fatalf("занятая запись продвинула счётчик: %+v", state.Alerts)
	}
	message := c.Message(&send[0], time.Now(), false)
	if !strings.Contains(message, fmt.Sprintf("занята: %s (pid %d)", filepath.Base(os.Args[0]), child.Process.Pid)) ||
		!strings.Contains(message, "не удалять, пока занята") {
		t.Fatalf("сводка не пометила запись занятой: %s", message)
	}

	if _, err := proc.StopChild(child.Process.Pid, 300*time.Millisecond, func() error {
		return proc.SignalChild(child.Process.Pid, syscall.SIGTERM)
	}); err != nil {
		t.Fatal(err)
	}
	stopped = true
	if err := child.Wait(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatal(err)
		}
	}
	holders, err = findTmpHolders("/proc", items)
	if err != nil {
		t.Fatal(err)
	}
	markTmpHolders(groups, holders)
	if groups[0].Items[0].Busy {
		t.Fatal("запись всё ещё занята после завершения процесса")
	}
	send, _ = c.Plan(time.Now(), groups, live, state, 1)
	if len(send) != 1 || send[0].Target.Name != "creator" || state.Alerts[policy.EntryKey(&groups[0].Items[0])].Sent != 1 {
		t.Fatalf("после освобождения не вернулся обычный порядок: send=%+v state=%+v", send, state.Alerts)
	}
}

func TestTmpInUseDetectsCwd(t *testing.T) {
	procRoot, entry := t.TempDir(), t.TempDir()
	procDir := fakeTmpProc(t, procRoot, 42001)
	if err := os.Symlink(entry, filepath.Join(procDir, "cwd")); err != nil {
		t.Fatal(err)
	}
	holders, err := findTmpHolders(procRoot, []policy.Item{{Entry: record.Entry{Path: entry}}})
	if err != nil {
		t.Fatal(err)
	}
	if holders[entry].pid != 42001 {
		t.Fatalf("cwd процесса не найден: %+v", holders)
	}
}

func TestTmpInUseDetectsDeletedFD(t *testing.T) {
	procRoot, entry := t.TempDir(), filepath.Join(t.TempDir(), "gone")
	procDir := fakeTmpProc(t, procRoot, 42002)
	if err := os.Symlink(entry+" (deleted)", filepath.Join(procDir, "fd", "3")); err != nil {
		t.Fatal(err)
	}
	holders, err := findTmpHolders(procRoot, []policy.Item{{Entry: record.Entry{Path: entry}}})
	if err != nil {
		t.Fatal(err)
	}
	if holders[entry].pid != 42002 {
		t.Fatalf("fd на удалённый файл не найден: %+v", holders)
	}
}

func fakeTmpProc(t *testing.T, procRoot string, pid int) string {
	t.Helper()
	procDir := filepath.Join(procRoot, fmt.Sprint(pid))
	if err := os.MkdirAll(filepath.Join(procDir, "fd"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(procDir, "comm"), []byte("holder\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(procDir, "cmdline"), []byte("holder\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	return procDir
}

func TestTmpRoundMarksBusyEntry(t *testing.T) {
	a, _ := quietObservedApp(t)
	a.out = &bytes.Buffer{}
	a.uid = os.Getuid()
	root, entryDir := t.TempDir(), t.TempDir()
	file := filepath.Join(entryDir, "held")
	if err := os.WriteFile(file, []byte("held"), 0o600); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(root, "ready")
	child := exec.Command(os.Args[0], "-test.run=^TestTmpInUseProcessHelper$")
	child.Env = append(os.Environ(), "AGUARD_TMP_HOLDER_FILE="+file,
		"AGUARD_TMP_HOLDER_SOCKET="+filepath.Join(entryDir, "s"), "AGUARD_TMP_HOLDER_READY="+ready)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := proc.StopChild(child.Process.Pid, 300*time.Millisecond, func() error {
			return proc.SignalChild(child.Process.Pid, syscall.SIGTERM)
		}); err != nil {
			t.Errorf("остановить дочерний процесс: %v", err)
		}
		_ = child.Wait()
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("дочерний процесс не готов")
		}
		time.Sleep(10 * time.Millisecond)
	}

	stat, err := proc.ReadStat("/proc", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	a.reg = session.Registry{Dir: filepath.Join(root, "sessions"), ProcRoot: "/proc"}
	if err := os.Mkdir(a.reg.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	info, err := json.Marshal(session.Info{Pid: os.Getpid(), ProcStart: stat.Start, Name: "lead", SocketPath: "@no-send"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.reg.Dir, fmt.Sprintf("%d.json", os.Getpid())), info, 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	run, track := t.TempDir(), t.TempDir()
	state, err := json.Marshal(record.State{Updated: now, Entries: []record.Entry{{Path: entryDir, UID: a.uid,
		Born: now.Add(-time.Hour), Bytes: 1 << 30}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{run, track} {
		if err := os.WriteFile(filepath.Join(dir, record.StateFile), state, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := policy.Default(a.home)
	cfg.Mode, cfg.Lead = policy.ModeObserve, "lead"
	cfg.RunDir, cfg.TrackDir = run, track
	cfg.Policy.MinEntry, cfg.Policy.MinOwner, cfg.Policy.BudgetGBM = 1, 1, 0.01
	if err := a.tmpRound(&cfg, true); err != nil {
		t.Fatalf("tmpRound при живом держателе: %v", err)
	}
	text := a.out.(*bytes.Buffer).String()
	if !strings.Contains(text, fmt.Sprintf("занята: %s (pid %d)", filepath.Base(os.Args[0]), child.Process.Pid)) {
		t.Fatalf("tmpRound не пометил занятую запись: %s", text)
	}
	a.out = &bytes.Buffer{}
	if err := a.tmpSendTest(&cfg, "lead", true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a.out.(*bytes.Buffer).String(), "не удалять, пока занята") {
		t.Fatalf("send-test не пометил занятую запись: %s", a.out.(*bytes.Buffer).String())
	}
	a.out = &bytes.Buffer{}
	if err := a.tmpReport(&cfg, 0); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a.out.(*bytes.Buffer).String(), fmt.Sprintf("занята: %s (pid %d)", filepath.Base(os.Args[0]), child.Process.Pid)) {
		t.Fatalf("tmp report не пометил занятую запись: %s", a.out.(*bytes.Buffer).String())
	}
}

func TestTmpMarkHoldersWarnsAndLeavesEntriesUnmarked(t *testing.T) {
	a, logs := quietObservedApp(t)
	root := filepath.Join(t.TempDir(), "proc-file")
	if err := os.WriteFile(root, []byte("not a proc directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	groups := []policy.Group{{Items: []policy.Item{{Entry: record.Entry{Path: "/tmp/test"}}}}}
	a.markTmpHolders(root, groups)
	if groups[0].Items[0].Busy || !strings.Contains(logs.String(), "занятость записей не проверена") {
		t.Fatalf("ошибка обхода не оставила чистую запись с предупреждением: busy=%v log=%s",
			groups[0].Items[0].Busy, logs.String())
	}
}

type tmpInUseLive struct {
	creator, lead policy.Target
}

func (l tmpInUseLive) ByPid(pid int, _ string) (policy.Target, bool) {
	return l.creator, pid == l.creator.Pid
}
func (l tmpInUseLive) ByName(name string) (policy.Target, bool) {
	if name == l.creator.Name {
		return l.creator, true
	}
	return l.lead, name == l.lead.Name
}
func (tmpInUseLive) WorkerByUnit(string) (policy.WorkerJob, bool, error) {
	return policy.WorkerJob{}, false, nil
}
