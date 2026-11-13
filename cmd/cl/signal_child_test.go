package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Сигнал claude доходит до прямого потомка cl и не трогает его потомков (фоновые задачи сессии). Внук отмечает
// полученный SIGTERM в файле got и выходит сам, когда тест создаёт файл stop; тест сигналит только свой sh.
func TestSignalChildLeavesGrandchild(t *testing.T) {
	dir := t.TempDir()
	got, stop := filepath.Join(dir, "got"), filepath.Join(dir, "stop")
	t.Cleanup(func() { _ = os.WriteFile(stop, nil, 0600) })
	grandchild := fmt.Sprintf(`trap 'echo TERM > %q' TERM; while [ ! -e %q ]; do sleep 0.05; done`, got, stop)
	cmd := exec.Command("sh", "-c", fmt.Sprintf("sh -c %q & echo $!; wait", grandchild))
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	grand, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	if err := signalChild(cmd.Process, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	st, ok := err.(*exec.ExitError)
	if !ok || st.Sys().(syscall.WaitStatus).Signal() != syscall.SIGTERM {
		t.Fatalf("sh не снят SIGTERM: %v", err)
	}
	time.Sleep(300 * time.Millisecond) // сигнал внуку, если бы он был, доходит асинхронно
	if _, err := os.Stat(got); err == nil {
		t.Fatalf("внук %d получил SIGTERM вместе с sh", grand)
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(grand) + "/stat")
	if err != nil {
		t.Fatalf("внук %d снят вместе с sh: %v", grand, err)
	}
	if _, rest, ok := strings.Cut(string(stat), ") "); !ok || strings.HasPrefix(rest, "Z") {
		t.Fatalf("внук %d — зомби: %s", grand, stat)
	}
}

// Вышедший и собранный claude — не ошибка: его выход и так придёт в done.
func TestSignalChildGoneIsNil(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if err := signalChild(cmd.Process, syscall.SIGTERM); err != nil {
		t.Fatalf("сигнал вышедшему: %v", err)
	}
}
