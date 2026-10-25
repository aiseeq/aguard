package memwatch

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
	"github.com/aiseeq/akit/proc"
	"github.com/aiseeq/akit/session"
)

// Config — раздел [memory] конфига aguard.
type Config struct {
	Interval policy.Duration `toml:"interval"`  // как часто брать пик; пик за интервал ядро считает само
	Slice    string          `toml:"slice"`     // папка cgroup слайса сессий; пусто — agents.slice своего user@
	TopProcs int             `toml:"top_procs"` // сколько крупных процессов писать
	Keep     policy.Duration `toml:"keep"`      // сколько хранить дневные файлы
}

const (
	defaultInterval = time.Minute
	defaultTop      = 3
	defaultKeep     = 60 * 24 * time.Hour
	maxArgs         = 120
)

// Default — значения по умолчанию.
func Default() Config {
	return Config{Interval: policy.Duration(defaultInterval), TopProcs: defaultTop, Keep: policy.Duration(defaultKeep)}
}

// Validate проверяет раздел и подставляет слайс по uid.
func (c *Config) Validate(uid int) error {
	if c.Interval.D() < 10*time.Second {
		return fmt.Errorf("memory.interval = %v: не чаще раза в 10 с", c.Interval.D())
	}
	if c.TopProcs < 0 || c.Keep.D() < 24*time.Hour {
		return errors.New("memory.top_procs >= 0 и memory.keep >= 24h")
	}
	if c.Slice == "" {
		c.Slice = fmt.Sprintf("/sys/fs/cgroup/user.slice/user-%d.slice/user@%d.service/agents.slice", uid, uid)
	}
	return nil
}

// Proc — процесс в записи.
type Proc struct {
	Pid  int    `json:"pid"`
	Comm string `json:"comm"`
	RSS  int64  `json:"rss"`
	Args string `json:"args,omitempty"`
}

// Sample — запись о сессии за интервал. Память — байты группы cgroup
// (с файловым кешем: его же считает и MemoryHigh).
type Sample struct {
	At       time.Time        `json:"at"`
	Scope    string           `json:"scope"`
	Name     string           `json:"name"`
	Pid      int              `json:"pid,omitempty"` // pid claude из реестра сессий
	Session  string           `json:"session,omitempty"`
	Cwd      string           `json:"cwd,omitempty"`
	Peak     int64            `json:"peak"`            // пик за интервал
	First    bool             `json:"first,omitempty"` // первая запись scope: Peak — за всю жизнь группы до неё
	Lifetime int64            `json:"lifetime"`        // пик за всю жизнь группы
	Current  int64            `json:"current"`
	Anon     int64            `json:"anon"`
	File     int64            `json:"file"`
	Swap     int64            `json:"swap"`
	High     int64            `json:"high"` // memory.high группы, -1 — нет
	Events   map[string]int64 `json:"events,omitempty"`
	Procs    int              `json:"procs"`
	Top      []Proc           `json:"top,omitempty"`
}

// Watcher держит открытые memory.peak между проходами.
type Watcher struct {
	ProcRoot string
	Reg      session.Registry
	peaks    map[string]*peakReader
	pageSize int64
}

// NewWatcher — наблюдатель над реестром reg.
func NewWatcher(reg session.Registry) *Watcher {
	return &Watcher{ProcRoot: "/proc", Reg: reg, peaks: map[string]*peakReader{}, pageSize: int64(os.Getpagesize())}
}

// Close закрывает открытые файлы.
func (w *Watcher) Close() error {
	var errs []error
	for _, k := range slices.Sorted(maps.Keys(w.peaks)) {
		errs = append(errs, w.peaks[k].close())
		delete(w.peaks, k)
	}
	return errors.Join(errs...)
}

// Pass — записи по всем живым scope сессий. Ошибка одного scope не мешает
// остальным: она возвращается вместе с прочитанным.
func (w *Watcher) Pass(cfg *Config, now time.Time) ([]Sample, error) {
	scopes, err := listScopes(cfg.Slice)
	if err != nil {
		return nil, err
	}
	alive := map[string]bool{}
	var out []Sample
	var errs []error
	for _, sc := range scopes {
		alive[sc] = true
		s, err := w.sample(cfg, sc, now)
		if errors.Is(err, fs.ErrNotExist) {
			continue // scope закрылся во время прохода
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", sc, err))
			continue
		}
		if err := w.details(cfg, &s); err != nil {
			errs = append(errs, fmt.Errorf("%s, процессы: %w", sc, err)) // запись неполная, но пик есть: пишем
		}
		out = append(out, s)
	}
	for _, sc := range slices.Sorted(maps.Keys(w.peaks)) {
		if !alive[sc] {
			errs = append(errs, w.peaks[sc].close())
			delete(w.peaks, sc)
		}
	}
	return out, errors.Join(errs...)
}

// sample — память группы. Ошибка — записи нет.
func (w *Watcher) sample(cfg *Config, scope string, now time.Time) (Sample, error) {
	dir := filepath.Join(cfg.Slice, scope)
	name, _, _ := proc.ScopeName(scope)
	s := Sample{At: now, Scope: scope, Name: name}
	pr, ok := w.peaks[scope]
	if !ok {
		var err error
		if pr, err = openPeak(dir); err != nil {
			return s, err
		}
		w.peaks[scope] = pr
		s.First = true
	}
	if err := w.groupMemory(dir, &s); err != nil {
		return s, err
	}
	var err error
	if s.Peak, err = pr.take(); err != nil {
		return s, err
	}
	return s, nil
}

// details — процессы и сессия группы. Ошибка — запись есть, но неполная.
func (w *Watcher) details(cfg *Config, s *Sample) error {
	pids, err := procsUnder(filepath.Join(cfg.Slice, s.Scope))
	if err != nil {
		return err
	}
	s.Procs = len(pids)
	var terr error
	s.Top, terr = w.top(pids, cfg.TopProcs)
	return errors.Join(terr, w.session(pids, s))
}

// groupMemory — текущая память, пик за жизнь, состав и события группы.
func (w *Watcher) groupMemory(dir string, s *Sample) error {
	var err error
	if s.Lifetime, err = readInt(filepath.Join(dir, "memory.peak")); err != nil {
		return err
	}
	if s.Current, err = readInt(filepath.Join(dir, "memory.current")); err != nil {
		return err
	}
	if s.Swap, err = readInt(filepath.Join(dir, "memory.swap.current")); err != nil {
		return err
	}
	if s.High, err = readInt(filepath.Join(dir, "memory.high")); err != nil {
		return err
	}
	st, err := readKeyed(filepath.Join(dir, "memory.stat"))
	if err != nil {
		return err
	}
	s.Anon, s.File = st["anon"], st["file"]
	ev, err := readKeyed(filepath.Join(dir, "memory.events"))
	if err != nil {
		return err
	}
	for k, v := range ev {
		if v != 0 && k != "sock_throttled" {
			if s.Events == nil {
				s.Events = map[string]int64{}
			}
			s.Events[k] = v
		}
	}
	return nil
}

// top — n процессов с наибольшим RSS. Процессы, ушедшие во время чтения,
// пропускаются; прочие ошибки чтения возвращаются вместе со списком.
func (w *Watcher) top(pids []int, n int) ([]Proc, error) {
	procs := make([]Proc, 0, len(pids))
	var errs []error
	for _, pid := range pids {
		p, err := w.readProc(pid)
		if proc.Gone(err) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		procs = append(procs, p)
	}
	slices.SortFunc(procs, func(a, b Proc) int { return cmp.Or(cmp.Compare(b.RSS, a.RSS), cmp.Compare(a.Pid, b.Pid)) })
	if len(procs) > n {
		procs = procs[:n]
	}
	for i := range procs {
		args, err := w.args(procs[i].Pid)
		if err != nil && !proc.Gone(err) {
			errs = append(errs, err)
		}
		procs[i].Args = args
	}
	return procs, errors.Join(errs...)
}

// gone — процесс завершился между списком группы и чтением.

func (w *Watcher) readProc(pid int) (Proc, error) {
	b, err := os.ReadFile(filepath.Join(w.ProcRoot, strconv.Itoa(pid), "statm"))
	if err != nil {
		return Proc{}, fmt.Errorf("statm %d: %w", pid, err)
	}
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return Proc{}, fmt.Errorf("statm %d: полей %d", pid, len(f))
	}
	pages, err := strconv.ParseInt(f[1], 10, 64)
	if err != nil {
		return Proc{}, fmt.Errorf("statm %d: %w", pid, err)
	}
	st, err := proc.ReadStat(w.ProcRoot, pid)
	if err != nil {
		return Proc{}, fmt.Errorf("процесс %d: %w", pid, err)
	}
	return Proc{Pid: pid, Comm: st.Comm, RSS: pages * w.pageSize}, nil
}

func (w *Watcher) args(pid int) (string, error) {
	b, err := os.ReadFile(filepath.Join(w.ProcRoot, strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return "", fmt.Errorf("cmdline %d: %w", pid, err)
	}
	s := strings.TrimSpace(strings.ReplaceAll(string(b), "\x00", " "))
	if r := []rune(s); len(r) > maxArgs {
		s = string(r[:maxArgs]) + "…"
	}
	return s, nil
}

// session — сессия Claude Code группы: первый процесс группы, у которого
// есть запись в реестре. Нечитаемая запись (пишется прямо сейчас) — ошибкой:
// сессию возьмём в следующий проход.
func (w *Watcher) session(pids []int, s *Sample) error {
	var errs []error
	for _, pid := range pids {
		in, err := w.Reg.Get(pid)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if in != nil {
			s.Pid, s.Session, s.Cwd = in.Pid, in.SessionID, in.Cwd
			return nil
		}
	}
	return errors.Join(errs...)
}
