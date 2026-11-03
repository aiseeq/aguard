package track

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/aiseeq/aguard/internal/record"
)

// eventLog — дозапись owners.jsonl с ротацией в owners.jsonl.1.
type eventLog struct {
	path   string
	f      *os.File
	size   int64
	rotate int64
	log    *slog.Logger
	err    error // журнал не открылся после ротации: сторож должен выйти
}

func openEventLog(path string, rotate int64, lg *slog.Logger) (*eventLog, error) {
	l := &eventLog{path: path, rotate: rotate, log: lg}
	return l, l.open()
}

func (l *eventLog) open() error {
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("журнал %s: %w", l.path, err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("журнал %s: stat: %w", l.path, err)
	}
	l.f, l.size = f, fi.Size()
	return nil
}

// write дописывает строку. Если после ротации журнал не открылся, строки
// больше не пишутся, а ошибку в l.err забирает главный цикл и завершает сторож.
func (l *eventLog) write(e record.Event) {
	if l.err != nil {
		return
	}
	b, err := json.Marshal(e)
	if err != nil {
		l.log.Error("aguard слежение: журнал, строка пропущена", "err", err)
		return
	}
	b = append(b, '\n')
	n, err := l.f.Write(b)
	l.size += int64(n)
	if err != nil {
		l.log.Error("aguard слежение: журнал, запись", "err", err)
	}
	if l.rotate > 0 && l.size >= l.rotate {
		_ = l.f.Close()
		if err := os.Rename(l.path, filepath.Join(filepath.Dir(l.path), record.RotatedOwner)); err != nil {
			l.log.Error("aguard слежение: ротация журнала, пишу дальше в тот же файл", "err", err)
		}
		if err := l.open(); err != nil {
			l.err = fmt.Errorf("журнал после ротации: %w", err)
		}
	}
}

func (l *eventLog) close() { _ = l.f.Close() }
