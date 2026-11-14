package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestHeapLoopWritesProfileOnSignal(t *testing.T) {
	old := cpuProfileFor
	cpuProfileFor = 100 * time.Millisecond
	t.Cleanup(func() { cpuProfileFor = old })
	a, log := quietObservedApp(t)
	sig := make(chan os.Signal, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.heapLoop(ctx, sig); close(done) }()
	sig <- syscall.SIGUSR1
	deadline := time.Now().Add(5 * time.Second)
	var files, cpu []string
	for time.Now().Before(deadline) {
		files, _ = filepath.Glob(filepath.Join(a.dir("debug"), "heap-*.pprof"))
		cpu, _ = filepath.Glob(filepath.Join(a.dir("debug"), "cpu-*.pprof"))
		if len(files) == 1 && len(cpu) == 1 { // журнал читаем после <-done: буфер не для параллельного чтения
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if len(files) != 1 || len(cpu) != 1 || !strings.Contains(log.String(), "aguard heap: профиль кучи") ||
		!strings.Contains(log.String(), "aguard cpu: профиль CPU") {
		t.Fatalf("профилей кучи %d, CPU %d: %s", len(files), len(cpu), log.String())
	}
	for _, f := range append(files, cpu...) {
		if fi, err := os.Stat(f); err != nil || fi.Size() == 0 || fi.Mode().Perm() != 0o600 {
			t.Fatalf("профиль %s: %v %v", f, fi, err)
		}
	}
}
