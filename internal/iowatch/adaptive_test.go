package iowatch

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
)

func TestAdaptiveDetailRatesUseFiveSecondDelta(t *testing.T) {
	base := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	path := "/user.slice/agent-test.scope"
	profiles, live := NewProfiles(), NewLive()
	r := NewRecorder(Default())
	r.Attach(nil, profiles, live)
	live.step(&Step{At: base}, 0, time.Second, time.Time{}, liveCase{})
	r.Detail(Step{At: base}) // establishes the prior detail timestamp without a cgroup sample
	r.Detail(Step{At: base.Add(5 * time.Second), Cgroups: []Cgroup{{Path: path, WriteBytes: 50 << 20}}})
	if got := profiles.open[path].peakWr; got != 10<<20 {
		t.Fatalf("profile peak write = %d B/s, ожидалось %d", got, int64(10<<20))
	}
	now, ok := live.Now()
	if !ok || len(now.Writers) != 1 || now.Writers[0].WriteBps != 10<<20 {
		t.Fatalf("live writer rate: %+v (ok=%v), ожидалось 10 MiB/s", now.Writers, ok)
	}
	r.Detail(Step{At: base.Add(6 * time.Second), Cgroups: []Cgroup{{Path: path, WriteBytes: 10 << 20}}})
	if got := profiles.open[path].peakWr; got != 10<<20 {
		t.Fatalf("mixed idle/active profile peak = %d B/s, ожидалось %d", got, int64(10<<20))
	}
	ended := profiles.End(map[string]string{path: "service"}, time.Second)
	if len(ended) != 1 || !ended[0].Start.Equal(base) {
		t.Fatalf("profile start должен использовать интервал первой записи cgroup: %+v", ended)
	}
}

func TestCappedWaitMixedFiveAndOneSecondIntervals(t *testing.T) {
	base := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	cap := Cap{Path: "/agents.slice", RBps: 10_000_000}
	entry := func(at time.Time, bytes, idleUS int64) Step {
		return Step{At: at, Cgroups: []Cgroup{{Path: cap.Path + "/job.scope", ReadBytes: bytes,
			IOFullUS: idleUS, Cap: cap}}}
	}
	// Случай [2s, 6s] пересекает запись в 5s за 3/5 её интервала.
	mixed := []Step{{At: base}, entry(base.Add(5*time.Second), 50_000_000, 4_000_000), entry(base.Add(6*time.Second), 10_000_000, 800_000)}
	steady := []Step{{At: base}}
	for second := 1; second <= 6; second++ {
		steady = append(steady, entry(base.Add(time.Duration(second)*time.Second), 10_000_000, 800_000))
	}
	start, end := base.Add(2*time.Second), base.Add(6*time.Second)
	// Номинал — 5 секунд; соседние реальные интервалы всё равно 5s→1s или 1s→1s.
	mixedVerdict := cappedWait(mixed, start, end, 5*time.Second, 4000, 0)
	steadyVerdict := cappedWait(steady, start, end, 5*time.Second, 4000, 0)
	if mixedVerdict.Capped != steadyVerdict.Capped || mixedVerdict.OutsideMS != steadyVerdict.OutsideMS || mixedVerdict.CappedMS != steadyVerdict.CappedMS {
		t.Fatalf("mixed (5s→1s) verdict %+v != steady 1s verdict %+v", mixedVerdict, steadyVerdict)
	}
	if !mixedVerdict.Capped || mixedVerdict.CappedMS != 3200 {
		t.Fatalf("для 10 MB/s при пределе 10 MB/s ожидался verdict capped с 3200 мс, получено %+v", mixedVerdict)
	}
}

func TestConfigDetailIdleMustBeMultipleOfDetail(t *testing.T) {
	cfg := Default()
	cfg.Socket = DefaultSocket("/tmp")
	cfg.DetailIdle = policy.Duration(5500 * time.Millisecond)
	if err := cfg.Validate(); err == nil {
		t.Fatal("detail_idle=5.5s при detail=1s должен быть отклонён")
	}
}

func TestRecorderPressureTracksOpenCase(t *testing.T) {
	r := NewRecorder(Default())
	base := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	if r.Pressure() {
		t.Fatal("new Recorder reports pressure")
	}
	r.Step(Step{At: base, IOFullUS: 80_000})
	if !r.Pressure() {
		t.Fatal("Recorder did not report pressure after opening a stalled case")
	}
	for i := 1; i <= 30; i++ {
		r.Step(Step{At: base.Add(time.Duration(i) * 100 * time.Millisecond)})
	}
	if r.Pressure() {
		t.Fatal("Recorder still reports pressure after the case closed")
	}
}

func TestClosedIdleCaseWaitsForDetailsAndReturnsCulprits(t *testing.T) {
	cfg := Default()
	cfg.CaseMin = policy.Duration(200 * time.Millisecond)
	r := NewRecorder(cfg)
	base := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	for i := 0; i <= 14; i++ {
		at := base.Add(time.Duration(i) * 100 * time.Millisecond)
		var idle int64
		if i >= 1 && i <= 6 {
			idle = 80_000
		}
		r.Step(Step{At: at, IOFullUS: idle})
	}
	closeAt := base.Add(1100 * time.Millisecond)
	detailAt := closeAt.Add(cfg.DetailIdle.D())
	path := fmt.Sprintf("/user.slice/user-%d.slice/user@%d.service/agents.slice/agent-test.scope", os.Getuid(), os.Getuid())
	process := Process{PID: 17, Start: 1, Comm: "writer", Cgroup: path, WriteBytes: 100}
	group := Cgroup{Path: process.Cgroup, WriteBytes: 100}
	r.Detail(Step{At: detailAt, Processes: []Process{process}, Cgroups: []Cgroup{group}})
	got := r.Step(Step{At: detailAt.Add(cfg.Step.D())})
	if len(got) != 1 || got[0].Culprits == nil || got[0].Culprits.Main != group.Path || got[0].Culprits.ProcWrite[0].Name != "writer(17)" {
		if len(got) > 0 && got[0].Culprits != nil {
			t.Fatalf("случай после detail_idle + step: main=%q procwrite=%+v from=%s end=%s", got[0].Culprits.Main, got[0].Culprits.ProcWrite, got[0].Culprits.From, got[0].End)
		}
		t.Fatalf("случай после detail_idle + step не вернулся с виновником: %+v", got)
	}
	if gotAt := detailAt.Add(cfg.Step.D()); gotAt.After(closeAt.Add(cfg.DetailIdle.D() + cfg.Step.D())) {
		t.Fatalf("выдача случая задержана до %s, предел %s", gotAt, closeAt.Add(cfg.DetailIdle.D()+cfg.Step.D()))
	}
}

func TestClosedCaseWaitsForLateCoveringDetail(t *testing.T) {
	for _, delay := range []time.Duration{time.Second, 3 * time.Second, 5 * time.Second} {
		t.Run(delay.String(), func(t *testing.T) {
			c, deliveredAt, sent := lateCoveringCase(t, delay)
			if !sent {
				t.Fatalf("case delivered at %s before detail covering its end arrived", deliveredAt)
			}
			if c.Culprits == nil || c.Culprits.Main != fmt.Sprintf("/user.slice/user-%d.slice/user@%d.service/agents.slice/agent-late.scope", os.Getuid(), os.Getuid()) {
				t.Fatalf("late covering detail was not used for culprit: %+v", c.Culprits)
			}
			if len(c.Culprits.ProcWrite) != 1 || c.Culprits.ProcWrite[0].Name != "late(9)" {
				t.Fatalf("late covering process missing: %+v", c.Culprits.ProcWrite)
			}
			if deliveredAt.Before(c.End.Add(delay)) {
				t.Fatalf("case delivered at %s before covering detail at %s", deliveredAt, c.End.Add(delay))
			}
		})
	}
	t.Run("no-covering-detail", func(t *testing.T) {
		_, deliveredAt, boundary := closedCaseWithoutCoveringDetail(t)
		if !deliveredAt.Equal(boundary) {
			t.Fatalf("case without a covering detail delivered at %s, want exact time boundary %s", deliveredAt, boundary)
		}
	})
}

func lateCoveringCase(t *testing.T, delay time.Duration) (Case, time.Time, bool) {
	t.Helper()
	cfg := Default()
	cfg.CaseMin = policy.Duration(200 * time.Millisecond)
	r := NewRecorder(cfg)
	base := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	path := fmt.Sprintf("/user.slice/user-%d.slice/user@%d.service/agents.slice/agent-late.scope", os.Getuid(), os.Getuid())
	r.Detail(Step{At: base.Add(-4 * time.Second)})
	r.Detail(Step{At: base.Add(150 * time.Millisecond)})
	end := base.Add(600 * time.Millisecond)
	coverAt := end.Add(delay).Truncate(100 * time.Millisecond).Add(50 * time.Millisecond)
	sent := false
	for i := 0; i <= 200; i++ {
		at := base.Add(time.Duration(i) * 100 * time.Millisecond)
		if !sent && at.After(coverAt) {
			p := Process{PID: 9, Start: 1, Comm: "late", Cgroup: path, WriteBytes: 1 << 20}
			r.Detail(Step{At: coverAt, Processes: []Process{p}, Cgroups: []Cgroup{{Path: path, WriteBytes: 1 << 20}}})
			sent = true
		}
		var idle int64
		if i >= 1 && i <= 6 {
			idle = 80_000
		}
		if got := r.Step(Step{At: at, IOFullUS: idle}); len(got) > 0 {
			return got[0], at, sent
		}
	}
	t.Fatal("case was not delivered")
	return Case{}, time.Time{}, false
}

func closedCaseWithoutCoveringDetail(t *testing.T) (Case, time.Time, time.Time) {
	t.Helper()
	cfg := Default()
	cfg.CaseMin = policy.Duration(200 * time.Millisecond)
	r := NewRecorder(cfg)
	base := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	r.Detail(Step{At: base.Add(-4 * time.Second)})
	r.Detail(Step{At: base.Add(150 * time.Millisecond)})
	end := base.Add(600 * time.Millisecond)
	boundary := end.Add(cfg.DetailIdle.D() + cfg.Detail.D() + cfg.Step.D())
	for i := 0; i <= 100; i++ {
		at := base.Add(time.Duration(i) * cfg.Step.D())
		var idle int64
		if i >= 1 && i <= 6 {
			idle = 80_000
		}
		if got := r.Step(Step{At: at, IOFullUS: idle}); len(got) > 0 {
			return got[0], at, boundary
		}
	}
	t.Fatal("case without a covering detail was not delivered")
	return Case{}, time.Time{}, boundary
}

func TestClosedCaseWithoutDetailsHasBoundedWait(t *testing.T) {
	cfg := Default()
	r := NewRecorder(cfg)
	base := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	r.Detail(Step{At: base.Add(-time.Second)}) // Последняя запись до случая; дальнейших проходов нет.
	closeAt := base.Add(1100 * time.Millisecond)
	var got []Case
	var delivered time.Time
	for i := 0; i <= 1200; i++ { // 120 s быстрых шагов, новых detail нет
		at := base.Add(time.Duration(i) * 100 * time.Millisecond)
		var idle int64
		if i >= 1 && i <= 6 {
			idle = 80_000
		}
		ready := r.Step(Step{At: at, IOFullUS: idle})
		if len(ready) > 0 && delivered.IsZero() {
			delivered = at
		}
		got = append(got, ready...)
	}
	if len(got) != 1 || got[0].Culprits == nil || got[0].Culprits.Main != "" || len(got[0].Culprits.ProcWrite) != 0 {
		t.Fatalf("случай без новых detail должен выйти без виновников: %+v", got)
	}
	deadline := closeAt.Add(cfg.DetailIdle.D() + cfg.Detail.D() + cfg.Step.D())
	if delivered.IsZero() || delivered.After(deadline) {
		t.Fatalf("случай отдан в %s после предела %s", delivered, deadline)
	}
}

func TestCaseDeviceWriteUsesPreviousIntervalOfFirstIncludedDetail(t *testing.T) {
	r := NewRecorder(Default())
	base := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	r.Detail(Step{At: base})
	for _, item := range []struct {
		at time.Duration
		wr int64
	}{{2 * time.Second, 3}, {4 * time.Second, 5}, {5 * time.Second, 7}, {6 * time.Second, 11}} {
		r.Step(Step{At: base.Add(item.at), WriteBytes: item.wr})
		if item.at == 5*time.Second || item.at == 6*time.Second {
			r.Detail(Step{At: base.Add(item.at)})
		}
	}
	c := Case{Start: base.Add(5100 * time.Millisecond), End: base.Add(5200 * time.Millisecond), DurMS: 1000}
	r.finish(&c)
	if got := c.Culprits.DeviceWrite; got != 26 {
		t.Fatalf("device write = %d, ожидалось 26 байт за собственный 5-секундный интервал первой записи", got)
	}
}

func TestDetailIntervalIsStoredWhenPreviousRecordFallsOutOfRing(t *testing.T) {
	cfg := Default()
	cfg.Ring = policy.Duration(time.Minute)
	ring := NewRing(cfg)
	base := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	ring.Add(Record{Kind: KindDetail, detailInterval: 5 * time.Minute, Step: Step{At: base}})
	ring.Add(Record{Kind: KindStep, Step: Step{At: base.Add(2 * time.Minute)}})
	at := base.Add(6 * time.Minute)
	ring.Add(Record{Kind: KindDetail, detailInterval: 6 * time.Minute, Step: Step{At: at}})
	if got := ring.DetailInterval(at, time.Second); got != 6*time.Minute {
		t.Fatalf("интервал детали после вытеснения предыдущей записи = %s, ожидалось 6m", got)
	}
}

func TestCaseFirstDetailKeepsItsIntervalAfterPriorDetailEviction(t *testing.T) {
	cfg := Default()
	cfg.Ring = policy.Duration(3 * time.Minute)
	r := NewRecorder(cfg)
	base := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	end := base.Add(6 * time.Minute)
	r.Detail(Step{At: base})
	r.Step(Step{At: end.Add(-2 * time.Minute), WriteBytes: 10})
	r.Step(Step{At: end.Add(-time.Second), WriteBytes: 20})
	r.Detail(Step{At: end})
	for _, rec := range r.ring.Records() {
		if rec.Kind == KindDetail && rec.At.Equal(base) {
			t.Fatal("previous detail should have been evicted by Recorder.Detail")
		}
	}
	c := Case{Start: end.Add(-100 * time.Millisecond), End: end, DurMS: 1000}
	r.finish(&c)
	if got := c.Culprits.DeviceWrite; got != 30 {
		t.Fatalf("device write after predecessor eviction = %d, ожидалось 30 за сохранённые 6 минут", got)
	}
}

func TestOpenCaseUsesStoredLongDetailIntervalForCapRate(t *testing.T) {
	cfg := Default()
	r := NewRecorder(cfg)
	live := NewLive()
	r.Attach(nil, nil, live)
	base := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	r.Detail(Step{At: base})
	for i := 0; i <= 3600; i++ {
		at := base.Add(time.Duration(i) * 100 * time.Millisecond)
		var idle int64
		if i == 3600 {
			idle = 80_000 // open a 100 ms case at the end of the six-minute detail interval
		}
		r.Step(Step{At: at, IOFullUS: idle})
	}
	path := "/agents.slice/job.scope"
	cap := Cap{Path: "/agents.slice", RBps: 10_000_000}
	r.Detail(Step{At: base.Add(6 * time.Minute), Cgroups: []Cgroup{{Path: path,
		ReadBytes: 360 * 5_000_000, IOFullUS: 360_000_000, Cap: cap}}})
	now, ok := live.Now()
	if !ok || !now.Open.Open || now.Open.Capped {
		t.Fatalf("ongoing case at 5 MB/s must stay below the 10 MB/s cap over its stored 6-minute interval: %+v (ok=%v)", now.Open, ok)
	}
}

func TestOpenCaseUsesNonemptyIdleDetailAcrossFiveSecondGap(t *testing.T) {
	cfg := Default()
	base := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	path := "/agents.slice/job.scope"
	verdict := func(includeCgroups bool) bool {
		r := NewRecorder(cfg)
		live := NewLive()
		r.Attach(nil, nil, live)
		r.Detail(Step{At: base.Add(-4 * time.Second)})
		r.Step(Step{At: base})
		r.Step(Step{At: base.Add(100 * time.Millisecond), IOFullUS: 80_000}) // case starts at base
		var firstCgroups []Cgroup
		if includeCgroups {
			cap := Cap{Path: "/agents.slice", RBps: 10_000_000}
			firstCgroups = []Cgroup{{Path: path, ReadBytes: 50_000_000, IOFullUS: 1_000_000, Cap: cap}}
		}
		// First detail contributing to the open window: its 5s interval is (-4s, +1s]
		// and therefore crosses the case start at 0s.
		r.Detail(Step{At: base.Add(time.Second), Cgroups: firstCgroups})
		for i := 2; i <= 50; i++ {
			r.Step(Step{At: base.Add(time.Duration(i) * 100 * time.Millisecond), IOFullUS: 80_000})
		}
		// A short uncapped idle contribution makes the first record decisive for the verdict.
		r.Detail(Step{At: base.Add(5 * time.Second), Cgroups: []Cgroup{{Path: path, IOFullUS: 100_000}}})
		return live.openCapped
	}
	if got := verdict(true); !got {
		t.Fatal("open case with the nonempty capped idle detail should be capped")
	}
	if got := verdict(false); got {
		t.Fatal("removing Cgroups from the first idle detail should change the open-case verdict to uncapped")
	}
}

func TestOpenCaseUsesStoredIntervalAfterPriorDetailEviction(t *testing.T) {
	cfg := Default()
	cfg.Ring = policy.Duration(time.Minute)
	r := NewRecorder(cfg)
	live := NewLive()
	r.Attach(nil, nil, live)
	base := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	r.Detail(Step{At: base.Add(-6 * time.Minute)})
	for i := 0; i <= 60; i++ {
		at := base.Add(time.Duration(i) * 100 * time.Millisecond)
		r.Step(Step{At: at, IOFullUS: 80_000})
		if i == 10 {
			cap := Cap{Path: "/agents.slice", RBps: 1}
			r.Detail(Step{At: base.Add(time.Second), Cgroups: []Cgroup{{Path: "/agents.slice/job.scope",
				ReadBytes: 361, IOFullUS: 5_000_000, Cap: cap}}})
		}
	}
	r.Detail(Step{At: base.Add(6 * time.Second), Cgroups: []Cgroup{{Path: "/agents.slice/job.scope", IOFullUS: 40_000}}})
	for _, rec := range r.ring.Records() {
		if rec.Kind == KindDetail && rec.At.Equal(base.Add(-6*time.Minute)) {
			t.Fatal("previous detail should have been evicted from the ring")
		}
	}
	now, ok := live.Now()
	if !ok || !now.Open.Open || now.Open.Capped {
		t.Fatalf("open capped verdict must use the evicted predecessor's stored interval: %+v (ok=%v)", now.Open, ok)
	}
}
