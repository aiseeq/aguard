package iowatch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/evictors"
	"github.com/aiseeq/akit/proc"
)

// DetailReader — детальная запись раз в detail: Dirty, Writeback, MemAvailable и занятый своп,
// листья cgroup, процессы.
// Принадлежит одной горутине (медленный проход), с быстрым шагом общих данных нет.
type DetailReader struct {
	procRoot  string
	graftDir  string
	graftSeen map[string]GraftScope
	meminfo   cachedFile
	buf       []byte
	cgroups   *cgroupReader
	processes *processReader
	evictors  evictors.Tracker
	graftMem  *GraftSliceMemReader
}

// Roots — корни файловых систем ядра; в тестах — папки с образцами.
type Roots struct {
	Proc, Cgroup, Block string
}

// SystemRoots — настоящие /proc, /sys/fs/cgroup и /sys/block.
func SystemRoots() Roots {
	return Roots{Proc: "/proc", Cgroup: "/sys/fs/cgroup", Block: "/sys/block"}
}

// NewDetailReader определяет устройства диска (сам диск и dm поверх него) один раз — при старте.
func NewDetailReader(roots Roots, device string) (*DetailReader, error) {
	devices, err := blockDevices(roots.Block, device)
	if err != nil {
		return nil, fmt.Errorf("устройства диска %s: %w", device, err)
	}
	return &DetailReader{procRoot: roots.Proc, meminfo: cachedFile{path: filepath.Join(roots.Proc, "meminfo")}, buf: make([]byte, 8192),
		cgroups: newCgroupReader(roots.Cgroup, devices), processes: newProcessReader(roots.Proc), evictors: evictors.NewTracker(evictors.DefaultSwapMin, evictors.DefaultAnonMin),
		graftMem: NewGraftSliceMemReader(roots.Cgroup, os.Getuid())}, nil
}

// SetGraftHeavyDir sets the location of graft's per-scope kind files.
func (r *DetailReader) SetGraftHeavyDir(dir string) { r.graftDir = dir }

// SetEvictorThresholds configures the two minimum deltas before the first Read.
func (r *DetailReader) SetEvictorThresholds(swapMin, anonMin int64) {
	r.evictors = evictors.NewTracker(swapMin, anonMin)
}

func (r *DetailReader) Close() {
	r.meminfo.close()
	r.cgroups.close()
	r.processes.close()
}

// Read — детальная запись на момент now. skipped — процессы, пропущенные из-за ошибки: запись
// годна без них; err — записи нет.
func (r *DetailReader) Read(now time.Time) (s Step, skipped, err error) {
	s = Step{At: now}
	defer func() { s.memoryGroups = nil }()
	var data []byte
	if r.buf, data, err = r.meminfo.read(r.buf); err != nil {
		return s, nil, err
	}
	mem, err := parseMemInfo(data)
	if err != nil {
		return s, nil, err
	}
	s.Dirty, s.Writeback, s.MemAvail, s.SwapUsed = mem.dirty, mem.writeback, mem.available, mem.swapUsed()
	s.GraftSliceMem, err = r.graftMem.Sample(now)
	s.GraftSliceMemKnown = err == nil
	graftErr := err
	groups, err := r.cgroups.read(&s)
	if err != nil {
		return s, nil, err
	}
	procs, err := r.processes.read(&s)
	graftScopesErr := r.captureGraftScopes(&s)
	s.evictorRow = r.evictors.Sample(now, s.memoryGroups)
	if s.evictorRow != nil {
		for i := range s.evictorRow.Changes {
			c := &s.evictorRow.Changes[i]
			if !c.Candidate {
				continue
			}
			process, procsErr := r.cgroups.processSource(c.Path, r.processes.root)
			c.Process = process
			if procsErr != nil {
				procs = errors.Join(procs, procsErr)
			}
		}
	}
	return s, errors.Join(graftErr, groups, procs, graftScopesErr), err
}

// captureGraftScopes resolves newly observed graft scopes once per live scope and carries the cached metadata into
// the recorder. cgroups.leaves includes scopes even when graft itself and its parent do no I/O.
func (r *DetailReader) captureGraftScopes(s *Step) error {
	active := make(map[string]bool, len(r.cgroups.graftLeaves))
	var first error
	for _, path := range r.cgroups.graftLeaves {
		active[path] = true
		info, ok := r.graftSeen[path]
		if !ok {
			var err error
			info, err = r.readGraftScope(path)
			if err != nil && first == nil {
				first = err
			}
			if r.graftSeen == nil {
				r.graftSeen = make(map[string]GraftScope)
			}
			r.graftSeen[path] = info
		}
		info.ParentChain = slices.Clone(info.ParentChain)
		s.graftScopes = append(s.graftScopes, info)
	}
	for path := range r.graftSeen {
		if !active[path] {
			delete(r.graftSeen, path)
		}
	}
	return first
}

func graftScopePath(path string) string {
	var current strings.Builder
	for part := range strings.SplitSeq(strings.TrimPrefix(path, "/"), "/") {
		current.WriteString("/" + part)
		if _, ok := graftScopePID(current.String()); ok {
			return current.String()
		}
	}
	return ""
}

func graftScopePID(path string) (int, bool) {
	base := filepath.Base(path)
	text, ok := strings.CutPrefix(base, graftHeavyPrefix)
	if !ok {
		return 0, false
	}
	text, ok = strings.CutSuffix(text, ".scope")
	if !ok || text == "" {
		return 0, false
	}
	for i := range len(text) {
		if text[i] < '0' || text[i] > '9' {
			return 0, false
		}
	}
	pid, err := strconv.Atoi(text)
	return pid, err == nil && pid > 0
}

func (r *DetailReader) readGraftScope(path string) (GraftScope, error) {
	info := GraftScope{Path: path}
	if r.graftDir != "" {
		if kind, err := graftKind(filepath.Join(r.graftDir, filepath.Base(path))); err == nil {
			info.Kind = kind
		} else if !errors.Is(err, os.ErrNotExist) {
			return info, fmt.Errorf("graft scope %s: %w", path, err)
		}
	}
	pid, ok := graftScopePID(path)
	if !ok {
		return info, nil
	}
	st, err := proc.ReadStat(r.procRoot, pid)
	if err != nil {
		if proc.Gone(err) {
			return info, nil
		}
		return info, fmt.Errorf("graft scope %s stat: %w", path, err)
	}
	cg, err := readProcCgroup(r.procRoot, pid)
	if errors.Is(err, os.ErrNotExist) || err == nil && cg != path {
		return info, nil
	}
	if err != nil {
		return info, fmt.Errorf("graft scope %s cgroup: %w", path, err)
	}
	seen := map[int]bool{pid: true}
	for parent, depth := st.Ppid, 0; parent > 1 && !seen[parent] && depth < 64; depth++ {
		seen[parent] = true
		parentStat, err := proc.ReadStat(r.procRoot, parent)
		if err != nil {
			if proc.Gone(err) {
				break
			}
			return info, fmt.Errorf("родитель graft scope %s stat: %w", path, err)
		}
		cg, err := readProcCgroup(r.procRoot, parent)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				break
			}
			return info, fmt.Errorf("родитель graft scope %s cgroup: %w", path, err)
		}
		info.ParentChain = append(info.ParentChain, Process{PID: parent, PPID: parentStat.Ppid, Comm: parentStat.Comm, Cgroup: cg})
		parent = parentStat.Ppid
	}
	return info, nil
}

func readProcCgroup(root string, pid int) (string, error) {
	b, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return "", fmt.Errorf("прочитать cgroup pid %d: %w", pid, err)
	}
	return proc.ParseCgroup(string(b)), nil
}
