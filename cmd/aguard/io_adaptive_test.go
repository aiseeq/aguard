package main

import (
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/iowatch"
)

type recorderPressureSink struct{ recorder *iowatch.Recorder }

func (s *recorderPressureSink) step(sample iowatch.Step)   { s.recorder.Step(sample) }
func (s *recorderPressureSink) detail(sample iowatch.Step) { s.recorder.Detail(sample) }
func (s *recorderPressureSink) sampleErr(string, error)    {}
func (s *recorderPressureSink) pressure() bool             { return s.recorder.Pressure() }

func TestIODetailIdleCadence30Seconds(t *testing.T) {
	base := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	idle := 5 * time.Second
	schedule := newIODetailSchedule(base, idle)
	var recorded []time.Time
	for at := base; !at.After(base.Add(30 * time.Second)); at = at.Add(100 * time.Millisecond) {
		if schedule.Due(at) {
			recorded = append(recorded, at)
			schedule.Recorded(at)
		}
	}
	if len(recorded) < 5 || len(recorded) > 7 {
		t.Fatalf("подробных записей за 30 с покоя: %d, ожидалось 6 ± 1", len(recorded))
	}
	for i := 1; i < len(recorded); i++ {
		if gap := recorded[i].Sub(recorded[i-1]); gap != idle {
			t.Fatalf("интервал записи %s, ожидался %s", gap, idle)
		}
	}
}

func TestRecorderPressureDrivesDetailIntervalAndCooldown(t *testing.T) {
	cfg := iowatch.Default()
	base := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	rec := iowatch.NewRecorder(cfg)
	sink := &recorderPressureSink{recorder: rec}
	var pressure ioPressureState
	control := make(chan ioDetailChange, 1)
	step := func(i int, stalled bool) (ioDetailChange, bool) {
		var idle int64
		if stalled {
			idle = 80_000
		}
		at := base.Add(time.Duration(i) * 100 * time.Millisecond)
		advanceIOFastStep(sink, iowatch.Step{At: at, IOFullUS: idle}, cfg, &pressure, control)
		select {
		case change := <-control:
			return change, true
		default:
			return ioDetailChange{}, false
		}
	}
	if _, changed := step(0, false); changed {
		t.Fatal("покой переключил интервал")
	}
	schedule := newIODetailSchedule(base, cfg.DetailIdle.D())
	change, changed := step(1, true)
	if !changed || change.interval != cfg.Detail.D() || !change.immediate {
		t.Fatalf("Recorder открыл случай, но не запросил внеочередной проход: %+v %v", change, changed)
	}
	at := base.Add(100 * time.Millisecond)
	schedule.Change(at, change)
	if !schedule.Due(at) {
		t.Fatal("внеочередной подробный проход не назначен на шаге давления")
	}
	recorded := []time.Time{at}
	schedule.Recorded(at)
	for i := 2; i <= 6; i++ {
		if _, changed := step(i, true); changed {
			t.Fatalf("лишнее переключение на шаге %d", i)
		}
	}
	closeAt := 0
	for i := 7; i < 20; i++ {
		_, changed := step(i, false)
		if changed {
			t.Fatalf("частота сменилась до конца context на шаге %d", i)
		}
		if !rec.Pressure() && closeAt == 0 {
			closeAt = i
		}
		fastAt := base.Add(time.Duration(i) * 100 * time.Millisecond)
		if schedule.Due(fastAt) {
			recorded = append(recorded, fastAt)
			schedule.Recorded(fastAt)
		}
	}
	if closeAt == 0 {
		t.Fatal("Recorder не закрыл синтетический случай")
	}
	var idleStarted time.Time
	for i := 20; i <= 65; i++ {
		change, changed = step(i, false)
		fastAt := base.Add(time.Duration(i) * 100 * time.Millisecond)
		if changed {
			if change.interval != cfg.DetailIdle.D() {
				t.Fatalf("интервал после context: %s", change.interval)
			}
			schedule.Change(fastAt, change)
			idleStarted = fastAt
			break
		}
		if schedule.Due(fastAt) {
			recorded = append(recorded, fastAt)
			schedule.Recorded(fastAt)
		}
	}
	wantIdle := base.Add(time.Duration(closeAt)*100*time.Millisecond + cfg.Context.D())
	if schedule.interval != cfg.DetailIdle.D() || !schedule.next.Equal(idleStarted.Add(cfg.DetailIdle.D())) || idleStarted.Before(wantIdle) {
		t.Fatalf("после закрытия + context расписание: %+v, конец хвоста %s", schedule, wantIdle)
	}
	if len(recorded) < 5 {
		t.Fatalf("недостаточно подробных записей для проверки активной частоты: %v", recorded)
	}
	for i := 1; i < len(recorded); i++ {
		gap := recorded[i].Sub(recorded[i-1])
		if gap > cfg.Detail.D() && gap != cfg.DetailIdle.D() {
			t.Fatalf("неожиданный интервал подробных записей %s", gap)
		}
	}

	// Повторное открытие внутри хвоста сбрасывает прежний quietTill и даёт полный context после закрытия.
	openIndex := 62
	if change, changed = step(openIndex, true); !changed || !change.immediate {
		t.Fatalf("повторное открытие не запросило внеочередной проход: %+v %v", change, changed)
	}
	for i := openIndex + 1; i <= openIndex+5; i++ {
		step(i, true)
	}
	lastStallSecond := openIndex + 5
	for i := lastStallSecond + 1; i <= lastStallSecond+5; i++ {
		if change, changed := step(i, false); changed {
			t.Fatalf("повторный случай потерял context при закрытии на шаге %d: %+v", i, change)
		}
	}
	closeSecond := lastStallSecond + 5
	for i := closeSecond + 1; i < closeSecond+50; i++ {
		if change, changed := step(i, false); changed {
			t.Fatalf("старый quietTill оборвал новый хвост на шаге %d: %+v", i, change)
		}
	}
	if change, changed := step(closeSecond+50, false); !changed || change.interval != cfg.DetailIdle.D() {
		t.Fatalf("после второго закрытия + context нет возврата к покою: %+v %v", change, changed)
	}
}

func TestRunIODetailReadChangeImmediateAndRecorded(t *testing.T) {
	ctx := t.Context()
	control := make(chan ioDetailChange, 1)
	out := make(chan ioDetailResult, 4)
	reads := make(chan time.Time, 4)
	go runIODetailRead(ctx, time.Hour, func(at time.Time) ioDetailResult {
		reads <- at
		return ioDetailResult{step: iowatch.Step{At: at}}
	}, out, control)

	// A one hour initial timer makes Change(immediate=true) the only possible first trigger.
	setIODetailInterval(control, time.Hour, time.Hour, true, true)
	var first time.Time
	select {
	case first = <-reads:
	case <-time.After(time.Second):
		t.Fatal("immediate Change did not trigger a detail read")
	}
	select {
	case got := <-out:
		if !got.step.At.Equal(first) {
			t.Fatalf("result At = %s, read time = %s", got.step.At, first)
		}
	case <-time.After(time.Second):
		t.Fatal("detail read result was not sent")
	}

	// After the read, Recorded schedules from its completion. Change to a regular short interval.
	setIODetailInterval(control, 30*time.Millisecond, 30*time.Millisecond, true, false)
	select {
	case at := <-reads:
		t.Fatalf("regular Change read immediately at %s", at)
	case <-time.After(10 * time.Millisecond):
	}
	select {
	case second := <-reads:
		if !second.After(first) {
			t.Fatalf("second read time %s is not after first read %s", second, first)
		}
	case <-time.After(time.Second):
		t.Fatal("Recorded/regular timer did not trigger the next read")
	}
}
