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
	a, log := quietObservedApp(t)
	sig := make(chan os.Signal, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.heapLoop(ctx, sig); close(done) }()
	sig <- syscall.SIGUSR1
	deadline := time.Now().Add(5 * time.Second)
	var files []string
	for time.Now().Before(deadline) {
		files, _ = filepath.Glob(filepath.Join(a.dir("debug"), "heap-*.pprof"))
		if len(files) == 1 { // журнал читаем после <-done: буфер не для параллельного чтения
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if len(files) != 1 || !strings.Contains(log.String(), "aguard heap: профиль кучи") {
		t.Fatalf("профилей %d: %s", len(files), log.String())
	}
	if fi, err := os.Stat(files[0]); err != nil || fi.Size() == 0 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("профиль %s: %v %v", files[0], fi, err)
	}
}
