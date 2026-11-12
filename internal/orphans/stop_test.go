package orphans

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/proc"
	"golang.org/x/sys/unix"
)

// startChild запускает команду и возвращает её процесс из настоящего /proc,
// когда он готов: comm стал want (exec прошёл), а если команда пишет строку
// в stdout, — после этой строки (sh успел поставить trap).
func startChild(t *testing.T, want string, ready bool, name string, args ...string) (*exec.Cmd, *proc.Proc) {
	t.Helper()
	cmd := exec.Command(name, args...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = proc.StopChild(cmd.Process.Pid, 0, func() error { return cmd.Process.Kill() })
		_ = cmd.Wait()
	})
	if ready {
		if _, err := bufio.NewReader(out).ReadString('\n'); err != nil {
			t.Fatalf("строка готовности: %v", err)
		}
	}
	comm := "/proc/" + strconv.Itoa(cmd.Process.Pid) + "/comm"
	for deadline := time.Now().Add(5 * time.Second); ; {
		b, err := os.ReadFile(comm)
		if err == nil && strings.TrimSpace(string(b)) == want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d: comm %q, ждали %q (%v)", cmd.Process.Pid, b, want, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	s, err := proc.ReadSnapshot("/proc", os.Getuid(), time.Now())
	if s == nil {
		t.Fatal(err)
	}
	p := s.Procs[cmd.Process.Pid]
	if p == nil {
		t.Fatalf("pid %d нет в снимке", cmd.Process.Pid)
	}
	return cmd, p
}

func TestStopTermAndKill(t *testing.T) {
	_, soft := startChild(t, "sleep", false, "sleep", "100")
	_, hard := startChild(t, "sh", true, "sh", "-c", "trap '' TERM; echo ready; while :; do sleep 1; done")
	trees := []*Tree{{Root: soft, Members: []*proc.Proc{soft}}, {Root: hard, Members: []*proc.Proc{hard}}}
	outs := Stop(context.Background(), trees, time.Second, "/proc")
	if o := outs[0]; o.Termed != 1 || o.Exited != 1 || o.Killed != 0 || len(o.Errs) != 0 {
		t.Errorf("sleep: %s; ожидалось выйти по SIGTERM", o.Text())
	}
	if o := outs[1]; o.Termed != 1 || o.Killed != 1 || len(o.Errs) != 0 {
		t.Errorf("sh с trap: %s; ожидалось добить SIGKILL", o.Text())
	}
}

func TestStopReusedPidUntouched(t *testing.T) {
	_, p := startChild(t, "sleep", false, "sleep", "100")
	fake := *p
	fake.StartRaw = "1" // как будто в снимке был другой процесс с этим pid
	outs := Stop(context.Background(), []*Tree{{Root: &fake, Members: []*proc.Proc{&fake}}}, time.Second, "/proc")
	if outs[0].Gone != 1 || outs[0].Termed != 0 {
		t.Fatalf("%s; процесс с другим временем старта трогать нельзя", outs[0].Text())
	}
	fd, err := unix.PidfdOpen(p.Pid, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if left := waitExit([]victim{{fd: fd, p: p}}, time.Now().Add(300*time.Millisecond)); !left[fd] {
		t.Fatal("процесс вышел, хотя pid «переиспользован» и трогать его было нельзя")
	}
}

func TestReadSnapshotOwnEnv(t *testing.T) {
	t.Setenv("CLAUDE_PID", "4242")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "s-1")
	_, p := startChild(t, "sleep", false, "sleep", "100")
	if p.Session != 4242 || p.SessionID != "s-1" || p.Comm != "sleep" || p.Cgroup == "" {
		t.Fatalf("снимок: %+v", p)
	}
	if d := time.Since(p.StartAt); d < 0 || d > time.Minute {
		t.Fatalf("StartAt %v далеко от сейчас", p.StartAt)
	}
}
