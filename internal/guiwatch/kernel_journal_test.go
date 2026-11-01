package guiwatch

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Мера тикета journal-kernel-files: сбор журнала ядра среза идёт по свежим файлам системного журнала (по всем — 559 МБ
// с диска при холодном кэше, замер 01.11), без --grep (код 1 на пустом окне), строки о графике отбирает Go.
func TestKernelJournalWindowUsesFreshSystemFiles(t *testing.T) {
	start := time.Date(2026, 11, 1, 12, 0, 0, 0, time.Local)
	dir := t.TempDir()
	for name, mtime := range map[string]time.Time{
		"system.journal":           start,
		"system@abc-1-2.journal":   start.Add(-time.Hour),
		"system@def-3-4.journal~":  start.Add(time.Second),
		"user-1000.journal":        start,
		"system-other.journal.bak": start,
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + calls + "\nprintf 'k: i915 0000:00:02.0: [drm] GPU HANG\\nk: usb 1-2: new device\\nk: DRM: atomic commit\\n'\n"
	if err := os.WriteFile(filepath.Join(bin, "journalctl"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	lines, note := kernelJournalWindow(context.Background(), "2026-11-01 12:00:00", "2026-11-01 12:00:10", start, dir, "")
	if note != "" {
		t.Fatalf("note = %q, want none", note)
	}
	want := []string{"k: i915 0000:00:02.0: [drm] GPU HANG", "k: DRM: atomic commit"}
	if !slices.Equal(lines, want) {
		t.Fatalf("lines = %q, want %q", lines, want)
	}
	b, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.TrimSpace(string(b))
	for _, f := range []string{"system.journal", "system@def-3-4.journal~"} {
		if !strings.Contains(args, "--file="+filepath.Join(dir, f)) {
			t.Fatalf("kernel collection misses fresh file %s: %s", f, args)
		}
	}
	if strings.Contains(args, "system@abc-1-2.journal") || strings.Contains(args, "user-1000") || strings.Contains(args, "--grep") {
		t.Fatalf("kernel collection reads stale or user files or greps: %s", args)
	}
}

func TestKernelJournalWindowFallbackAndError(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "journalctl"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	lines, note := kernelJournalWindow(context.Background(), "a", "b", time.Now(), t.TempDir(), "")
	if lines != nil || !strings.HasPrefix(note, "journal kernel fallback: no fresh system journal files") || !strings.Contains(note, "journalctl kernel error: exit status 1") {
		t.Fatalf("lines = %q, note = %q", lines, note)
	}
}
