package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/activity"
	"github.com/aiseeq/aguard/internal/ceiling"
	"github.com/aiseeq/aguard/internal/config"
	"github.com/aiseeq/aguard/internal/meminfo"
	"github.com/aiseeq/akit/proc"
	"github.com/aiseeq/akit/session"
	"golang.org/x/sys/unix"
)

type ceilingState = ceiling.State

var oomMemcgRE = regexp.MustCompile(`oom_memcg=([^ ,]+)`)
var oomVictimRE = regexp.MustCompile(`Killed process ([0-9]+) \(([^)]+)\).*anon-rss:\s*([0-9]+)kB`)

type oomVictim struct {
	PID  int
	Name string
	RSS  int64
}

type ceilingOpError struct {
	kind string
	err  error
}

func (e *ceilingOpError) Error() string { return e.err.Error() }
func (e *ceilingOpError) Unwrap() error { return e.err }

type kernelOOMRecord struct {
	Message   string `json:"MESSAGE"`
	Timestamp string `json:"__REALTIME_TIMESTAMP"`
}

type ceilingSeedRetry struct {
	Attempts int
	Next     time.Time
}

const ceilingPIDRetention = 30 * time.Second

var errCeilingUnavailable = errors.New("потолка нет")

func parseCeilingOOMJSON(raw, unit string, after time.Time) ([]oomVictim, time.Time, error) {
	return parseCeilingOOMJSONForPIDs(raw, unit, after, nil)
}

func parseCeilingOOMJSONForPIDs(raw, unit string, after time.Time, pids map[int]bool) ([]oomVictim, time.Time, error) {
	var victims []oomVictim
	var matched bool
	var latest time.Time
	for line := range strings.SplitSeq(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		var row kernelOOMRecord
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, latest, fmt.Errorf("journalctl JSON: %w", err)
		}
		micros, err := strconv.ParseInt(row.Timestamp, 10, 64)
		if err != nil {
			return nil, latest, fmt.Errorf("journal timestamp %q: %w", row.Timestamp, err)
		}
		at := time.UnixMicro(micros)
		if m := oomMemcgRE.FindStringSubmatch(row.Message); m != nil {
			matched = strings.HasSuffix(m[1], "/"+unit)
			continue
		}
		m := oomVictimRE.FindStringSubmatch(row.Message)
		if m == nil {
			if strings.Contains(row.Message, "Killed process") {
				return nil, latest, fmt.Errorf("journal victim line malformed: %q", row.Message)
			}
			continue
		}
		pid, pidErr := strconv.Atoi(m[1])
		rss, rssErr := strconv.ParseInt(m[3], 10, 64)
		if pidErr != nil || rssErr != nil {
			return nil, latest, fmt.Errorf("journal victim fields: %w", errors.Join(pidErr, rssErr))
		}
		if at.After(after) && (matched || pids[pid]) {
			victims = append(victims, oomVictim{PID: pid, Name: m[2], RSS: rss * 1024})
			if at.After(latest) {
				latest = at
			}
		}
	}
	return victims, latest, nil
}

func (a *app) ceilingCmd(args []string) error {
	if len(args) > 1 {
		return fmt.Errorf("aguard memory ceiling [размер]")
	}
	name, unit, root, statePath, st, err := a.ceilingCommandState()
	if errors.Is(err, errCeilingUnavailable) {
		fmt.Fprintln(a.out, "потолка нет")
		return nil
	}
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return a.ceilingShow(unit, root, st)
	}
	return a.ceilingSet(name, unit, root, statePath, st, args[0])
}

func (a *app) ceilingCommandState() (string, string, string, string, ceiling.State, error) {
	cg, err := os.ReadFile(filepath.Join(a.ceilingProcRoot(), "self/cgroup"))
	if err != nil {
		a.log.Info("aguard memory ceiling: cgroup процесса недоступен", "err", err)
		return "", "", "", "", ceiling.State{}, errCeilingUnavailable
	}
	var cgroup string
	for line := range strings.SplitSeq(string(cg), "\n") {
		if after, ok := strings.CutPrefix(line, "0::"); ok {
			cgroup = after
		}
	}
	name, _, ok := proc.SessionScope(cgroup)
	if !ok {
		return "", "", "", "", ceiling.State{}, errCeilingUnavailable
	}
	unit := filepath.Base(cgroup)
	statePath := a.ceilingStatePath(unit)
	st, err := ceiling.Read(statePath)
	if errors.Is(err, os.ErrNotExist) {
		return "", "", "", "", ceiling.State{}, errCeilingUnavailable
	}
	if err != nil {
		return "", "", "", "", ceiling.State{}, err
	}
	root := filepath.Join(a.ceilingCgroupRoot(), strings.TrimPrefix(cgroup, "/"))
	max, err := ceiling.CurrentMax(root)
	if err != nil {
		return "", "", "", "", ceiling.State{}, err
	}
	if max == ceiling.Unlimited {
		return "", "", "", "", ceiling.State{}, errCeilingUnavailable
	}
	return name, unit, root, statePath, st, nil
}

func (a *app) ceilingShow(unit, root string, st ceiling.State) error {
	vals, err := readCeilingValues(filepath.Join(root, "memory.stat"))
	if err != nil {
		return err
	}
	max, err := ceiling.CurrentMax(root)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.out, "unit %s, base %s, swap %s, MemoryMax %s, shmem %s\n", unit, formatBytes(st.Base), formatBytes(st.Swap), formatLimit(max), formatBytes(vals["shmem"]))
	return nil
}

func (a *app) ceilingSet(name, unit, root, statePath string, st ceiling.State, size string) error {
	newBase, err := ceiling.ParseConfiguredSize(size)
	if err != nil {
		return err
	}
	available, availableErr := availableMemory()
	if availableErr != nil {
		fmt.Fprintln(a.out, "подсказка: доступную память прочитать не удалось:", availableErr)
	} else if newBase > available {
		fmt.Fprintf(a.out, "подсказка: размер больше доступной памяти (%s)\n", formatBytes(available))
	}
	vals, err := readCeilingValues(filepath.Join(root, "memory.stat"))
	if err != nil {
		return err
	}
	current, err := ceiling.CurrentMax(root)
	if err != nil {
		return err
	}
	previous := st
	st, err = ceiling.Update(statePath, func(currentState *ceiling.State) error {
		currentState.Base, currentState.By, currentState.At = newBase, name, time.Now()
		return nil
	})
	if err != nil {
		return err
	}
	target := ceiling.Target(newBase, vals["shmem"])
	if target > current {
		if err := ceilingSetProperty(unit, target); err != nil {
			_, rollbackErr := ceiling.Update(statePath, func(currentState *ceiling.State) error {
				currentState.Base, currentState.By, currentState.At = previous.Base, previous.By, previous.At
				return nil
			})
			return errors.Join(err, rollbackErr)
		}
	} else if newBase < previous.Base {
		fmt.Fprintln(a.out, "снижение выполнит наблюдатель, когда memory.current − file + shmem станет ниже цели")
	}
	return nil
}

func (a *app) ceilingProcRoot() string {
	if a.procDir != "" {
		return a.procDir
	}
	return procRoot
}
func (a *app) ceilingCgroupRoot() string {
	if a.cgroupDir != "" {
		return a.cgroupDir
	}
	return "/sys/fs/cgroup"
}
func (a *app) ceilingStatePath(unit string) string { return ceiling.StatePath(a.home, unit) }

func formatBytes(n int64) string {
	const mib = int64(1 << 20)
	const gib = int64(1 << 30)
	if n >= gib {
		return fmt.Sprintf("%.2f ГиБ", float64(n)/float64(gib))
	}
	return fmt.Sprintf("%.0f МиБ", float64(n)/float64(mib))
}
func formatLimit(n int64) string {
	if n == ceiling.Unlimited {
		return "max"
	}
	return formatBytes(n)
}
func availableMemory() (int64, error) {
	values, err := meminfo.Read("MemAvailable:")
	if err != nil {
		return 0, err
	}
	return values["MemAvailable:"], nil
}

func readCeilingValues(path string) (map[string]int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	v, err := ceiling.Values(string(b))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return v, nil
}
func setCeiling(unit string, n int64) error {
	cmd := exec.Command("systemctl", "--user", "set-property", "--runtime", unit, "MemoryMax="+strconv.FormatInt(n, 10))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl set-property: %w: %s", err, out)
	}
	return nil
}

var ceilingSetProperty = setCeiling

func (a *app) ceilingLoop(ctx context.Context) {
	const configEvery = 10 * time.Second
	cfgNext, nextNote := time.Time{}, time.Time{}
	cfgValid := false
	var cfg config.Config
	for ctx.Err() == nil {
		now := time.Now()
		if cfgNext.IsZero() || !now.Before(cfgNext) {
			loaded, err := a.config()
			cfgNext = now.Add(configEvery)
			if err != nil {
				cfgValid = false
				a.log.Error("aguard ceiling: конфиг", "err", err)
				a.note("ceiling", activity.KindConfigSkipped, "", "", 0, "")
			} else {
				cfg = loaded
				cfgValid = true
			}
		}
		if cfgValid {
			if err := a.ceilingRound(ctx, cfg.Memory.Slice); err != nil && ctx.Err() == nil {
				a.log.Error("aguard ceiling: проход", "err", err)
				a.note("ceiling", activity.KindPassFailed, "", "", 0, "")
			}
		}
		var due bool
		due, nextNote = ceilingPassNext(now, nextNote)
		if due {
			a.note("ceiling", activity.KindPass, "", "", 0, "")
		}
		pause := 200 * time.Millisecond
		if cfgValid {
			pause = cfg.Ceiling.Interval.D()
		}
		if pause <= 0 {
			pause = 200 * time.Millisecond
		}
		if cfgValid {
			a.waitCeiling(ctx, pause, cfg.Memory.Slice)
		} else {
			waitCeilingContext(ctx, pause)
		}
		if ctx.Err() != nil {
			return
		}
	}
}

func (a *app) ceilingRound(ctx context.Context, slice string) error {
	dir := filepath.Dir(a.ceilingStatePath("x"))
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read ceiling states %s: %w", dir, err)
	}
	alive, aliveErr := a.ceilingSessions(time.Now())
	if aliveErr != nil {
		a.ceilingFailure("registry_read_failed", "", aliveErr)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		path := filepath.Join(dir, entry.Name())
		st, e := ceiling.Read(path)
		if e != nil {
			a.ceilingFailure("read_failed", entry.Name(), e)
			continue
		}
		root := filepath.Join(slice, st.Unit)
		if _, e = os.Stat(root); errors.Is(e, os.ErrNotExist) {
			a.ceilingGone(ctx, path, st)
			continue
		} else if e != nil {
			a.ceilingFailure("read_failed", st.Unit, e)
			continue
		}
		if e = a.ceilingScope(ctx, path, root, &st, alive, aliveErr); e != nil {
			kind := "read_failed"
			var op *ceilingOpError
			if errors.As(e, &op) {
				kind = op.kind
			}
			a.ceilingFailure(kind, st.Unit, e)
		}
	}
	return nil
}

func clearCeilingMissing(path string, st *ceiling.State) error {
	if st.MissingAt.IsZero() {
		return nil
	}
	st.MissingAt, st.MissingCheckAt, st.MissingAttempts = time.Time{}, time.Time{}, 0
	updated, err := ceiling.SaveObserved(path, *st)
	if err == nil {
		*st = updated
	}
	return err
}

func (a *app) ceilingScope(ctx context.Context, statePath, root string, st *ceiling.State, alive map[string]session.Info, aliveErr error) error {
	if err := clearCeilingMissing(statePath, st); err != nil {
		return &ceilingOpError{kind: "state_write_failed", err: err}
	}
	name, _, ok := proc.ScopeName(st.Unit)
	if ok && aliveErr == nil {
		if info, exists := alive[name]; exists {
			st.ClaudePID = info.Pid
		}
	}
	stat, err := readCeilingValues(filepath.Join(root, "memory.stat"))
	if err != nil {
		return err
	}
	current, err := ceiling.ReadInt(filepath.Join(root, "memory.current"))
	if err != nil {
		return err
	}
	maxNow, err := ceiling.CurrentMax(root)
	if err != nil {
		return err
	}
	if maxNow == ceiling.Unlimited {
		return nil
	}
	if err := a.ceilingObserveOOM(ctx, statePath, root, st, maxNow, stat["shmem"], alive, aliveErr); err != nil {
		return err
	}
	want := ceiling.Target(st.Base, stat["shmem"])
	nextMax := maxNow
	if want > maxNow || want < maxNow && ceiling.MayLower(current, stat["file"], stat["shmem"], want) {
		if err = ceilingSetProperty(st.Unit, want); err != nil {
			return &ceilingOpError{kind: "set_failed", err: err}
		}
		nextMax = want
		a.note("ceiling", "changed", fmt.Sprintf("%s by=%s was=%d shmem=%d", st.Unit, st.By, maxNow, stat["shmem"]), "", want, "bytes")
	}
	if a.ceilingLimits == nil {
		a.ceilingLimits = make(map[string]int64)
		a.ceilingShmem = make(map[string]int64)
	}
	a.ceilingLimits[st.Unit], a.ceilingShmem[st.Unit] = nextMax, stat["shmem"]
	if a.ceilingUnpersisted[st.Unit] {
		return nil
	}
	updated, err := ceiling.SaveObserved(statePath, *st)
	if err != nil {
		return &ceilingOpError{kind: "state_write_failed", err: err}
	}
	*st = updated
	return nil
}

func (a *app) ceilingObserveOOM(ctx context.Context, statePath, root string, st *ceiling.State, maxNow, shmem int64, alive map[string]session.Info, aliveErr error) error {
	events, err := readCeilingValues(filepath.Join(root, "memory.events"))
	if err != nil {
		return err
	}
	currentPIDs, err := scopePIDs(root)
	if err != nil {
		return err
	}
	if a.ceilingPIDs == nil {
		a.ceilingPIDs = make(map[string]map[int]time.Time)
	}
	pids := a.ceilingRememberPIDs(st.Unit, currentPIDs, time.Now())
	return a.ceilingOOMEvents(ctx, statePath, st, events["oom_kill"], maxNow, shmem, pids, alive, aliveErr)
}

func (a *app) ceilingRememberPIDs(unit string, current []int, now time.Time) map[int]bool {
	if a.ceilingPIDs == nil {
		a.ceilingPIDs = make(map[string]map[int]time.Time)
	}
	seen := a.ceilingPIDs[unit]
	if seen == nil {
		seen = make(map[int]time.Time)
		a.ceilingPIDs[unit] = seen
	}
	for pid, at := range seen {
		if now.Sub(at) > ceilingPIDRetention {
			delete(seen, pid)
		}
	}
	for _, pid := range current {
		seen[pid] = now
	}
	set := make(map[int]bool, len(seen))
	for pid := range seen {
		set[pid] = true
	}
	return set
}

func (a *app) ceilingOOMEvents(ctx context.Context, statePath string, st *ceiling.State, eventCount, maxNow, shmem int64, pids map[int]bool, alive map[string]session.Info, aliveErr error) error {
	if !st.OOMSeen {
		seeded, err := a.ceilingSeedOOM(ctx, st, eventCount, pids)
		if err != nil {
			return err
		}
		if seeded {
			return nil
		}
	}
	if st.OOMNotified == 0 && st.OOM > 0 {
		st.OOMNotified = st.OOM
	}
	notified := max(st.OOMNotified, a.ceilingNotified[st.Unit])
	delta := eventCount - notified
	if delta <= 0 {
		if eventCount > st.OOM {
			st.OOM = eventCount
		}
		return nil
	}
	if a.ceilingUnpersisted != nil {
		a.ceilingUnpersisted[st.Unit] = false
	}
	if st.OOMPendingCount != eventCount || st.OOMPendingAt.IsZero() {
		st.OOMPendingAt, st.OOMPendingCount = time.Now(), eventCount
		updated, err := ceiling.SaveObserved(statePath, *st)
		if err != nil {
			return &ceilingOpError{kind: "state_write_failed", err: err}
		}
		*st = updated
		return nil
	}
	if st.OOMAttempts > 0 && time.Since(st.OOMAttemptAt) < time.Second {
		return nil
	}
	victims, cursor, journalErr := a.ceilingJournalCursor(ctx, st.Unit, st.OOMCursor, time.Time{}, pids)
	if journalErr != nil {
		a.ceilingFailure("read_failed", st.Unit, journalErr)
		victims = nil
	}
	if int64(len(victims)) < delta && time.Since(st.OOMPendingAt) < 2*time.Second {
		return nil
	}
	return a.ceilingDeliverOOM(statePath, st, victims, cursor, eventCount, delta, maxNow, shmem, alive, aliveErr)
}

func (a *app) ceilingSeedOOM(ctx context.Context, st *ceiling.State, eventCount int64, pids map[int]bool) (bool, error) {
	if a.ceilingBaseline == nil {
		a.ceilingBaseline = make(map[string]int64)
	}
	if a.ceilingCursors == nil {
		a.ceilingCursors = make(map[string]string)
	}
	if a.ceilingUnpersisted == nil {
		a.ceilingUnpersisted = make(map[string]bool)
	}
	if a.ceilingSeedRetries == nil {
		a.ceilingSeedRetries = make(map[string]ceilingSeedRetry)
	}
	base, exists := a.ceilingBaseline[st.Unit]
	if !exists {
		base = eventCount
		a.ceilingBaseline[st.Unit] = base
		a.ceilingUnpersisted[st.Unit] = true
	}
	cursor, cursorReady := a.ceilingCursors[st.Unit]
	if !cursorReady {
		retry := a.ceilingSeedRetries[st.Unit]
		if !time.Now().Before(retry.Next) {
			_, foundCursor, err := a.ceilingJournalCursor(ctx, st.Unit, "", time.Time{}, pids)
			if err != nil {
				retry.Attempts++
				retry.Next = time.Now().Add(time.Second << min(retry.Attempts-1, 5))
				a.ceilingSeedRetries[st.Unit] = retry
				a.ceilingFailure("read_failed", st.Unit, err)
			} else {
				cursor = foundCursor
				a.ceilingCursors[st.Unit] = cursor
				delete(a.ceilingSeedRetries, st.Unit)
			}
		}
	}
	st.OOMSeen, st.OOM, st.OOMNotified, st.OOMCursor = true, base, base, cursor
	return eventCount == base, nil
}

func (a *app) ceilingDeliverOOM(statePath string, st *ceiling.State, victims []oomVictim, cursor string, eventCount, delta, maxNow, shmem int64, alive map[string]session.Info, aliveErr error) error {
	if err := a.sendCeilingOOMNotice(*st, victims, delta, maxNow, shmem, alive, aliveErr); err != nil {
		return a.ceilingOOMSendFailed(statePath, st, err, eventCount, delta, cursor)
	}
	st.OOM, st.OOMNotified = eventCount, eventCount
	st.OOMCursor = cursor
	st.OOMPendingAt, st.OOMPendingCount = time.Time{}, 0
	st.OOMAttempts, st.OOMAttemptAt = 0, time.Time{}
	if a.ceilingNotified == nil {
		a.ceilingNotified = make(map[string]int64)
	}
	a.ceilingNotified[st.Unit] = eventCount
	updated, err := ceiling.SaveObserved(statePath, *st)
	if err != nil {
		return &ceilingOpError{kind: "state_write_failed", err: err}
	}
	*st = updated
	return nil
}

func (a *app) ceilingOOMSendFailed(statePath string, st *ceiling.State, sendErr error, eventCount, delta int64, cursor string) error {
	st.OOMAttempts++
	st.OOMAttemptAt = time.Now()
	a.note("ceiling", "send_failed", st.Unit, sendErr.Error(), delta, "processes")
	if st.OOMAttempts >= 5 {
		st.OOM, st.OOMNotified, st.OOMCursor = eventCount, eventCount, cursor
		st.OOMPendingAt, st.OOMPendingCount = time.Time{}, 0
		if a.ceilingNotified == nil {
			a.ceilingNotified = make(map[string]int64)
		}
		a.ceilingNotified[st.Unit] = eventCount
	}
	updated, err := ceiling.SaveObserved(statePath, *st)
	if err == nil {
		*st = updated
	}
	return err
}

func scopePIDs(root string) ([]int, error) {
	data, err := os.ReadFile(filepath.Join(root, "cgroup.procs"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read cgroup.procs: %w", err)
	}
	var pids []int
	for field := range strings.FieldsSeq(string(data)) {
		pid, err := strconv.Atoi(field)
		if err != nil {
			return nil, fmt.Errorf("parse cgroup.procs pid %q: %w", field, err)
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

func (a *app) ceilingSessions(now time.Time) (map[string]session.Info, error) {
	if !a.ceilingAliveAt.IsZero() && now.Sub(a.ceilingAliveAt) < 2*time.Second {
		return a.ceilingAlive, a.ceilingAliveErr
	}
	a.ceilingAlive, a.ceilingAliveErr = a.reg.Alive()
	a.ceilingAliveAt = now
	return a.ceilingAlive, a.ceilingAliveErr
}

func (a *app) ceilingJournalCursor(ctx context.Context, unit, cursor string, after time.Time, pids map[int]bool) ([]oomVictim, string, error) {
	if a.ceilingJournalSource != nil {
		victims, _, err := a.ceilingJournalSource(ctx, unit, after)
		return victims, cursor, err
	}
	args := []string{"-k"}
	if cursor != "" {
		args = append(args, "--after-cursor="+cursor)
	} else {
		args = append(args, "-n", "1")
	}
	args = append(args, "--show-cursor", "--no-pager", "-o", "json")
	out, err := exec.CommandContext(ctx, "journalctl", args...).Output()
	if err != nil {
		return nil, cursor, fmt.Errorf("journalctl -k: %w", err)
	}
	text := string(out)
	var next string
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if after0, ok := strings.CutPrefix(line, "-- cursor: "); ok {
			next = after0
			lines = lines[:i]
			break
		}
	}
	victims, _, err := parseCeilingOOMJSONForPIDs(strings.Join(lines, "\n"), unit, time.Time{}, pids)
	if err != nil {
		return nil, cursor, err
	}
	if next == "" {
		next = cursor
	}
	return victims, next, nil
}

func (a *app) ceilingGone(ctx context.Context, path string, st ceiling.State) {
	now := time.Now()
	runtimeMax, runtimeShmem := a.ceilingLimits[st.Unit], a.ceilingShmem[st.Unit]
	runtimePIDs := slices.Sorted(maps.Keys(a.ceilingPIDs[st.Unit]))
	st, due, err := prepareCeilingGone(path, st, now)
	if err != nil {
		a.ceilingFailure("state_write_failed", st.Unit, err)
		return
	}
	if !due {
		return
	}
	if err := a.ceilingGoneNotify(ctx, path, &st, runtimePIDs, runtimeMax, runtimeShmem, now); err != nil {
		a.ceilingFailure("state_write_failed", st.Unit, err)
	}
	const missingGrace = 2 * time.Minute
	const missingMaxAttempts = 5
	if now.Sub(st.MissingAt) >= missingGrace || st.MissingAttempts >= missingMaxAttempts {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			a.ceilingFailure("state_remove_failed", st.Unit, err)
		} else {
			a.ceilingForgetUnit(st.Unit)
		}
	}
}

func (a *app) ceilingForgetUnit(unit string) {
	delete(a.ceilingPIDs, unit)
	delete(a.ceilingBaseline, unit)
	delete(a.ceilingCursors, unit)
	delete(a.ceilingLimits, unit)
	delete(a.ceilingShmem, unit)
	delete(a.ceilingNotified, unit)
	delete(a.ceilingUnpersisted, unit)
	delete(a.ceilingSeedRetries, unit)
}

func (a *app) ceilingGoneNotify(ctx context.Context, path string, st *ceiling.State, runtimePIDs []int, maxNow, shmem int64, now time.Time) error {
	pids := pidSet(runtimePIDs)
	for pid := range a.ceilingPIDs[st.Unit] {
		pids[pid] = true
	}
	victims, cursor, err := a.ceilingJournalCursor(ctx, st.Unit, st.OOMCursor, time.Time{}, pids)
	if err != nil {
		a.ceilingFailure("read_failed", st.Unit, err)
		victims = nil
	}
	count := max(st.OOM, st.OOMPendingCount) - max(st.OOMNotified, a.ceilingNotified[st.Unit])
	if count <= 0 || int64(len(victims)) < count && time.Since(st.OOMPendingAt) < 2*time.Second {
		return nil
	}
	if st.OOMAttempts > 0 && time.Since(st.OOMAttemptAt) < time.Second {
		return nil
	}
	st.OOMCursor, st.OOM = cursor, max(st.OOM, st.OOMPendingCount)
	if err := a.ceilingGoneNotice(path, st, victims, count, maxNow, shmem, now); err != nil {
		return a.ceilingGoneSendFailed(path, st, err, count)
	}
	st.OOMPendingAt, st.OOMPendingCount = time.Time{}, 0
	st.OOMAttempts, st.OOMAttemptAt = 0, time.Time{}
	_, err = ceiling.SaveObserved(path, *st)
	return err
}

func (a *app) ceilingGoneSendFailed(path string, st *ceiling.State, sendErr error, count int64) error {
	st.OOMAttempts++
	st.OOMAttemptAt = time.Now()
	a.note("ceiling", "send_failed", st.Unit, sendErr.Error(), count, "processes")
	if st.OOMAttempts >= 5 {
		st.OOMNotified = st.OOMPendingCount
		st.OOMPendingAt, st.OOMPendingCount = time.Time{}, 0
	}
	_, err := ceiling.SaveObserved(path, *st)
	return err
}

func prepareCeilingGone(path string, st ceiling.State, now time.Time) (ceiling.State, bool, error) {
	if st.MissingAt.IsZero() {
		st.MissingAt = now
	}
	if !st.MissingCheckAt.IsZero() && now.Sub(st.MissingCheckAt) < time.Second {
		return st, false, nil
	}
	st.MissingCheckAt = now
	st.MissingAttempts++
	updated, err := ceiling.SaveObserved(path, st)
	return updated, err == nil, err
}

func (a *app) ceilingGoneNotice(path string, st *ceiling.State, victims []oomVictim, count, maxNow, shmem int64, now time.Time) error {
	if len(victims) == 0 && count <= 0 {
		return nil
	}
	alive, aliveErr := a.ceilingSessions(now)
	if aliveErr != nil {
		a.ceilingFailure("registry_read_failed", st.Unit, aliveErr)
		alive, aliveErr = map[string]session.Info{}, nil
	}
	if err := a.sendCeilingOOMNotice(*st, victims, count, maxNow, shmem, alive, aliveErr); err != nil {
		return err
	}
	st.OOMNotified = st.OOM
	if a.ceilingNotified == nil {
		a.ceilingNotified = make(map[string]int64)
	}
	a.ceilingNotified[st.Unit] = st.OOM
	updated, err := ceiling.SaveObserved(path, *st)
	if err == nil {
		*st = updated
	}
	return err
}

func pidSet(pids []int) map[int]bool {
	set := make(map[int]bool, len(pids))
	for _, pid := range pids {
		set[pid] = true
	}
	return set
}

func (a *app) ceilingFailure(kind, unit string, err error) {
	a.log.Error("aguard ceiling: scope", "unit", unit, "err", err)
	a.note("ceiling", kind, unit, "", 1, "error")
}

func (a *app) waitCeiling(ctx context.Context, pause time.Duration, slice string) {
	entries, err := os.ReadDir(filepath.Dir(a.ceilingStatePath("x")))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			a.ceilingFailure("read_failed", "", err)
		}
		waitCeilingContext(ctx, pause)
		return
	}
	var files []*os.File
	var polls []unix.PollFd
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		st, e := ceiling.Read(filepath.Join(filepath.Dir(a.ceilingStatePath("x")), entry.Name()))
		if e != nil {
			a.ceilingFailure("read_failed", entry.Name(), e)
			continue
		}
		f, e := os.Open(filepath.Join(slice, st.Unit, "memory.events"))
		if e != nil {
			if !errors.Is(e, os.ErrNotExist) {
				a.ceilingFailure("poll_failed", st.Unit, e)
			}
			continue
		}
		if _, e = io.Copy(io.Discard, f); e != nil {
			a.ceilingFailure("poll_failed", st.Unit, e)
			_ = f.Close()
			continue
		}
		if _, e = f.Seek(0, 0); e != nil {
			a.ceilingFailure("poll_failed", st.Unit, e)
			_ = f.Close()
			continue
		}
		files = append(files, f)
		polls = append(polls, unix.PollFd{Fd: int32(f.Fd()), Events: unix.POLLPRI})
	}
	if len(polls) == 0 {
		waitCeilingContext(ctx, pause)
		return
	}
	ms := max(int(math.Ceil(float64(pause)/float64(time.Millisecond))), 1)
	for ctx.Err() == nil {
		_, e := unix.Poll(polls, ms)
		if e == unix.EINTR {
			continue
		}
		if e != nil {
			a.ceilingFailure("poll_failed", "", e)
		}
		break
	}
}
func waitCeilingContext(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func (a *app) sendCeilingOOMNotice(st ceiling.State, victims []oomVictim, count, current, shmem int64, alive map[string]session.Info, aliveErr error) error {
	if a.ceilingNoticeSend != nil {
		return a.ceilingNoticeSend(st, victims, count, current, shmem, alive, aliveErr)
	}
	return a.ceilingOOMNotice(st, victims, count, current, shmem, alive, aliveErr)
}

func (a *app) ceilingOOMNotice(st ceiling.State, victims []oomVictim, count, current, shmem int64, alive map[string]session.Info, aliveErr error) error {
	name, _, ok := proc.ScopeName(st.Unit)
	if !ok {
		return fmt.Errorf("scope name %q is not a session", st.Unit)
	}
	cfg, err := a.config()
	if err != nil {
		return fmt.Errorf("read config for OOM notice: %w", err)
	}
	if aliveErr != nil {
		return fmt.Errorf("read live session registry: %w", aliveErr)
	}
	to, claudeVictim := ceilingOOMRoute(name, victims, alive, st.ClaudePID)
	type topVictim struct {
		name  string
		rss   int64
		count int
	}
	byName := map[string]*topVictim{}
	for _, v := range victims {
		t := byName[v.Name]
		if t == nil {
			t = &topVictim{name: v.Name}
			byName[v.Name] = t
		}
		t.rss += v.RSS
		t.count++
	}
	ordered := make([]topVictim, 0, len(byName))
	for _, v := range byName {
		ordered = append(ordered, *v)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].rss != ordered[j].rss {
			return ordered[i].rss > ordered[j].rss
		}
		return ordered[i].name < ordered[j].name
	})
	var top []string
	for _, v := range ordered[:min(3, len(ordered))] {
		top = append(top, fmt.Sprintf("%s ×%d, суммарный RSS %s", v.name, v.count, formatBytes(v.rss)))
	}
	countText := strconv.FormatInt(count, 10)
	if count < 0 {
		countText = "неизвестно: scope исчез до чтения memory.events"
	}
	incomplete := count < 0 || int64(len(victims)) < count
	if len(top) == 0 {
		top = append(top, "имена не все: ядро прорядило журнал")
	}
	if incomplete && len(top) > 0 && !strings.Contains(strings.Join(top, "; "), "имена не все") {
		top = append(top, "имена не все: ядро прорядило журнал")
	}
	leadLine := ""
	if claudeVictim {
		to = cfg.Lead
		leadLine = fmt.Sprintf(" Снят сам claude сессии %s.", name)
	} else if _, aliveNow := alive[name]; !aliveNow {
		to = cfg.Lead
		leadLine = fmt.Sprintf(" Сессия %s уже завершилась.", name)
	}
	msg := fmt.Sprintf("aguard: потолок памяти сессии %s (%s) снял процессов по memory.events: %s; крупнейшие: %s. MemoryMax %s, shmem %s. Поднять: aguard memory ceiling <N> или graft exec --heavy --memory-max <N>.%s", name, st.Unit, countText, strings.Join(top, "; "), formatBytes(current), formatBytes(shmem), leadLine)
	if err := a.sendUrgent(alive, to, cfg.SendAuth, "ceiling.oom", msg); err != nil {
		return fmt.Errorf("send OOM notice to %s: %w", to, err)
	}
	a.note("ceiling", "oom_notice", st.Unit, to, count, "processes")
	return nil
}

func ceilingOOMRoute(sessionName string, victims []oomVictim, alive map[string]session.Info, claudePID int) (string, bool) {
	if claudePID == 0 {
		claudePID = alive[sessionName].Pid
	}
	for _, victim := range victims {
		if claudePID > 0 && victim.PID == claudePID {
			return "lead", true
		}
	}
	if _, ok := alive[sessionName]; !ok {
		return "lead", false
	}
	return sessionName, false
}

func ceilingPassNext(now, next time.Time) (bool, time.Time) {
	if !next.IsZero() && now.Before(next) {
		return false, next
	}
	return true, now.Add(10 * time.Minute)
}
