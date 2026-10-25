// Package daily — дневные файлы записей JSONL: <dir>/2006-01-02.jsonl; прошлые дни сжимаются в .jsonl.gz, старше
// срока хранения удаляются. Запись идёт в файл своего дня, поэтому дописывать можно и задним числом.
package daily

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Store — дневные файлы записей типа T. At — момент записи, What — что в файлах (для текста ошибок).
type Store[T any] struct {
	Dir  string
	What string
	At   func(*T) time.Time
}

const dayForm = time.DateOnly

func (s Store[T]) dayPath(t time.Time) string {
	return filepath.Join(s.Dir, t.Format(dayForm)+".jsonl")
}

// Append дописывает записи, каждую в файл её дня.
func (s Store[T]) Append(items []T) error {
	if len(items) == 0 {
		return nil
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("%s: %w", s.What, err)
	}
	byDay := map[string][]*T{}
	for i := range items {
		p := s.dayPath(s.At(&items[i]))
		byDay[p] = append(byDay[p], &items[i])
	}
	var errs []error
	for _, p := range slices.Sorted(maps.Keys(byDay)) {
		errs = append(errs, s.appendFile(p, byDay[p]))
	}
	return errors.Join(errs...)
}

func (s Store[T]) appendFile(path string, items []*T) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("%s: %w", s.What, err)
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for _, it := range items {
		if err := enc.Encode(it); err != nil {
			return fmt.Errorf("%s: %w", s.What, errors.Join(err, f.Close()))
		}
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("%s: %w", s.What, errors.Join(err, f.Close()))
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("%s: %w", s.What, err)
	}
	return nil
}

// Rotate сжимает файлы прошлых дней и удаляет файлы старше keep.
func (s Store[T]) Rotate(now time.Time, keep time.Duration) error {
	ents, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: %w", s.What, err)
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
	if err := place(out.Name(), path+".gz"); err != nil {
		return fmt.Errorf("сжатие: %w", errors.Join(err, os.Remove(out.Name())))
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("сжатие: %w", err)
	}
	return nil
}

// Read отдаёт записи начиная с from по порядку дней. Битые строки (оборванная
// последняя при падении) пропускаются и считаются в bad.
func (s Store[T]) Read(from time.Time, fn func(*T)) (bad int, err error) {
	ents, err := os.ReadDir(s.Dir)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", s.What, err)
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

func (s Store[T]) readFile(path string, from time.Time, fn func(*T)) (bad int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", s.What, err)
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
		var smp T
		if json.Unmarshal(sc.Bytes(), &smp) != nil {
			bad++
			continue
		}
		if !s.At(&smp).Before(from) {
			fn(&smp)
		}
	}
	if err := sc.Err(); err != nil {
		return bad, fmt.Errorf("%s: %w", path, err)
	}
	return bad, nil
}

// place кладёт сжатый кусок на место .gz. Файл дня уже сжат (записи пришли задним числом) — кусок дописывается
// следующим членом gzip: gzip.Reader читает такие файлы целиком. Сбой между дописыванием и удалением .jsonl даст
// повтор записей этого куска при следующем сжатии — лучше повтор, чем потеря.
func place(tmp, gz string) error {
	if _, err := os.Stat(gz); errors.Is(err, os.ErrNotExist) {
		return os.Rename(tmp, gz)
	}
	in, err := os.Open(tmp)
	if err != nil {
		return fmt.Errorf("сжатый кусок: %w", err)
	}
	defer in.Close()
	out, err := os.OpenFile(gz, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("сжатый день: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("дописать %s: %w", gz, errors.Join(err, out.Close()))
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("дописать %s: %w", gz, err)
	}
	return os.Remove(tmp)
}
