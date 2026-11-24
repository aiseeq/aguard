package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	cfg, err := a.config()
	if err != nil {
		return err
	}
	switch cmd {
	case "tick":
		s, err := a.forkguardSample(cfg.Forkguard)
		if err != nil {
			return err
		}
		engine := forkguard.New(cfg.Forkguard.EngineConfig())
		result := engine.Observe(s)
		fmt.Fprintf(a.out, "forkguard tick: at=%s slice_current=%d slice_max=%d units=%d\n", s.At.Format(time.RFC3339), s.SliceCurrent, s.SliceMax, len(s.Units))
		for _, line := range result.Lines {
			fmt.Fprintln(a.out, line)
		}
		for _, d := range result.Decisions {
			fmt.Fprintln(a.out, decisionLineWithTree(d))
		}
		return nil
	case "run":
		if runFor > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, runFor)
			defer cancel()
		}
		return a.forkguardRun(ctx, &cfg.Forkguard)
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
			for _, d := range r.Decisions {
				a.log.Warn(decisionLineWithTree(d))
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
	cur, max, _, err := readForkguardCounters(root)
	if err != nil {
		return forkguard.Sample{}, err
	}
	s := forkguard.Sample{At: at, SliceCurrent: cur, SliceMax: max}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if path == root {
				return walkErr
			}
			return nil
		}
		if d.IsDir() && path != root && (strings.HasSuffix(d.Name(), ".scope") || strings.HasSuffix(d.Name(), ".service")) {
			uc, um, events, e := readForkguardCounters(path)
			if e == nil {
				s.Units = append(s.Units, forkguard.Unit{Name: d.Name(), Path: path, Current: uc, Max: um, EventsMax: events})
			}
		}
		return nil
	})
	return s, err
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
		d.TreePID, d.TreeName, err = selectForkguardTree(d.Unit.Path)
		if err != nil {
			d.TreeUnavailable = err.Error()
		}
	}
	return d.LogLine()
}

type forkProc struct {
	pid, ppid int
	name      string
}

func selectForkguardTree(cgroup string) (int, string, error) {
	b, err := os.ReadFile(filepath.Join(cgroup, "cgroup.procs"))
	if err != nil {
		return 0, "", fmt.Errorf("read scope cgroup.procs: %w", err)
	}
	pids := map[int]bool{}
	for f := range strings.FieldsSeq(string(b)) {
		if p, e := strconv.Atoi(f); e == nil && p > 0 {
			pids[p] = true
		}
	}
	procs := make(map[int]forkProc, len(pids))
	unreadable := 0
	for pid := range pids {
		b, e := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if e != nil {
			unreadable++
			continue
		}
		p, ok := parseForkProc(string(b))
		if ok {
			procs[pid] = p
		} else {
			unreadable++
		}
	}
	children := map[int][]int{}
	roots := []int{}
	for pid, p := range procs {
		if !pids[p.ppid] {
			roots = append(roots, pid)
		} else {
			children[p.ppid] = append(children[p.ppid], pid)
		}
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
	for _, root := range roots {
		for _, child := range children[root] {
			n := count(child, map[int]bool{})
			if n > bestN {
				best, bestN, bestName = child, n, procs[child].name
			}
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

func parseForkProc(raw string) (forkProc, bool) {
	i := strings.IndexByte(raw, '(')
	j := strings.LastIndexByte(raw, ')')
	if i < 0 || j <= i {
		return forkProc{}, false
	}
	f := strings.Fields(raw[j+1:])
	if len(f) < 2 {
		return forkProc{}, false
	}
	ppid, e := strconv.Atoi(f[1])
	if e != nil {
		return forkProc{}, false
	}
	pid, e := strconv.Atoi(strings.TrimSpace(raw[:i]))
	if e != nil {
		return forkProc{}, false
	}
	return forkProc{pid: pid, ppid: ppid, name: raw[i+1 : j]}, true
}
