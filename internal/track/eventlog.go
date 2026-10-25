package track

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"

	"tmpguard/internal/record"
)

// eventLog — дозапись owners.jsonl с ротацией в owners.jsonl.1.
type eventLog struct {
	path   string
	f      *os.File
	size   int64
	rotate int64
}

func openEventLog(path string, rotate int64) (*eventLog, error) {
	l := &eventLog{path: path, rotate: rotate}
	return l, l.open()
}

func (l *eventLog) open() error {
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	l.f, l.size = f, fi.Size()
	return nil
}

func (l *eventLog) write(e record.Event) {
	b, err := json.Marshal(e)
	if err != nil {
		log.Printf("tmpguard-track: журнал: %v", err)
		return
	}
	b = append(b, '\n')
	n, err := l.f.Write(b)
	l.size += int64(n)
	if err != nil {
		log.Printf("tmpguard-track: журнал: %v", err)
	}
	if l.rotate > 0 && l.size >= l.rotate {
		_ = l.f.Close()
		if err := os.Rename(l.path, filepath.Join(filepath.Dir(l.path), record.RotatedOwner)); err != nil {
			log.Printf("tmpguard-track: ротация журнала: %v", err)
		}
		if err := l.open(); err != nil {
			log.Fatalf("tmpguard-track: журнал после ротации: %v", err)
		}
	}
}

func (l *eventLog) close() { _ = l.f.Close() }
