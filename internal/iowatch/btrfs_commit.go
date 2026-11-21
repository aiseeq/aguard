package iowatch

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

type btrfsCommitStats struct {
	commits, curMS, lastMS int64
}

var (
	btrfsCommitStatFields     = [...]string{"commits", "cur_commit_ms", "last_commit_ms", "max_commit_ms", "total_commit_ms"}
	btrfsCommitRequiredFields = [...]string{"commits", "last_commit_ms", "max_commit_ms", "total_commit_ms"}
)

// BtrfsCommitSampler samples the sysfs counters once per caller-selected interval.
type BtrfsCommitSampler struct {
	Root string
	prev map[string]int64
}

// Sample aggregates all registered filesystems. Missing sysfs files are supported by old kernels.
func (s *BtrfsCommitSampler) Sample() (curMS, commits, lastMS int64, err error) {
	dirs, err := os.ReadDir(s.Root)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, 0, 0, nil
	}
	if err != nil {
		return 0, 0, 0, fmt.Errorf("read Btrfs sysfs %s: %w", s.Root, err)
	}
	if s.prev == nil {
		s.prev = make(map[string]int64)
	}
	next := make(map[string]int64)
	var failures []error
	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}
		path := filepath.Join(s.Root, dir.Name(), "commit_stats")
		stats, readErr := readBtrfsCommitStats(path)
		if errors.Is(readErr, fs.ErrNotExist) {
			continue
		}
		if readErr != nil {
			failures = append(failures, fmt.Errorf("%s: %w", dir.Name(), readErr))
			if prev, ok := s.prev[dir.Name()]; ok {
				next[dir.Name()] = prev
			}
			continue
		}
		curMS = max(curMS, stats.curMS)
		if prev, ok := s.prev[dir.Name()]; ok {
			if stats.commits >= prev {
				commits += stats.commits - prev
			}
			if stats.commits > prev {
				lastMS = max(lastMS, stats.lastMS)
			}
		}
		next[dir.Name()] = stats.commits
	}
	s.prev = next
	return curMS, commits, lastMS, errors.Join(failures...)
}

func readBtrfsCommitStats(path string) (btrfsCommitStats, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return btrfsCommitStats{}, fmt.Errorf("read commit_stats: %w", err)
	}
	values := make(map[string]int64, 5)
	for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return btrfsCommitStats{}, fmt.Errorf("malformed commit_stats line %q", line)
		}
		if !slices.Contains(btrfsCommitStatFields[:], fields[0]) {
			continue
		}
		value, parseErr := strconv.ParseInt(fields[1], 10, 64)
		if parseErr != nil || value < 0 {
			return btrfsCommitStats{}, fmt.Errorf("invalid %s value %q", fields[0], fields[1])
		}
		values[fields[0]] = value
	}
	for _, key := range btrfsCommitRequiredFields {
		if _, ok := values[key]; !ok {
			return btrfsCommitStats{}, fmt.Errorf("missing %s", key)
		}
	}
	return btrfsCommitStats{commits: values["commits"], curMS: values["cur_commit_ms"], lastMS: values["last_commit_ms"]}, nil
}
