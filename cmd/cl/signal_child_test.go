package main

import (
	"bufio"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// Сигнал claude доходит до прямого потомка cl и не трогает его потомков (фоновые задачи сессии): внук sleep
// переживает выход sh и выходит сам через 3 с — тест его не сигналит.
func TestSignalChildLeavesGrandchild(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 3 & echo $!; wait")
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
	if _, err := os.Stat("/proc/" + strconv.Itoa(grand)); err != nil {
		t.Fatalf("внук %d снят вместе с sh: %v", grand, err)
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
