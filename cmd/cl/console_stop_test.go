package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// stopChild убивает claude вместе с его потомками (хуки, MCP) и прибирает его: зомби не остаётся.
func TestStopChildKillsTreeAndReaps(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	// sleep кончается сам через 30 с, если stopChild его не убьёт
	cmd := exec.Command("sh", "-c", `sleep 30 & echo $! >"$0"; wait`, pidFile)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var grandchild int
	for deadline := time.Now().Add(3 * time.Second); grandchild == 0 && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if b, err := os.ReadFile(pidFile); err == nil {
			grandchild, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
	}
	if grandchild == 0 {
		t.Fatal("потомок не стартовал")
	}
	if err := stopChild(cmd); err != nil {
		t.Fatal(err)
	}
	if cmd.ProcessState == nil {
		t.Fatal("claude не прибран: Wait не звали")
	}
	// потомка прибирает его новый родитель: ждём, пока он пропадёт или станет зомби
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(grandchild), "stat"))
		if err != nil {
			return
		}
		if s := string(b); strings.Contains(s[strings.LastIndexByte(s, ')')+1:], " Z ") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("потомок %d жив после stopChild", grandchild)
		}
	}
}
