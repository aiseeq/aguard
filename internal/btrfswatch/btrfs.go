// Package btrfswatch reads Btrfs allocation statistics exposed by sysfs.
package btrfswatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
	"github.com/aiseeq/aguard/internal/record"
)

const (
	GiB                           = int64(1 << 30)
	defaultMetadataPercent        = 85
	defaultMetadataUnallocatedGiB = 40
	stateFileMode                 = 0o600
)

func Reference() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home for Btrfs reference path: %w", err)
	}
	return filepath.Join(home, "git/machine/docs/reference/reference_snapper_setup.md"), nil
}

type Config struct {
	Interval               policy.Duration `toml:"interval"`
	SysfsRoot              string          `toml:"sysfs_root"`
	UnallocatedMin         policy.Size     `toml:"unallocated_min"`
	MetadataPercent        int             `toml:"metadata_percent"`
	MetadataUnallocatedMax policy.Size     `toml:"metadata_unallocated_max"`
	Lead                   string          `toml:"-"`
	Tell                   []string        `toml:"tell"`
	SendAuth               bool            `toml:"-"`
}

func Default() Config {
	return Config{Interval: policy.Duration(time.Minute), SysfsRoot: "/sys/fs/btrfs", UnallocatedMin: policy.Size(20 * GiB), MetadataPercent: defaultMetadataPercent, MetadataUnallocatedMax: policy.Size(defaultMetadataUnallocatedGiB * GiB), Tell: []string{"git-admin"}}
}

type FS struct {
	UUID                                                             string
	DeviceBytes, Allocated, Unallocated, MetadataUsed, MetadataTotal int64
	Qgroups                                                          bool
}
type Alert struct{ Key, Text string }

// Read discovers registered (mounted) Btrfs filesystems and uses sysfs counters only.
func Read(root string) ([]FS, error) {
	dirs, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read Btrfs sysfs %s: %w", root, err)
	}
	var out []FS
	var failures []ReadFailure
	for _, d := range dirs {
		if !d.IsDir() || !isUUID(d.Name()) {
			continue
		}
		f, err := readFS(root, d.Name())
		if err != nil {
			failures = append(failures, ReadFailure{UUID: d.Name(), Err: err})
			continue
		}
		out = append(out, f)
	}
	if len(failures) > 0 {
		return out, &ReadError{Failures: failures}
	}
	return out, nil
}

type ReadFailure struct {
	UUID string
	Err  error
}

type ReadError struct{ Failures []ReadFailure }

func (e *ReadError) Error() string {
	parts := make([]string, 0, len(e.Failures))
	for _, f := range e.Failures {
		parts = append(parts, fmt.Sprintf("%s: %v", f.UUID, f.Err))
	}
	return "read Btrfs filesystems: " + strings.Join(parts, "; ")
}

func (e *ReadError) Unwrap() []error {
	errs := make([]error, 0, len(e.Failures))
	for _, f := range e.Failures {
		errs = append(errs, f.Err)
	}
	return errs
}

func (e *ReadError) Failed(uuid string) bool {
	for _, f := range e.Failures {
		if f.UUID == uuid {
			return true
		}
	}
	return false
}

func readFS(root, uuid string) (FS, error) {
	base := filepath.Join(root, uuid)
	f := FS{UUID: uuid}
	devs, err := os.ReadDir(filepath.Join(base, "devices"))
	if err != nil {
		return FS{}, fmt.Errorf("devices: %w", err)
	}
	if len(devs) == 0 {
		return FS{}, errors.New("devices is empty")
	}
	for _, dev := range devs {
		b, err := os.ReadFile(filepath.Join(base, "devices", dev.Name(), "size"))
		if err != nil {
			return FS{}, fmt.Errorf("device %s size: %w", dev.Name(), err)
		}
		n, err := number(b)
		if err != nil || n == 0 {
			if err == nil {
				err = errors.New("device size is zero")
			}
			return FS{}, fmt.Errorf("device %s size: %w", dev.Name(), err)
		}
		f.DeviceBytes += n * 512
	}
	for _, kind := range []string{"data", "metadata", "system"} {
		p := filepath.Join(base, "allocation", kind, "disk_total")
		b, err := os.ReadFile(p)
		if err != nil {
			return FS{}, fmt.Errorf("allocation %s disk_total: %w", kind, err)
		}
		n, err := number(b)
		if err != nil {
			return FS{}, fmt.Errorf("allocation %s disk_total: %w", kind, err)
		}
		f.Allocated += n
		if kind == "metadata" {
			u, err := os.ReadFile(filepath.Join(base, "allocation", kind, "bytes_used"))
			if err != nil {
				return FS{}, fmt.Errorf("metadata bytes_used: %w", err)
			}
			f.MetadataUsed, err = number(u)
			if err != nil {
				return FS{}, fmt.Errorf("metadata bytes_used: %w", err)
			}
			t, err := os.ReadFile(filepath.Join(base, "allocation", kind, "total_bytes"))
			if err != nil {
				return FS{}, fmt.Errorf("metadata total_bytes: %w", err)
			}
			f.MetadataTotal, err = number(t)
			if err != nil {
				return FS{}, fmt.Errorf("metadata total_bytes: %w", err)
			}
		}
	}
	f.Unallocated = f.DeviceBytes - f.Allocated
	if f.Unallocated < 0 {
		return FS{}, fmt.Errorf("negative unallocated space: device=%d allocated=%d", f.DeviceBytes, f.Allocated)
	}
	if st, err := os.Stat(filepath.Join(base, "qgroups")); err == nil {
		f.Qgroups = st.IsDir()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return FS{}, fmt.Errorf("qgroups: %w", err)
	}
	return f, nil
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if r != '-' {
				return false
			}
		} else if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}

func number(b []byte) (int64, error) {
	n, e := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if e != nil || n < 0 {
		return 0, fmt.Errorf("invalid sysfs number %q", b)
	}
	return n, nil
}

func (f FS) Alerts(c Config) []Alert {
	var out []Alert
	if f.Unallocated < int64(c.UnallocatedMin) {
		out = append(out, Alert{"unallocated", fmt.Sprintf("нераспределено %s, порог меньше %s", policy.HumanBytes(f.Unallocated), policy.HumanBytes(int64(c.UnallocatedMin)))})
	}
	if f.MetadataTotal > 0 && f.MetadataUsed*100 > f.MetadataTotal*int64(c.MetadataPercent) && f.Unallocated < int64(c.MetadataUnallocatedMax) {
		out = append(out, Alert{"metadata", fmt.Sprintf("метаданные %s / %s (%d%%), порог больше %d%%; нераспределено %s, порог меньше %s", policy.HumanBytes(f.MetadataUsed), policy.HumanBytes(f.MetadataTotal), f.MetadataUsed*100/f.MetadataTotal, c.MetadataPercent, policy.HumanBytes(f.Unallocated), policy.HumanBytes(int64(c.MetadataUnallocatedMax)))})
	}
	if f.Qgroups {
		out = append(out, Alert{"qgroups", "включены квоты (каталог sysfs qgroups существует)"})
	}
	return out
}

type Told map[string]time.Time

func Load(path string) (Told, error) {
	m := Told{}
	b, e := os.ReadFile(path)
	if errors.Is(e, fs.ErrNotExist) {
		return m, nil
	}
	if e != nil {
		return nil, e
	}
	if e = json.Unmarshal(b, &m); e != nil {
		return nil, e
	}
	return m, nil
}
func (m Told) Due(key string, active bool, now time.Time) bool {
	if !active {
		delete(m, key)
		return false
	}
	next, ok := m[key]
	return !ok || !now.Before(next)
}
func (m Told) Mark(key string, now time.Time, sent bool) {
	if sent {
		m[key] = now.Add(24 * time.Hour)
	} else {
		m[key] = now.Add(time.Hour)
	}
}
func (m Told) Save(path string) error {
	b, e := json.Marshal(m)
	if e != nil {
		return e
	}
	return record.WriteFileAtomic(path, b, stateFileMode)
}
