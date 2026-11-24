package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aiseeq/aguard/internal/config"
	"github.com/aiseeq/aguard/internal/forkguard"
)

func (a *app) forkguardCmd(ctx context.Context, args []string) error {
	cmd, rest := sub(args)
	var runFor time.Duration
	if cmd == "run" {
		fl := flag.NewFlagSet("forkguard run", flag.ContinueOnError)
		fl.SetOutput(a.out)
		fl.DurationVar(&runFor, "for", 0, "завершиться через заданное время; 0 — без срока")
		if err := fl.Parse(rest); err != nil {
			return fmt.Errorf("aguard forkguard run: %w", err)
		}
		if len(fl.Args()) != 0 || runFor < 0 {
			return fmt.Errorf("aguard forkguard run [-for duration]")
		}
	}
	if cmd != "run" && len(rest) != 0 {
		return fmt.Errorf("aguard forkguard %s не принимает аргументы", cmd)
	}
	forkCfg, err := config.LoadForkguard(a.cfgPath)
	if err != nil {
		return err
	}
	switch cmd {
	case "tick":
		s, err := a.forkguardSample(forkCfg)
		if err != nil {
			return err
		}
		engine := forkguard.New(forkCfg.EngineConfig())
		result := engine.Observe(s)
		fmt.Fprintf(a.out, "forkguard tick: at=%s slice_current=%d slice_max=%d units=%d\n", s.At.Format(time.RFC3339), s.SliceCurrent, s.SliceMax, len(s.Units))
		for _, line := range result.Lines {
			fmt.Fprintln(a.out, line)
		}
		for i := range result.Decisions {
			fmt.Fprintln(a.out, decisionLineWithTree(result.Decisions[i]))
		}
		return nil
	case "run":
		if runFor > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, runFor)
			defer cancel()
		}
		return a.forkguardRun(ctx, &forkCfg)
	default:
		return fmt.Errorf("aguard forkguard: ожидалось run или tick")
	}
}

func (a *app) forkguardRun(ctx context.Context, c *config.ForkguardConfig) error {
	engine := forkguard.New(c.EngineConfig())
	for {
		s, err := a.forkguardSample(*c)
		if err != nil {
			a.log.Error("aguard forkguard: sample", "err", err)
		} else {
			r := engine.Observe(s)
			if r.Summary != "" {
				a.log.Info(r.Summary)
			}
			for _, line := range r.Lines {
				a.log.Warn(line)
			}
			for i := range r.Decisions {
				a.log.Warn(decisionLineWithTree(r.Decisions[i]))
			}
		}
		t := time.NewTimer(c.Interval.D())
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
}

func (a *app) forkguardSample(c config.ForkguardConfig) (forkguard.Sample, error) {
	base := a.cgroupDir
	if base == "" {
		base = "/sys/fs/cgroup"
	}
	root := filepath.Join(base, "user.slice", fmt.Sprintf("user-%d.slice", a.uid), fmt.Sprintf("user@%d.service", a.uid), c.Slice)
	return ReadForkguardTree(root, time.Now())
}

// ReadForkguardTree recursively reads cgroup v2 counters, including nested slices.
func ReadForkguardTree(root string, at time.Time) (forkguard.Sample, error) {
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return forkguard.Sample{At: at, SliceMissing: true}, nil
	} else if err != nil {
		return forkguard.Sample{}, fmt.Errorf("stat slice directory: %w", err)
	}
	cur, max, events, err := readForkguardCounters(root)
	if err != nil {
		return forkguard.Sample{}, err
	}
	// pids.peak — с ядра 6.x; нет файла — пик 0, замер идёт без него
	var peak int64
	if peakBytes, err := os.ReadFile(filepath.Join(root, "pids.peak")); err == nil {
		if peak, err = strconv.ParseInt(strings.TrimSpace(string(peakBytes)), 10, 64); err != nil {
			return forkguard.Sample{}, fmt.Errorf("parse slice pids.peak: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return forkguard.Sample{}, fmt.Errorf("read slice pids.peak: %w", err)
	}
	s := forkguard.Sample{At: at, SliceCurrent: cur, SliceMax: max, SliceEventsMax: events, SlicePidsPeak: peak}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if path == root {
				return walkErr
			}
			if isForkguardUnitPath(path) {
				unit, exists, statErr := existingForkguardUnit(path)
				if statErr != nil {
					unit.Error = statErr.Error()
					s.Skipped = append(s.Skipped, unit)
				} else if exists {
					unit.Error = walkErr.Error()
					s.Skipped = append(s.Skipped, unit)
				}
			}
			return nil
		}
		if !d.IsDir() || path == root {
			return nil
		}
		if isForkguardUnitPath(path) {
			id, exists, statErr := existingForkguardUnit(path)
			if statErr != nil {
				id.Error = statErr.Error()
				s.Skipped = append(s.Skipped, id)
				return fs.SkipDir
			}
			if !exists {
				return fs.SkipDir
			}
			if id.Inode == 0 {
				s.Skipped = append(s.Skipped, id)
				return fs.SkipDir
			}
			uc, um, events, e := readForkguardCounters(path)
			if e == nil {
				s.Units = append(s.Units, forkguard.Unit{Name: id.Name, Path: path, Inode: id.Inode, Current: uc, Max: um, EventsMax: events})
			} else {
				_, statErr := os.Stat(path)
				if errors.Is(statErr, os.ErrNotExist) {
					return fs.SkipDir
				}
				if statErr != nil {
					e = errors.Join(e, statErr)
				}
				id.Error = e.Error()
				s.Skipped = append(s.Skipped, id)
			}
			return fs.SkipDir
		}
		return nil
	})
	return s, err
}

func isForkguardUnitPath(path string) bool {
	name := filepath.Base(path)
	return strings.HasSuffix(name, ".scope") || strings.HasSuffix(name, ".service")
}

func existingForkguardUnit(path string) (forkguard.UnitIdentity, bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return forkguard.UnitIdentity{}, false, nil
		}
		return forkguard.UnitIdentity{Name: filepath.Base(path)}, true, fmt.Errorf("stat cgroup directory: %w", err)
	}
	if !info.IsDir() {
		return forkguard.UnitIdentity{}, false, nil
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return forkguard.UnitIdentity{Name: filepath.Base(path)}, true, fmt.Errorf("cgroup stat has unexpected type %T", info.Sys())
	}
	return forkguard.UnitIdentity{Name: filepath.Base(path), Inode: stat.Ino}, true, nil
}

func readForkguardCounters(dir string) (int64, int64, int64, error) {
	readNum := func(name string) (int64, error) {
		b, e := os.ReadFile(filepath.Join(dir, name))
		if e != nil {
			return 0, fmt.Errorf("read %s: %w", name, e)
		}
		n, e := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		if e != nil {
			return 0, fmt.Errorf("parse %s: %w", name, e)
		}
		return n, nil
	}
	cur, err := readNum("pids.current")
	if err != nil {
		return 0, 0, 0, fmt.Errorf("pids.current: %w", err)
	}
	maxRaw, err := os.ReadFile(filepath.Join(dir, "pids.max"))
	if err != nil {
		return 0, 0, 0, fmt.Errorf("read pids.max: %w", err)
	}
	max := int64(0)
	if strings.TrimSpace(string(maxRaw)) != "max" {
		max, err = strconv.ParseInt(strings.TrimSpace(string(maxRaw)), 10, 64)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("parse pids.max: %w", err)
		}
	}
	eventsRaw, err := os.ReadFile(filepath.Join(dir, "pids.events"))
	if err != nil {
		return 0, 0, 0, fmt.Errorf("read pids.events: %w", err)
	}
	var events int64
	for line := range strings.SplitSeq(string(eventsRaw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "max" {
			events, err = strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0, 0, 0, fmt.Errorf("parse pids.events max: %w", err)
			}
		}
	}
	return cur, max, events, nil
}

func decisionLineWithTree(d forkguard.Decision) string {
	if strings.HasPrefix(d.Unit.Name, "agent-") && d.TreePID == 0 {
		var err error
		d.TreePID, d.TreeName, err = selectForkguardTree(d.Unit.Path, "/proc")
		if err != nil {
			d.TreeUnavailable = err.Error()
		}
	}
	return d.LogLine()
}

type forkProc struct {
	ppid      int
	name      string
	startTime uint64
}

// The oldest process in cgroup.procs is treated as the scope main process; only its direct children can be command roots.
func selectForkguardTree(cgroup, procRoot string) (int, string, error) {
	b, err := os.ReadFile(filepath.Join(cgroup, "cgroup.procs"))
	if err != nil {
		return 0, "", fmt.Errorf("read scope cgroup.procs: %w", err)
	}
	pids := map[int]bool{}
	for f := range strings.FieldsSeq(string(b)) {
		p, parseErr := strconv.Atoi(f)
		if parseErr != nil {
			return 0, "", fmt.Errorf("parse PID %q in cgroup.procs: %w", f, parseErr)
		}
		if p <= 0 {
			return 0, "", fmt.Errorf("invalid PID %d in cgroup.procs", p)
		}
		pids[p] = true
	}
	procs := make(map[int]forkProc, len(pids))
	unreadable := 0
	for pid := range pids {
		b, e := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
		if e != nil {
			unreadable++
			continue
		}
		p, parseErr := parseForkProc(string(b))
		if parseErr != nil {
			unreadable++
			continue
		}
		procs[pid] = p
	}
	mainPID, mainStart := 0, ^uint64(0)
	for pid, p := range procs {
		if p.startTime < mainStart || p.startTime == mainStart && (mainPID == 0 || pid < mainPID) {
			mainPID, mainStart = pid, p.startTime
		}
	}
	children := map[int][]int{}
	for pid, p := range procs {
		children[p.ppid] = append(children[p.ppid], pid)
	}
	best, bestN := 0, 0
	bestName := ""
	var count func(int, map[int]bool) int
	count = func(pid int, seen map[int]bool) int {
		if seen[pid] {
			return 0
		}
		seen[pid] = true
		n := 1
		for _, ch := range children[pid] {
			n += count(ch, seen)
		}
		return n
	}
	for _, child := range children[mainPID] {
		n := count(child, map[int]bool{})
		if n > bestN {
			best, bestN, bestName = child, n, procs[child].name
		}
	}
	if best == 0 {
		return 0, "", fmt.Errorf("no command descendant found in scope (unreadable proc entries: %d)", unreadable)
	}
	if unreadable > 0 {
		return best, bestName, fmt.Errorf("incomplete proc tree: %d entries unreadable", unreadable)
	}
	return best, bestName, nil
}

func parseForkProc(raw string) (forkProc, error) {
	i := strings.IndexByte(raw, '(')
	j := strings.LastIndexByte(raw, ')')
	if i < 0 || j <= i {
		return forkProc{}, fmt.Errorf("malformed proc stat command name")
	}
	f := strings.Fields(raw[j+1:])
	if len(f) < 2 {
		return forkProc{}, fmt.Errorf("proc stat lacks ppid")
	}
	if len(f) < 20 {
		return forkProc{}, fmt.Errorf("proc stat lacks start time")
	}
	ppid, err := strconv.Atoi(f[1])
	if err != nil {
		return forkProc{}, fmt.Errorf("parse proc stat ppid: %w", err)
	}
	if _, pidErr := strconv.Atoi(strings.TrimSpace(raw[:i])); pidErr != nil {
		return forkProc{}, fmt.Errorf("parse proc stat pid: %w", pidErr)
	}
	startTime, err := strconv.ParseUint(f[19], 10, 64)
	if err != nil {
		return forkProc{}, fmt.Errorf("parse proc stat start time: %w", err)
	}
	return forkProc{ppid: ppid, name: raw[i+1 : j], startTime: startTime}, nil
}
