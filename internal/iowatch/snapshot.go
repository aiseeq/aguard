package iowatch

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Snapshot — ручной снимок буфера, который пишет своя горутина: главный цикл диска не ждёт.
type Snapshot struct {
	Path string
	done chan error
}

// startSnapshot пишет records в dir/manual-<at>.jsonl.part и переименовывает, когда записан целиком.
func startSnapshot(dir string, at time.Time, records []Record) *Snapshot {
	s := &Snapshot{Path: filepath.Join(dir, ManualName(at)), done: make(chan error, 1)}
	go func() { s.done <- writeSnapshot(s.Path, records) }()
	return s
}

// Wait — дождаться записи и переименования.
func (s *Snapshot) Wait() error {
	if err := <-s.done; err != nil {
		return fmt.Errorf("ручной снимок %s: %w", s.Path, err)
	}
	return nil
}

func writeSnapshot(path string, records []Record) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("создать папку: %w", err)
	}
	return writeAtomic(path, func(w *bufio.Writer) error {
		for i := range records {
			if err := WriteRecord(w, records[i]); err != nil {
				return err
			}
		}
		return nil
	})
}

// CleanupSnapshots удаляет снимки старше keep (по времени изменения).
func CleanupSnapshots(dir string, keep time.Duration, now time.Time) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("прочитать папку снимков %s: %w", dir, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".jsonl") && !strings.HasSuffix(name, ".part") {
			continue
		}
		info, err := entry.Info()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("снимок %s: %w", name, err)
		}
		if now.Sub(info.ModTime()) > keep {
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("удалить старый снимок %s: %w", name, err)
			}
		}
	}
	return nil
}
