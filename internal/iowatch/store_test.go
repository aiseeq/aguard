package iowatch

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Два писателя одного файла друг другу не мешают (копия сводок раз в час и остановка службы — разные горутины): у
// каждого свой временный файл; сбой записи временного файла не оставляет (01c863b).
func TestWriteAtomicTwoWritersAndFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cases.jsonl")
	err := writeAtomic(path, func(w *bufio.Writer) error {
		if _, err := w.WriteString("первый\n"); err != nil {
			return err
		}
		return writeAtomic(path, func(w *bufio.Writer) error { // второй — пока первый не дописал
			_, err := w.WriteString("второй\n")
			return err
		})
	})
	if err != nil {
		t.Fatalf("два писателя: %v", err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "первый\n" {
		t.Fatalf("файл после двух писателей: %q %v", b, err)
	}
	boom := errors.New("сбой")
	if err := writeAtomic(path, func(*bufio.Writer) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("сбой записи: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "cases.jsonl" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("после сбоя в папке %v, ждал только cases.jsonl", names)
	}
}

// Временный файл снимка (<имя>.jsonl.<случайное>.part), брошенный падением, уходит вместе со старыми снимками.
func TestCleanupRemovesOldTempPart(t *testing.T) {
	dir := t.TempDir()
	part := filepath.Join(dir, "manual-20261029-120000.jsonl.123456.part")
	writeFile(t, part, "{}\n")
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(part, past, past); err != nil {
		t.Fatal(err)
	}
	if err := CleanupSnapshots(dir, time.Hour, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Fatalf("старый временный файл снимка остался: %v", err)
	}
}
