package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
)

// TestRSSProfile по запросу воспроизводит проход на замороженном снимке слежения.
func TestRSSProfile(t *testing.T) {
	dir := os.Getenv("AGUARD_RSS_FIXTURE")
	if dir == "" {
		t.Skip("AGUARD_RSS_FIXTURE не задан")
	}
	outputDir := os.Getenv("AGUARD_RSS_OUTPUT")
	if outputDir == "" {
		outputDir = t.TempDir()
	}
	cfg := policy.Default("/nonexistent")
	cfg.RunDir, cfg.TrackDir = dir, dir
	a := &app{uid: os.Getuid(), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	stop := make(chan struct{})
	done := make(chan uint64, 1)
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		var peak uint64
		for {
			select {
			case <-stop:
				done <- peak
				return
			case <-ticker.C:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				if m.HeapAlloc > peak+(8<<20) {
					peak = m.HeapAlloc
					f, err := os.Create(filepath.Join(outputDir, "peak.pb.gz"))
					if err == nil {
						_ = pprof.WriteHeapProfile(f)
						_ = f.Close()
					}
				}
			}
		}
	}()
	snap, err := a.snapshot(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	items := cfg.Items(snap.Entries, os.Getuid())
	groups := cfg.Over(time.Now(), items, 1)
	close(stop)
	t.Logf("sampled peak heap %.1f MiB", float64(<-done)/(1<<20))
	runtime.GC()
	f, err := os.Create(filepath.Join(outputDir, "inuse.pb.gz"))
	if err != nil {
		t.Fatal(err)
	}
	if err := pprof.WriteHeapProfile(f); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("entries=%d items=%d groups=%d", len(snap.Entries), len(items), len(groups))
	runtime.KeepAlive(items)
	runtime.KeepAlive(snap)
}
