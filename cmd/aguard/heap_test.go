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

// Пик памяти: первый замер и рост на hwmStepKB — снимки стеков и кучи и строка журнала; меньший рост — ничего;
// нечитаемый status — ошибка (тикет rss-growth-1118).
func TestHWMCheckSnapshotsOnGrowth(t *testing.T) {
	a, log := quietObservedApp(t)
	status := filepath.Join(t.TempDir(), "status")
	write := func(kb string) {
		if err := os.WriteFile(status, []byte("Name:\taguard\nVmPeak:\t 9999999 kB\nVmHWM:\t "+kb+" kB\nVmRSS:\t 1 kB\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	count := func() (int, int) {
		g, _ := filepath.Glob(filepath.Join(a.dir("debug"), "hwm-goroutine-*.txt"))
		h, _ := filepath.Glob(filepath.Join(a.dir("debug"), "hwm-heap-*.pprof"))
		return len(g), len(h)
	}
	at := time.Date(2026, 11, 26, 16, 0, 0, 0, time.UTC)
	write("100000")
	if got, err := a.hwmCheck(at, 0, status); got != 100000 || err != nil {
		t.Fatalf("первый замер: %d", got)
	}
	if g, h := count(); g != 1 || h != 1 {
		t.Fatalf("первый замер: стеков %d, куч %d", g, h)
	}
	write("132767")
	if got, err := a.hwmCheck(at.Add(time.Second), 100000, status); got != 100000 || err != nil {
		t.Fatalf("рост меньше шага: %d", got)
	}
	if g, h := count(); g != 1 || h != 1 {
		t.Fatalf("рост меньше шага снял снимки: стеков %d, куч %d", g, h)
	}
	write("132768")
	if got, err := a.hwmCheck(at.Add(2*time.Second), 100000, status); got != 132768 || err != nil {
		t.Fatalf("рост на шаг: %d", got)
	}
	if g, h := count(); g != 2 || h != 2 {
		t.Fatalf("рост на шаг: стеков %d, куч %d", g, h)
	}
	g, _ := filepath.Glob(filepath.Join(a.dir("debug"), "hwm-goroutine-*.txt"))
	if b, err := os.ReadFile(g[0]); err != nil || !strings.Contains(string(b), "goroutine profile:") {
		t.Fatalf("стеки: %v %.80q", err, b)
	}
	if err := os.Remove(status); err != nil {
		t.Fatal(err)
	}
	if _, err := a.hwmCheck(at.Add(3*time.Second), 132768, status); err == nil {
		t.Fatal("нечитаемый status без ошибки")
	}
	if s := log.String(); strings.Count(s, "aguard: пик памяти вырос") != 2 {
		t.Fatalf("журнал: %s", s)
	}
}

// Снимки роста пика старше hwmKeep удаляются, свежие и чужие файлы debug/ остаются.
func TestPruneHWMSnapshotsKeepsFreshAndForeign(t *testing.T) {
	a, _ := quietObservedApp(t)
	dir := a.dir("debug")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for name, age := range map[string]time.Duration{
		"hwm-heap-old.pprof":     hwmKeep + time.Hour,
		"hwm-goroutine-old.txt":  hwmKeep + time.Hour,
		"hwm-heap-fresh.pprof":   time.Hour,
		"heap-old-by-usr1.pprof": hwmKeep + time.Hour,
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	a.pruneHWMSnapshots(now)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if strings.Join(left, " ") != "heap-old-by-usr1.pprof hwm-heap-fresh.pprof" {
		t.Fatalf("осталось: %v", left)
	}
}
