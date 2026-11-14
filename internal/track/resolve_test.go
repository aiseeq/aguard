package track

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/aiseeq/akit/proc"
)

func TestReadProcMatchesLiveProcesses(t *testing.T) {
	for _, pid := range []int{os.Getpid(), 1} {
		got, err := readProc(pid)
		if err != nil {
			t.Fatalf("readProc(%d): %v", pid, err)
		}
		want, err := legacyReadProc(pid)
		if err != nil {
			t.Fatalf("legacyReadProc(%d): %v", pid, err)
		}
		if got != want {
			t.Errorf("readProc(%d) = %+v, прежнее чтение = %+v", pid, got, want)
		}
	}
}

func TestReadProcMatchesLegacyForAllPids(t *testing.T) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	differentOwner := 0
	for _, entry := range entries {
		if entry.Name() == "" || entry.Name()[0] < '0' || entry.Name()[0] > '9' {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			t.Fatalf("неверное имя PID в /proc: %q: %v", entry.Name(), err)
		}
		got, gotErr, want, wantErr, matched := readProcAgainstLegacy(pid)
		if !matched {
			t.Errorf("pid %d: новое чтение %+v (%v), старое чтение %+v (%v)", pid, got, gotErr, want, wantErr)
			continue
		}
		var st syscall.Stat_t
		if err := syscall.Stat("/proc/"+entry.Name(), &st); err == nil && st.Uid != st.Gid && gotErr == nil {
			differentOwner++
		}
	}
	if differentOwner == 0 {
		t.Skip("в /proc нет доступного процесса с uid, отличным от gid")
	}
}

func TestReadProcCmdlineBoundariesAndEmptyArgs(t *testing.T) {
	const script = "echo ready; read x"
	base := len("sh") + 1 + len("-c") + 1 + len(script) + 1 + len("n") + 1
	for at := 190; at <= 204; at++ {
		args := []string{"n", strings.Repeat("x", at-base)}
		for range 20 {
			args = append(args, "")
		}
		args = append(args, "tail")
		child := startReadProcChild(t, script, args...)
		assertReadProcMatchesLegacy(t, "empty args at byte "+strconv.Itoa(at), child.pid)
	}
	// Пустые аргументы в конце cmdline должны быть удалены только после EOF.
	child := startReadProcChild(t, script, "n", strings.Repeat("x", argsMax-32), "", "", "")
	assertReadProcMatchesLegacy(t, "trailing empty args", child.pid)
}

func TestReadProcTrailingNULPastBuffer(t *testing.T) {
	const script = "echo ready; read x"
	base := len("sh") + 1 + len("-c") + 1 + len(script) + 1 + len("n") + 1
	for _, at := range []int{190, 195, 199, 200, 203, 204} {
		for _, count := range []int{5, 20, 300} {
			args := []string{"n", strings.Repeat("x", at-base)}
			for range count {
				args = append(args, "")
			}
			child := startReadProcChild(t, script, args...)
			assertReadProcMatchesLegacy(t, fmt.Sprintf("trailing NUL at %d with %d empty args", at, count), child.pid)
		}
	}
}

func TestReadProcUTF8CutBoundaries(t *testing.T) {
	const script = "echo ready; read x"
	base := len("sh") + 1 + len("-c") + 1 + len(script) + 1 + len("n") + 1
	for _, runeText := range []string{"é", "€", "𝄞"} {
		for at := 196; at <= 204; at++ {
			child := startReadProcChild(t, script, "n", strings.Repeat("x", at-base), strings.Repeat(runeText, 5))
			assertReadProcMatchesLegacy(t, fmt.Sprintf("rune %q at byte %d", runeText, at), child.pid)
		}
	}
}

func TestReadProcCommTrimSpace(t *testing.T) {
	child := startReadProcChild(t, "printf '  lead\\n' > /proc/$$/comm; echo ready; read x")
	assertReadProcMatchesLegacy(t, "comm with leading spaces", child.pid)
	got, err := readProc(child.pid)
	if err != nil || got.comm != "lead" {
		t.Fatalf("comm = %q, err %v; want TrimSpace result", got.comm, err)
	}
}

func TestReadProcZombieCwdError(t *testing.T) {
	child := startReadProcChild(t, "echo ready; read x")
	if err := child.in.Close(); err != nil {
		t.Fatal(err)
	}
	waitForZombie(t, child.pid)
	_, gotErr := readProc(child.pid)
	_, wantErr := legacyReadProc(child.pid)
	if gotErr == nil || wantErr == nil || !sameProcRead(procData{}, gotErr, procData{}, wantErr) {
		t.Fatalf("зомби: новая ошибка %v, старая ошибка %v", gotErr, wantErr)
	}
	if !strings.Contains(gotErr.Error(), "cwd") {
		t.Fatalf("ошибка зомби не сообщает о cwd: %v", gotErr)
	}
	child.wait()
}

func TestReadProcClosesDescriptors(t *testing.T) {
	child := startReadProcChild(t, "echo ready; read x")
	if err := child.in.Close(); err != nil {
		t.Fatal(err)
	}
	waitForZombie(t, child.pid)

	beforeZombie := procFDCount(t)
	pids := []int{os.Getpid(), 1, child.pid, 999999999, 0, -1}
	for range 20 {
		for _, pid := range pids {
			_, _ = readProc(pid)
		}
	}
	if afterZombie := procFDCount(t); afterZombie != beforeZombie {
		t.Fatalf("fd для живых/zombie pid: было %d, стало %d", beforeZombie, afterZombie)
	}
	child.wait()
	beforeReaped := procFDCount(t)
	for range 20 {
		_, _ = readProc(child.pid) // уже убранный процесс: отказ при open.
	}
	if afterReaped := procFDCount(t); afterReaped != beforeReaped {
		t.Fatalf("fd для уже убранного pid: было %d, стало %d", beforeReaped, afterReaped)
	}
}

func TestReadProcExitedProcess(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := readProc(cmd.Process.Pid); err == nil || errors.Is(err, syscall.EACCES) {
		t.Fatalf("вышедший процесс должен дать ошибку чтения, получили %v", err)
	}
}

type readProcChild struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	pid    int
	waited bool
}

func startReadProcChild(t testing.TB, script string, args ...string) *readProcChild {
	t.Helper()
	cmd := exec.Command("sh", append([]string{"-c", script}, args...)...)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("дочерний процесс не готов: %q, %v", line, err)
	}
	child := &readProcChild{cmd: cmd, in: in, pid: cmd.Process.Pid}
	t.Cleanup(child.stop)
	return child
}

func (c *readProcChild) stop() {
	if c.waited {
		return
	}
	_, _ = proc.StopChild(c.pid, 0, func() error { return c.cmd.Process.Kill() })
	c.wait()
}

func (c *readProcChild) wait() {
	if c.waited {
		return
	}
	_ = c.cmd.Wait()
	c.waited = true
}

func assertReadProcMatchesLegacy(t *testing.T, name string, pid int) {
	t.Helper()
	got, gotErr := readProc(pid)
	want, wantErr := legacyReadProc(pid)
	if !sameProcRead(got, gotErr, want, wantErr) {
		t.Errorf("%s pid %d: новое %+v (%v), старое %+v (%v)", name, pid, got, gotErr, want, wantErr)
	}
}

func sameProcRead(a procData, ea error, b procData, eb error) bool {
	if (ea == nil) != (eb == nil) {
		return false
	}
	if ea != nil {
		var aa, bb syscall.Errno
		return errors.As(ea, &aa) && errors.As(eb, &bb) && aa == bb
	}
	return a == b
}

func readProcAgainstLegacy(pid int) (procData, error, procData, error, bool) {
	var got procData
	var gotErr error
	var want procData
	var wantErr error
	for range 3 {
		before, beforeErr := legacyReadProc(pid)
		got, gotErr = readProc(pid)
		want, wantErr = legacyReadProc(pid)
		if sameProcRead(got, gotErr, before, beforeErr) || sameProcRead(got, gotErr, want, wantErr) {
			return got, gotErr, want, wantErr, true
		}
	}
	return got, gotErr, want, wantErr, false
}

func waitForZombie(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			t.Fatalf("прочитать stat процесса %d: %v", pid, err)
		}
		if strings.Contains(string(stat), ") Z ") {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("процесс %d не стал зомби", pid)
}

func procFDCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func legacyReadProc(pid int) (procData, error) {
	ps := "/proc/" + strconv.Itoa(pid)
	d := procData{uid: -1}
	comm, err := os.ReadFile(ps + "/comm")
	if err != nil {
		return d, fmt.Errorf("процесс %d: comm: %w", pid, err)
	}
	d.comm = strings.TrimSpace(string(comm))
	cmdline, err := os.ReadFile(ps + "/cmdline")
	if err != nil {
		return d, fmt.Errorf("процесс %d: cmdline: %w", pid, err)
	}
	d.args = strings.Clone(truncate(strings.ReplaceAll(strings.TrimRight(string(cmdline), "\x00"), "\x00", " "), argsMax))
	if d.cwd, err = os.Readlink(ps + "/cwd"); err != nil && !errors.Is(err, os.ErrPermission) {
		return d, fmt.Errorf("процесс %d: cwd: %w", pid, err)
	}
	fi, err := os.Stat(ps)
	if err != nil {
		return d, fmt.Errorf("процесс %d: %w", pid, err)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		d.uid = int(st.Uid)
	}
	return d, nil
}

// truncate — тестовая копия прежнего правила границы UTF-8.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
