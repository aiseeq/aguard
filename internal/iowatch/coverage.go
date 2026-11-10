package iowatch

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// Учёт покрытия замера: интервалы, когда цикл замера службы действительно шёл (по его шагам), и когда служба
// стояла в mode=off. Итог на окне отчёта (Cover) отличает ноль случаев от «замера не было».

// CoverGapMax — шаги дальше этого по стенным часам — разрыв учёта, меньше — один интервал. Задержка цикла не
// разрыв: PSI и счётчики накопительные, шаг после задержки меряет её целиком (Recorder.Step берёт настоящий
// интервал), а фриз сам задерживает цикл — порог в несколько шагов выкинул бы из покрытия как раз фризы.
// Граница та же, что у ряда (seriesSpreadMax): длиннее и без простоя — пропуск (сон машины, остановленный
// процесс), а не задержка. Записи off идут раз в ioReloadEvery (10 с) — тоже внутри порога.
const CoverGapMax = seriesSpreadMax

// SpanSeries — режим интервала, засеянного из посекундного ряда: замер шёл, log или alert — неизвестно.
const SpanSeries = "series"

// Причины пробела покрытия.
const (
	GapNotRunning = "not_running" // замера не было: служба не работала, цикл не шёл (нет диска, конфиг не читается, перезапуск)
	GapModeOff    = "mode_off"    // служба работала с mode = off
	GapBeforeKeep = "before_keep" // глубже cases_keep: случаи оттуда срезаны
	GapNoRecord   = "no_record"   // до начала учёта: шёл ли замер — не знаем
)

// Источник учёта в отчёте.
const (
	CoverService = "service" // память службы по сокету
	CoverFile    = "file"    // копия в файле: служба не запущена
)

// Span — интервал учёта [From, To]: To — последний шаг. Mode — log, alert, off или series.
type Span struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	Mode string    `json:"mode"`
}

// CoverRecord — учёт: раньше Start его нет (no_record), с Start — интервалы по времени.
type CoverRecord struct {
	Start time.Time `json:"start"`
	Spans []Span    `json:"spans"`
}

// Coverer — учёт в памяти службы; принадлежит горутине цикла.
type Coverer struct {
	rec  CoverRecord
	open bool // последний интервал идёт: следующий шаг его продлевает
}

// NewCoverer — учёт с прошлой копии; её нет (Start нулевой) — учёт начинается с now.
func NewCoverer(rec CoverRecord, now time.Time) *Coverer {
	if rec.Start.IsZero() {
		rec.Start = now.Round(0)
	}
	return &Coverer{rec: rec}
}

// Mark — шаг замера (или проход в mode=off) в at: продлевает идущий интервал того же режима, если шаг не дальше
// CoverGapMax по стенным часам и не раньше его конца; смена log ↔ alert без разрыва — новый интервал встык; иначе
// (и между off и замером: проход off раз в 10 с, замер — с первого шага) — новый интервал с at (прыжок часов
// назад — тоже новый: перекрытие Cover учтёт один раз).
func (c *Coverer) Mark(at time.Time, mode string) {
	at = at.Round(0) // стенные часы: монотонные не идут во сне машины и не видят перевода часов
	if n := len(c.rec.Spans); c.open && n > 0 {
		last := &c.rec.Spans[n-1]
		if d := at.Sub(last.To); d >= 0 && d <= CoverGapMax {
			if last.Mode == mode {
				last.To = at
				return
			}
			if last.Mode != ModeOff && mode != ModeOff {
				c.rec.Spans = append(c.rec.Spans, Span{From: last.To, To: at, Mode: mode})
				return
			}
		}
	}
	c.rec.Spans = append(c.rec.Spans, Span{From: at, To: at, Mode: mode})
	c.open = true
}

// Break — цикл замера кончился (перезапуск, ошибка, остановка): следующий шаг — новый интервал.
func (c *Coverer) Break() { c.open = false }

// Trim — интервалы до keepFrom срезаются, начало учёта — не раньше keepFrom.
func (c *Coverer) Trim(keepFrom time.Time) {
	keepFrom = keepFrom.Round(0)
	if c.rec.Start.Before(keepFrom) {
		c.rec.Start = keepFrom
	}
	n := 0
	for _, s := range c.rec.Spans {
		if s.To.Before(keepFrom) {
			continue
		}
		if s.From.Before(keepFrom) {
			s.From = keepFrom
		}
		c.rec.Spans[n] = s
		n++
	}
	c.rec.Spans = c.rec.Spans[:n]
}

// Record — копия учёта на now: идущий интервал со свежим шагом (не дальше CoverGapMax) — до now.
func (c *Coverer) Record(now time.Time) CoverRecord {
	out := CoverRecord{Start: c.rec.Start, Spans: slices.Clone(c.rec.Spans)}
	if out.Spans == nil {
		out.Spans = []Span{}
	}
	now = now.Round(0)
	if n := len(out.Spans); c.open && n > 0 {
		if d := now.Sub(out.Spans[n-1].To); d > 0 && d <= CoverGapMax {
			out.Spans[n-1].To = now
		}
	}
	return out
}

// SeedCoverage — учёт из посекундного ряда (до появления учёта): секунда ряда есть — замер шёл; секунды
// с разрывом не дальше CoverGapMax — один интервал до конца последней. Пустой ряд — пустой учёт.
func SeedCoverage(seconds []MachineSecond) CoverRecord {
	rec := CoverRecord{Spans: []Span{}}
	for i := range seconds {
		from := seconds[i].At.Round(0)
		to := from.Add(time.Second)
		if n := len(rec.Spans); n > 0 {
			if d := from.Sub(rec.Spans[n-1].To); d >= 0 && d <= CoverGapMax {
				rec.Spans[n-1].To = to
				continue
			}
		}
		rec.Spans = append(rec.Spans, Span{From: from, To: to, Mode: SpanSeries})
	}
	if len(rec.Spans) > 0 {
		rec.Start = rec.Spans[0].From
	}
	return rec
}

const whatCoverage = "учёт покрытия"

// SaveCoverage пишет учёт в path целиком (через временный файл и переименование).
func SaveCoverage(path string, rec CoverRecord) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("папка %s: %w", whatCoverage, err)
	}
	err := writeAtomic(path, func(w *bufio.Writer) error { return json.NewEncoder(w).Encode(&rec) })
	if err != nil {
		return fmt.Errorf("%s %s: %w", whatCoverage, path, err)
	}
	return nil
}

// LoadCoverage читает учёт из path; файла нет — пустой учёт (Start нулевой: no_record на всё). Битый JSON, нет
// начала или интервал с концом раньше начала — ошибка: пустое покрытие вместо неё выдало бы «не знаем» за порчу.
func LoadCoverage(path string) (CoverRecord, error) {
	var rec CoverRecord
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return rec, nil
	}
	if err != nil {
		return rec, fmt.Errorf("%s: %w", whatCoverage, err)
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return CoverRecord{}, fmt.Errorf("%s %s: %w", whatCoverage, path, err)
	}
	if rec.Start.IsZero() {
		return CoverRecord{}, fmt.Errorf("%s %s: нет start", whatCoverage, path)
	}
	for i, s := range rec.Spans {
		if s.To.Before(s.From) {
			return CoverRecord{}, fmt.Errorf("%s %s: интервал %d кончается раньше начала", whatCoverage, path, i+1)
		}
	}
	return rec, nil
}

// Gap — пробел покрытия [From, To) с причиной.
type Gap struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	Why  string    `json:"why"`
}

// Coverage — покрытие окна [Since, Until) замером: CoveredMS + длины Gaps = WindowMS. Modes — режимы замера
// в окне (log, alert, series).
type Coverage struct {
	Source    string    `json:"source"`
	Since     time.Time `json:"since"`
	Until     time.Time `json:"until"`
	CoveredMS int64     `json:"covered_ms"`
	WindowMS  int64     `json:"window_ms"`
	Modes     []string  `json:"modes"`
	Gaps      []Gap     `json:"gaps"`
}

// interval — [from, to) для объединения интервалов.
type interval struct{ from, to time.Time }

// Cover — покрытие окна [since, until) по учёту rec: до keepFrom — before_keep, до начала учёта — no_record,
// дальше — интервалы замера; между ними — mode_off там, где служба стояла в off, иначе not_running.
func Cover(rec CoverRecord, source string, since, until, keepFrom time.Time) Coverage {
	since, until, keepFrom = since.Round(0), until.Round(0), keepFrom.Round(0)
	c := Coverage{Source: source, Since: since, Until: until, WindowMS: until.Sub(since).Milliseconds(), Modes: []string{}, Gaps: []Gap{}}
	cur := since
	if keepFrom.After(cur) {
		end := minTime(keepFrom, until)
		c.gap(cur, end, GapBeforeKeep)
		cur = end
	}
	if rec.Start.IsZero() || rec.Start.After(cur) {
		end := until
		if !rec.Start.IsZero() {
			end = minTime(rec.Start, until)
		}
		c.gap(cur, end, GapNoRecord)
		cur = maxTime(cur, end)
	}
	if !cur.Before(until) {
		return c
	}
	var measured, off []interval
	for _, s := range rec.Spans {
		from, to := maxTime(s.From, cur), minTime(s.To, until)
		if !to.After(from) {
			continue
		}
		if s.Mode == ModeOff {
			off = append(off, interval{from, to})
			continue
		}
		measured = append(measured, interval{from, to})
		if !slices.Contains(c.Modes, s.Mode) {
			c.Modes = append(c.Modes, s.Mode)
		}
	}
	slices.Sort(c.Modes)
	off = union(off)
	for _, m := range union(measured) {
		c.uncovered(cur, m.from, off)
		c.CoveredMS += m.to.Sub(m.from).Milliseconds()
		cur = m.to
	}
	c.uncovered(cur, until, off)
	return c
}

// uncovered — пробел [from, to) без замера: части под off — mode_off, остальное — not_running.
func (c *Coverage) uncovered(from, to time.Time, off []interval) {
	for _, o := range off {
		if !o.to.After(from) || !o.from.Before(to) {
			continue
		}
		start := maxTime(o.from, from)
		c.gap(from, start, GapNotRunning)
		end := minTime(o.to, to)
		c.gap(start, end, GapModeOff)
		from = end
	}
	c.gap(from, to, GapNotRunning)
}

// gap — пробел [from, to); пустой не пишется. Встык с прошлым той же причины не бывает: замер между пробелами
// не пуст (пустые интервалы Cover пропускает), off объединены union.
func (c *Coverage) gap(from, to time.Time, why string) {
	if !to.After(from) {
		return
	}
	c.Gaps = append(c.Gaps, Gap{From: from, To: to, Why: why})
}

// union — интервалы по началу, перекрытые и встык — слиты.
func union(list []interval) []interval {
	slices.SortFunc(list, func(a, b interval) int { return a.from.Compare(b.from) })
	var out []interval
	for _, iv := range list {
		if n := len(out); n > 0 && !iv.from.After(out[n-1].to) {
			out[n-1].to = maxTime(out[n-1].to, iv.to)
			continue
		}
		out = append(out, iv)
	}
	return out
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
