// Package memwatch пишет память сессий агентов: раз в интервал по каждому
// scope сессии в agents.slice (agent-<имя>-<pid>.scope, так их запускает cl) —
// пик памяти группы за интервал, текущую память, своп, события memory.high/max/oom
// и самые крупные процессы. Отдельный наблюдатель ceiling управляет заданным cl потолком.
package memwatch

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aiseeq/aguard/internal/ceiling"
	"github.com/aiseeq/aguard/internal/cgroupstats"
	"github.com/aiseeq/akit/proc"
)

// listScopes — scope сессий в папке слайса.
func listScopes(slice string) ([]string, error) {
	ents, err := os.ReadDir(slice)
	if err != nil {
		return nil, fmt.Errorf("слайс %s: %w", slice, err)
	}
	var out []string
	for _, e := range ents {
		if _, _, ok := proc.ScopeName(e.Name()); ok && e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// readKeyed — файл вида «ключ число» построчно (memory.stat, memory.events).
func readKeyed(path string) (map[string]int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cgroup: %w", err)
	}
	defer f.Close()
	return parseKeyed(f)
}

func parseKeyed(r io.Reader) (map[string]int64, error) {
	return cgroupstats.ParseKeyed(r, nil)
}

// procsUnder — pid всех процессов группы и её подгрупп.
func procsUnder(dir string) ([]int, error) {
	var out []int
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // подгруппа ушла во время обхода
			}
			return err
		}
		if d.IsDir() || d.Name() != "cgroup.procs" {
			return nil
		}
		b, err := os.ReadFile(p)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		for f := range strings.FieldsSeq(string(b)) {
			pid, err := strconv.Atoi(f)
			if err != nil {
				return fmt.Errorf("%s: %q: %w", p, f, err)
			}
			out = append(out, pid)
		}
		return nil
	})
	if err != nil {
		return out, fmt.Errorf("процессы группы: %w", err)
	}
	return out, nil
}

// peakReader читает пик памяти группы за интервал: memory.peak, открытый один
// раз, после записи в него отдаёт пик с момента записи (fd-local, ядро 6.12+).
type peakReader struct {
	f *os.File
}

func openPeak(dir string) (*peakReader, error) {
	f, err := os.OpenFile(filepath.Join(dir, "memory.peak"), os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("memory.peak: %w", err)
	}
	return &peakReader{f: f}, nil
}

// take — пик с прошлого take (в первый раз — за всю жизнь группы) и сброс.
func (p *peakReader) take() (int64, error) {
	buf := make([]byte, 32)
	n, err := p.f.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, fmt.Errorf("memory.peak: %w", err)
	}
	v, err := ceiling.ParseInt(string(buf[:n]))
	if err != nil {
		return 0, err
	}
	if _, err := p.f.WriteAt([]byte("reset\n"), 0); err != nil {
		return v, fmt.Errorf("memory.peak: сброс: %w", err)
	}
	return v, nil
}

func (p *peakReader) close() error { return p.f.Close() }
