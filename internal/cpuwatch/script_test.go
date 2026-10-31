package cpuwatch

import (
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// runScript — RemoteScript через sh по поддельному /proc в root/p; arg — аргумент скрипта (load или tasks).
func runScript(t *testing.T, root, arg string) []byte {
	t.Helper()
	cmd := exec.Command("sh", "-s", arg)
	cmd.Dir = root
	cmd.Stdin = strings.NewReader(strings.ReplaceAll(RemoteScript, "/proc/", "./p/"))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("sh: %v", err)
	}
	return out
}

// Скрипт замера PC: без tasks потоки не обходит; cgroup — только строка v2, имя процесса с табуляцией не рвёт строку.
func TestRemoteScript(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "p")
	write(t, filepath.Join(p, "stat"), "cpu  1 2 3\ncpu0 1 2 3\ncpu1 1 2 3\nprocs_running 3\n")
	write(t, filepath.Join(p, "loadavg"), "2.50 2.25 0.75 9/700 4242\n")
	write(t, filepath.Join(p, "10", "task", "1001", "stat"), "1001 (go x) R 1 2 3")
	write(t, filepath.Join(p, "10", "task", "1002", "stat"), "1002 (go x) S 1 2 3")
	write(t, filepath.Join(p, "10", "comm"), "go\tx\n")
	write(t, filepath.Join(p, "10", "cgroup"), "1:name=systemd:/old\n0::"+userSlice+"/app.slice/farm-x.0123456789abcdef.scope\n")
	write(t, filepath.Join(p, "10", "environ"), "A=1\x00CLAUDE_PID=77\x00SSH_CONNECTION=10.0.0.1 40000 10.0.0.2 22\x00")
	write(t, filepath.Join(p, "11", "task", "1101", "stat"), "1101 (idle) S 1 2 3")
	write(t, filepath.Join(p, "11", "comm"), "idle\n")

	tasks, err := ParseRemote(runScript(t, root, "tasks"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := []Task{{Pid: 10, N: 1, Comm: "go x", Cgroup: userSlice + "/app.slice/farm-x.0123456789abcdef.scope", Claude: 77,
		SSH: "10.0.0.1 40000 10.0.0.2 22"}}
	if !slices.Equal(tasks, want) {
		t.Errorf("готовые потоки:\n got %+v\nwant %+v", tasks, want)
	}

	dir := t.TempDir()
	if tasks, err := ParseRemote(runScript(t, root, "load"), dir); err != nil || tasks != nil {
		t.Errorf("без tasks потоки не обходятся: %+v, %v", tasks, err)
	}
	var s Sample
	if err := s.ReadLoad(dir); err != nil || s.Load.Avg1 != 2.5 || s.Runnable != 2 || s.Threads != 2 {
		t.Errorf("load average и готовые: %+v, %v", s, err)
	}
}

// Due только спрашивает: отметка — Sent после отправки, и не дошедшее письмо уходит на следующем замере, а не через
// repeat.
func TestDueUntilSent(t *testing.T) {
	var h Host
	at := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	h.Judge(sample(at, 30, 30, nil), byCgroup)
	if !h.Due("glint-master", at, 30*time.Minute) || !h.Due("glint-master", at.Add(time.Minute), 30*time.Minute) {
		t.Error("письмо не ушло — адресат всё ещё ждёт его")
	}
	h.Sent("glint-master", at.Add(time.Minute))
	if h.Due("glint-master", at.Add(2*time.Minute), 30*time.Minute) {
		t.Error("после отправки — не раньше repeat")
	}
}
