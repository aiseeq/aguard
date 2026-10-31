package guiwatch

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
	"time"

	"github.com/aiseeq/akit/proc"
)

type Sample struct {
	At     time.Time `json:"-"`
	AtMS   int64     `json:"at_ms"`
	State  string    `json:"state"`
	RunMS  int64     `json:"run_ms"`
	WaitMS int64     `json:"wait_ms"`
	Majflt uint64    `json:"majflt"`
	Wchan  string    `json:"wchan,omitempty"`
}
type Thread struct {
	TID   int    `json:"tid"`
	State string `json:"state"`
	Wchan string `json:"wchan,omitempty"`
}
type Slice struct {
	ID         string            `json:"id,omitempty"`
	Starttime  uint64            `json:"starttime,omitempty"`
	Target     string            `json:"target"`
	PID        int               `json:"pid"`
	TID        int               `json:"tid"`
	Kind       string            `json:"kind"`
	Start      time.Time         `json:"start"`
	End        time.Time         `json:"end"`
	DurationMS int64             `json:"duration_ms"`
	Suspend    bool              `json:"suspend"`
	Merged     int               `json:"merged"`
	SelfWaitMS int64             `json:"self_wait_ms"`
	Samples    []Sample          `json:"samples"`
	Threads    []Thread          `json:"threads"`
	Journal    []string          `json:"journal"`
	PSI        map[string]string `json:"psi"`
	Top        []ProcessCPU      `json:"top"`
	Errors     []string          `json:"errors,omitempty"`
}
type ProcessCPU struct {
	PID   int    `json:"pid"`
	Name  string `json:"name"`
	CPUMS uint64 `json:"cpu_ms"`
}
type SleepStat struct {
	Date      string `json:"date"`
	Target    string `json:"target"`
	Wchan     string `json:"wchan"`
	LongestMS int64  `json:"longest_ms"`
	Over500MS int64  `json:"over_500ms"`
	Over1SMS  int64  `json:"over_1s"`
	Over2SMS  int64  `json:"over_2s"`
	Over5SMS  int64  `json:"over_5s"`
}
type sleepRun struct {
	since time.Time
	last  time.Time
	wchan string
	stat  *SleepStat
}

func dedupeSamples(in []Sample) []Sample {
	if len(in) < 2 {
		return in
	}
	out := in[:0]
	for _, x := range in {
		if len(out) == 0 || out[len(out)-1].AtMS != x.AtMS {
			out = append(out, x)
		}
	}
	return out
}

const (
	DirectoryMode   = 0o700
	privateFileMode = 0o600
)

type Document struct {
	Version    int         `json:"version"`
	Slices     []Slice     `json:"slices"`
	Suppressed int         `json:"suppressed"`
	Sleeps     []SleepStat `json:"sleeps"`
}
type Target struct {
	Name  string
	PID   int
	Start uint64
	Main  bool
}

type openFile struct {
	file *os.File
	fd   int
	buf  []byte
}

type watchTick struct {
	C    <-chan time.Time
	stop func()
}

func (t watchTick) Stop() {
	if t.stop != nil {
		t.stop()
	}
}

func (f *openFile) read() ([]byte, error) {
	for {
		n, err := unix.Pread(f.fd, f.buf, 0)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("pread /proc: %w", err)
		}
		return f.buf[:n], nil
	}
}
func open(path string) (*openFile, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	return &openFile{file: f, fd: int(f.Fd()), buf: make([]byte, 2048)}, nil
}
func (f *openFile) close() {
	if f != nil && f.file != nil {
		_ = f.file.Close()
	}
}

type targetState struct {
	Target
	files       map[string]*openFile
	run, wait   uint64
	sampled     time.Time
	ring        []Sample
	ringPruned  time.Time
	lastWchan   string
	readErrors  []string
	episode     *episode
	closing     []*episode
	lastWritten time.Time
	journal     []string
}
type episode struct {
	start, end time.Time
	kind       string
	quietSince time.Time
	detected   bool
	suspend    bool
	sliceID    string
	suppressed bool
	closing    bool
}
type Watcher struct {
	ProcRoot, Dir       string
	Config              Config
	PIDs                []Target
	newTicker           func(time.Duration) watchTick
	now                 func() time.Time
	clock               func() (time.Duration, time.Duration, error)
	boottime, monotonic time.Duration
	document            Document
	states              map[int]*targetState
	lastDiscover        time.Time
	lastAny             time.Time
	top                 map[int]uint64
	topStart            map[int]uint64
	topDelta            map[int]uint64
	topComm             map[int]string
	topFiles            map[int]*openFile
	lastTop             time.Time
	topTracking         bool
	selfWait            uint64
	lastSelfWait        int64
	selfSched           *openFile
	sleepStats          map[string]*SleepStat
	sleepCurrent        map[int]sleepRun
	baseSuppressed      int
	OnTargets           func(string, []string)
	targetSummary       string
	targetIdentity      string
	ReloadConfig        func() (Config, error)
	lastConfigCheck     time.Time
	journalMu           sync.Mutex
	journalPending      map[string]bool
	scannedTargets      map[int]Target
	targetStatBuf       []byte
}

var ErrConfigChanged = errors.New("gui config changed")

func New(root, dir string, c Config, targets []Target) *Watcher {
	return &Watcher{ProcRoot: root, Dir: dir, Config: c, PIDs: targets, now: time.Now, states: map[int]*targetState{}, document: Document{Version: 1, Slices: []Slice{}}, top: map[int]uint64{}, topStart: map[int]uint64{}, topDelta: map[int]uint64{}, topComm: map[int]string{}, topFiles: map[int]*openFile{}, sleepStats: map[string]*SleepStat{}, sleepCurrent: map[int]sleepRun{}, journalPending: map[string]bool{}}
}

func (w *Watcher) Watch(ctx context.Context) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := os.MkdirAll(w.Dir, DirectoryMode); err != nil {
		return fmt.Errorf("создать каталог срезов: %w", err)
	}
	defer w.closeAll()
	if err := w.load(); err != nil {
		return err
	}
	var tick watchTick
	if w.newTicker != nil {
		tick = w.newTicker(w.Config.Interval.D())
	} else {
		realTicker := time.NewTicker(w.Config.Interval.D())
		tick = watchTick{C: realTicker.C, stop: realTicker.Stop}
	}
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := w.finishAll(w.now(), false); err != nil {
				return err
			}
			for pid, r := range w.sleepCurrent {
				w.finishSleep(r, w.now())
				delete(w.sleepCurrent, pid)
			}
			if err := w.saveSleeps(); err != nil {
				return err
			}
			return w.save()
		case <-tick.C:
			if err := w.step(w.now()); err != nil {
				return err
			}
		}
	}
}

func (w *Watcher) step(at time.Time) error {
	if err := w.checkClock(at); err != nil {
		return err
	}
	discoverDue := w.lastDiscover.IsZero() || at.Sub(w.lastDiscover) >= w.Config.Rediscover.D()
	if w.topTracking || (discoverDue && len(w.PIDs) == 0) {
		if err := w.updateTop(); err != nil {
			return err
		}
	}
	if discoverDue {
		var err error
		if len(w.PIDs) == 0 && w.scannedTargets != nil {
			err = w.discoverTargets(w.scannedTargets)
		} else {
			err = w.discover()
		}
		if err != nil {
			return err
		}
		w.lastDiscover = at
	}
	for pid, s := range w.states {
		interval := w.Config.Interval.D()
		if !s.Main {
			interval = w.Config.RendererEvery.D()
		}
		if at.Sub(s.sampled) >= interval {
			if err := w.sample(s, at); err != nil {
				if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ESRCH) {
					return fmt.Errorf("опрос %s pid=%d: %w", s.Name, pid, err)
				}
				if forgetErr := w.forget(pid); forgetErr != nil {
					return forgetErr
				}
				if err := w.discover(); err != nil {
					return err
				}
				w.lastDiscover = at
			}
		}
	}
	w.topTracking = w.hasEpisode()
	return nil
}

func (w *Watcher) hasEpisode() bool {
	for _, s := range w.states {
		if s.episode != nil || len(s.closing) > 0 {
			return true
		}
	}
	return false
}

func (w *Watcher) checkClock(at time.Time) error {
	if w.ReloadConfig != nil && (w.lastConfigCheck.IsZero() || at.Sub(w.lastConfigCheck) >= time.Minute) {
		w.lastConfigCheck = at
		cfg, err := w.ReloadConfig()
		if err != nil {
			return fmt.Errorf("перечитать конфиг gui: %w", err)
		}
		if cfg != w.Config {
			return ErrConfigChanged
		}
	}
	clock := w.clock
	if clock == nil {
		clock = clockTimes
	}
	boot, mono, err := clock()
	if err != nil {
		return fmt.Errorf("часы ядра: %w", err)
	}
	selfWait, err := w.readSelfWait()
	if err != nil {
		return fmt.Errorf("schedstat потока сторожа: %w", err)
	}
	if w.selfWait != 0 && selfWait >= w.selfWait {
		w.lastSelfWait = int64(selfWait-w.selfWait) / 1e6
	}
	w.selfWait = selfWait
	if w.lastAny.IsZero() {
		w.lastAny = at
		w.boottime = boot
		w.monotonic = mono
	}
	gap := at.Sub(w.lastAny)
	sleepDelta := (boot - w.boottime) - (mono - w.monotonic)
	w.lastAny = at
	w.boottime = boot
	w.monotonic = mono
	if sleepDelta > time.Second {
		if err := w.finishAll(at.Add(-sleepDelta), true); err != nil {
			return err
		}
	}
	if gap >= 300*time.Millisecond && sleepDelta <= time.Second {
		if err := w.recordSamplerGap(at, gap); err != nil {
			return err
		}
	}
	return nil
}

func (w *Watcher) readSelfWait() (uint64, error) {
	if w.selfSched == nil {
		f, err := open(filepath.Join(w.ProcRoot, "thread-self", "schedstat"))
		if err != nil {
			return 0, fmt.Errorf("открыть schedstat потока сторожа: %w", err)
		}
		w.selfSched = f
	}
	b, err := w.selfSched.read()
	if err != nil {
		return 0, fmt.Errorf("прочитать schedstat потока сторожа: %w", err)
	}
	_, rest, ok := nextField(b)
	if !ok {
		return 0, fmt.Errorf("schedstat malformed")
	}
	field, _, ok := nextField(rest)
	if !ok {
		return 0, fmt.Errorf("schedstat malformed")
	}
	return parseDecimal(field)
}

func clockTimes() (time.Duration, time.Duration, error) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		return 0, 0, fmt.Errorf("CLOCK_BOOTTIME: %w", err)
	}
	b := time.Duration(ts.Sec)*time.Second + time.Duration(ts.Nsec)
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return 0, 0, fmt.Errorf("CLOCK_MONOTONIC: %w", err)
	}
	m := time.Duration(ts.Sec)*time.Second + time.Duration(ts.Nsec)
	return b, m, nil
}

func (w *Watcher) discover() error {
	wanted, err := w.findTargets()
	if err != nil {
		return err
	}
	return w.discoverTargets(wanted)
}

func (w *Watcher) discoverTargets(wanted map[int]Target) error {
	for pid, s := range w.states {
		t, ok := wanted[pid]
		if !ok || t.Start != s.Start {
			if err := w.forget(pid); err != nil {
				return err
			}
		}
	}
	for pid, t := range wanted {
		if w.states[pid] != nil {
			continue
		}
		s, err := w.newState(t)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("открыть цель %s pid=%d: %w", t.Name, pid, err)
		}
		w.states[pid] = s
	}
	w.reportTargets()
	return nil
}

func (w *Watcher) reportTargets() {
	if w.OnTargets == nil {
		return
	}
	main := map[string][]Target{}
	renderers := 0
	for _, s := range w.states {
		if s.Name == "chrome-renderer" {
			renderers++
		} else if s.Main {
			main[s.Name] = append(main[s.Name], s.Target)
		}
	}
	parts := []string{}
	missing := []string{}
	if len(w.PIDs) > 0 { // заданы -pid: строка о них, свои цели не ищутся и не «отсутствуют»
		alive := map[int]string{}
		for _, s := range w.states {
			alive[s.PID] = s.Name
		}
		for _, t := range w.PIDs {
			if alive[t.PID] == t.Name {
				parts = append(parts, fmt.Sprintf("%s pid %d", t.Name, t.PID))
			} else {
				parts = append(parts, t.Name+" отсутствует")
				missing = append(missing, t.Name)
			}
		}
		w.announceTargets("цели: "+strings.Join(parts, ", "), missing)
		return
	}
	for _, name := range []string{"kwin", "plasmashell", "chrome", "chrome-gpu"} {
		targets := main[name]
		sort.Slice(targets, func(i, j int) bool { return targets[i].PID < targets[j].PID })
		if len(targets) > 0 {
			for _, target := range targets {
				parts = append(parts, fmt.Sprintf("%s pid %d", name, target.PID))
			}
		} else {
			parts = append(parts, fmt.Sprintf("%s отсутствует", name))
			missing = append(missing, name)
		}
	}
	parts = append(parts, fmt.Sprintf("рендереров %d", renderers))
	w.announceTargets("цели: "+strings.Join(parts, ", "), missing)
}

// announceTargets — строка целей наружу, только когда набор сменился.
func (w *Watcher) announceTargets(summary string, missing []string) {
	identity := targetSetIdentity(w.states, missing)
	if identity != w.targetIdentity {
		w.targetIdentity = identity
		w.targetSummary = summary
		w.OnTargets(summary, missing)
	} else {
		w.targetSummary = summary
	}
}

func targetSetIdentity(states map[int]*targetState, missing []string) string {
	targets := make([]Target, 0, len(states))
	for _, state := range states {
		if state.Main && state.Name != "chrome-renderer" {
			targets = append(targets, state.Target)
		}
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].PID == targets[j].PID {
			return targets[i].Start < targets[j].Start
		}
		return targets[i].PID < targets[j].PID
	})
	var b strings.Builder
	for _, target := range targets {
		fmt.Fprintf(&b, "%d:%d;", target.PID, target.Start)
	}
	b.WriteByte('|')
	for _, name := range missing {
		b.WriteString(name)
		b.WriteByte(';')
	}
	return b.String()
}

func (w *Watcher) findTargets() (map[int]Target, error) {
	wanted := map[int]Target{}
	if len(w.PIDs) > 0 {
		for _, t := range w.PIDs {
			start, err := readStart(w.procPath(t.PID, "stat"))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			t.Start = start
			if t.Start != 0 {
				wanted[t.PID] = t
			}
		}
	} else {
		entries, err := os.ReadDir(w.ProcRoot)
		if err != nil {
			return nil, fmt.Errorf("прочитать proc: %w", err)
		}
		for _, ent := range entries {
			pid, err := strconv.Atoi(ent.Name())
			if err != nil || pid <= 0 {
				continue
			}
			target, err := w.procTarget(pid, ent.Name())
			if err != nil {
				return nil, err
			}
			if target.PID != 0 {
				wanted[pid] = target
			}
		}
	}
	return wanted, nil
}

func (w *Watcher) procTarget(pid int, dir string) (Target, error) {
	base := filepath.Join(w.ProcRoot, dir)
	stat, err := os.ReadFile(filepath.Join(base, "stat"))
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
		return Target{}, nil
	}
	if err != nil {
		return Target{}, fmt.Errorf("прочитать stat pid=%d: %w", pid, err)
	}
	return w.targetFromStat(pid, dir, stat)
}

func (w *Watcher) targetFromStat(pid int, dir string, stat []byte) (Target, error) {
	base := filepath.Join(w.ProcRoot, dir)
	commName := statComm(stat)
	if commName != "kwin_wayland" && commName != "plasmashell" && commName != "chrome" {
		return Target{}, nil
	}
	parsed, err := proc.ParseStat(string(stat))
	if err != nil {
		return Target{}, fmt.Errorf("разобрать starttime pid=%d: %w", pid, err)
	}
	name, main := "", true
	if commName == "kwin_wayland" {
		name = "kwin"
	} else if commName == "plasmashell" {
		name = "plasmashell"
	} else {
		data, err := os.ReadFile(filepath.Join(base, "cmdline"))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
				return Target{}, nil
			}
			return Target{}, fmt.Errorf("прочитать cmdline pid=%d: %w", pid, err)
		}
		cmd := cmdlineText(data)
		fields := strings.Fields(cmd)
		if len(fields) == 0 {
			return Target{}, nil
		}
		name, main = targetName(filepath.Base(fields[0]), cmd)
		if name == "" {
			return Target{}, nil
		}
		if name == "chrome-gpu" || name == "chrome-renderer" {
			ok, err := w.hasBrowserAncestor(parsed.Ppid)
			if err != nil {
				return Target{}, err
			}
			if !ok {
				return Target{}, nil
			}
		}
	}
	start, err := strconv.ParseUint(parsed.Start, 10, 64)
	if err != nil {
		return Target{}, fmt.Errorf("starttime pid=%d: %w", pid, err)
	}
	if start == 0 {
		return Target{}, nil
	}
	return Target{Name: name, PID: pid, Start: start, Main: main}, nil
}

func statComm(stat []byte) string {
	left := bytes.IndexByte(stat, '(')
	right := bytes.LastIndexByte(stat, ')')
	if left < 0 || right <= left {
		return ""
	}
	return string(stat[left+1 : right])
}

func targetName(comm, cmd string) (string, bool) {
	switch {
	case comm == "kwin_wayland":
		return "kwin", true
	case comm == "plasmashell":
		return "plasmashell", true
	case isChromeExecutable(cmd) && !strings.Contains(cmd, "--type=") && !hasHeadlessFlag(cmd):
		return "chrome", true
	case strings.Contains(cmd, "--type=gpu-process"):
		return "chrome-gpu", true
	case strings.Contains(cmd, "--type=renderer"):
		return "chrome-renderer", false
	default:
		return "", false
	}
}

// cmdlineText — /proc/<pid>/cmdline одной строкой через пробел: аргументы разделены NUL,
// а Chrome, переписавший заголовок процесса, кладёт всё в один аргумент через пробелы.
func cmdlineText(b []byte) string {
	return strings.ReplaceAll(string(bytes.TrimRight(b, "\x00")), "\x00", " ")
}

func isChromeExecutable(cmd string) bool {
	fields := strings.Fields(cmd)
	return len(fields) > 0 && fields[0] == "/opt/google/chrome/chrome"
}

func hasHeadlessFlag(cmd string) bool {
	for arg := range strings.FieldsSeq(cmd) {
		if strings.HasPrefix(arg, "--headless") {
			return true
		}
	}
	return false
}

func (w *Watcher) hasBrowserAncestor(pid int) (bool, error) {
	for depth := 0; pid > 0 && depth < 4; depth++ {
		stat, err := os.ReadFile(w.procPath(pid, "stat"))
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("прочитать stat предка pid=%d: %w", pid, err)
		}
		parsed, err := proc.ParseStat(string(stat))
		if err != nil {
			return false, fmt.Errorf("разобрать stat предка pid=%d: %w", pid, err)
		}
		if parsed.Comm == "chrome" {
			browser, err := w.isBrowserProcess(pid)
			if err != nil {
				return false, err
			}
			if browser {
				return true, nil
			}
		}
		pid = parsed.Ppid
	}
	return false, nil
}

func (w *Watcher) isBrowserProcess(pid int) (bool, error) {
	cmdline, err := os.ReadFile(w.procPath(pid, "cmdline"))
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("прочитать cmdline предка pid=%d: %w", pid, err)
	}
	cmd := cmdlineText(cmdline)
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return false, nil
	}
	name, _ := targetName(filepath.Base(fields[0]), cmd)
	return name == "chrome", nil
}
func (w *Watcher) procPath(pid int, leaf string) string {
	return filepath.Join(w.ProcRoot, strconv.Itoa(pid), leaf)
}
func readStart(path string) (uint64, error) {
	b, e := os.ReadFile(path)
	if e != nil {
		return 0, e
	}
	s, e := proc.ParseStat(string(b))
	if e != nil {
		return 0, fmt.Errorf("разобрать stat: %w", e)
	}
	n, err := strconv.ParseUint(s.Start, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("starttime: %w", err)
	}
	return n, nil
}
func (w *Watcher) newState(t Target) (*targetState, error) {
	base := filepath.Join(w.ProcRoot, strconv.Itoa(t.PID), "task", strconv.Itoa(t.PID))
	s := &targetState{Target: t, files: map[string]*openFile{}}
	for _, n := range []string{"stat", "schedstat", "wchan"} {
		f, e := open(filepath.Join(base, n))
		if e != nil {
			s.close()
			return nil, e
		}
		s.files[n] = f
	}
	return s, nil
}
func (s *targetState) close() {
	for _, f := range s.files {
		f.close()
	}
}
func (w *Watcher) sample(s *targetState, at time.Time) error {
	stat, e := s.files["stat"].read()
	if e != nil {
		if errors.Is(e, os.ErrNotExist) || errors.Is(e, syscall.ESRCH) {
			return e
		}
		s.readErrors = append(s.readErrors, "thread stat: "+e.Error())
		s.sampled = at
		return nil
	}
	state, maj, run, wait, err := readSampleCounters(s, stat)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			return err
		}
		s.readErrors = append(s.readErrors, err.Error())
		s.sampled = at
		return nil
	}
	cur := Sample{At: at, State: state, Majflt: maj}
	if s.sampled.IsZero() {
		s.run = run
		s.wait = wait
		s.sampled = at
		return nil
	}
	elapsed := at.Sub(s.sampled)
	if elapsed <= 0 {
		elapsed = w.Config.Interval.D()
	}
	runDelta, waitDelta := run-s.run, wait-s.wait
	cur.RunMS = int64(runDelta) / 1e6
	cur.WaitMS = int64(waitDelta) / 1e6
	s.run, s.wait, s.sampled = run, wait, at
	if state != "R" {
		b, err := s.files["wchan"].read()
		if err != nil {
			return err
		}
		b = bytes.TrimSpace(b)
		if equalStringBytes(s.lastWchan, b) {
			cur.Wchan = s.lastWchan
		} else {
			s.lastWchan = string(b)
			cur.Wchan = s.lastWchan
		}
	}
	w.trackSleep(s, cur, at)
	w.appendSample(s, cur, at)
	return w.judge(s, sampleKind(state, runDelta, waitDelta, elapsed), at)
}

func sleepKey(date, target, wchan string) string { return date + "\x00" + target + "\x00" + wchan }
func (w *Watcher) trackSleep(s *targetState, x Sample, at time.Time) {
	r, ok := w.sleepCurrent[s.PID]
	if x.State != "S" || (ok && (r.wchan != x.Wchan || at.Sub(r.last) > 300*time.Millisecond)) {
		if ok {
			w.finishSleep(r, at)
		}
		delete(w.sleepCurrent, s.PID)
		ok = false
	}
	if x.State == "S" {
		if !ok {
			date := at.Format("2006-01-02")
			key := sleepKey(date, s.Name, x.Wchan)
			stat := w.sleepStats[key]
			if stat == nil {
				stat = &SleepStat{Date: date, Target: s.Name, Wchan: x.Wchan}
				w.sleepStats[key] = stat
			}
			r = sleepRun{since: at, last: at, wchan: x.Wchan, stat: stat}
			w.sleepCurrent[s.PID] = r
		}
		w.recordSleep(r, at)
		r.last = at
		w.sleepCurrent[s.PID] = r
	}
}
func (w *Watcher) recordSleep(r sleepRun, at time.Time) {
	stat := r.stat
	if stat == nil {
		return
	}
	d := at.Sub(r.since)
	if d < 0 {
		return
	}
	if d.Milliseconds() > stat.LongestMS {
		stat.LongestMS = d.Milliseconds()
	}
}
func (w *Watcher) finishSleep(r sleepRun, at time.Time) {
	w.recordSleep(r, at)
	stat := r.stat
	if stat == nil {
		return
	}
	d := at.Sub(r.since)
	if d >= 500*time.Millisecond {
		stat.Over500MS++
	}
	if d >= time.Second {
		stat.Over1SMS++
	}
	if d >= 2*time.Second {
		stat.Over2SMS++
	}
	if d >= 5*time.Second {
		stat.Over5SMS++
	}
}

func equalStringBytes(value string, data []byte) bool {
	if len(value) != len(data) {
		return false
	}
	for i := range data {
		if value[i] != data[i] {
			return false
		}
	}
	return true
}

func readSampleCounters(s *targetState, stat []byte) (string, uint64, uint64, uint64, error) {
	right := bytes.LastIndexByte(stat, ')')
	if right < 0 || right+1 >= len(stat) {
		return "", 0, 0, 0, fmt.Errorf("stat: нет закрывающей скобки comm")
	}
	fields := stat[right+1:]
	var state string
	var maj uint64
	for i := 0; i <= 9; i++ {
		field, rest, ok := nextField(fields)
		if !ok {
			return "", 0, 0, 0, fmt.Errorf("stat: majflt отсутствует")
		}
		fields = rest
		if i == 0 {
			state = sampleState(field)
		}
		if i == 9 {
			var e error
			maj, e = parseDecimal(field)
			if e != nil {
				return "", 0, 0, 0, fmt.Errorf("stat majflt: %w", e)
			}
		}
	}
	sched, e := s.files["schedstat"].read()
	if e != nil {
		return "", 0, 0, 0, e
	}
	cpuField, rest, ok := nextField(sched)
	if !ok {
		return "", 0, 0, 0, fmt.Errorf("schedstat malformed")
	}
	waitField, _, ok := nextField(rest)
	if !ok {
		return "", 0, 0, 0, fmt.Errorf("schedstat malformed")
	}
	run, e := parseDecimal(cpuField)
	if e != nil {
		return "", 0, 0, 0, fmt.Errorf("schedstat cpu: %w", e)
	}
	wait, e := parseDecimal(waitField)
	if e != nil {
		return "", 0, 0, 0, fmt.Errorf("schedstat queue: %w", e)
	}
	return state, maj, run, wait, nil
}

func sampleState(field []byte) string {
	if len(field) == 1 {
		switch field[0] {
		case 'R':
			return "R"
		case 'S':
			return "S"
		case 'D':
			return "D"
		case 'T':
			return "T"
		case 't':
			return "t"
		case 'Z':
			return "Z"
		case 'X':
			return "X"
		}
	}
	return string(field)
}

func nextField(data []byte) ([]byte, []byte, bool) {
	i := 0
	for i < len(data) && (data[i] == ' ' || data[i] == '\t' || data[i] == '\n') {
		i++
	}
	start := i
	for i < len(data) && data[i] != ' ' && data[i] != '\t' && data[i] != '\n' {
		i++
	}
	if start == i {
		return nil, nil, false
	}
	return data[start:i], data[i:], true
}

func parseDecimal(data []byte) (uint64, error) {
	if len(data) == 0 {
		return 0, fmt.Errorf("пустое число")
	}
	var value uint64
	for _, digit := range data {
		if digit < '0' || digit > '9' {
			return 0, fmt.Errorf("неверное число %q", data)
		}
		n := uint64(digit - '0')
		if value > (^uint64(0)-n)/10 {
			return 0, fmt.Errorf("число переполнено")
		}
		value = value*10 + n
	}
	return value, nil
}

func (w *Watcher) appendSample(s *targetState, cur Sample, at time.Time) {
	s.ring = append(s.ring, cur)
	if !s.ringPruned.IsZero() && at.Sub(s.ringPruned) < time.Second {
		return
	}
	s.ringPruned = at
	cut := at.Add(-w.Config.Ring.D())
	i := 0
	for i < len(s.ring) && s.ring[i].At.Before(cut) {
		i++
	}
	if i > 0 {
		copy(s.ring, s.ring[i:])
		clear(s.ring[len(s.ring)-i:])
		s.ring = s.ring[:len(s.ring)-i]
	}
}

func sampleKind(state string, runDelta, waitDelta uint64, elapsed time.Duration) string {
	kind := ""
	switch state {
	case "T", "t":
		kind = "stopped"
	case "D":
		kind = "D"
	}
	if kind == "" && state != "S" && float64(runDelta+waitDelta) >= .8*float64(elapsed) {
		if runDelta > waitDelta {
			kind = "busy"
		} else {
			kind = "queue"
		}
	}
	return kind
}

func (w *Watcher) judge(s *targetState, kind string, at time.Time) error {
	e := s.episode
	for i := 0; i < len(s.closing); {
		pending := s.closing[i]
		if err := w.closeEpisode(s, pending, at); err != nil {
			return err
		}
		if at.Sub(pending.end) >= w.Config.Context.D() {
			s.closing = append(s.closing[:i], s.closing[i+1:]...)
		} else {
			i++
		}
	}
	if kind != "" {
		if e == nil {
			e = &episode{start: at, end: at, kind: kind}
			s.episode = e
			if !w.topTracking {
				w.top = map[int]uint64{}
				w.topStart = map[int]uint64{}
				w.topComm = map[int]string{}
				w.topDelta = map[int]uint64{}
				w.lastTop = time.Time{}
				if err := w.updateTop(); err != nil {
					return err
				}
				w.topTracking = true
			}
		} else {
			e.end = at
			if kind == "stopped" || kind == "D" {
				e.kind = kind
			}
		}
		e.quietSince = time.Time{}
		if !e.detected && e.end.Sub(e.start) >= w.Config.MinStall.D() {
			e.detected = true
			if err := w.writeSlice(s, e); err != nil {
				return err
			}
		} else if e.detected && at.Sub(s.lastWritten) >= time.Second {
			if err := w.writeSlice(s, e); err != nil {
				return err
			}
		}
		return nil
	}
	if e != nil {
		if e.quietSince.IsZero() {
			e.quietSince = at
		}
		if at.Sub(e.quietSince) > w.Config.Gap.D() {
			e.closing = true
			s.episode = nil
			s.closing = append(s.closing, e)
			if err := w.closeEpisode(s, e, at); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *Watcher) closeEpisode(s *targetState, e *episode, at time.Time) error {
	if at.Before(e.end.Add(w.Config.Context.D())) {
		return nil
	}
	if e.detected {
		if err := w.writeSlice(s, e); err != nil {
			return err
		}
	}
	return nil
}

func (w *Watcher) forget(pid int) error {
	s := w.states[pid]
	if s == nil {
		return nil
	}
	if s.episode != nil && s.episode.detected {
		if err := w.writeSlice(s, s.episode); err != nil {
			return err
		}
	}
	for _, e := range s.closing {
		if e.detected {
			if err := w.writeSlice(s, e); err != nil {
				return err
			}
		}
	}
	s.close()
	delete(w.states, pid)
	return nil
}
func (w *Watcher) finishAll(at time.Time, suspend bool) error {
	for _, s := range w.states {
		if suspend {
			if r, ok := w.sleepCurrent[s.PID]; ok {
				w.finishSleep(r, at)
				delete(w.sleepCurrent, s.PID)
			}
		}
		if s.episode != nil {
			if !s.episode.detected {
				s.episode = nil
				continue
			}
			if !s.episode.closing {
				s.episode.end = at
			}
			if suspend && !s.episode.closing {
				s.episode.suspend = true
			}
			if err := w.writeSlice(s, s.episode); err != nil {
				return err
			}
			s.episode = nil
		}
		for _, e := range s.closing {
			if e.detected {
				if err := w.writeSlice(s, e); err != nil {
					return err
				}
			}
		}
		s.closing = nil
		if suspend {
			s.sampled = time.Time{}
			s.run = 0
			s.wait = 0
		}
	}
	return nil
}
func (w *Watcher) closeAll() {
	if w.selfSched != nil {
		w.selfSched.close()
		w.selfSched = nil
	}
	for _, s := range w.states {
		s.close()
	}
	for pid, f := range w.topFiles {
		f.close()
		delete(w.topFiles, pid)
	}
}

func (w *Watcher) writeSlice(s *targetState, e *episode) error {
	if e == nil {
		return nil
	}
	if w.topTracking {
		if err := w.updateTop(); err != nil {
			return err
		}
	}
	if e.detected && e.sliceID != "" {
		item := w.makeSlice(s, e, false)
		for i := range w.document.Slices {
			old := &w.document.Slices[i]
			if old.ID == e.sliceID {
				item.ID, item.Starttime, item.Merged = old.ID, old.Starttime, old.Merged
				item.Samples = dedupeSamples(append(old.Samples, item.Samples...))
				item.Threads, item.Top = old.Threads, old.Top
				w.document.Slices[i] = item
				break
			}
		}
		s.lastWritten = w.now()
		if err := w.save(); err != nil {
			return err
		}
		w.queueJournal(e.sliceID, e.start, e.end)
		return nil
	}
	return w.createSlice(s, e)
}

func (w *Watcher) createSlice(s *targetState, e *episode) error {
	recent := w.document.Slices
	now := w.now()
	merged := -1
	for i := range recent {
		if recent[i].Target == s.Name && recent[i].PID == s.PID && recent[i].Starttime == s.Start && now.Sub(recent[i].End) < 30*time.Second {
			merged = i
			break
		}
	}
	if merged >= 0 {
		old := &w.document.Slices[merged]
		item := w.makeSlice(s, e, true)
		old.Merged++
		old.End = item.End
		old.DurationMS = old.End.Sub(old.Start).Milliseconds()
		old.Samples = dedupeSamples(append(old.Samples, item.Samples...))
		e.start = old.Start
		e.detected = true
		e.sliceID = old.ID
		s.lastWritten = now
		if err := w.save(); err != nil {
			return err
		}
		w.queueJournal(e.sliceID, e.start, e.end)
		return nil
	}
	count := 0
	cut := now.Add(-time.Hour)
	for i := range w.document.Slices {
		item := &w.document.Slices[i]
		if item.End.After(cut) {
			count++
		}
	}
	if count >= w.Config.MaxPerHour {
		if !e.suppressed {
			w.document.Suppressed++
			e.suppressed = true
		}
		return w.save()
	}
	item := w.makeSlice(s, e, true)
	var idBytes [16]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return fmt.Errorf("создать id среза: %w", err)
	}
	item.ID = hex.EncodeToString(idBytes[:])
	item.Starttime = s.Start
	w.document.Slices = append(w.document.Slices, item)
	e.detected = true
	e.sliceID = item.ID
	s.lastWritten = now
	if err := w.save(); err != nil {
		return err
	}
	w.queueJournal(item.ID, e.start, e.end)
	return nil
}

func (w *Watcher) queueJournal(id string, start, end time.Time) {
	if id == "" {
		return
	}
	w.journalMu.Lock()
	if w.journalPending[id] {
		w.journalMu.Unlock()
		return
	}
	w.journalPending[id] = true
	w.journalMu.Unlock()
	go func() {
		defer func() { w.journalMu.Lock(); delete(w.journalPending, id); w.journalMu.Unlock() }()
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		lines := journalWindow(ctx, start, end)
		b, err := json.Marshal(lines)
		if err != nil {
			lines = []string{"journal encode error: " + err.Error()}
			b = []byte("[\"journal encode error\"]")
		}
		tmp := filepath.Join(w.Dir, "journal-"+id+"-tmp.json")
		path := filepath.Join(w.Dir, "journal-"+id+".json")
		if err := os.WriteFile(tmp, b, privateFileMode); err != nil {
			lines = append(lines, "journal write error: "+err.Error())
			return
		}
		if err := os.Rename(tmp, path); err != nil {
			lines = append(lines, "journal rename error: "+err.Error())
			if removeErr := os.Remove(tmp); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				lines = append(lines, "journal temp cleanup error: "+removeErr.Error())
			}
		}
	}()
}

func (w *Watcher) makeSlice(s *targetState, e *episode, includeThreads bool) Slice {
	start := e.start.Add(-w.Config.Context.D())
	samples := make([]Sample, 0, len(s.ring))
	for _, x := range s.ring {
		if !x.At.Before(start) && !x.At.After(e.end.Add(w.Config.Context.D())) {
			x.AtMS = x.At.Sub(e.start).Milliseconds()
			samples = append(samples, x)
		}
	}
	var threads []Thread
	var snapshotErrors []string
	if includeThreads {
		threads, snapshotErrors = w.threadSnapshot(s.PID)
	}
	psi, psiErrors := readPSI(w.ProcRoot)
	snapshotErrors = append(snapshotErrors, psiErrors...)
	snapshotErrors = append(snapshotErrors, s.readErrors...)
	slice := Slice{Target: s.Name, PID: s.PID, TID: s.PID, Kind: e.kind, Start: e.start, End: e.end, DurationMS: e.end.Sub(e.start).Milliseconds(), Suspend: e.suspend, Samples: samples, Threads: threads, PSI: psi, Top: w.topSnapshot(), Errors: snapshotErrors}
	slice.Journal = s.journal
	return slice
}
func (w *Watcher) recordSamplerGap(at time.Time, d time.Duration) error {
	for _, s := range w.states {
		boundary := s.sampled
		for _, e := range append(append([]*episode(nil), s.closing...), s.episode) {
			if e != nil && e.detected {
				if !boundary.IsZero() && boundary.Before(e.end) {
					e.end = boundary
				}
				if err := w.writeSlice(s, e); err != nil {
					return err
				}
			}
		}
		s.episode = nil
		s.closing = nil
		s.sampled = time.Time{}
		s.run = 0
		s.wait = 0
	}
	if err := w.updateTop(); err != nil {
		return err
	}
	cut := at.Add(-time.Hour)
	count := 0
	for i := range w.document.Slices {
		s := &w.document.Slices[i]
		if s.End.After(cut) {
			count++
		}
	}
	if count >= w.Config.MaxPerHour {
		w.document.Suppressed++
	} else {
		w.document.Slices = append(w.document.Slices, Slice{Target: "aguard", Kind: "sampler_gap", Start: at.Add(-d), End: at, DurationMS: d.Milliseconds(), SelfWaitMS: w.lastSelfWait})
	}
	return w.save()
}

func (w *Watcher) threadSnapshot(pid int) ([]Thread, []string) {
	dir := filepath.Join(w.ProcRoot, strconv.Itoa(pid), "task")
	ents, e := os.ReadDir(dir)
	if e != nil {
		if errors.Is(e, os.ErrNotExist) || errors.Is(e, syscall.ESRCH) {
			return nil, nil
		}
		return nil, []string{"threads: " + e.Error()}
	}
	out := make([]Thread, 0, len(ents))
	var errs []string
	for _, ent := range ents {
		tid, e := strconv.Atoi(ent.Name())
		if e != nil {
			continue
		}
		p := filepath.Join(dir, ent.Name())
		b, e := os.ReadFile(filepath.Join(p, "stat"))
		if e != nil {
			if !errors.Is(e, os.ErrNotExist) && !errors.Is(e, syscall.ESRCH) {
				errs = append(errs, fmt.Sprintf("thread %d stat: %v", tid, e))
			}
			continue
		}
		st, e := proc.ParseStat(string(b))
		if e != nil {
			errs = append(errs, fmt.Sprintf("thread %d stat: %v", tid, e))
			continue
		}
		th := Thread{TID: tid, State: st.State}
		if st.State != "R" {
			if b, e := os.ReadFile(filepath.Join(p, "wchan")); e == nil {
				th.Wchan = strings.TrimSpace(string(b))
			} else if !errors.Is(e, os.ErrNotExist) && !errors.Is(e, syscall.ESRCH) {
				errs = append(errs, fmt.Sprintf("thread %d wchan: %v", tid, e))
			}
		}
		out = append(out, th)
	}
	return out, errs
}

func readPSI(root string) (map[string]string, []string) {
	out := map[string]string{}
	var errs []string
	for _, n := range []string{"cpu", "io", "memory"} {
		b, e := os.ReadFile(filepath.Join(root, "pressure", n))
		if e == nil {
			out[n] = string(b)
		} else {
			errs = append(errs, fmt.Sprintf("pressure/%s: %v", n, e))
		}
	}
	return out, errs
}
func journalWindow(ctx context.Context, start, end time.Time) []string {
	from := start.Add(-5 * time.Second).Format("2006-01-02 15:04:05")
	to := end.Add(5 * time.Second).Format("2006-01-02 15:04:05")
	cmd := exec.CommandContext(ctx, "journalctl", "--user", "-o", "short-iso-precise", "--since", from, "--until", to, "-u", "plasma-kwin_wayland.service", "-u", "plasma-plasmashell.service", "--no-pager")
	a, err := cmd.Output()
	if err != nil {
		a = []byte("journalctl user error: " + err.Error())
	}
	cmd = exec.CommandContext(ctx, "journalctl", "-k", "-o", "short-iso-precise", "--since", from, "--until", to, "--grep=i915|drm", "--no-pager")
	b, err := cmd.Output()
	if err != nil {
		b = []byte("journalctl kernel error: " + err.Error())
	}
	return append(nonemptyLines(a), nonemptyLines(b)...)
}

func nonemptyLines(data []byte) []string {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

func (w *Watcher) updateTop() error {
	now := w.now()
	collectTargets := len(w.PIDs) == 0 && (w.lastDiscover.IsZero() || now.Sub(w.lastDiscover) >= w.Config.Rediscover.D())
	if !collectTargets && !w.lastTop.IsZero() && now.Sub(w.lastTop) < time.Second {
		return nil
	}
	w.lastTop = now
	w.topDelta = map[int]uint64{}
	next, nextStart, nextComm, targets, err := w.readTopProcesses(collectTargets)
	if err != nil {
		return err
	}
	if collectTargets {
		w.scannedTargets = targets
	}
	for pid, f := range w.topFiles {
		if _, ok := next[pid]; !ok {
			f.close()
			delete(w.topFiles, pid)
		}
	}
	w.top = next
	w.topStart = nextStart
	w.topComm = nextComm
	return nil
}

func (w *Watcher) readTopProcesses(collectTargets bool) (map[int]uint64, map[int]uint64, map[int]string, map[int]Target, error) {
	ents, e := os.ReadDir(w.ProcRoot)
	if e != nil {
		return nil, nil, nil, nil, fmt.Errorf("перечитать proc для top: %w", e)
	}
	next := map[int]uint64{}
	nextStart := map[int]uint64{}
	nextComm := map[int]string{}
	var targets map[int]Target
	if collectTargets {
		targets = map[int]Target{}
	}
	for _, ent := range ents {
		pid, e := strconv.Atoi(ent.Name())
		if e != nil {
			continue
		}
		var b []byte
		var ok bool
		var err error
		if collectTargets && !w.topTracking {
			b, ok, err = w.readTargetStat(pid, ent.Name())
		} else {
			b, ok, err = w.readTopStat(pid, ent.Name())
		}
		if err != nil {
			return nil, nil, nil, nil, err
		}
		if !ok {
			continue
		}
		if collectTargets {
			target, err := w.targetFromStat(pid, ent.Name(), b)
			if err != nil {
				return nil, nil, nil, nil, err
			}
			if target.PID != 0 {
				targets[pid] = target
			}
		}
		// Outside an episode, the scan exists only to rediscover targets. CPU
		// baselines are reset when an episode starts, so parsing and retaining
		// counters for every process here only adds work to the steady-state poll.
		if collectTargets && !w.topTracking {
			continue
		}
		total, start, err := parseTopStat(pid, b)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		if old, ok := w.top[pid]; ok && w.topStart[pid] == start && total >= old {
			w.topDelta[pid] = total - old
			nextComm[pid] = w.topComm[pid]
		} else {
			nextComm[pid] = statComm(b)
		}
		next[pid] = total
		nextStart[pid] = start
	}
	return next, nextStart, nextComm, targets, nil
}

func (w *Watcher) readTargetStat(pid int, dir string) ([]byte, bool, error) {
	if w.targetStatBuf == nil {
		w.targetStatBuf = make([]byte, 2048)
	}
	f, err := os.Open(filepath.Join(w.ProcRoot, dir, "stat"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("открыть stat pid=%d для поиска целей: %w", pid, err)
	}
	n, err := unix.Pread(int(f.Fd()), w.targetStatBuf, 0)
	_ = f.Close()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.ESRCH) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("прочитать stat pid=%d для поиска целей: %w", pid, err)
	}
	if n == 0 {
		return nil, false, nil
	}
	return w.targetStatBuf[:n], true, nil
}

func (w *Watcher) readTopStat(pid int, dir string) ([]byte, bool, error) {
	f := w.topFiles[pid]
	if f == nil {
		var err error
		f, err = open(filepath.Join(w.ProcRoot, dir, "stat"))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
				return nil, false, nil
			}
			return nil, false, fmt.Errorf("открыть stat pid=%d для top: %w", pid, err)
		}
		w.topFiles[pid] = f
	}
	b, err := f.read()
	if err == nil && len(b) > 0 {
		return b, true, nil
	}
	f.close()
	delete(w.topFiles, pid)
	if err == nil || errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.ESRCH) {
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("прочитать stat pid=%d для top: %w", pid, err)
}

func parseTopStat(pid int, data []byte) (uint64, uint64, error) {
	right := bytes.LastIndexByte(data, ')')
	if right < 0 || right+1 >= len(data) {
		return 0, 0, fmt.Errorf("разобрать stat pid=%d для top: нет закрывающей скобки comm", pid)
	}
	fields := data[right+1:]
	var cpu uint64
	var start uint64
	for index := 0; index <= 19; index++ {
		field, rest, ok := nextField(fields)
		if !ok {
			return 0, 0, fmt.Errorf("stat pid=%d для top: не хватает полей", pid)
		}
		fields = rest
		if index == 11 || index == 12 || index == 19 {
			value, err := parseDecimal(field)
			if err != nil {
				return 0, 0, fmt.Errorf("stat pid=%d для top, поле %d: %w", pid, index, err)
			}
			if index == 19 {
				start = value
			} else {
				cpu += value
			}
		}
	}
	return cpu, start, nil
}
func (w *Watcher) topSnapshot() []ProcessCPU {
	out := []ProcessCPU{}
	for pid, delta := range w.topDelta {
		out = append(out, ProcessCPU{pid, w.topComm[pid], delta * 10})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CPUMS != out[j].CPUMS {
			return out[i].CPUMS > out[j].CPUMS
		}
		return out[i].PID < out[j].PID
	})
	if len(out) > 5 {
		out = out[:5]
	}
	return out
}

func (w *Watcher) load() error {
	p := filepath.Join(w.Dir, "slices.json")
	b, e := os.ReadFile(p)
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	if e = json.Unmarshal(b, &w.document); e != nil {
		return e
	}
	if w.document.Version != 1 {
		return fmt.Errorf("неподдерживаемая версия gui slices: %d", w.document.Version)
	}
	w.baseSuppressed = w.document.Suppressed
	return nil
}
func (w *Watcher) save() (retErr error) {
	if err := os.MkdirAll(w.Dir, DirectoryMode); err != nil {
		return fmt.Errorf("создать каталог gui: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(w.Dir, "slices.lock"), os.O_CREATE|os.O_RDWR, privateFileMode)
	if err != nil {
		return fmt.Errorf("открыть блокировку gui: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, lock.Close()) }()
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("заблокировать срезы gui: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, unix.Flock(int(lock.Fd()), unix.LOCK_UN)) }()
	path := filepath.Join(w.Dir, "slices.json")
	if b, e := os.ReadFile(path); e == nil {
		var disk Document
		if json.Unmarshal(b, &disk) == nil {
			byID := map[string]int{}
			for i := range w.document.Slices {
				s := &w.document.Slices[i]
				if s.ID != "" {
					byID[s.ID] = i
				}
			}
			for j := range disk.Slices {
				s := &disk.Slices[j]
				if i, ok := byID[s.ID]; s.ID != "" && !ok {
					w.document.Slices = append(w.document.Slices, *s)
				} else if ok && s.End.After(w.document.Slices[i].End) {
					w.document.Slices[i] = *s
				}
			}
			delta := max(w.document.Suppressed-w.baseSuppressed, 0)
			w.document.Suppressed = disk.Suppressed + delta
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return fmt.Errorf("прочитать сохранённые срезы: %w", e)
	}
	if err := w.prune(); err != nil {
		return err
	}
	b, e := json.MarshalIndent(w.document, "", "  ")
	if e != nil {
		return fmt.Errorf("сериализовать срезы: %w", e)
	}
	tmp := filepath.Join(w.Dir, fmt.Sprintf("slices-%d.json.tmp", os.Getpid()))
	if e = os.WriteFile(tmp, b, privateFileMode); e != nil {
		return fmt.Errorf("записать временный файл срезов: %w", e)
	}
	if err = os.Rename(tmp, path); err != nil {
		return fmt.Errorf("прочитать каталог gui: %w", err)
	}
	w.baseSuppressed = w.document.Suppressed
	return nil
}
func (w *Watcher) saveSleeps() error {
	byDate := map[string][]SleepStat{}
	for _, s := range w.sleepStats {
		byDate[s.Date] = append(byDate[s.Date], *s)
	}
	for date, stats := range byDate {
		b, err := json.MarshalIndent(stats, "", "  ")
		if err != nil {
			return fmt.Errorf("сериализовать статистику сна: %w", err)
		}
		p := filepath.Join(w.Dir, "sleeps-"+date+".json")
		if err = os.WriteFile(p, b, privateFileMode); err != nil {
			return fmt.Errorf("сохранить статистику сна: %w", err)
		}
	}
	entries, err := os.ReadDir(w.Dir)
	if err != nil {
		return fmt.Errorf("прочитать каталог статистики сна: %w", err)
	}
	cut := w.now().Add(-w.Config.Keep.D()).Format("2006-01-02")
	for _, ent := range entries {
		n := ent.Name()
		if strings.HasPrefix(n, "sleeps-") && strings.HasSuffix(n, ".json") {
			d := strings.TrimSuffix(strings.TrimPrefix(n, "sleeps-"), ".json")
			if d < cut {
				if err := os.Remove(filepath.Join(w.Dir, n)); err != nil && !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("удалить старую статистику сна: %w", err)
				}
			}
		}
	}
	return nil
}
func (w *Watcher) prune() error {
	now := w.now()
	removed := []string{}
	keep := w.document.Slices[:0]
	for i := range w.document.Slices {
		s := &w.document.Slices[i]
		if s.End.After(now.Add(-w.Config.Keep.D())) {
			keep = append(keep, *s)
		} else if s.ID != "" {
			removed = append(removed, s.ID)
		}
	}
	w.document.Slices = keep
	for {
		b, err := json.MarshalIndent(w.document, "", "  ")
		if err != nil {
			return fmt.Errorf("сериализовать срезы для очистки: %w", err)
		}
		entries, err := os.ReadDir(w.Dir)
		if err != nil {
			return fmt.Errorf("прочитать каталог gui для очистки: %w", err)
		}
		size := int64(len(b))
		for _, ent := range entries {
			if ent.Name() == "slices.json" {
				continue
			}
			info, e := ent.Info()
			if errors.Is(e, os.ErrNotExist) {
				continue
			}
			if e != nil {
				return fmt.Errorf("получить размер %s: %w", ent.Name(), e)
			}
			size += info.Size()
		}
		if size <= w.Config.MaxBytes {
			break
		}
		if len(w.document.Slices) == 0 {
			return fmt.Errorf("gui max_bytes=%d меньше размера файлов %d", w.Config.MaxBytes, size)
		}
		removed = append(removed, w.document.Slices[0].ID)
		if id := w.document.Slices[0].ID; id != "" {
			if err := os.Remove(filepath.Join(w.Dir, "journal-"+id+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("удалить старый journal: %w", err)
			}
		}
		w.document.Slices = w.document.Slices[1:]
	}
	for _, id := range removed {
		if id != "" {
			if err := os.Remove(filepath.Join(w.Dir, "journal-"+id+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("удалить journal: %w", err)
			}
		}
	}
	return nil
}

func ReadReport(dir string, since time.Time) (Document, error) {
	b, e := os.ReadFile(filepath.Join(dir, "slices.json"))
	if e != nil {
		if errors.Is(e, os.ErrNotExist) {
			b = []byte(`{"version":1,"slices":[]}`)
		} else {
			return Document{}, e
		}
	}
	var d Document
	if e = json.Unmarshal(b, &d); e != nil {
		return d, e
	}
	if d.Slices == nil {
		d.Slices = []Slice{}
	}
	if d.Sleeps == nil {
		d.Sleeps = []SleepStat{}
	}
	out := d.Slices[:0]
	for i := range d.Slices {
		s := &d.Slices[i]
		if s.End.After(since) {
			for j := range s.Samples {
				s.Samples[j].At = s.Start.Add(time.Duration(s.Samples[j].AtMS) * time.Millisecond)
			}
			out = append(out, *s)
		}
	}
	d.Slices = out
	for i := range d.Slices {
		if d.Slices[i].ID == "" {
			continue
		}
		jb, e := os.ReadFile(filepath.Join(dir, "journal-"+d.Slices[i].ID+".json"))
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return Document{}, e
		}
		if e = json.Unmarshal(jb, &d.Slices[i].Journal); e != nil {
			return Document{}, e
		}
	}
	var err error
	d.Sleeps, err = readSleepReport(dir, since)
	if err != nil {
		return Document{}, err
	}
	return d, nil
}

func readSleepReport(dir string, since time.Time) ([]SleepStat, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []SleepStat{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("прочитать каталог статистики сна: %w", err)
	}
	stats := map[string]SleepStat{}
	for _, ent := range entries {
		n := ent.Name()
		if !strings.HasPrefix(n, "sleeps-") || !strings.HasSuffix(n, ".json") {
			continue
		}
		b, e := os.ReadFile(filepath.Join(dir, n))
		if e != nil {
			return nil, fmt.Errorf("прочитать %s: %w", n, e)
		}
		var day []SleepStat
		if e = json.Unmarshal(b, &day); e != nil {
			return nil, fmt.Errorf("разобрать %s: %w", n, e)
		}
		for _, st := range day {
			if st.Date >= since.Format("2006-01-02") {
				key := st.Date + "\x00" + st.Target + "\x00" + st.Wchan
				prev := stats[key]
				if st.LongestMS > prev.LongestMS {
					prev.LongestMS = st.LongestMS
				}
				prev.Date, prev.Target, prev.Wchan = st.Date, st.Target, st.Wchan
				prev.Over500MS += st.Over500MS
				prev.Over1SMS += st.Over1SMS
				prev.Over2SMS += st.Over2SMS
				prev.Over5SMS += st.Over5SMS
				stats[key] = prev
			}
		}
	}
	sleeps := make([]SleepStat, 0, len(stats))
	for _, st := range stats {
		sleeps = append(sleeps, st)
	}
	sort.Slice(sleeps, func(i, j int) bool {
		if sleeps[i].Target != sleeps[j].Target {
			return sleeps[i].Target < sleeps[j].Target
		}
		return sleeps[i].Wchan < sleeps[j].Wchan
	})
	return sleeps, nil
}
func LongestSleep(d Document) time.Duration {
	maxSleep := time.Duration(0)
	for _, s := range d.Sleeps {
		if d := time.Duration(s.LongestMS) * time.Millisecond; d > maxSleep {
			maxSleep = d
		}
	}
	for i := range d.Slices {
		s := &d.Slices[i]
		var start, last time.Time
		var chanName string
		for j := range s.Samples {
			x := &s.Samples[j]
			at := s.Start.Add(time.Duration(x.AtMS) * time.Millisecond)
			if x.State == "S" {
				if start.IsZero() || x.Wchan != chanName || at.Sub(last) > 250*time.Millisecond {
					start = at
					chanName = x.Wchan
				}
				last = at
				if dur := at.Sub(start); dur > maxSleep {
					maxSleep = dur
				}
			} else {
				start = time.Time{}
			}
		}
	}
	return maxSleep
}
