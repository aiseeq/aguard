package forkguard

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func testConfig() Config {
	return Config{Window: 10 * time.Second, Hard: DefaultHard, Floor: DefaultFloor, Rise: 1000, SliceWarn: 80}
}

func TestLegalBuildDoesNotDecide(t *testing.T) {
	cfg := testConfig()
	cfg.Floor = 800
	e := New(cfg)
	at := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	for i := 0; i <= 20; i++ {
		current := int64(100 + min(i, 10)*80)
		if i > 10 {
			current = 1000
		}
		r := e.Observe(Sample{At: at.Add(time.Duration(i) * time.Second), Units: []Unit{{Name: "graft-heavy-1.scope", Current: current}}, SliceCurrent: current})
		if len(r.Decisions) != 0 {
			t.Fatalf("unexpected decision at second %d: %+v", i, r.Decisions)
		}
	}
}

func TestHardThresholdAndSingleDecision(t *testing.T) {
	e := New(testConfig())
	at := time.Now()
	var got Result
	decidedAt := -1
	for i, n := range []int64{100, 1000, 5000, 8000} {
		got = e.Observe(Sample{At: at.Add(time.Duration(i*2) * time.Second), Units: []Unit{{Name: "worker-job-7.service", Current: n}}})
		if len(got.Decisions) > 0 {
			decidedAt = i
		}
	}
	if decidedAt != 2 || len(got.Decisions) != 0 {
		t.Fatalf("decision must occur at first hard threshold sample, index=%d result=%+v", decidedAt, got)
	}
	r := e.Observe(Sample{At: at.Add(9 * time.Second), Units: []Unit{{Name: "worker-job-7.service", Current: 9000}}})
	if len(r.Decisions) != 0 {
		t.Fatalf("repeated decision: %+v", r.Decisions)
	}
}

func TestEventsMaxImmediateAndDisappearance(t *testing.T) {
	e := New(testConfig())
	at := time.Now()
	u := Unit{Name: "graft-heavy-1.scope", Current: 120, Max: 500, EventsMax: 1}
	e.Observe(Sample{At: at, Units: []Unit{u}})
	u.EventsMax = 2
	r := e.Observe(Sample{At: at.Add(time.Second), Units: []Unit{u}})
	if len(r.Decisions) != 1 {
		t.Fatalf("decision: %+v", r)
	}
	r = e.Observe(Sample{At: at.Add(2 * time.Second)})
	if len(r.Lines) != 1 || !strings.Contains(r.Lines[0], "last_events_max=2") {
		t.Fatalf("disappearance line: %+v", r.Lines)
	}
}

func TestWorkerRoundUsesFighterID(t *testing.T) {
	name := "worker-round-1124-0255-termax-deal-block-1.service"
	if got := Would(name); got != "worker stop 1124-0255-termax-deal-block" {
		t.Fatalf("Would(%q)=%q", name, got)
	}
	if got := Would("worker-round-123-3.service"); got != "worker stop 123" {
		t.Fatalf("colliding id: %q", got)
	}
}

func TestWouldUnescapesUnicodeWorkerIDs(t *testing.T) {
	id := "1124-1111-skill-reload-суета"
	var escaped strings.Builder
	for i := 0; i < len(id); i++ {
		if id[i] >= 0x80 {
			fmt.Fprintf(&escaped, `\x%02x`, id[i])
		} else {
			escaped.WriteByte(id[i])
		}
	}
	for _, name := range []string{"worker-job-" + escaped.String() + ".service", "worker-round-" + escaped.String() + "-3.service"} {
		if got := Would(name); got != "worker stop "+id {
			t.Errorf("Would(%q)=%q", name, got)
		}
	}
}

func TestNewUnitSeedsOnlyInitialEvents(t *testing.T) {
	e := New(testConfig())
	at := time.Now()
	u := Unit{Name: "worker-job-7.service", Inode: 11, EventsMax: 5, Current: 50}
	r := e.Observe(Sample{At: at, Units: []Unit{u}})
	if len(r.Decisions) != 0 {
		t.Fatalf("initial events are baseline: %+v", r.Decisions)
	}
	r = e.Observe(Sample{At: at.Add(time.Second), Units: []Unit{u, {Name: "worker-job-8.service", Inode: 12, EventsMax: 1, Current: 50}}})
	if len(r.Decisions) != 1 || r.Decisions[0].Unit.Name != "worker-job-8.service" {
		t.Fatalf("new unit with events: %+v", r.Decisions)
	}
	r = e.Observe(Sample{At: at.Add(2 * time.Second)})
	if len(r.Lines) != 1 || !strings.Contains(r.Lines[0], "worker-job-8.service") || !strings.Contains(r.Lines[0], "last_events_max=1") {
		t.Fatalf("disappearance: %+v", r.Lines)
	}
}

func TestSameNameWithNewInodeIsNewUnit(t *testing.T) {
	e := New(testConfig())
	at := time.Now()
	u := Unit{Name: "worker-job-7.service", Inode: 21, Current: 10}
	e.Observe(Sample{At: at, Units: []Unit{u}})
	u.Inode, u.EventsMax = 22, 1
	r := e.Observe(Sample{At: at.Add(time.Second), Units: []Unit{u}})
	if len(r.Decisions) != 1 || r.Decisions[0].Unit.Inode != 22 {
		t.Fatalf("recreated unit: %+v", r.Decisions)
	}
}

func TestSkippedUnitDoesNotDisappear(t *testing.T) {
	e := New(testConfig())
	at := time.Now()
	u := Unit{Name: "worker-job-7.service", Inode: 31, Current: DefaultHard}
	e.Observe(Sample{At: at, Units: []Unit{u}})
	skip := UnitIdentity{Name: u.Name, Inode: u.Inode, Error: "read pids.current: temporary error"}
	r := e.Observe(Sample{At: at.Add(time.Second), Skipped: []UnitIdentity{skip}})
	if len(r.Lines) != 1 || !strings.Contains(r.Lines[0], "read failed") || strings.Contains(r.Lines[0], "disappeared") {
		t.Fatalf("transient read error: %+v", r.Lines)
	}
	r = e.Observe(Sample{At: at.Add(2 * time.Second), Skipped: []UnitIdentity{skip}})
	if len(r.Lines) != 0 {
		t.Fatalf("repeated read warning: %+v", r.Lines)
	}
	r = e.Observe(Sample{At: at.Add(3 * time.Second)})
	if len(r.Lines) != 1 || !strings.Contains(r.Lines[0], "last_current=3000") {
		t.Fatalf("missing dir: %+v", r.Lines)
	}
}

func TestSliceMissingAndAppearedAreTransitionLines(t *testing.T) {
	e := New(testConfig())
	at := time.Now()
	for _, s := range []Sample{{At: at, SliceMissing: true}, {At: at.Add(time.Second), SliceMissing: true}, {At: at.Add(2 * time.Second)}} {
		r := e.Observe(s)
		if s.At.Equal(at) && !hasForkguardLine(r, "slice missing") {
			t.Fatalf("missing transition: %+v", r.Lines)
		}
		if s.At.Equal(at.Add(time.Second)) && len(r.Lines) != 0 {
			t.Fatalf("repeated transition: %+v", r.Lines)
		}
		if s.At.Equal(at.Add(2*time.Second)) && !hasForkguardLine(r, "slice appeared") {
			t.Fatalf("appeared transition: %+v", r.Lines)
		}
	}
}

func TestGrowthSurvivesPollingPauseUpToTwoWindows(t *testing.T) {
	cfg := testConfig()
	cfg.Window = 10 * time.Second
	cfg.Rise = 500
	cfg.Floor = 100
	cfg.Hard = 10000
	e := New(cfg)
	at := time.Now()
	e.Observe(Sample{At: at, Units: []Unit{{Name: "worker-job-1.service", Current: 100}}})
	e.Observe(Sample{At: at.Add(15 * time.Second), Units: []Unit{{Name: "worker-job-1.service", Current: 700}}})
	r := e.Observe(Sample{At: at.Add(16 * time.Second), Units: []Unit{{Name: "worker-job-1.service", Current: 800}}})
	if len(r.Decisions) != 1 {
		t.Fatalf("history older than window was discarded: %+v", r.Decisions)
	}
}

func TestMinuteSummaryReportsRiseStreakEvenBelowFloor(t *testing.T) {
	cfg := testConfig()
	cfg.Rise = 10
	cfg.Floor = 1000
	cfg.Hard = 5000
	e := New(cfg)
	at := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	for i, current := range []int64{100, 110, 120} {
		e.Observe(Sample{At: at.Add(time.Duration(i) * time.Second), Units: []Unit{{Name: "calibration.service", Current: current}}})
	}
	r := e.Observe(Sample{At: at.Add(time.Minute)})
	if !strings.Contains(r.Summary, "rise_streak=2") || !strings.Contains(r.Summary, "current=120") {
		t.Fatalf("growth calibration summary: %s", r.Summary)
	}
}

func TestAggregateGrowthOnlyWhenNoUnitExplainsItAndThrottles(t *testing.T) {
	cfg := testConfig()
	cfg.Rise = 100
	cfg.Hard = 10000
	cfg.Floor = 1
	at := time.Now()
	e := New(cfg)
	e.Observe(Sample{At: at, Units: []Unit{{Name: "one.service", Current: 100}}, SliceCurrent: 0})
	r := e.Observe(Sample{At: at.Add(time.Second), Units: []Unit{{Name: "one.service", Current: 250}}, SliceCurrent: 150})
	if hasForkguardLine(r, "aggregate growth") {
		t.Fatalf("unit explains growth: %+v", r.Lines)
	}
	e = New(cfg)
	e.Observe(Sample{At: at, SliceCurrent: 0})
	r = e.Observe(Sample{At: at.Add(time.Second), SliceCurrent: 200})
	if !hasForkguardLine(r, "aggregate growth") {
		t.Fatalf("missing aggregate line: %+v", r.Lines)
	}
	r = e.Observe(Sample{At: at.Add(2 * time.Second), SliceCurrent: 300})
	if hasForkguardLine(r, "aggregate growth") {
		t.Fatalf("repeated in minute: %+v", r.Lines)
	}
}

func TestSummaryAndDecisionCarryCalibrationCounters(t *testing.T) {
	cfg := testConfig()
	cfg.Hard = 100
	e := New(cfg)
	at := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	u := Unit{Name: "worker-job-7.service", Current: 100, Max: 500, EventsMax: 2}
	r := e.Observe(Sample{At: at, Units: []Unit{u}, SliceCurrent: 70, SliceMax: 100, SliceEventsMax: 4, SlicePidsPeak: 88})
	if len(r.Decisions) != 1 {
		t.Fatalf("decision: %+v", r.Decisions)
	}
	line := r.Decisions[0].LogLine()
	for _, field := range []string{"max=500", "events_max=2", "rise=0"} {
		if !strings.Contains(line, field) {
			t.Errorf("line misses %q: %s", field, line)
		}
	}
	e.Observe(Sample{At: at.Add(time.Second), Units: []Unit{u}, SliceCurrent: 75, SliceMax: 100, SliceEventsMax: 5, SlicePidsPeak: 91})
	r = e.Observe(Sample{At: at.Add(time.Minute), SliceMax: 100, SliceEventsMax: 5, SlicePidsPeak: 91})
	for _, field := range []string{"slice_max=100", "slice_events_max=5", "slice_pids_peak=91", "rise_streak=0"} {
		if !strings.Contains(r.Summary, field) {
			t.Errorf("summary misses %q: %s", field, r.Summary)
		}
	}
}

func hasForkguardLine(r Result, part string) bool {
	for _, line := range r.Lines {
		if strings.Contains(line, part) {
			return true
		}
	}
	return false
}

func TestSliceWarnThrottledAndMinuteSummary(t *testing.T) {
	e := New(testConfig())
	at := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	for i := range 3 {
		r := e.Observe(Sample{At: at.Add(time.Duration(i*20) * time.Second), SliceCurrent: 81, SliceMax: 100})
		if i == 0 && len(r.Lines) != 1 {
			t.Fatalf("first warning: %+v", r.Lines)
		}
		if i > 0 && len(r.Lines) != 0 {
			t.Fatalf("warning repeated: %+v", r.Lines)
		}
	}
	r := e.Observe(Sample{At: at.Add(time.Minute), SliceCurrent: 70, SliceMax: 100})
	if !strings.Contains(r.Summary, "slice_peak=81") {
		t.Fatalf("minute summary: %q", r.Summary)
	}
}

func TestOneDecisionForGrowingUnit(t *testing.T) {
	c := testConfig()
	c.Rise = 100
	c.Floor = 200
	e := New(c)
	at := time.Now()
	r := e.Observe(Sample{At: at, Units: []Unit{{Name: "misc.service", Current: 100}}})
	r = e.Observe(Sample{At: at.Add(time.Second), Units: []Unit{{Name: "misc.service", Current: 250}}})
	if len(r.Decisions) != 0 {
		t.Fatal(r.Decisions)
	}
	r = e.Observe(Sample{At: at.Add(2 * time.Second), Units: []Unit{{Name: "misc.service", Current: 300}}})
	if len(r.Decisions) != 1 {
		t.Fatalf("second consecutive qualifying sample should decide: %+v", r)
	}
}

// Рост считается за window, а не за всю историю (2×window): равномерные +60 в секунду 20 с — за 10 с только 600.
func TestUniformRiseCountsOnlyWindow(t *testing.T) {
	cfg := testConfig()
	cfg.Floor = 100
	e := New(cfg)
	at := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	for i := 0; i <= 20; i++ {
		n := int64(100 + 60*i)
		r := e.Observe(Sample{At: at.Add(time.Duration(i) * time.Second), Units: []Unit{{Name: "graft-heavy-1.scope", Current: n}}, SliceCurrent: n})
		if len(r.Decisions) != 0 {
			t.Fatalf("решение на секунде %d при росте 600 за окно: %+v", i, r.Decisions)
		}
		for _, l := range r.Lines {
			if strings.Contains(l, "aggregate") {
				t.Fatalf("строка совокупного роста на секунде %d: %s", i, l)
			}
		}
	}
}

// Один юнит дал рост ≥ rise, второй рядом подрос на 10 — это не «совокупный рост без одного юнита».
func TestAggregateLineNotWhenOneUnitGivesRise(t *testing.T) {
	cfg := testConfig()
	cfg.Floor = 5000 // решения по (в) нет — смотрим только строку слайса
	e := New(cfg)
	at := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	for i := 0; i <= 10; i++ {
		a, b := int64(100+120*i), int64(50+i)
		r := e.Observe(Sample{At: at.Add(time.Duration(i) * time.Second), Units: []Unit{{Name: "graft-heavy-1.scope", Current: a}, {Name: "graft-heavy-2.scope", Current: b}}, SliceCurrent: a + b})
		for _, l := range r.Lines {
			if strings.Contains(l, "aggregate") {
				t.Fatalf("строка совокупного роста при одном юните-виновнике (секунда %d): %s", i, l)
			}
		}
	}
}

// Слайс появился после старта сторожа: юнит, сразу упёршийся в свой предел, — решение, не засев.
func TestSliceAppearedAfterStartHasNoSeed(t *testing.T) {
	e := New(testConfig())
	at := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	e.Observe(Sample{At: at, SliceMissing: true})
	r := e.Observe(Sample{At: at.Add(time.Second), Units: []Unit{{Name: "worker-job-1124-1200-x.service", Current: 100, Max: 100, EventsMax: 5}}, SliceCurrent: 100})
	if len(r.Decisions) != 1 {
		t.Fatalf("юнит в пределе при появлении слайса не дал решения: %+v", r)
	}
}
