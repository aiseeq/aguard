package iowatch

import (
	"math"
	"time"
)

// MachineSecond — секунда машины из быстрых шагов: простой и байты — сумма, очередь — пик, ожидание
// записи — среднее за секунду (WMS/WIOS), Stall — мс шагов с простоем (Stalled); Dirty и Writeback —
// последние из детального прохода.
type MachineSecond struct {
	At          time.Time `json:"at"`
	IOFullMS    int64     `json:"io_full_ms,omitempty"`
	IOSomeMS    int64     `json:"io_some_ms,omitempty"`
	MemFullMS   int64     `json:"mem_full_ms,omitempty"`
	StallMS     int64     `json:"stall_ms,omitempty"`
	ReadB       int64     `json:"rd_b,omitempty"`
	WriteB      int64     `json:"wr_b,omitempty"`
	WAwaitMS    int64     `json:"w_await_ms,omitempty"`
	InFlightMax int64     `json:"inflight_max,omitempty"`
	Flushes     int64     `json:"flushes,omitempty"`
	Dirty       int64     `json:"dirty,omitempty"`
	Writeback   int64     `json:"writeback,omitempty"`
	SwapIn      int64     `json:"swapin,omitempty"`
	Majflt      int64     `json:"majflt,omitempty"`
}

// secondRow — секунда ряда в памяти: 72 байта (байты диска — int64, остальное — int32, Dirty и
// Writeback — КиБ, как в meminfo). Сутки — 86 400 строк, около 6 МБ.
type secondRow struct {
	at, readB, writeB                                int64 // at — unix-секунды
	ioFull, ioSome, memFull, stall, wAwait, inflight int32
	flushes, swapIn, majflt, dirtyKiB, writebackKiB  int32
}

// i32 — значение в int32 с насыщением: ряд не врёт знаком на редком выбросе.
func i32(v int64) int32 { return int32(max(math.MinInt32, min(math.MaxInt32, v))) }

func (r *secondRow) second() MachineSecond {
	return MachineSecond{At: time.Unix(r.at, 0), IOFullMS: int64(r.ioFull), IOSomeMS: int64(r.ioSome), MemFullMS: int64(r.memFull),
		StallMS: int64(r.stall), ReadB: r.readB, WriteB: r.writeB, WAwaitMS: int64(r.wAwait), InFlightMax: int64(r.inflight),
		Flushes: int64(r.flushes), Dirty: int64(r.dirtyKiB) << 10, Writeback: int64(r.writebackKiB) << 10,
		SwapIn: int64(r.swapIn), Majflt: int64(r.majflt)}
}

func rowOf(s *MachineSecond) secondRow {
	return secondRow{at: s.At.Unix(), readB: s.ReadB, writeB: s.WriteB, ioFull: i32(s.IOFullMS), ioSome: i32(s.IOSomeMS),
		memFull: i32(s.MemFullMS), stall: i32(s.StallMS), wAwait: i32(s.WAwaitMS), inflight: i32(s.InFlightMax),
		flushes: i32(s.Flushes), swapIn: i32(s.SwapIn), majflt: i32(s.Majflt), dirtyKiB: i32(s.Dirty >> 10),
		writebackKiB: i32(s.Writeback >> 10)}
}

// secondAcc — идущая секунда: суммы быстрых шагов в их единицах.
type secondAcc struct {
	at                                               int64 // unix-секунды; 0 — секунды нет
	ioFull, ioSome, memFull, stall                   int64 // мкс
	rd, wr, wms, wios, inflight, flush, swap, majflt int64
}

// Series — посекундный ряд машины за keep: кольцо строк, свёрнутых из быстрых шагов. Принадлежит
// горутине замера; живёт дольше одного цикла (служба передаёт его новому Recorder).
type Series struct {
	rows             []secondRow
	head, count      int
	keep             time.Duration
	cur              secondAcc
	dirty, writeback int64 // последние из детального прохода
}

// NewSeries — ряд на keep секунд; память под кольцо выделяется сразу.
func NewSeries(keep time.Duration) *Series {
	return &Series{rows: make([]secondRow, max(1, int(keep/time.Second))), keep: keep}
}

// step сворачивает быстрый шаг в его секунду (по At, как Window.Seconds); шаг следующей секунды
// закрывает идущую. stallUS — длительность шага, если он с простоем, иначе 0.
func (s *Series) step(st *Step, stallUS int64) {
	sec := st.At.Unix()
	if s.cur.at != 0 && sec != s.cur.at {
		s.flush()
	}
	c := &s.cur
	c.at = sec
	c.ioFull += st.IOFullUS
	c.ioSome += st.IOSomeUS
	c.memFull += st.MemFullUS
	c.stall += stallUS
	c.rd += st.ReadBytes
	c.wr += st.WriteBytes
	c.wms += st.WMS
	c.wios += st.WIOS
	c.inflight = max(c.inflight, st.InFlight)
	c.flush += st.FIOS
	c.swap += st.SwapIn
	c.majflt += st.Majflt
}

// detail — Dirty и Writeback детального прохода: идут в секунды, закрытые после него.
func (s *Series) detail(dirty, writeback int64) { s.dirty, s.writeback = dirty, writeback }

// flush — идущая секунда в кольцо.
func (s *Series) flush() {
	c := &s.cur
	s.push(secondRow{at: c.at, readB: c.rd, writeB: c.wr, ioFull: i32(c.ioFull / 1000), ioSome: i32(c.ioSome / 1000),
		memFull: i32(c.memFull / 1000), stall: i32(c.stall / 1000), wAwait: i32(AwaitMS(c.wms, c.wios)), inflight: i32(c.inflight),
		flushes: i32(c.flush), swapIn: i32(c.swap), majflt: i32(c.majflt), dirtyKiB: i32(s.dirty >> 10), writebackKiB: i32(s.writeback >> 10)})
	s.cur = secondAcc{}
}

// push кладёт строку и вытесняет строки старше keep от неё.
func (s *Series) push(row secondRow) {
	if s.count == len(s.rows) {
		s.head = (s.head + 1) % len(s.rows)
		s.count--
	}
	s.rows[(s.head+s.count)%len(s.rows)] = row
	s.count++
	cutoff := row.at - int64(s.keep/time.Second)
	for s.count > 0 && s.rows[s.head].at < cutoff {
		s.head = (s.head + 1) % len(s.rows)
		s.count--
	}
}

// Add — секунда из файла копии (при старте службы), по порядку времени; старше уже лежащих — не берётся.
func (s *Series) Add(sec MachineSecond) {
	row := rowOf(&sec)
	if s.count > 0 && row.at <= s.rows[(s.head+s.count-1)%len(s.rows)].at {
		return
	}
	s.push(row)
}

// Len — сколько закрытых секунд в ряду.
func (s *Series) Len() int { return s.count }

// Rows — закрытые секунды с At не раньше since: копия строк (дешёвая, для горутины замера),
// развернуть — SeriesRows.Seconds в своей горутине.
func (s *Series) Rows(since time.Time) SeriesRows {
	from := since.Unix()
	if since.IsZero() {
		from = math.MinInt64
	}
	out := make(SeriesRows, 0, s.count)
	for i := range s.count {
		if row := s.rows[(s.head+i)%len(s.rows)]; row.at >= from {
			out = append(out, row)
		}
	}
	return out
}

// Resize — ряд на новый keep с сохранением строк, которые в него влезают.
func (s *Series) Resize(keep time.Duration) *Series {
	next := NewSeries(keep)
	for _, row := range s.Rows(time.Time{}) {
		next.push(row)
	}
	next.cur, next.dirty, next.writeback = s.cur, s.dirty, s.writeback
	return next
}

// SeriesRows — копия строк ряда.
type SeriesRows []secondRow

// Seconds — строки в секунды для ответа и файла.
func (rows SeriesRows) Seconds() []MachineSecond {
	out := make([]MachineSecond, len(rows))
	for i := range rows {
		out[i] = rows[i].second()
	}
	return out
}

// Keep — срок ряда.
func (s *Series) Keep() time.Duration { return s.keep }
