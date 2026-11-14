// Package ceiling описывает формат и расчёты потолка памяти сессии.
package ceiling

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aiseeq/aguard/internal/cgroupstats"
	"github.com/aiseeq/aguard/internal/meminfo"
	"github.com/aiseeq/aguard/internal/policy"
	"github.com/aiseeq/aguard/internal/record"
)

type Config struct {
	Interval policy.Duration `toml:"interval"`
}

type ClMemory struct {
	Max     string `json:"max"`
	SwapMax string `json:"swap_max"`
}

func Default() Config { return Config{Interval: policy.Duration(200 * time.Millisecond)} }
func (c Config) Validate() error {
	if c.Interval.D() <= 0 {
		return fmt.Errorf("ceiling.interval must be positive")
	}
	return nil
}

const DefaultSwap = "256M"
const percentMaximum = 100
const (
	decimalKilo = 1000
	binaryTera  = 1 << 40
)

func ClConfigPath(home string) string {
	if path := os.Getenv("CL_CONFIG"); path != "" {
		return path
	}
	return filepath.Join(home, ".config", "cl", "config.json")
}

// ReadMemoryConfig reads the memory section from cl's config file. A missing
// file means memory limits are disabled, matching cl's default behavior.
func ReadMemoryConfig(path string) (ClMemory, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ClMemory{}, nil
	}
	if err != nil {
		return ClMemory{}, fmt.Errorf("read cl config %s: %w", path, err)
	}
	var cfg struct {
		Memory ClMemory `json:"memory"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return ClMemory{}, fmt.Errorf("decode cl config %s: %w", path, err)
	}
	return cfg.Memory, nil
}

const quantum int64 = 64 << 20
const Unlimited int64 = -1

type State struct {
	Unit            string    `json:"unit"`
	Base            int64     `json:"base"`
	Swap            int64     `json:"swap"`
	By              string    `json:"by"`
	At              time.Time `json:"at"`
	OOMSeen         bool      `json:"oom_seen,omitempty"`
	OOM             int64     `json:"oom_kill,omitempty"`
	OOMNotified     int64     `json:"oom_notified,omitempty"`
	MissingAttempts int       `json:"missing_attempts,omitempty"`
	OOMCursor       string    `json:"oom_cursor,omitempty"`
	OOMPendingAt    time.Time `json:"oom_pending_at"`
	OOMPendingCount int64     `json:"oom_pending_count,omitempty"`
	OOMAttemptAt    time.Time `json:"oom_attempt_at"`
	OOMAttempts     int       `json:"oom_attempts,omitempty"`
	MissingAt       time.Time `json:"missing_at"`
	MissingCheckAt  time.Time `json:"missing_check_at"`
	ClaudePID       int       `json:"claude_pid,omitempty"`
}

func StatePath(home, unit string) string {
	return filepath.Join(home, ".local", "state", "aguard", "ceiling", unit+".json")
}

func Read(path string) (state State, result error) {
	if _, err := os.Stat(path); err != nil {
		return State{}, fmt.Errorf("read ceiling state %s: %w", path, err)
	}
	lock, err := openLock(path, false)
	if err != nil {
		return State{}, fmt.Errorf("lock ceiling state %s: %w", path, err)
	}
	defer func() { result = errors.Join(result, closeLock(lock)) }()
	return readUnlocked(path)
}

func Write(path string, state State) (result error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create ceiling state directory: %w", err)
	}
	lock, err := openLock(path, true)
	if err != nil {
		return fmt.Errorf("lock ceiling state %s: %w", path, err)
	}
	defer func() { result = errors.Join(result, closeLock(lock)) }()
	return writeUnlocked(path, state)
}

// Update сериализует read-modify-write между cl и наблюдателем.
func Update(path string, change func(*State) error) (state State, result error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return State{}, fmt.Errorf("create ceiling state directory: %w", err)
	}
	lock, err := openLock(path, true)
	if err != nil {
		return State{}, fmt.Errorf("lock ceiling state %s: %w", path, err)
	}
	defer func() { result = errors.Join(result, closeLock(lock)) }()
	state, err = readUnlocked(path)
	if errors.Is(err, os.ErrNotExist) {
		return State{}, fmt.Errorf("update ceiling state %s: %w", path, err)
	} else if err != nil {
		return State{}, err
	}
	before := state
	if err := change(&state); err != nil {
		return State{}, err
	}
	if reflect.DeepEqual(before, state) {
		return state, nil
	}
	if err := writeUnlocked(path, state); err != nil {
		return State{}, err
	}
	return state, nil
}

// SaveObserved обновляет поля наблюдателя, сохраняя base и другие поля владельца scope.
func SaveObserved(path string, observed State) (State, error) {
	return Update(path, func(current *State) error {
		current.OOMSeen = observed.OOMSeen
		current.OOM = observed.OOM
		current.OOMNotified = observed.OOMNotified
		current.MissingAt = observed.MissingAt
		current.MissingCheckAt = observed.MissingCheckAt
		current.MissingAttempts = observed.MissingAttempts
		current.OOMCursor = observed.OOMCursor
		current.OOMPendingAt = observed.OOMPendingAt
		current.OOMPendingCount = observed.OOMPendingCount
		current.OOMAttemptAt = observed.OOMAttemptAt
		current.OOMAttempts = observed.OOMAttempts
		return nil
	})
}

func readUnlocked(path string) (State, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return State{}, fmt.Errorf("read ceiling state %s: %w", path, err)
	}
	var state State
	if err := json.Unmarshal(b, &state); err != nil {
		return State{}, fmt.Errorf("decode ceiling state %s: %w", path, err)
	}
	return state, nil
}

func writeUnlocked(path string, state State) error {
	b, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode ceiling state: %w", err)
	}
	if err := record.WriteFileAtomic(path, b, 0o600); err != nil {
		return fmt.Errorf("write ceiling state %s: %w", path, err)
	}
	return nil
}

func openLock(path string, exclusive bool) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(filepath.Dir(path), ".ceiling.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open ceiling state lock: %w", err)
	}
	mode := syscall.LOCK_SH
	if exclusive {
		mode = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(f.Fd()), mode); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

func closeLock(f *os.File) error {
	return errors.Join(syscall.Flock(int(f.Fd()), syscall.LOCK_UN), f.Close())
}

func ParseSize(raw string, totalRAM int64) (int64, error) {
	s := strings.TrimSpace(raw)
	if before, ok := strings.CutSuffix(s, "%"); ok {
		p, err := strconv.ParseFloat(before, 64)
		if err != nil || p <= 0 || p > percentMaximum || totalRAM <= 0 || math.IsNaN(p) || math.IsInf(p, 0) {
			return 0, fmt.Errorf("invalid memory percentage %q", raw)
		}
		return int64(math.Round(float64(totalRAM) * p / 100)), nil
	}
	var n float64
	var suffix string
	if strings.ContainsAny(s, " \t\r\n") {
		return 0, fmt.Errorf("invalid memory size %q", raw)
	}
	if _, err := fmt.Sscanf(s, "%f%s", &n, &suffix); err != nil || n <= 0 || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, fmt.Errorf("invalid memory size %q", raw)
	}
	mult, ok := map[string]float64{"B": 1, "K": 1 << 10, "KB": decimalKilo, "M": 1 << 20, "MB": 1e6, "G": 1 << 30, "GB": 1e9, "T": binaryTera, "TB": 1e12, "": 1}[strings.ToUpper(suffix)]
	if !ok || n*mult > math.MaxInt64 {
		return 0, fmt.Errorf("invalid memory size %q", raw)
	}
	return int64(n * mult), nil
}

func ParseConfiguredSize(raw string) (int64, error) {
	total := int64(0)
	if strings.HasSuffix(strings.TrimSpace(raw), "%") {
		var err error
		total, err = configuredHostMemoryTotal()
		if err != nil {
			return 0, err
		}
	}
	return ParseSize(raw, total)
}

func MemoryLimits(mem ClMemory) (int64, int64, error) {
	base, err := ParseConfiguredSize(mem.Max)
	if err != nil {
		return 0, 0, fmt.Errorf("memory.max: %w", err)
	}
	swapText := mem.SwapMax
	if swapText == "" {
		swapText = DefaultSwap
	}
	swap, err := ParseSize(swapText, 0)
	if err != nil {
		return 0, 0, fmt.Errorf("memory.swap_max: %w", err)
	}
	return base, swap, nil
}

// ConfiguredBase returns the session base in bytes, whether a base is enabled,
// or an error. It validates swap exactly as cl does before enabling a limit.
func ConfiguredBase(mem ClMemory) (int64, bool, error) {
	if strings.TrimSpace(mem.Max) == "" {
		return 0, false, nil
	}
	base, _, err := MemoryLimits(mem)
	if err != nil {
		return 0, false, err
	}
	return base, true, nil
}

var configuredHostMemoryTotal = HostMemoryTotal

func HostMemoryTotal() (int64, error) {
	values, err := meminfo.Read("MemTotal:")
	if err != nil {
		return 0, err
	}
	return values["MemTotal:"], nil
}

func Target(base, shmem int64) int64 {
	n := base + shmem
	return (n + quantum - 1) / quantum * quantum
}

func MayLower(current, file, shmem, target int64) bool { return current-file+shmem < target }

func ParseInt(raw string) (int64, error) {
	s := strings.TrimSpace(raw)
	if s == "max" {
		return Unlimited, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("cgroup integer %q: invalid", s)
	}
	return n, nil
}

func CurrentMax(cgroup string) (int64, error) {
	path := filepath.Join(cgroup, "memory.max")
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read memory.max: %w", err)
	}
	s := strings.TrimSpace(string(b))
	if s == "max" {
		return Unlimited, nil
	}
	n, err := ParseInt(s)
	if err != nil {
		return 0, fmt.Errorf("parse memory.max: %w", err)
	}
	return n, nil
}

func ReadInt(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	n, err := ParseInt(string(b))
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", path, err)
	}
	return n, nil
}

func Values(raw string) (map[string]int64, error) {
	return cgroupstats.ParseKeyed(strings.NewReader(raw), nil)
}
