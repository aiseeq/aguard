package iowatch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/aiseeq/aguard/internal/evictors"
)

// DetailReader — детальная запись раз в detail: Dirty, Writeback, MemAvailable и занятый своп,
// листья cgroup, процессы.
// Принадлежит одной горутине (медленный проход), с быстрым шагом общих данных нет.
type DetailReader struct {
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
	return &DetailReader{meminfo: cachedFile{path: filepath.Join(roots.Proc, "meminfo")}, buf: make([]byte, 8192),
		cgroups: newCgroupReader(roots.Cgroup, devices), processes: newProcessReader(roots.Proc), evictors: evictors.NewTracker(evictors.DefaultSwapMin, evictors.DefaultAnonMin),
		graftMem: NewGraftSliceMemReader(roots.Cgroup, os.Getuid())}, nil
}

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
	return s, errors.Join(graftErr, groups, procs), err
}
