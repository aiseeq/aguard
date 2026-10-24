package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func write(t *testing.T, file, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadHookState(t *testing.T) {
	dir := t.TempDir()
	p := paths{hookStates: dir}
	if rec, err := readHookState(p, "s1"); err != nil || rec.state != "" {
		t.Errorf("нет записи — пустое состояние: %+v %v", rec, err)
	}
	write(t, filepath.Join(dir, "s1"), "paused\n")
	write(t, filepath.Join(dir, "s1.tasks"), "{}")
	rec, err := readHookState(p, "s1")
	if err != nil || rec.state != "paused" || !rec.tasks || rec.written.IsZero() {
		t.Errorf("readHookState = %+v %v", rec, err)
	}
}

func TestDescendantsAndVersion(t *testing.T) {
	proc := t.TempDir()
	write(t, filepath.Join(proc, "10", "stat"), "10 (claude) S 1 10 10 0")
	write(t, filepath.Join(proc, "11", "stat"), "11 (bash) S 10 11 11 0")
	write(t, filepath.Join(proc, "12", "stat"), "12 (sleep) S 11 12 12 0")
	write(t, filepath.Join(proc, "13", "stat"), "13 (konsole) S 1 13 13 0")
	write(t, filepath.Join(proc, "self", "stat"), "x")
	got, err := descendants(proc, 10)
	if err != nil || !slices.Equal(got, []string{"11 bash", "12 sleep"}) {
		t.Errorf("descendants = %q %v", got, err)
	}
	if err := os.Symlink("/home/u/.local/share/claude/versions/2.1.309 (deleted)", filepath.Join(proc, "10", "exe")); err != nil {
		t.Fatal(err)
	}
	if v, err := runningVersion(proc, 10); err != nil || v != "2.1.309" {
		t.Errorf("runningVersion = %q %v", v, err)
	}
}

func TestTranscriptFile(t *testing.T) {
	dir := t.TempDir()
	p := paths{projects: dir}
	if _, err := transcriptFile(p, "s1"); err == nil {
		t.Error("нет транскрипта — ошибка")
	}
	write(t, filepath.Join(dir, "-home-u", "s1.jsonl"), "")
	if f, err := transcriptFile(p, "s1"); err != nil || f != filepath.Join(dir, "-home-u", "s1.jsonl") {
		t.Errorf("transcriptFile = %q %v", f, err)
	}
}
