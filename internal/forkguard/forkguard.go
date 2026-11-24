// Package forkguard decides when agent cgroups show process avalanche growth.
package forkguard

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Unit struct {
	Name, Path              string
	Inode                   uint64
	Current, Max, EventsMax int64
}

type UnitIdentity struct {
	Name  string
	Inode uint64
	Error string
}

type Sample struct {
	At                                                    time.Time
	Units                                                 []Unit
	Skipped                                               []UnitIdentity // dirs that still exist but could not be sampled this pass
	SliceMissing                                          bool
	SliceCurrent, SliceMax, SliceEventsMax, SlicePidsPeak int64
}

type Config struct {
	Window                       time.Duration
	Hard, Floor, Rise, SliceWarn int64
}

const DefaultHard int64 = 3000
const DefaultFloor int64 = 1500

type Decision struct {
	Unit                      Unit
	Reason, Would             string
	Growth                    int64
	TreePID                   int
	TreeName, TreeUnavailable string
}
type Result struct {
	Decisions []Decision
	Lines     []string
	Summary   string
}

type point struct {
	at              time.Time
	current, events int64
}
type unitState struct {
	history                []point
	decided, wasRelevant   bool
	last                   Unit
	qualifying, riseStreak int
}
type unitKey struct {
	name  string
	inode uint64
}

type Engine struct {
	cfg                                               Config
	units                                             map[unitKey]*unitState
	sliceHistory                                      []point
	sliceWarnAt, aggregateWarnAt                      time.Time
	minute                                            time.Time
	minutePeak                                        int64
	unitPeak                                          Unit
	maxRiseName                                       string
	maxRise, maxRiseCurrent                           int64
	maxRiseStreak, count                              int
	sliceMissing, sliceKnown, seeded                  bool
	minuteSliceMax, minuteSliceEvents, minutePidsPeak int64
	readErrors                                        map[unitKey]string
}

func New(cfg Config) *Engine {
	if cfg.Window <= 0 {
		cfg.Window = 10 * time.Second
	}
	if cfg.Hard <= 0 {
		cfg.Hard = DefaultHard
	}
	if cfg.Floor <= 0 {
		cfg.Floor = DefaultFloor
	}
	if cfg.Rise <= 0 {
		cfg.Rise = 1000
	}
	if cfg.SliceWarn <= 0 {
		cfg.SliceWarn = 80
	}
	return &Engine{cfg: cfg, units: make(map[unitKey]*unitState), readErrors: make(map[unitKey]string)}
}

func (e *Engine) Observe(s Sample) Result {
	res := Result{}
	if e.units == nil {
		e.units = make(map[unitKey]*unitState)
	}
	if e.readErrors == nil {
		e.readErrors = make(map[unitKey]string)
	}
	if e.minute.IsZero() {
		e.minute = s.At.Truncate(time.Minute)
	}
	if !s.At.Truncate(time.Minute).Equal(e.minute) {
		res.Summary = e.summary()
		e.resetMinute(s.At)
	}
	e.observeSlicePresence(s, &res)
	if !s.SliceMissing {
		e.recordSlice(s)
	}
	maxUnitGrowth := e.observeUnits(s, &res)
	e.observeSliceWarnings(s, maxUnitGrowth, &res)
	return res
}

func (e *Engine) observeSlicePresence(s Sample, res *Result) {
	if s.SliceMissing {
		if !e.sliceKnown || !e.sliceMissing {
			res.Lines = append(res.Lines, "forkguard slice missing")
		}
		e.sliceKnown, e.sliceMissing = true, true
	} else {
		if e.sliceKnown && e.sliceMissing {
			res.Lines = append(res.Lines, "forkguard slice appeared")
		}
		e.sliceKnown, e.sliceMissing = true, false
	}
}

func (e *Engine) recordSlice(s Sample) {
	if s.SliceCurrent > e.minutePeak {
		e.minutePeak = s.SliceCurrent
	}
	if s.SliceMax > 0 {
		e.minuteSliceMax = s.SliceMax
	}
	if s.SliceEventsMax > e.minuteSliceEvents {
		e.minuteSliceEvents = s.SliceEventsMax
	}
	if s.SlicePidsPeak > e.minutePidsPeak {
		e.minutePidsPeak = s.SlicePidsPeak
	}
	e.sliceHistory = appendWindow(e.sliceHistory, point{at: s.At, current: s.SliceCurrent}, s.At, 2*e.cfg.Window)
}

func (e *Engine) observeUnits(s Sample, res *Result) int64 {
	seedPass := !e.seeded && !s.SliceMissing
	seen := make(map[unitKey]bool, len(s.Units)+len(s.Skipped))
	var maxUnitGrowth int64
	for _, u := range s.Units {
		key := unitKey{u.Name, u.Inode}
		seen[key] = true
		e.clearReadError(key)
		d, growth, streak := e.observeUnit(s.At, u, !seedPass && e.seeded)
		if growth > maxUnitGrowth {
			maxUnitGrowth = growth
		}
		if growth > e.maxRise {
			e.maxRise, e.maxRiseName, e.maxRiseCurrent, e.maxRiseStreak = growth, u.Name, u.Current, streak
		}
		if d != nil {
			res.Decisions = append(res.Decisions, *d)
		}
	}
	for _, skip := range s.Skipped {
		key := unitKey{skip.Name, skip.Inode}
		seen[key] = true
		if skip.Inode == 0 {
			for unitID := range e.units {
				if unitID.name == skip.Name {
					seen[unitID] = true
				}
			}
		}
		if skip.Error != "" && e.readErrors[key] != skip.Error {
			res.Lines = append(res.Lines, fmt.Sprintf("forkguard unit read failed: unit=%s error=%q", skip.Name, skip.Error))
			e.readErrors[key] = skip.Error
		}
	}
	for key, st := range e.units {
		if seen[key] {
			continue
		}
		if st.wasRelevant {
			res.Lines = append(res.Lines, fmt.Sprintf("forkguard unit disappeared: unit=%s inode=%d last_current=%d last_max=%d last_events_max=%d", key.name, key.inode, st.last.Current, st.last.Max, st.last.EventsMax))
		}
		delete(e.units, key)
		delete(e.readErrors, key)
	}
	e.count = len(s.Units) + len(s.Skipped)
	if seedPass {
		e.seeded = true
	}
	return maxUnitGrowth
}

func (e *Engine) clearReadError(key unitKey) {
	delete(e.readErrors, key)
	delete(e.readErrors, unitKey{name: key.name})
}

func (e *Engine) observeSliceWarnings(s Sample, maxUnitGrowth int64, res *Result) {
	if s.SliceMissing {
		return
	}
	if s.SliceMax > 0 && s.SliceCurrent*100 >= s.SliceMax*e.cfg.SliceWarn && (e.sliceWarnAt.IsZero() || s.At.Sub(e.sliceWarnAt) >= time.Minute) {
		res.Lines = append(res.Lines, fmt.Sprintf("forkguard slice warning: current=%d max=%d percent=%d", s.SliceCurrent, s.SliceMax, s.SliceCurrent*100/s.SliceMax))
		e.sliceWarnAt = s.At
	}
	if len(e.sliceHistory) > 1 && s.At.Sub(e.aggregateWarnAt) >= time.Minute {
		growth := s.SliceCurrent - e.sliceHistory[0].current
		if growth >= e.cfg.Rise && growth > maxUnitGrowth {
			res.Lines = append(res.Lines, fmt.Sprintf("forkguard aggregate growth without one unit: rise=%d window=%s", growth, e.cfg.Window))
			e.aggregateWarnAt = s.At
		}
	}
}

func (e *Engine) observeUnit(at time.Time, u Unit, lateUnit bool) (*Decision, int64, int) {
	key := unitKey{u.Name, u.Inode}
	st := e.units[key]
	newUnit := st == nil
	if newUnit {
		st = &unitState{}
		e.units[key] = st
	}
	prevEvents := int64(-1)
	if len(st.history) > 0 {
		prevEvents = st.history[len(st.history)-1].events
	}
	st.history = appendWindow(st.history, point{at, u.Current, u.EventsMax}, at, 2*e.cfg.Window)
	growth := int64(0)
	if len(st.history) > 1 {
		growth = max(0, u.Current-st.history[0].current)
	}
	if growth >= e.cfg.Rise {
		st.riseStreak++
	} else {
		st.riseStreak = 0
	}
	if growth >= e.cfg.Rise && u.Current >= e.cfg.Floor {
		st.qualifying++
	} else {
		st.qualifying = 0
	}
	st.last = u
	if u.Current > e.unitPeak.Current {
		e.unitPeak = u
	}
	reason := ""
	switch {
	case u.Current >= e.cfg.Hard:
		reason = "hard threshold"
	case newUnit && lateUnit && u.EventsMax > 0:
		reason = "new unit has pids.events max"
	case prevEvents >= 0 && u.EventsMax > prevEvents:
		reason = "pids.events max increased"
	case st.qualifying >= 2:
		reason = "rapid growth"
	}
	if reason == "" {
		return nil, growth, st.riseStreak
	}
	st.wasRelevant = true
	if st.decided {
		return nil, growth, st.riseStreak
	}
	st.decided = true
	return &Decision{Unit: u, Reason: reason, Would: Would(u.Name), Growth: growth}, growth, st.riseStreak
}

func appendWindow(h []point, p point, now time.Time, retention time.Duration) []point {
	i := 0
	for i < len(h) && now.Sub(h[i].at) > retention {
		i++
	}
	h = append(h[:0], h[i:]...)
	return append(h, p)
}
func (e *Engine) resetMinute(at time.Time) {
	e.minute = at.Truncate(time.Minute)
	e.minutePeak = 0
	e.unitPeak = Unit{}
	e.maxRiseName = ""
	e.maxRise = 0
	e.maxRiseCurrent = 0
	e.maxRiseStreak = 0
	e.count = 0
	e.minuteSliceMax = 0
	e.minuteSliceEvents = 0
	e.minutePidsPeak = 0
}
func (e *Engine) summary() string {
	return fmt.Sprintf("forkguard minute: slice_peak=%d slice_max=%s slice_events_max=%d slice_pids_peak=%d unit_peak=%s:%d window_rise=%s:%d current=%d rise_streak=%d units=%d", e.minutePeak, limitString(e.minuteSliceMax), e.minuteSliceEvents, e.minutePidsPeak, e.unitPeak.Name, e.unitPeak.Current, e.maxRiseName, e.maxRise, e.maxRiseCurrent, e.maxRiseStreak, e.count)
}
func limitString(n int64) string {
	if n == 0 {
		return "max"
	}
	return strconv.FormatInt(n, 10)
}

func (d Decision) LogLine() string {
	line := fmt.Sprintf("forkguard would stop: unit=%s current=%d max=%s events_max=%d rise=%d reason=%q", d.Unit.Name, d.Unit.Current, limitString(d.Unit.Max), d.Unit.EventsMax, d.Growth, d.Reason)
	if d.Would != "" {
		line += " action=" + d.Would
	}
	if d.TreePID > 0 {
		line += fmt.Sprintf(" tree_pid=%d tree_root=%s", d.TreePID, d.TreeName)
	}
	if d.TreeUnavailable != "" {
		line += fmt.Sprintf(" tree_warning=%q", d.TreeUnavailable)
	}
	return line
}

var jobRE = regexp.MustCompile(`^worker-job-(.+)\.service$`)
var roundRE = regexp.MustCompile(`^worker-round-(.+)-([0-9]+)\.service$`)

func Would(name string) string {
	decoded := unescapeUnitName(name)
	if m := jobRE.FindStringSubmatch(decoded); m != nil {
		return "worker stop " + m[1]
	}
	if m := roundRE.FindStringSubmatch(decoded); m != nil {
		return "worker stop " + m[1]
	}
	if strings.HasPrefix(name, "graft-heavy-") && strings.HasSuffix(name, ".scope") || strings.HasPrefix(name, "worker-") && strings.HasSuffix(name, "-command.scope") {
		return "systemctl --user stop " + name
	}
	return ""
}

func unescapeUnitName(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if i+3 < len(s) && s[i] == '\\' && s[i+1] == 'x' {
			v, err := strconv.ParseUint(s[i+2:i+4], 16, 8)
			if err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
