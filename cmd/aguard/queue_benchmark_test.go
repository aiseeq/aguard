package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/config"
	"github.com/aiseeq/aguard/internal/queue"
)

// Benchmark a copied 4-project root with the installed tk binary. No live task roots are read or changed.
func BenchmarkQueueRefreshSynthetic(b *testing.B) {
	tk := "/home/aisee/bin/tk"
	if _, err := os.Stat(tk); err != nil {
		b.Skipf("installed tk unavailable: %v", err)
	}
	for _, mode := range []string{"before-full-root", "after-partial-project"} {
		b.Run(mode, func(b *testing.B) {
			dir := b.TempDir()
			b.Setenv("HOME", dir)
			root := filepath.Join(dir, "root-copy")
			projects := make([]string, 4)
			for p := range projects {
				project := filepath.Join(root, fmt.Sprintf("project-%d", p))
				projects[p] = project
				if err := os.MkdirAll(filepath.Join(project, "docs", "tasks"), 0o755); err != nil {
					b.Fatal(err)
				}
				if out, err := exec.Command("git", "init", "-q", project).CombinedOutput(); err != nil {
					b.Fatalf("git init %s: %v: %s", project, err, out)
				}
				for i := range 40 {
					name := fmt.Sprintf("202611%02d-%02d00-LOW-%d-%03d.md", 1+i/24, i%24, p, i)
					body := fmt.Appendf(nil, "---\nstatus: открыт\nback: lead\n---\n# ticket %d-%d\n", p, i)
					if err := os.WriteFile(filepath.Join(project, "docs", "tasks", name), body, 0o644); err != nil {
						b.Fatal(err)
					}
				}
			}
			a := &app{home: dir, stateDir: filepath.Join(dir, "state"), log: slog.New(slog.DiscardHandler), out: &strings.Builder{}}
			c := &config.Config{Lead: "lead", Queue: queue.Config{GitRoots: []string{root}, Tk: tk}}
			dirs, err := queueFingerprintDirs(&c.Queue)
			if err != nil {
				b.Fatal(err)
			}
			change := filepath.Join(projects[0], "docs", "tasks", "20261101-0000-LOW-0-000.md")
			if mode == "after-partial-project" {
				if _, err := a.cachedQueueTickets(context.Background(), c, time.Now()); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				body := fmt.Appendf(nil, "---\nstatus: открыт\nback: lead\n---\n# edit %d\n", i)
				if err := os.WriteFile(change, body, 0o644); err != nil {
					b.Fatal(err)
				}
				if mode == "before-full-root" {
					if _, err := queue.Fingerprint(dirs); err != nil {
						b.Fatal(err)
					}
					if _, err := a.queueTickets(context.Background(), c); err != nil {
						b.Fatal(err)
					}
				} else if _, err := a.cachedQueueTickets(context.Background(), c, time.Now()); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
