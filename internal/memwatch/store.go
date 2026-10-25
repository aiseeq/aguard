package memwatch

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Store — дневные файлы записей: <dir>/2006-01-02.jsonl; прошлые дни сжимаются
// в .jsonl.gz, старше keep удаляются.
type Store struct {
	Dir string
}

const dayForm = time.DateOnly

func (s Store) dayPath(t time.Time) string {
	return filepath.Join(s.Dir, t.Format(dayForm)+".jsonl")
}

// Append дописывает записи в файл дня записи.
func (s Store) Append(samples []Sample) error {
	if len(samples) == 0 {
		return nil
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("записи памяти: %w", err)
	}
	f, err := os.OpenFile(s.dayPath(samples[0].At), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("записи памяти: %w", err)
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for i := range samples {
		if err := enc.Encode(&samples[i]); err != nil {
			_ = f.Close()
			return fmt.Errorf("записи памяти: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		return fmt.Errorf("записи памяти: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("записи памяти: %w", err)
	}
	return nil
}

// Rotate сжимает файлы прошлых дней и удаляет файлы старше keep.
func (s Store) Rotate(now time.Time, keep time.Duration) error {
	ents, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("записи памяти: %w", err)
	}
	today := now.Format(dayForm)
	var errs []error
	for _, e := range ents {
		day, gz, ok := dayOf(e.Name())
		if !ok {
			continue
		}
		p := filepath.Join(s.Dir, e.Name())
		t, err := time.ParseInLocation(dayForm, day, now.Location())
		if err != nil {
			errs = append(errs, fmt.Errorf("день в имени %s: %w", e.Name(), err))
			continue
		}
		switch {
		case now.Sub(t) > keep:
			errs = append(errs, os.Remove(p))
		case !gz && day != today:
			errs = append(errs, compress(p))
		}
	}
	return errors.Join(errs...)
}

// dayOf — день из имени файла записей и сжат ли он. Имя проверяется по форме
// dayForm (цифры и дефисы на своих местах); сам день разбирает вызывающий.
func dayOf(name string) (day string, gz, ok bool) {
	day, gz = strings.CutSuffix(name, ".jsonl.gz")
	if !gz {
		var found bool
		if day, found = strings.CutSuffix(name, ".jsonl"); !found {
			return "", false, false
		}
	}
	if len(day) != len(dayForm) {
		return "", false, false
	}
	for i := range len(day) {
		if (dayForm[i] == '-') != (day[i] == '-') || day[i] != '-' && !isDigit(day[i]) {
			return "", false, false
		}
	}
	return day, gz, true
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func compress(path string) error {
	in, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("сжатие: %w", err)
	}
	defer in.Close()
	out, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("сжатие: %w", err)
	}
	zw := gzip.NewWriter(out)
	_, cerr := io.Copy(zw, in)
	if err := errors.Join(cerr, zw.Close(), out.Close()); err != nil {
		return fmt.Errorf("сжатие %s: %w", path, errors.Join(err, os.Remove(out.Name())))
	}
	if err := os.Rename(out.Name(), path+".gz"); err != nil {
		return fmt.Errorf("сжатие: %w", errors.Join(err, os.Remove(out.Name())))
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("сжатие: %w", err)
	}
	return nil
}

// Read отдаёт записи начиная с from по порядку дней. Битые строки (оборванная
// последняя при падении) пропускаются и считаются в bad.
func (s Store) Read(from time.Time, fn func(*Sample)) (bad int, err error) {
	ents, err := os.ReadDir(s.Dir)
	if err != nil {
		return 0, fmt.Errorf("записи памяти: %w", err)
	}
	var names []string
	for _, e := range ents {
		day, _, ok := dayOf(e.Name())
		if ok && day >= from.Format(dayForm) {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	for _, n := range names {
		b, err := s.readFile(filepath.Join(s.Dir, n), from, fn)
		bad += b
		if err != nil {
			return bad, err
		}
	}
	return bad, nil
}

func (s Store) readFile(path string, from time.Time, fn func(*Sample)) (bad int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("записи памяти: %w", err)
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", path, err)
		}
		defer zr.Close()
		r = zr
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<20)
	for sc.Scan() {
		var smp Sample
		if json.Unmarshal(sc.Bytes(), &smp) != nil {
			bad++
			continue
		}
		if !smp.At.Before(from) {
			fn(&smp)
		}
	}
	if err := sc.Err(); err != nil {
		return bad, fmt.Errorf("%s: %w", path, err)
	}
	return bad, nil
}
