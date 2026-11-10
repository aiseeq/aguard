package iowatch

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// coverT0 — опорное время тестов покрытия: стенные часы без монотонной части, как из файла.
var coverT0 = time.Date(2026, 11, 9, 22, 0, 0, 0, time.Local)

// coverMarks — шаги замера режима mode раз в every на [from, to].
func coverMarks(c *Coverer, from, to time.Time, every time.Duration, mode string) {
	for at := from; !at.After(to); at = at.Add(every) {
		c.Mark(at, mode)
	}
}

// coverSum — покрытие и пробелы складываются в окно: ни одна миллисекунда не потеряна и не учтена дважды.
func coverSum(t *testing.T, c Coverage) {
	t.Helper()
	sum := c.CoveredMS
	for _, g := range c.Gaps {
		sum += g.To.Sub(g.From).Milliseconds()
	}
	if sum != c.WindowMS || c.WindowMS != c.Until.Sub(c.Since).Milliseconds() {
		t.Fatalf("покрытие %d + пробелы ≠ окно %d: %+v", c.CoveredMS, c.WindowMS, c.Gaps)
	}
}

// Служба стояла час в середине окна: пробел not_running ровно на этот час, остальное покрыто.
func TestCoverServiceStoppedPart(t *testing.T) {
	c := NewCoverer(CoverRecord{}, coverT0)
	coverMarks(c, coverT0, coverT0.Add(time.Hour), 10*time.Second, ModeAlert)
	c.Break() // остановка службы
	coverMarks(c, coverT0.Add(2*time.Hour), coverT0.Add(3*time.Hour), 10*time.Second, ModeAlert)
	got := Cover(c.Record(coverT0.Add(3*time.Hour)), CoverService, coverT0, coverT0.Add(3*time.Hour), coverT0.Add(-14*24*time.Hour))
	coverSum(t, got)
	want := []Gap{{From: coverT0.Add(time.Hour), To: coverT0.Add(2 * time.Hour), Why: GapNotRunning}}
	if !reflect.DeepEqual(got.Gaps, want) || got.CoveredMS != (2*time.Hour).Milliseconds() || !reflect.DeepEqual(got.Modes, []string{ModeAlert}) {
		t.Fatalf("покрытие: %+v", got)
	}
}

// Порог разрыва — CoverGapMax по стенным часам: шаг через него ровно — тот же интервал, дальше — новый с
// пробелом; перезапуск цикла (Break) — новый интервал и без паузы; смена режима без паузы — новый
// интервал встык.
func TestCoverGapThresholdBreakAndMode(t *testing.T) {
	c := NewCoverer(CoverRecord{}, coverT0)
	c.Mark(coverT0, ModeAlert)
	c.Mark(coverT0.Add(CoverGapMax), ModeAlert)
	if r := c.Record(coverT0.Add(time.Hour)); len(r.Spans) != 1 || !r.Spans[0].To.Equal(coverT0.Add(CoverGapMax)) {
		t.Fatalf("шаг через порог ровно: %+v", r.Spans)
	}
	far := coverT0.Add(2*CoverGapMax + time.Millisecond)
	c.Mark(far, ModeAlert)
	if r := c.Record(coverT0.Add(time.Hour)); len(r.Spans) != 2 || !r.Spans[1].From.Equal(far) {
		t.Fatalf("шаг дальше порога: %+v", r.Spans)
	}
	c.Break()
	c.Mark(far.Add(100*time.Millisecond), ModeAlert)
	if r := c.Record(coverT0.Add(time.Hour)); len(r.Spans) != 3 || !r.Spans[2].From.Equal(far.Add(100*time.Millisecond)) {
		t.Fatalf("перезапуск цикла: %+v", r.Spans)
	}
	at := far.Add(200 * time.Millisecond)
	c.Mark(at, ModeLog)
	r := c.Record(coverT0.Add(time.Hour))
	if len(r.Spans) != 4 || r.Spans[3].Mode != ModeLog || !r.Spans[3].From.Equal(far.Add(100*time.Millisecond)) || !r.Spans[2].To.Equal(r.Spans[3].From) {
		t.Fatalf("смена режима: %+v", r.Spans)
	}
}

// mode=off: служба записывает, что стояла в off, — пробел mode_off, покрытия нет.
func TestCoverModeOff(t *testing.T) {
	c := NewCoverer(CoverRecord{}, coverT0)
	coverMarks(c, coverT0, coverT0.Add(time.Hour), ioReloadTestEvery, ModeOff)
	coverMarks(c, coverT0.Add(time.Hour), coverT0.Add(2*time.Hour), time.Second, ModeLog)
	got := Cover(c.Record(coverT0.Add(2*time.Hour)), CoverService, coverT0, coverT0.Add(2*time.Hour), coverT0.Add(-24*time.Hour))
	coverSum(t, got)
	want := []Gap{{From: coverT0, To: coverT0.Add(time.Hour), Why: GapModeOff}}
	if !reflect.DeepEqual(got.Gaps, want) || got.CoveredMS != time.Hour.Milliseconds() || !reflect.DeepEqual(got.Modes, []string{ModeLog}) {
		t.Fatalf("mode=off: %+v", got)
	}
	// между off и замером — не встык: замер с первого шага, off — с первого прохода
	c.Mark(coverT0.Add(2*time.Hour+5*time.Second), ModeOff)
	c.Mark(coverT0.Add(2*time.Hour+15*time.Second), ModeAlert)
	if r := c.Record(coverT0.Add(3 * time.Hour)); len(r.Spans) != 4 || !r.Spans[2].From.Equal(coverT0.Add(2*time.Hour+5*time.Second)) ||
		!r.Spans[3].From.Equal(coverT0.Add(2*time.Hour+15*time.Second)) {
		t.Fatalf("off ↔ замер: %+v", r.Spans)
	}
	only := Cover(c.Record(coverT0.Add(2*time.Hour)), CoverService, coverT0, coverT0.Add(time.Hour), coverT0.Add(-24*time.Hour))
	if only.CoveredMS != 0 || len(only.Gaps) != 1 || only.Gaps[0].Why != GapModeOff || len(only.Modes) != 0 {
		t.Fatalf("всё окно в off: %+v", only)
	}
}

// ioReloadTestEvery — шаг записи off в цикле службы (ioReloadEvery в cmd/aguard).
const ioReloadTestEvery = 10 * time.Second

// Окно глубже cases_keep: до границы хранения — before_keep, покрытие начинается не раньше её, даже если
// интервалы старше ещё не срезаны.
func TestCoverBeforeKeep(t *testing.T) {
	c := NewCoverer(CoverRecord{}, coverT0)
	coverMarks(c, coverT0, coverT0.Add(3*time.Hour), 30*time.Second, ModeAlert)
	keepFrom := coverT0.Add(time.Hour)
	got := Cover(c.Record(coverT0.Add(3*time.Hour)), CoverFile, coverT0, coverT0.Add(3*time.Hour), keepFrom)
	coverSum(t, got)
	want := []Gap{{From: coverT0, To: keepFrom, Why: GapBeforeKeep}}
	if !reflect.DeepEqual(got.Gaps, want) || got.CoveredMS != (2*time.Hour).Milliseconds() {
		t.Fatalf("глубже cases_keep: %+v", got)
	}
	// окно целиком глубже границы
	deep := Cover(c.Record(coverT0.Add(3*time.Hour)), CoverFile, coverT0, coverT0.Add(30*time.Minute), keepFrom)
	coverSum(t, deep)
	if deep.CoveredMS != 0 || len(deep.Gaps) != 1 || deep.Gaps[0].Why != GapBeforeKeep || !deep.Gaps[0].To.Equal(coverT0.Add(30*time.Minute)) {
		t.Fatalf("окно глубже границы: %+v", deep)
	}
	// срез в памяти: интервалы целиком до границы уходят, задевшие — обрезаются, начало учёта — не раньше границы
	old := NewCoverer(CoverRecord{Start: coverT0.Add(-time.Hour), Spans: []Span{{From: coverT0.Add(-time.Hour), To: coverT0.Add(-30 * time.Minute), Mode: ModeAlert}}}, coverT0)
	old.Trim(keepFrom)
	if r := old.Record(coverT0); len(r.Spans) != 0 {
		t.Fatalf("интервал целиком до границы остался: %+v", r.Spans)
	}
	c.Trim(keepFrom)
	r := c.Record(coverT0.Add(3 * time.Hour))
	if !r.Start.Equal(keepFrom) || len(r.Spans) != 1 || !r.Spans[0].From.Equal(keepFrom) {
		t.Fatalf("срез по cases_keep: %+v", r)
	}
}

// Без записи учёта — no_record на всё окно, не «0 случаев при полном покрытии»; учёт начат посреди окна —
// до начала no_record, после — по интервалам.
func TestCoverNoRecord(t *testing.T) {
	none := Cover(CoverRecord{}, CoverFile, coverT0, coverT0.Add(time.Hour), coverT0.Add(-24*time.Hour))
	coverSum(t, none)
	if none.CoveredMS != 0 || !reflect.DeepEqual(none.Gaps, []Gap{{From: coverT0, To: coverT0.Add(time.Hour), Why: GapNoRecord}}) {
		t.Fatalf("без учёта: %+v", none)
	}
	c := NewCoverer(CoverRecord{}, coverT0.Add(30*time.Minute))
	c.Mark(coverT0.Add(40*time.Minute), ModeAlert)
	c.Mark(coverT0.Add(time.Hour), ModeAlert) // разрыв: интервал с 40-й минуты
	got := Cover(c.Record(coverT0.Add(time.Hour)), CoverService, coverT0, coverT0.Add(time.Hour), coverT0.Add(-24*time.Hour))
	coverSum(t, got)
	want := []Gap{
		{From: coverT0, To: coverT0.Add(30 * time.Minute), Why: GapNoRecord},
		{From: coverT0.Add(30 * time.Minute), To: coverT0.Add(time.Hour), Why: GapNotRunning},
	}
	if !reflect.DeepEqual(got.Gaps, want) || got.CoveredMS != 0 {
		t.Fatalf("учёт посреди окна: %+v", got)
	}
}

// Учёт начат позже окна — всё окно no_record, пробел не выходит за until.
func TestCoverRecordAfterWindow(t *testing.T) {
	rec := CoverRecord{Start: coverT0.Add(2 * time.Hour), Spans: []Span{}}
	got := Cover(rec, CoverFile, coverT0, coverT0.Add(time.Hour), coverT0.Add(-24*time.Hour))
	coverSum(t, got)
	if !reflect.DeepEqual(got.Gaps, []Gap{{From: coverT0, To: coverT0.Add(time.Hour), Why: GapNoRecord}}) {
		t.Fatalf("учёт позже окна: %+v", got)
	}
}

// Вложенный интервал (часы назад внутри идущего) не укорачивает покрытие; off после пробела не растягивает
// пробел до себя.
func TestCoverNestedAndOffAfterGap(t *testing.T) {
	m := func(from, to int) (time.Time, time.Time) {
		return coverT0.Add(time.Duration(from) * time.Minute), coverT0.Add(time.Duration(to) * time.Minute)
	}
	span := func(from, to int, mode string) Span {
		f, e := m(from, to)
		return Span{From: f, To: e, Mode: mode}
	}
	gap := func(from, to int, why string) Gap {
		f, e := m(from, to)
		return Gap{From: f, To: e, Why: why}
	}
	rec := CoverRecord{Start: coverT0, Spans: []Span{span(0, 10, ModeAlert), span(2, 5, ModeAlert), span(20, 30, ModeAlert), span(40, 50, ModeOff)}}
	got := Cover(rec, CoverService, coverT0, coverT0.Add(time.Hour), coverT0.Add(-24*time.Hour))
	coverSum(t, got)
	want := []Gap{gap(10, 20, GapNotRunning), gap(30, 40, GapNotRunning), gap(40, 50, GapModeOff), gap(50, 60, GapNotRunning)}
	if got.CoveredMS != (20*time.Minute).Milliseconds() || !reflect.DeepEqual(got.Gaps, want) {
		t.Fatalf("вложенный и off после пробела: %+v", got)
	}
}

// Падение службы (kill -9): в файле — копия с концом интервала на момент копии; отчёт по файлу показывает
// хвост после неё как not_running, а не покрытие «до сейчас». Живая служба продлевает открытый интервал
// до сейчас, только если шаг свежий (не дальше CoverGapMax) и цикл не перезапущен.
func TestCoverCrashTailAndOpenSpan(t *testing.T) {
	c := NewCoverer(CoverRecord{}, coverT0)
	coverMarks(c, coverT0, coverT0.Add(time.Hour), time.Second, ModeAlert)
	copyAt := coverT0.Add(time.Hour)
	saved := c.Record(copyAt)
	now := coverT0.Add(90 * time.Minute)
	got := Cover(saved, CoverFile, coverT0, now, coverT0.Add(-24*time.Hour))
	coverSum(t, got)
	if !reflect.DeepEqual(got.Gaps, []Gap{{From: copyAt, To: now, Why: GapNotRunning}}) {
		t.Fatalf("хвост после копии: %+v", got)
	}
	fresh := c.Record(copyAt.Add(CoverGapMax))
	if !fresh.Spans[0].To.Equal(copyAt.Add(CoverGapMax)) {
		t.Fatalf("свежий шаг не продлил интервал до сейчас: %+v", fresh.Spans)
	}
	if stale := c.Record(copyAt.Add(CoverGapMax + time.Millisecond)); !stale.Spans[0].To.Equal(copyAt) {
		t.Fatalf("давний шаг продлил интервал: %+v", stale.Spans)
	}
	c.Break()
	if r := c.Record(copyAt.Add(time.Second)); !r.Spans[0].To.Equal(copyAt) {
		t.Fatalf("после перезапуска цикла интервал продлён: %+v", r.Spans)
	}
}

// Часы прыгнули: вперёд дальше порога — новый интервал (прыжок не покрыт), назад — новый интервал, перекрытие
// по стенным часам не считается дважды.
func TestCoverClockJump(t *testing.T) {
	c := NewCoverer(CoverRecord{}, coverT0)
	coverMarks(c, coverT0, coverT0.Add(10*time.Minute), time.Second, ModeAlert)
	coverMarks(c, coverT0.Add(time.Hour), coverT0.Add(70*time.Minute), time.Second, ModeAlert)      // вперёд на 50 мин
	coverMarks(c, coverT0.Add(65*time.Minute), coverT0.Add(80*time.Minute), time.Second, ModeAlert) // назад на 5 мин
	r := c.Record(coverT0.Add(80 * time.Minute))
	if len(r.Spans) != 3 {
		t.Fatalf("интервалы при прыжках часов: %+v", r.Spans)
	}
	got := Cover(r, CoverService, coverT0, coverT0.Add(80*time.Minute), coverT0.Add(-24*time.Hour))
	coverSum(t, got)
	if got.CoveredMS != (30*time.Minute).Milliseconds() || !reflect.DeepEqual(got.Gaps, []Gap{{From: coverT0.Add(10 * time.Minute), To: coverT0.Add(time.Hour), Why: GapNotRunning}}) {
		t.Fatalf("покрытие при прыжках часов: %+v", got)
	}
}

// Засев из посекундного ряда: секунды подряд (разрыв не дальше порога) — интервал до конца последней секунды,
// начало учёта — первая секунда ряда.
func TestSeedCoverageFromSeries(t *testing.T) {
	var seconds []MachineSecond
	for i := range 120 {
		seconds = append(seconds, MachineSecond{At: coverT0.Add(time.Duration(i) * time.Second)})
	}
	later := coverT0.Add(time.Hour)
	seconds = append(seconds, MachineSecond{At: later}, MachineSecond{At: later.Add(CoverGapMax + time.Second)}) // разрыв ровно порог
	got := SeedCoverage(seconds)
	want := CoverRecord{Start: coverT0, Spans: []Span{
		{From: coverT0, To: coverT0.Add(120 * time.Second), Mode: SpanSeries},
		{From: later, To: later.Add(CoverGapMax + 2*time.Second), Mode: SpanSeries},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("засев:\n%+v\nждали\n%+v", got, want)
	}
	if empty := SeedCoverage(nil); !empty.Start.IsZero() || len(empty.Spans) != 0 {
		t.Fatalf("пустой ряд: %+v", empty)
	}
	cov := Cover(got, CoverFile, coverT0.Add(-time.Hour), later.Add(time.Hour), coverT0.Add(-24*time.Hour))
	coverSum(t, cov)
	if cov.Gaps[0].Why != GapNoRecord || !reflect.DeepEqual(cov.Modes, []string{SpanSeries}) {
		t.Fatalf("покрытие засева: %+v", cov)
	}
}

// Файл учёта: копия и чтение без потерь; нет файла — пусто без ошибки (no_record); битый — явная ошибка.
func TestCoverageFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "coverage.json")
	if r, err := LoadCoverage(path); err != nil || !r.Start.IsZero() || len(r.Spans) != 0 {
		t.Fatalf("нет файла: %+v %v", r, err)
	}
	rec := CoverRecord{Start: coverT0, Spans: []Span{{From: coverT0, To: coverT0.Add(time.Hour), Mode: ModeAlert}, {From: coverT0.Add(2 * time.Hour), To: coverT0.Add(3 * time.Hour), Mode: ModeOff}}}
	if err := SaveCoverage(path, rec); err != nil {
		t.Fatal(err)
	}
	got, err := LoadCoverage(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Start.Equal(rec.Start) || len(got.Spans) != 2 || !got.Spans[1].To.Equal(rec.Spans[1].To) || got.Spans[1].Mode != ModeOff {
		t.Fatalf("после чтения: %+v", got)
	}
	for _, broken := range []string{"{битая", `{"start":"2026-11-09T22:00:00Z","spans":[{"from":"2026-11-09T23:00:00Z","to":"2026-11-09T22:00:00Z","mode":"alert"}]}`, `{"spans":[]}`} {
		if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadCoverage(path); err == nil || !strings.Contains(err.Error(), "учёт покрытия") {
			t.Fatalf("битый файл %q: %v", broken, err)
		}
	}
}
