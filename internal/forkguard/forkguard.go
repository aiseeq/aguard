// Package forkguard decides when agent cgroups show process avalanche growth.
package forkguard

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

type Unit struct {
	Name, Path              string
	Current, Max, EventsMax int64
}

type Sample struct {
	At                     time.Time
	Units                  []Unit
	SliceCurrent, SliceMax int64
}

type Config struct {
	Window                       time.Duration
	Hard, Floor, Rise, SliceWarn int64
}

const DefaultHard int64 = 3000
const DefaultFloor int64 = 1500

type Decision struct {
	Unit            Unit
	Reason          string
	Would           string
	TreePID         int
	TreeName        string
	TreeUnavailable string
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
	history     []point
	decided     bool
	last        Unit
	wasRelevant bool
	qualifying  int
}
type Engine struct {
	cfg          Config
	units        map[string]*unitState
	sliceHistory []point
	sliceWarnAt  time.Time
	minute       time.Time
	minutePeak   int64
	unitPeak     Unit
	maxRiseName  string
	maxRise      int64
	count        int
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
	return &Engine{cfg: cfg, units: make(map[string]*unitState)}
}

func (e *Engine) Observe(s Sample) Result {
	if e.units == nil {
		e.units = make(map[string]*unitState)
	}
	res := Result{}
	seen := make(map[string]bool, len(s.Units))
	var unitFlag bool
	if e.minute.IsZero() {
		e.minute = s.At.Truncate(time.Minute)
	}
	if !s.At.Truncate(time.Minute).Equal(e.minute) {
		res.Summary = e.summary()
		e.resetMinute(s.At)
	}
	if s.SliceCurrent > e.minutePeak {
		e.minutePeak = s.SliceCurrent
	}
	e.sliceHistory = appendWindow(e.sliceHistory, point{s.At, s.SliceCurrent, 0}, s.At, e.cfg.Window)
	for _, u := range s.Units {
		seen[u.Name] = true
		d, flagged, growth := e.observeUnit(s.At, u)
		unitFlag = unitFlag || flagged
		if growth > e.maxRise {
			e.maxRise = growth
			e.maxRiseName = u.Name
		}
		if d != nil {
			res.Decisions = append(res.Decisions, *d)
		}
	}
	for name, st := range e.units {
		if seen[name] {
			continue
		}
		if st.wasRelevant {
			res.Lines = append(res.Lines, fmt.Sprintf("forkguard unit disappeared: unit=%s last_current=%d last_max=%d last_events_max=%d", name, st.last.Current, st.last.Max, st.last.EventsMax))
		}
		delete(e.units, name)
	}
	if s.SliceMax > 0 && s.SliceCurrent*100 >= s.SliceMax*e.cfg.SliceWarn && (e.sliceWarnAt.IsZero() || s.At.Sub(e.sliceWarnAt) >= time.Minute) {
		res.Lines = append(res.Lines, fmt.Sprintf("forkguard slice warning: current=%d max=%d percent=%d", s.SliceCurrent, s.SliceMax, s.SliceCurrent*100/s.SliceMax))
		e.sliceWarnAt = s.At
	}
	if len(e.sliceHistory) > 1 && !unitFlag {
		growth := s.SliceCurrent - e.sliceHistory[0].current
		if growth >= e.cfg.Rise {
			res.Lines = append(res.Lines, fmt.Sprintf("forkguard aggregate growth without one unit: rise=%d window=%s", growth, e.cfg.Window))
		}
	}
	e.count = len(s.Units)
	return res
}

func (e *Engine) observeUnit(at time.Time, u Unit) (*Decision, bool, int64) {
	st := e.units[u.Name]
	if st == nil {
		st = &unitState{}
		e.units[u.Name] = st
	}
	previousEvents := int64(-1)
	if len(st.history) > 0 {
		previousEvents = st.history[len(st.history)-1].events
	}
	st.history = appendWindow(st.history, point{at, u.Current, u.EventsMax}, at, e.cfg.Window)
	growth := int64(0)
	if len(st.history) > 1 {
		growth = max(0, u.Current-st.history[0].current)
	}
	st.last = u
	reason := ""
	switch {
	case u.Current >= e.cfg.Hard:
		reason = "hard threshold"
	case previousEvents >= 0 && u.EventsMax > previousEvents:
		reason = "pids.events max increased"
	case growth >= e.cfg.Rise && u.Current >= e.cfg.Floor:
		st.qualifying++
		if st.qualifying >= 2 {
			reason = "rapid growth"
		}
	default:
		st.qualifying = 0
	}
	if u.Current > e.unitPeak.Current {
		e.unitPeak = u
	}
	if reason == "" {
		return nil, false, growth
	}
	st.wasRelevant = true
	if st.decided {
		return nil, true, growth
	}
	st.decided = true
	return &Decision{Unit: u, Reason: reason, Would: Would(u.Name)}, true, growth
}

func appendWindow(h []point, p point, now time.Time, w time.Duration) []point {
	i := 0
	for i < len(h) && now.Sub(h[i].at) > w {
		i++
	}
	h = append(h[:0], h[i:]...)
	return append(h, p)
}
func (e *Engine) resetMinute(at time.Time) {
	e.minute = at.Truncate(time.Minute)
	e.minutePeak = 0
	e.unitPeak = Unit{}
	e.maxRise = 0
	e.maxRiseName = ""
	e.count = 0
}
func (e *Engine) summary() string {
	return fmt.Sprintf("forkguard minute: slice_peak=%d unit_peak=%s:%d window_rise=%s:%d units=%d", e.minutePeak, e.unitPeak.Name, e.unitPeak.Current, e.maxRiseName, e.maxRise, e.count)
}
func (d Decision) LogLine() string {
	line := fmt.Sprintf("forkguard would stop: unit=%s current=%d reason=%q", d.Unit.Name, d.Unit.Current, d.Reason)
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
	if m := jobRE.FindStringSubmatch(name); m != nil {
		return "worker stop " + m[1]
	}
	if m := roundRE.FindStringSubmatch(name); m != nil {
		return "worker stop " + m[1]
	}
	if strings.HasPrefix(name, "graft-heavy-") && strings.HasSuffix(name, ".scope") || strings.HasPrefix(name, "worker-") && strings.HasSuffix(name, "-command.scope") {
		return "systemctl --user stop " + name
	}
	return ""
}
