package iowatch

import (
	"math"
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
	Quiet bool `json:"quiet,omitempty"`
	// Capped — ожидание под потолком io.max, а не фриз (cappedWait): только у случаев от case_min. OutsideIdleMS —
	// наибольший простой cgroup вне потолка за случай. В письма и в счёт фризов не идёт.
	Capped        bool     `json:"capped,omitempty"`
	OutsideIdleMS int64    `json:"outside_idle_ms,omitempty"`
	Culprits      *Culprit `json:"culprits,omitempty"` // только у случаев от case_min
}

// Culprit — виновники случая за окно From..End: до caseTop имён на меру.
type Culprit struct {
	From           time.Time                 `json:"from"`
	Main           string                    `json:"main,omitempty"`       // путь cgroup с наибольшим I/O
	MainValue      int64                     `json:"main_value,omitempty"` // мера, по которой главная cgroup выбрана главной
	MainWrite      int64                     `json:"main_write,omitempty"` // запись процессов главной cgroup
	MainRead       int64                     `json:"main_read,omitempty"`  // чтение главной cgroup
	ProcWrite      []Ranked                  `json:"proc_write,omitempty"`
	ProcRead       []Ranked                  `json:"proc_read,omitempty"`
	ProcMajflt     []Ranked                  `json:"proc_majflt,omitempty"`
	ProcessDetails map[string]ProcessDetails `json:"process_details,omitempty"`
	CgroupWrite    []Ranked                  `json:"cgroup_write,omitempty"`
	CgroupRead     []Ranked                  `json:"cgroup_read,omitempty"`
	CgroupSwapIn   []Ranked                  `json:"cgroup_swapin,omitempty"`
	CgroupIdle     []Ranked                  `json:"cgroup_idle,omitempty"` // мс PSI full io
	CappedIdle     []Ranked                  `json:"capped_idle,omitempty"` // мс PSI full io под потолками, в которые упирались
	// CgroupDirty — запись процессов cgroup (write_bytes, данные и метаданные): кто порождает запись.
	CgroupDirty []Ranked `json:"cgroup_dirty,omitempty"`
	// CgroupOwn — запись процессов для каждой своей (Readable) cgroup из CgroupWrite: io.stat намного больше своей —
	// коммит btrfs по fsync; cgroup чужого uid здесь нет — её своя запись не видна.
	// Пустая — тоже в JSON: nil после чтения — случай до 30.10 без пометок.
	CgroupOwn map[string]int64 `json:"cgroup_own"`
	// Foreign — cgroup чужого uid, у которой I/O по io.stat больше всей меры главной (Culprits.ForeignOver): главная неточна.
	Foreign Ranked `json:"foreign,omitzero"`
	// DeviceWrite и KernelWrite — запись устройства за окно и её часть вне листьев cgroup: потоки ядра (writeback,
	// коммит и метаданные btrfs) в корневой cgroup.
	DeviceWrite int64 `json:"device_wr,omitempty"`
	KernelWrite int64 `json:"kernel_wr,omitempty"`
	CleanerMS   int64 `json:"cleaner_ms,omitempty"`
}

// ReadMostly — главная cgroup читала больше, чем писали её процессы.
func (c *Culprit) ReadMostly() bool { return c != nil && c.MainRead > c.MainWrite }

// KernelOver — запись вне cgroup перевешивает главную cgroup и не меньше чужой; без обеих мер любой фон ядра виден.
func (c *Culprit) KernelOver() bool {
	return c != nil && (c.MainValue > 0 && c.KernelWrite > c.MainValue || c.Foreign.Value > 0 && c.KernelWrite > c.Foreign.Value || c.Main == "" && c.MainValue == 0 && c.Foreign.Value == 0 && c.KernelWrite > 0) && c.KernelWrite >= c.Foreign.Value
}

// cleanerBusyMS — CPU btrfs-cleaner за окно, с которого он виновник фона ядра: за 17 дней (до 03.11) поток набрал
// 498 с, в среднем ~1 мс на 10-секундное окно, а при удалении снапшотов 03.11 01:22 — 2,4 с за минуту.
const cleanerBusyMS = 100

// SnapshotDeletion — btrfs-cleaner (удаление снапшотов и subvolume) работал не меньше cleanerBusyMS, пока фон ядра
// перевешивал cgroup.
func (c *Culprit) SnapshotDeletion() bool { return c.KernelOver() && c.CleanerMS >= cleanerBusyMS }

// KernelPercent — доля фона ядра в записи устройства за окно, округлённая; записи нет — 0.
func (c *Culprit) KernelPercent() int64 {
	if c.DeviceWrite <= 0 {
		return 0
	}
	return (100*c.KernelWrite + c.DeviceWrite/2) / c.DeviceWrite
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
	lastAt  time.Time
	hasStep bool
	waiting []Case // закрыты, ждут детальной записи за свой конец
	// Ряд, профили и живое состояние службы (Attach); у `aguard io record` и import их нет.
	series   *Series
	profiles *Profiles
	live     *Live
}

func NewRecorder(cfg Config) *Recorder {
	return &Recorder{cfg: cfg, ring: NewRing(cfg)}
}

// SetConfig меняет пороги на ходу; размер буфера — с новым Recorder.
func (r *Recorder) SetConfig(cfg Config) { r.cfg = cfg }

// Attach — посекундный ряд, профили cgroup и живое состояние службы: они живут дольше цикла замера,
// каждый новый Recorder получает те же. nil — не копить.
func (r *Recorder) Attach(series *Series, profiles *Profiles, live *Live) {
	r.series, r.profiles, r.live = series, profiles, live
}

// Step кладёт быстрый шаг; готовые случаи (с виновниками) возвращаются.
func (r *Recorder) Step(s Step) []Case {
	r.ring.Add(Record{Kind: KindStep, Step: s})
	step := r.cfg.Step.D()
	interval := step
	if r.hasStep {
		interval = max(s.At.Sub(r.lastAt), 0)
	}
	r.lastAt, r.hasStep = s.At, true
	stalled := Stalled(s, r.cfg, interval)
	var closed liveCase
	if r.open != nil {
		sinceLastStall := s.At.Sub(r.open.last)
		closeAfter := r.cfg.Gap.D() + step
		if stalled {
			// Простой текущего интервала связывает его с предыдущим шагом, а не с At.
			sinceLastStall -= interval
			closeAfter = r.cfg.Gap.D()
		}
		if sinceLastStall > closeAfter {
			closed = liveCase{start: r.open.start, end: r.open.last}
			r.waiting = append(r.waiting, r.open.toCase())
			r.open = nil
		}
	}
	switch {
	case stalled && r.open == nil:
		r.open = &openCase{start: s.At.Add(-interval), last: s.At}
		r.open.sum.add(s)
	case stalled:
		r.open.sum.merge(r.open.calm)
		r.open.sum.add(s)
		r.open.calm, r.open.last = stepSum{}, s.At
	case r.open != nil:
		r.open.calm.add(s)
	}
	var stallUS int64
	if stalled {
		stallUS = interval.Microseconds()
	}
	if r.series != nil {
		r.series.step(&s, stallUS, interval)
	}
	if r.profiles != nil {
		r.profiles.step(&s)
	}
	if r.live != nil {
		var open time.Time
		if r.open != nil {
			open = r.open.start
		}
		r.live.step(&s, stallUS, interval, open, closed)
	}
	return r.ready(s.At.Add(-r.cfg.Detail.D() - step))
}

// Detail кладёт детальную запись в буфер, в ряд (Dirty, Writeback, память), профили и живое состояние.
func (r *Recorder) Detail(s Step) {
	r.ring.Add(Record{Kind: KindDetail, Step: s})
	if r.series != nil {
		r.series.detail(&s)
	}
	if r.profiles != nil {
		r.profiles.detail(&s, r.cfg.Detail.D())
	}
	if r.live != nil {
		r.live.detail(&s, r.cfg.Detail.D())
	}
}

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
	procWrite, procRead := Rank(sum.ProcWrite, caseTop), Rank(sum.ProcRead, caseTop)
	processDetails := make(map[string]ProcessDetails)
	for _, top := range [][]Ranked{procWrite, procRead} {
		for _, p := range top {
			if info, ok := sum.ProcessDetails[p.Name]; ok {
				processDetails[p.Name] = info
			}
		}
	}
	main := sum.MainCgroup()
	c.Culprits = &Culprit{From: from, Main: main, MainValue: sum.cgroupTotal[main], MainWrite: sum.cgroupWrite[main], MainRead: sum.cgroupRead[main],
		ProcWrite: procWrite, ProcRead: procRead, ProcMajflt: Rank(sum.ProcMajflt, caseTop),
		ProcessDetails: processDetails,
		CgroupWrite:    Rank(sum.CgroupWrite, caseTop), CgroupRead: Rank(sum.CgroupRead, caseTop),
		CgroupSwapIn: Rank(sum.CgroupSwapIn, caseTop), CgroupIdle: Rank(sum.CgroupIdle, caseTop),
		CgroupDirty: Rank(sum.CgroupDirty, caseTop), CgroupOwn: map[string]int64{}}
	c.Culprits.Foreign, _ = sum.ForeignOver(c.Culprits.Main)
	c.Capped, c.OutsideIdleMS, c.Culprits.CappedIdle = cappedWait(details, c.Start, c.End, r.cfg.Detail.D(), c.DurMS, c.BusyMS)
	for i := range details {
		c.Culprits.CleanerMS += details[i].CleanerMS
	}
	for _, g := range c.Culprits.CgroupWrite {
		if sum.Readable[g.Name] {
			c.Culprits.CgroupOwn[g.Name] = sum.CgroupDirty[g.Name]
		}
	}
	if len(details) > 0 { // детальная запись — дельта за (At − Detail, At]
		c.Culprits.DeviceWrite = r.ring.DeviceWrite(details[0].At.Add(-r.cfg.Detail.D()), details[len(details)-1].At)
		var cgroups int64
		for _, v := range sum.CgroupWrite {
			cgroups += v
		}
		c.Culprits.KernelWrite = max(0, c.Culprits.DeviceWrite-cgroups)
	}
}

// Close при остановке закрывает идущий случай и отдаёт все ждущие с тем, что есть в буфере.
func (r *Recorder) Close() []Case {
	if r.open != nil {
		r.waiting = append(r.waiting, r.open.toCase())
		r.open = nil
	}
	return r.ready(farFuture)
}

// farFuture — позже конца любого случая.
var farFuture = time.Unix(math.MaxInt64>>1, 0)

// Dump — ручной снимок буфера в dir/manual-<at>.jsonl. Файл появляется под этим именем, когда
// записан целиком; Wait — дождаться этого.
func (r *Recorder) Dump(dir string, at time.Time) *Snapshot {
	return startSnapshot(dir, at, r.ring.Records())
}
