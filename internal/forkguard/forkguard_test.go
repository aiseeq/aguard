package forkguard

import (
	"strings"
	"testing"
	"time"
)

func testConfig() Config {
	return Config{Window: 10 * time.Second, Hard: DefaultHard, Floor: DefaultFloor, Rise: 1000, SliceWarn: 80}
}

func TestLegalBuildDoesNotDecide(t *testing.T) {
	e := New(testConfig())
	at := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	for i, n := range []int64{100, 900, 1000} {
		r := e.Observe(Sample{At: at.Add(time.Duration(i) * 5 * time.Second), Units: []Unit{{Name: "graft-heavy-1.scope", Current: n}}, SliceCurrent: n})
		if len(r.Decisions) != 0 {
			t.Fatalf("unexpected decision: %+v", r.Decisions)
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
