package orphans

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// startChild запускает команду и возвращает её процесс из настоящего /proc.
func startChild(t *testing.T, name string, args ...string) (*exec.Cmd, *Proc) {
	t.Helper()
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	time.Sleep(200 * time.Millisecond) // sh успевает поставить trap
	s, err := ReadSnapshot("/proc", os.Getuid(), time.Now())
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
	_, soft := startChild(t, "sleep", "100")
	_, hard := startChild(t, "sh", "-c", "trap '' TERM; while :; do sleep 1; done")
	trees := []*Tree{{Root: soft, Members: []*Proc{soft}}, {Root: hard, Members: []*Proc{hard}}}
	outs := Stop(context.Background(), trees, time.Second, "/proc")
	if o := outs[0]; o.Termed != 1 || o.Exited != 1 || o.Killed != 0 || len(o.Errs) != 0 {
		t.Errorf("sleep: %s; ожидалось выйти по SIGTERM", o.Text())
	}
	if o := outs[1]; o.Termed != 1 || o.Killed != 1 || len(o.Errs) != 0 {
		t.Errorf("sh с trap: %s; ожидалось добить SIGKILL", o.Text())
	}
}

func TestStopReusedPidUntouched(t *testing.T) {
	_, p := startChild(t, "sleep", "100")
	fake := *p
	fake.StartRaw = "1" // как будто в снимке был другой процесс с этим pid
	outs := Stop(context.Background(), []*Tree{{Root: &fake, Members: []*Proc{&fake}}}, time.Second, "/proc")
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
	_, p := startChild(t, "sleep", "100")
	if p.Session != 4242 || p.SessionID != "s-1" || p.Comm != "sleep" || p.Cgroup == "" {
		t.Fatalf("снимок: %+v", p)
	}
	if d := time.Since(p.StartAt); d < 0 || d > time.Minute {
		t.Fatalf("StartAt %v далеко от сейчас", p.StartAt)
	}
}
