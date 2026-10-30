package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCollectRunStatsUsesMaxAcrossDevices(t *testing.T) {
	root := t.TempDir()
	proc := filepath.Join(root, "proc")
	cgroup := filepath.Join(root, "cgroup")
	for _, dir := range []string{filepath.Join(proc, "self"), filepath.Join(cgroup, "aguard.service")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, source string) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("testdata", source))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(proc, "self", "cgroup"), "run-cgroup.txt")
	write(filepath.Join(cgroup, "aguard.service", "io.stat"), "run-io.stat")
	write(filepath.Join(proc, "self", "status"), "run-status.txt")
	stats := collectRunStats(proc, cgroup, filepath.Join(proc, "self", "status"))
	if stats.readErr != nil || stats.writeErr != nil || stats.readBytes != 999999 || stats.writeBytes != 888888 {
		t.Fatalf("I/O stats = %+v", stats)
	}
	if stats.vmHWMErr != nil || stats.vmHWMKB != 256000 {
		t.Fatalf("RSS stats = %+v", stats)
	}
}
