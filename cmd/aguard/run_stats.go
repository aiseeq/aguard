package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/iowatch"
)

// logRunSummary — строка журнала «итог запуска» при остановке: байты диска своей cgroup и пик RSS. systemd их
// не сохраняет как надо (IOReadBytes складывает диск и dm поверх него), а tools/perf/unit-day.sh считает по ней
// read, write и rss за сутки.
func (a *app) logRunSummary(started time.Time) {
	attrs := []any{"started", started.Format(time.RFC3339)}
	stats := collectRunStats(procRoot, "/sys/fs/cgroup", filepath.Join(procRoot, "self", "status"))
	var errs []string
	if stats.readErr == nil {
		attrs = append(attrs, "read_bytes", stats.readBytes)
	} else {
		errs = append(errs, "read_bytes: "+stats.readErr.Error())
	}
	if stats.writeErr == nil {
		attrs = append(attrs, "write_bytes", stats.writeBytes)
	} else {
		errs = append(errs, "write_bytes: "+stats.writeErr.Error())
	}
	if stats.vmHWMErr == nil {
		attrs = append(attrs, "vm_hwm_kb", stats.vmHWMKB)
	} else {
		errs = append(errs, "vm_hwm_kb: "+stats.vmHWMErr.Error())
	}
	if len(errs) != 0 {
		attrs = append(attrs, "err", strings.Join(errs, "; "))
	}
	a.log.Info("aguard: итог запуска", attrs...)
}

type runStats struct {
	readBytes, writeBytes int64
	vmHWMKB               int64
	readErr, writeErr     error
	vmHWMErr              error
}

func collectRunStats(procRoot, cgroupRoot, statusPath string) runStats {
	var stats runStats
	cgroup, err := unifiedCgroup(filepath.Join(procRoot, "self", "cgroup"))
	if err == nil {
		var data []byte
		data, err = os.ReadFile(filepath.Join(cgroupRoot, cgroup, "io.stat"))
		if err == nil {
			stats.readBytes, stats.writeBytes, err = iowatch.ParseIOStatMax(data)
		}
	}
	if err != nil {
		stats.readErr, stats.writeErr = fmt.Errorf("cgroup io: %w", err), fmt.Errorf("cgroup io: %w", err)
	}
	data, err := os.ReadFile(statusPath)
	if err == nil {
		stats.vmHWMKB, err = parseVmHWM(data)
	}
	if err != nil {
		stats.vmHWMErr = fmt.Errorf("VmHWM: %w", err)
	}
	return stats
}

func unifiedCgroup(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("прочитать %s: %w", path, err)
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if after, ok := strings.CutPrefix(line, "0::/"); ok {
			return after, nil
		}
	}
	return "", fmt.Errorf("%s: нет unified cgroup 0::", path)
}

func parseVmHWM(data []byte) (int64, error) {
	for line := range strings.SplitSeq(string(data), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || key != "VmHWM" {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) < 2 || fields[1] != "kB" {
			return 0, fmt.Errorf("неверная строка VmHWM: %q", line)
		}
		n, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("неверное значение VmHWM: %w", err)
		}
		return n, nil
	}
	return 0, fmt.Errorf("нет VmHWM")
}
