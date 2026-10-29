package iowatch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// Window — шаги и детальные записи снимков папки в окне, по времени, каждая строка один раз
// (буфер события и ручной снимок могут нести одни и те же шаги).
type Window struct {
	Steps, Details []Step
}

// spanSlack — запас к началу снимка: буфер пишется от первого шага, тикер может отставать.
const spanSlack = time.Minute

// ReadSnapshots читает записи [from, to) из снимков dir. Файл, чей промежуток не пересекает окно,
// не открывается: начало — время в имени минус ring (в начале снимка — буфер до события), конец —
// время изменения. Строки вне окна не хранятся. Нет папки — пустое окно.
func ReadSnapshots(dir string, from, to time.Time, ring time.Duration, warn func(error)) (Window, error) {
	var w Window
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return w, nil
	}
	if err != nil {
		return w, fmt.Errorf("прочитать папку снимков %s: %w", dir, err)
	}
	seen := map[string]map[int64]bool{KindStep: {}, KindDetail: {}}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		info, err := entry.Info()
		if errors.Is(err, os.ErrNotExist) {
			continue // срок вышел между ReadDir и Info
		}
		if err != nil {
			return w, fmt.Errorf("снимок %s: %w", name, err)
		}
		if info.ModTime().Before(from) {
			continue
		}
		if start, ok := nameTime(name); ok && !start.Add(-ring-spanSlack).Before(to) {
			continue
		}
		path := filepath.Join(dir, name)
		f, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return w, fmt.Errorf("открыть снимок %s: %w", name, err)
		}
		err = ParseSnapshot(f, func(rec Record) {
			key := rec.At.UnixNano()
			if rec.At.Before(from) || !rec.At.Before(to) || seen[rec.Kind][key] {
				return
			}
			seen[rec.Kind][key] = true
			if rec.Kind == KindStep {
				w.Steps = append(w.Steps, rec.Step)
			} else {
				w.Details = append(w.Details, rec.Step)
			}
		}, func(err error) {
			if warn != nil {
				warn(fmt.Errorf("снимок %s: битая строка пропущена: %w", name, err))
			}
		})
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return w, fmt.Errorf("прочитать снимок %s: %w", name, err)
		}
	}
	byTime := func(a, b Step) int { return a.At.Compare(b.At) }
	slices.SortFunc(w.Steps, byTime)
	slices.SortFunc(w.Details, byTime)
	return w, nil
}

// Empty — в окне нет ни шагов, ни детальных записей.
func (w Window) Empty() bool { return len(w.Steps) == 0 && len(w.Details) == 0 }

// NearestSnapshot — время в имени ближайшего к at снимка.
func NearestSnapshot(dir string, at time.Time) (time.Time, bool, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("прочитать папку снимков %s: %w", dir, err)
	}
	var nearest time.Time
	var best time.Duration
	for _, entry := range entries {
		stamp, ok := nameTime(entry.Name())
		if !ok {
			continue
		}
		d := stamp.Sub(at).Abs()
		if nearest.IsZero() || d < best {
			nearest, best = stamp, d
		}
	}
	return nearest, !nearest.IsZero(), nil
}

// Second — секунда окна: быстрые шаги — сумма, плюс пик шага и очереди; детальные записи — как есть.
type Second struct {
	At                                     time.Time
	Steps                                  int // быстрых шагов в секунде; 0 — только детальные записи
	IOFullUS, MemFullUS, PeakUS, SwapIn    int64
	ReadBytes, WriteBytes, IOTicks         int64
	RIOS, RMS, WIOS, WMS, Flushes, InQueue int64
	Details                                []Step
}

// Seconds — окно по секундам, по порядку.
func (w Window) Seconds() []Second {
	var out []Second
	at := func(t time.Time) *Second {
		sec := t.Truncate(time.Second)
		i := sort.Search(len(out), func(i int) bool { return !out[i].At.Before(sec) })
		if i == len(out) || !out[i].At.Equal(sec) {
			out = slices.Insert(out, i, Second{At: sec})
		}
		return &out[i]
	}
	for i := range w.Steps {
		s := &w.Steps[i]
		b := at(s.At)
		b.Steps++
		b.IOFullUS += s.IOFullUS
		b.MemFullUS += s.MemFullUS
		b.PeakUS = max(b.PeakUS, Idle(*s))
		b.SwapIn += s.SwapIn
		b.ReadBytes += s.ReadBytes
		b.WriteBytes += s.WriteBytes
		b.IOTicks += s.IOTicks
		b.RIOS += s.RIOS
		b.RMS += s.RMS
		b.WIOS += s.WIOS
		b.WMS += s.WMS
		b.Flushes += s.FIOS
		b.InQueue = max(b.InQueue, s.InFlight)
	}
	for i := range w.Details {
		b := at(w.Details[i].At)
		b.Details = append(b.Details, w.Details[i])
	}
	return out
}
