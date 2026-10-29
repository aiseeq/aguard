package iowatch

import (
	"slices"
	"time"
)

// Case — сводка случая фриза: подряд шаги с простоем (Stalled), спокойные промежутки до gap случай
// не делят. Хранится только сводка: машина за случай и виновники; сырые шаги остаются в буфере.
type Case struct {
	Start     time.Time `json:"start"`  // начало первого шага с простоем
	End       time.Time `json:"end"`    // конец последнего шага с простоем
	DurMS     int64     `json:"dur_ms"` // End − Start, по стене
	IdleMS    int64     `json:"idle_ms"`
	PeakMS    int64     `json:"peak_ms"` // наибольший простой шага
	IOFullMS  int64     `json:"io_full_ms,omitempty"`
	IOSomeMS  int64     `json:"io_some_ms,omitempty"`
	MemFullMS int64     `json:"mem_full_ms,omitempty"`
	MemSomeMS int64     `json:"mem_some_ms,omitempty"`
	SwapIn    int64     `json:"swapin,omitempty"` // страниц
	SwapOut   int64     `json:"swapout,omitempty"`
	Majflt    int64     `json:"majflt,omitempty"`
	ReadB     int64     `json:"rd_b,omitempty"`
	WriteB    int64     `json:"wr_b,omitempty"`
	BusyMS    int64     `json:"busy_ms,omitempty"`    // занятость устройства (io_ticks)
	RAwaitMS  int64     `json:"r_await_ms,omitempty"` // среднее ожидание операции чтения
	WAwaitMS  int64     `json:"w_await_ms,omitempty"`
	InFlight  int64     `json:"inflight,omitempty"` // наибольшая очередь устройства
	Flushes   int64     `json:"flushes,omitempty"`
	Dirty     int64     `json:"dirty,omitempty"` // наибольшие Dirty и Writeback за окно виновников, байт
	Writeback int64     `json:"writeback,omitempty"`
	// Quiet — обмен с диском за случай меньше quiet_bytes: простой без трафика (одна задача ждёт
	// датчик, как thinkfan) — PSI считает его 100 % full, а машина не стоит. В письма не идёт.
	Quiet    bool     `json:"quiet,omitempty"`
	Culprits *Culprit `json:"culprits,omitempty"` // только у случаев от case_min
}

// Culprit — виновники случая за окно From..End: до caseTop имён на меру.
type Culprit struct {
	From         time.Time `json:"from"`
	Main         string    `json:"main,omitempty"` // путь cgroup с наибольшим I/O
	ProcWrite    []Ranked  `json:"proc_write,omitempty"`
	ProcRead     []Ranked  `json:"proc_read,omitempty"`
	ProcMajflt   []Ranked  `json:"proc_majflt,omitempty"`
	CgroupWrite  []Ranked  `json:"cgroup_write,omitempty"`
	CgroupRead   []Ranked  `json:"cgroup_read,omitempty"`
	CgroupSwapIn []Ranked  `json:"cgroup_swapin,omitempty"`
	CgroupIdle   []Ranked  `json:"cgroup_idle,omitempty"` // мс PSI full io
}

const caseTop = 5

// Dur — длительность случая.
func (c *Case) Dur() time.Duration { return time.Duration(c.DurMS) * time.Millisecond }

// stepSum — сумма быстрых шагов.
type stepSum struct {
	idle, peak, ioFull, ioSome, memFull, memSome  int64
	swapIn, swapOut, majflt, rd, wr, ticks, flush int64
	rms, rios, wms, wios, inflight                int64
}

func (t *stepSum) add(s Step) {
	idle := Idle(s)
	t.idle += idle
	t.peak = max(t.peak, idle)
	t.ioFull += s.IOFullUS
	t.ioSome += s.IOSomeUS
	t.memFull += s.MemFullUS
	t.memSome += s.MemSomeUS
	t.swapIn += s.SwapIn
	t.swapOut += s.SwapOut
	t.majflt += s.Majflt
	t.rd += s.ReadBytes
	t.wr += s.WriteBytes
	t.ticks += s.IOTicks
	t.flush += s.FIOS
	t.rms += s.RMS
	t.rios += s.RIOS
	t.wms += s.WMS
	t.wios += s.WIOS
	t.inflight = max(t.inflight, s.InFlight)
}

func (t *stepSum) merge(o stepSum) {
	t.idle += o.idle
	t.peak = max(t.peak, o.peak)
	t.ioFull += o.ioFull
	t.ioSome += o.ioSome
	t.memFull += o.memFull
	t.memSome += o.memSome
	t.swapIn += o.swapIn
	t.swapOut += o.swapOut
	t.majflt += o.majflt
	t.rd += o.rd
	t.wr += o.wr
	t.ticks += o.ticks
	t.flush += o.flush
	t.rms += o.rms
	t.rios += o.rios
	t.wms += o.wms
	t.wios += o.wios
	t.inflight = max(t.inflight, o.inflight)
}

// openCase — идущий случай: шаги с простоем и спокойные между ними (calm — после последнего с
// простоем: войдут, если случай продлится).
type openCase struct {
	start, last time.Time
	sum, calm   stepSum
}

func (o *openCase) toCase() Case {
	t := o.sum
	return Case{Start: o.start, End: o.last, DurMS: o.last.Sub(o.start).Milliseconds(),
		IdleMS: t.idle / 1000, PeakMS: t.peak / 1000, IOFullMS: t.ioFull / 1000, IOSomeMS: t.ioSome / 1000,
		MemFullMS: t.memFull / 1000, MemSomeMS: t.memSome / 1000, SwapIn: t.swapIn, SwapOut: t.swapOut, Majflt: t.majflt,
		ReadB: t.rd, WriteB: t.wr, BusyMS: t.ticks, RAwaitMS: AwaitMS(t.rms, t.rios), WAwaitMS: AwaitMS(t.wms, t.wios),
		InFlight: t.inflight, Flushes: t.flush}
}

// Recorder — кольцевой буфер сырых записей и случаи фриза: один для службы, `aguard io record` и
// `aguard io import`. Принадлежит одной горутине (главный цикл замера).
//
// Шаг с простоем открывает случай или продлевает его; случай закрывается, когда после последнего
// шага с простоем прошло больше gap. Виновники нужны из детальных записей до конца случая, а запись
// за последнюю секунду приходит позже: закрытый случай ждёт detail и отдаётся с виновниками.
type Recorder struct {
	cfg     Config
	ring    *Ring
	open    *openCase
	waiting []Case // закрыты, ждут детальной записи за свой конец
}

func NewRecorder(cfg Config) *Recorder {
	return &Recorder{cfg: cfg, ring: NewRing(cfg)}
}

// SetConfig меняет пороги на ходу; размер буфера — с новым Recorder.
func (r *Recorder) SetConfig(cfg Config) { r.cfg = cfg }

// Step кладёт быстрый шаг; готовые случаи (с виновниками) возвращаются.
func (r *Recorder) Step(s Step) []Case {
	r.ring.Add(Record{Kind: KindStep, Step: s})
	step := r.cfg.Step.D()
	if r.open != nil && s.At.Sub(r.open.last) > r.cfg.Gap.D()+step {
		r.waiting = append(r.waiting, r.open.toCase())
		r.open = nil
	}
	switch {
	case Stalled(s, r.cfg) && r.open == nil:
		r.open = &openCase{start: s.At.Add(-step), last: s.At}
		r.open.sum.add(s)
	case Stalled(s, r.cfg):
		r.open.sum.merge(r.open.calm)
		r.open.sum.add(s)
		r.open.calm, r.open.last = stepSum{}, s.At
	case r.open != nil:
		r.open.calm.add(s)
	}
	return r.ready(s.At.Add(-r.cfg.Detail.D() - step))
}

// Detail кладёт детальную запись в буфер.
func (r *Recorder) Detail(s Step) { r.ring.Add(Record{Kind: KindDetail, Step: s}) }

// ready — закрытые случаи с концом не позже covered (детальные записи за них уже в буфере).
func (r *Recorder) ready(covered time.Time) []Case {
	n := 0
	for n < len(r.waiting) && !r.waiting[n].End.After(covered) {
		n++
	}
	if n == 0 {
		return nil
	}
	out := slices.Clone(r.waiting[:n])
	r.waiting = slices.Delete(r.waiting, 0, n)
	for i := range out {
		r.finish(&out[i])
	}
	return out
}

// finish — Dirty, Writeback и (от case_min) виновники из детальных записей окна случая.
func (r *Recorder) finish(c *Case) {
	c.Quiet = c.ReadB+c.WriteB < r.cfg.QuietBytes
	from := c.Start.Add(-r.cfg.Context.D())
	details := r.ring.Details(from, c.End.Add(r.cfg.Detail.D()))
	for i := range details {
		c.Dirty = max(c.Dirty, details[i].Dirty)
		c.Writeback = max(c.Writeback, details[i].Writeback)
	}
	if c.Dur() < r.cfg.CaseMin.D() {
		return
	}
	sum := SumCulprits(details)
	c.Culprits = &Culprit{From: from, Main: sum.MainCgroup(),
		ProcWrite: Rank(sum.ProcWrite, caseTop), ProcRead: Rank(sum.ProcRead, caseTop), ProcMajflt: Rank(sum.ProcMajflt, caseTop),
		CgroupWrite: Rank(sum.CgroupWrite, caseTop), CgroupRead: Rank(sum.CgroupRead, caseTop),
		CgroupSwapIn: Rank(sum.CgroupSwapIn, caseTop), CgroupIdle: Rank(sum.CgroupIdle, caseTop)}
}

// Close при остановке закрывает идущий случай и отдаёт все ждущие с тем, что есть в буфере.
func (r *Recorder) Close() []Case {
	if r.open != nil {
		r.waiting = append(r.waiting, r.open.toCase())
		r.open = nil
	}
	return r.ready(time.Unix(1<<62, 0))
}

// Dump — ручной снимок буфера в dir/manual-<at>.jsonl. Файл появляется под этим именем, когда
// записан целиком; Wait — дождаться этого.
func (r *Recorder) Dump(dir string, at time.Time) *Snapshot {
	return startSnapshot(dir, at, r.ring.Records())
}
