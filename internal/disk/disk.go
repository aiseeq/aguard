// Package disk — наблюдатель диска (задание лида 26.10 по реплике пользователя 12:14 CET о мусоре на диске: «Ну вот
// вроде бы aguard должен за этим всем следить»). Записи — дети папок кэшей и состояний (~/.cache/*,
// ~/.local/state/*, /var/tmp/*) и дети папок-контейнеров (scratchpad сессий, worktree). Три сигнала: запись выросла
// за сутки сверх порога (grew), большая запись днями без изменений — остаток временного (stale), свободное место
// быстро тает — кто его ест (fast). Режим log: только журнал, кому и что сообщил бы.
package disk

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
	"github.com/aiseeq/aguard/internal/record"
)

// Config — раздел [disk].
type Config struct {
	Mode       string          `toml:"mode"`        // log — только журнал
	Interval   policy.Duration `toml:"interval"`    // замер свободного места
	Walk       policy.Duration `toml:"walk"`        // минимальный промежуток между обходами записей
	WalkChange policy.Size     `toml:"walk_change"` // изменение свободного места для обхода
	WalkMax    policy.Duration `toml:"walk_max"`    // максимум между обходами записей
	Roots      []string        `toml:"roots"`       // их дети — записи
	Containers []string        `toml:"containers"`  // шаблоны glob: дети совпавших папок — записи, предки — нет
	Grow       policy.Size     `toml:"grow"`        // рост записи за сутки
	Fast       policy.Size     `toml:"fast"`        // убыль свободного места за fast_window
	FastWindow policy.Duration `toml:"fast_window"` // окно убыли
	Stale      policy.Duration `toml:"stale"`       // столько без изменений — остаток
	StaleMin   policy.Size     `toml:"stale_min"`   // остаток от этого размера
	// Lasting — долгоживущие кэши инструментов (glob по пути записи): не остатки, сколько бы ни лежали.
	Lasting []string `toml:"lasting"`
	// FiemapMin — файл меньше этого места по st_blocks считается без FIEMAP (0 — FIEMAP на все): reflink мелких файлов
	// считается в каждой копии. Причина и ошибка — docs/design.md, наблюдатель disk.
	FiemapMin policy.Size `toml:"fiemap_min"`
	// Owners — папка (запись или её предок) -> сессия-хозяин; остальных хозяин ищется по сессиям, иначе — лид.
	Owners map[string]string `toml:"owners"`
	Keep   policy.Duration   `toml:"keep"` // срок журнала
	// Traces — папки, где каждый ребёнок — след работы (traces.go): их дети — тоже записи.
	Traces []TraceClass `toml:"traces"`
	// TraceMin — scratchpad умершей сессии и копии транскриптов — следы от этого размера; находка по классу — от него.
	TraceMin policy.Size `toml:"trace_min"`
	// Track — снимки слежения aguard по ~/.cache и ~/.local/state (экземпляр track-home): кто создал запись.
	// Первый свежий из списка; нет ни одного — создатель неизвестен.
	Track []string `toml:"track"`
	// FreeMin — нижняя граница свободного места на /home: ниже — письмо лиду и FreeTell раз в сутки, пока не поднимется
	// (задание git-admin 28.10: go-build растёт до полки 300–500 ГБ, по сигналу — go clean -cache). Свой режим FreeMode:
	// mode наблюдателя — log, а этот сигнал шлётся.
	FreeMin  policy.Size `toml:"free_min"`
	FreeMode string      `toml:"free_mode"` // off | log | alert
	FreeTell []string    `toml:"free_tell"` // кому сверх лида
	Lead     string      `toml:"-"`
	SendAuth bool        `toml:"-"`
}

// defaultFreeMin — 150 ГБ: стоп-линия свободного места на /home (backlog.md, go-build до полки 300–500 ГБ).
const defaultFreeMin = 150 << 30

// Режимы сигнала о свободном месте.
const (
	FreeOff   = "off"
	FreeAlert = "alert"
)

// ModeLog — только журнал.
const ModeLog = "log"

// Default — значения по умолчанию: пороги по замеру за неделю до 26.10 (docs/design.md, наблюдатель disk).
func Default() Config {
	return Config{
		Mode: ModeLog, Interval: policy.Duration(time.Minute), Walk: policy.Duration(time.Hour),
		WalkChange: 2 << 30, WalkMax: policy.Duration(6 * time.Hour),
		Roots:      []string{"~/.cache", "~/.local/state", "/var/tmp"},
		Containers: []string{"~/.cache/claude-tmp/claude-*/*", "~/.cache/worktrees/*"},
		Grow:       5 << 30, Fast: 5 << 30, FastWindow: policy.Duration(10 * time.Minute),
		Stale: policy.Duration(48 * time.Hour), StaleMin: 1 << 30,
		Lasting: []string{"~/.cache/go-build", "~/.cache/gopls", "~/.cache/goimports", "~/.cache/staticcheck",
			"~/.cache/golangci-lint", "~/.cache/glint", "~/.cache/graft", "~/.cache/google-chrome*",
			"~/.cache/mesa_shader_cache*", "~/.cache/ms-playwright", "~/.cache/puppeteer", "~/.cache/pip",
			"~/.cache/uv", "~/.cache/node-gyp", "~/.cache/trivy", "~/.cache/libdnf5", "~/.cache/fontconfig",
			"~/.cache/typescript", "~/.cache/JetBrains", "~/.cache/mozilla"},
		Keep:     policy.Duration(60 * 24 * time.Hour),
		TraceMin: 1 << 30,
		Track:    []string{filepath.Join(record.TrackRunDir("track-home"), record.StateFile), "~/.local/state/aguard/track-home/" + record.StateFile},
		FreeMin:  defaultFreeMin, FreeMode: FreeAlert, FreeTell: []string{"git-admin"},
	}
}

// Validate раскрывает ~ в путях и проверяет пороги.
func (c *Config) Validate(home string) error {
	if c.Mode != ModeLog {
		return fmt.Errorf("disk.mode = %q: есть только %q", c.Mode, ModeLog)
	}
	if c.Interval.D() < 10*time.Second || c.Walk.D() < c.Interval.D() || c.WalkMax.D() < c.Walk.D() || c.FastWindow.D() < c.Interval.D() {
		return errors.New("disk: interval >= 10s, walk и fast_window не меньше interval, walk_max не меньше walk")
	}
	if c.WalkChange <= 0 {
		return errors.New("disk.walk_change > 0")
	}
	if c.Grow <= 0 || c.Fast <= 0 || c.StaleMin <= 0 || c.TraceMin <= 0 || c.Stale.D() < time.Hour || c.Keep.D() < 24*time.Hour {
		return errors.New("disk: grow, fast, stale_min, trace_min > 0, stale >= 1h, keep >= 24h")
	}
	if c.FiemapMin < 0 {
		return errors.New("disk.fiemap_min >= 0")
	}
	if c.FreeMode != FreeOff && c.FreeMode != ModeLog && c.FreeMode != FreeAlert {
		return fmt.Errorf("disk.free_mode = %q: off, log или alert", c.FreeMode)
	}
	if c.FreeMin <= 0 {
		return errors.New("disk.free_min > 0")
	}

	expand := func(ps []string) {
		for i, p := range ps {
			ps[i] = policy.ExpandHome(p, home)
		}
	}
	expand(c.Roots)
	expand(c.Containers)
	expand(c.Lasting)
	if err := c.validateTraces(home); err != nil {
		return err
	}
	owners := make(map[string]string, len(c.Owners))
	for k, v := range c.Owners {
		owners[filepath.Clean(policy.ExpandHome(k, home))] = v
	}
	c.Owners = owners
	return nil
}

// IsLasting — запись — долгоживущий кэш.
func (c *Config) IsLasting(path string) bool {
	return slices.ContainsFunc(c.Lasting, func(p string) bool {
		ok, err := filepath.Match(p, path)
		return err == nil && ok
	})
}

// Entries — записи: дети корней и контейнеров. Корень или его ребёнок, внутри которого лежит контейнер, сам записью
// не бывает: его рост и так виден по детям контейнера.
func (c *Config) Entries() ([]string, error) {
	var containers []string
	var errs []error // одна нечитаемая папка не прячет записи остальных
	for _, pat := range c.Containers {
		m, err := filepath.Glob(pat)
		if err != nil {
			return nil, fmt.Errorf("disk.containers %q: %w", pat, err)
		}
		for _, p := range m {
			fi, err := os.Lstat(p)
			switch {
			case errors.Is(err, fs.ErrNotExist): // исчезла между Glob и Lstat
			case err != nil:
				errs = append(errs, fmt.Errorf("disk: %w", err))
			case fi.IsDir(): // шаблон задевает и файлы рядом с папками
				containers = append(containers, p)
			}
		}
	}
	holds := func(p string) bool {
		return slices.ContainsFunc(containers, func(ct string) bool { return ct == p || strings.HasPrefix(ct, p+"/") })
	}
	var out []string
	for _, dir := range append(slices.Clone(c.Roots), containers...) {
		des, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("disk: %w", err))
			continue
		}
		for _, de := range des {
			if p := filepath.Join(dir, de.Name()); !holds(p) && de.Type()&fs.ModeSymlink == 0 {
				out = append(out, p)
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out), errors.Join(errs...)
}

// Usage — замер записи: занятое место по экстентам (extents.go: общий экстент reflink-копий внутри записи — один
// раз, жёсткие ссылки — один раз за обход), место по st_blocks, как у du (Blocks), файлы, последнее изменение (ctime:
// его не скрыть копированием с сохранением mtime). Unread — сколько папок и файлов не прочиталось, Err — первые из них.
type Usage struct {
	Bytes   int64
	Blocks  int64
	Files   int64
	Changed time.Time
	Unread  int
	Err     error
	Copies  int64 // копии транскриптов Claude Code (copyName) по st_blocks
	Fiemaps int64 // файлов, измеренных по экстентам
}

type inode struct{ dev, ino uint64 }

// unreadKept — сколько ошибок обхода записи хранить: остальные только считаются.
const unreadKept = 3

// walkEntry обходит запись в пределах её файловой системы. Нечитаемое не прерывает обход: оно считается в unread,
// первые ошибки — в err; ошибка visit — тоже.
func walkEntry(ctx context.Context, path string, visit func(p string, d fs.DirEntry, st *syscall.Stat_t) error) (int, error) {
	var dev uint64
	unread := 0
	var errs []error
	note := func(err error) {
		unread++
		if len(errs) < unreadKept {
			errs = append(errs, err)
		}
	}
	werr := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if cerr := ctx.Err(); cerr != nil { // остановка: обход в 2 млн файлов держал бы её минуту
			return cerr
		}
		if err != nil {
			note(err)
			return nil
		}
		info, err := d.Info()
		if err != nil {
			note(err)
			return nil
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return nil
		}
		if p == path {
			dev = st.Dev
		} else if st.Dev != dev {
			return filepath.SkipDir // другая файловая система
		}
		if err := visit(p, d, st); err != nil {
			note(err)
		}
		return nil
	})
	if werr != nil {
		errs = append(errs, werr)
	}
	return unread, errors.Join(errs...)
}

// fileSpace — место файла: по экстентам (своё и общие, дописанные в shared) или по st_blocks — у долгоживущего кэша
// (exact = false), не обычного файла, файла меньше min и на файловой системе без FIEMAP. Ошибка FIEMAP — место по
// st_blocks и ошибка.
type fileSpace struct {
	exact   bool
	min     int64
	buf     *fiemapBuf
	shared  []span
	fiemaps int64
}

func (f *fileSpace) add(p string, d fs.DirEntry, st *syscall.Stat_t) (int64, error) {
	if !f.exact || !d.Type().IsRegular() || st.Blocks == 0 || st.Blocks*512 < f.min {
		return st.Blocks * 512, nil
	}
	f.fiemaps++
	own, more, err := extents(p, f.buf, f.shared)
	if errors.Is(err, errNoFiemap) {
		f.exact = false
		return st.Blocks * 512, nil
	}
	if err != nil {
		return st.Blocks * 512, err
	}
	f.shared = more
	return own, nil
}

// measure обходит запись. exact — место по экстентам; иначе по st_blocks: долгоживущий кэш инструмента (go-build —
// 2,1 млн файлов 26.10) reflink-копий не делает, а экстенты — половина времени обхода.
// fiemapMin — порог файла для экстентов (fileSpace.min). tick — на каждый файл и папку (MeasureAll считает их на весь
// обход); nil — не считать.
func measure(ctx context.Context, path string, seen map[inode]bool, buf *fiemapBuf, exact bool, fiemapMin int64, tick func()) Usage {
	var u Usage
	space := fileSpace{exact: exact, min: fiemapMin, buf: buf}
	u.Unread, u.Err = walkEntry(ctx, path, func(p string, d fs.DirEntry, st *syscall.Stat_t) error {
		if tick != nil {
			tick()
		}
		if ct := time.Unix(st.Ctim.Unix()); ct.After(u.Changed) {
			u.Changed = ct
		}
		if st.Nlink > 1 && !d.IsDir() {
			key := inode{st.Dev, st.Ino}
			if seen[key] {
				return nil
			}
			seen[key] = true
		}
		u.Blocks += st.Blocks * 512
		u.Files++
		if copyName.MatchString(d.Name()) {
			u.Copies += st.Blocks * 512
		}
		n, err := space.add(p, d, st)
		u.Bytes += n
		return err
	})
	u.Bytes += spanBytes(space.shared)
	u.Fiemaps = space.fiemaps
	return u
}

// Free — свободное для пользователя место на файловой системе path.
func Free(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("свободное место %s: %w", path, err)
	}
	return int64(st.Bavail) * st.Bsize, nil
}

// ByDay — место записи по местным дням последнего изменения файлов (ctime; ключ — полночь дня): прикидка роста
// задним числом. Видит только то, что лежит сейчас, и относит файл ко дню его последнего изменения.
func ByDay(ctx context.Context, path string, exact bool) (map[time.Time]int64, error) {
	out := map[time.Time]int64{}
	days := map[time.Time]*fileSpace{}
	buf := new(fiemapBuf)
	_, err := walkEntry(ctx, path, func(p string, d fs.DirEntry, st *syscall.Stat_t) error {
		y, m, dd := time.Unix(st.Ctim.Unix()).Date()
		day := time.Date(y, m, dd, 0, 0, 0, 0, time.Local)
		f := days[day]
		if f == nil {
			f = &fileSpace{exact: exact, buf: buf}
			days[day] = f
		}
		n, err := f.add(p, d, st)
		out[day] += n
		return err
	})
	for day, f := range days {
		out[day] += spanBytes(f.shared)
	}
	return out, err
}

// validateTraces раскрывает папки классов следов и добавляет их в контейнеры: каждый их ребёнок — отдельная запись.
// Вызывается после раскрытия контейнеров.
func (c *Config) validateTraces(home string) error {
	for i := range c.Traces {
		t := &c.Traces[i]
		if t.Dir == "" || t.Kind == "" {
			return fmt.Errorf("disk.traces[%d]: нужны dir и kind", i)
		}
		t.Dir = filepath.Clean(policy.ExpandHome(t.Dir, home))
		if !slices.Contains(c.Containers, t.Dir) {
			c.Containers = append(c.Containers, t.Dir)
		}
	}
	return nil
}
