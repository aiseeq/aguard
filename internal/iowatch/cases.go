package iowatch

import (
	"maps"
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
	// Quiet — обмен с диском за случай меньше quiet_bytes и memory full меньше половины случая: простой без трафика
	// (одна задача ждёт датчик, как thinkfan) — PSI считает его 100 % full, а машина не стоит. В письма не идёт.
	// Простой памяти без обмена (reclaim, сжатие) — не тихий: у 219 тихих случаев от 1 с по cases.jsonl на 10.11
	// доля memory full не выше 31 % (io-memory-only-quiet).
	Quiet bool `json:"quiet,omitempty"`
	// Capped — ожидание под потолком io.max, а не фриз (cappedWait): только у случаев от case_min. OutsideIdleMS —
	// максимум суммы полных PSI-дельт каждой cgroup вне потолка по detail-интервалам, пересекающим случай; может быть больше DurMS.
	// В письма и в счёт фризов не идёт.
	Capped        bool  `json:"capped,omitempty"`
	OutsideIdleMS int64 `json:"outside_idle_ms,omitempty"`
	// Waiting — диск занят малую долю случая, а cgroup провела в PSI full io не менее 60% окна деталей.
	// Это локальное ожидание cgroup, не фриз машины.
	Waiting  bool     `json:"waiting,omitempty"`
	Culprits *Culprit `json:"culprits,omitempty"` // только у случаев от case_min
	// OwnerOffline — хозяин случая был не в сети, когда маршрут решал (ioroute): засев после перезапуска повторяет
	// тогдашнее решение, а не по сессиям в сети сейчас.
	OwnerOffline bool `json:"owner_offline,omitempty"`
}

// Culprit — виновники случая за окно From..End: до caseTop имён на меру.
type Culprit struct {
	From           time.Time                 `json:"from"`
	Main           string                    `json:"main,omitempty"`             // путь cgroup, если известен, иначе сохранённое имя
	MainValue      int64                     `json:"main_value,omitempty"`       // мера, по которой главная cgroup выбрана главной
	MainWrite      int64                     `json:"main_write,omitempty"`       // запись процессов; у выбранного io.stat писателя — запись cgroup
	MainRead       int64                     `json:"main_read,omitempty"`        // чтение главной cgroup
	MainProc       string                    `json:"main_proc,omitempty"`        // процесс главной cgroup с наибольшим I/O, comm(pid): класс тикета, хозяин временного scope
	IdleWindowMS   int64                     `json:"idle_window_ms,omitempty"`   // интервал деталей, из которого сложен CgroupIdle
	CgroupMainProc map[string]string         `json:"cgroup_main_proc,omitempty"` // процесс с наибольшим I/O по имени cgroup
	CgroupPaths    map[string]string         `json:"cgroup_paths,omitempty"`     // реальные пути cgroup, наблюдавшиеся в деталях
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

// ReadMostly — главная cgroup читала больше, чем записала по мере, сохранённой в MainWrite.
func (c *Culprit) ReadMostly() bool { return c != nil && c.MainRead > c.MainWrite }

// pageBytes — страница свопа и majflt в байтах (x86_64).
const pageBytes = 4096

// waitDiskBusyPercent — ниже этой доли занятости диска случай относим к ожиданию cgroup.
// Целевые случаи 10.11 занимали 0,5–5,6%; порог оставляет запас до настоящих фризов с заметной занятостью.
const waitDiskBusyPercent = 10

const (
	waitMemoryFullPercent = 25 // существенный PSI памяти означает машинное давление, а не локальное ожидание диска
	waitIdleWindowPercent = 60 // PSI full io должен занимать большую часть того же окна, что и CgroupIdle
	writebackOwnerMin     = 20 << 20
	ioFsyncShare          = 4
	ioFsyncMin            = 32 << 20
)

// PagingVictim — главная читала больше, чем писала, и подкачка всей машины за случай (swap-in и majflt) — не меньше
// половины её чтения: чтение может быть возвратом вытесненного, главная — жертва. Читающая файлы главная (подкачка
// меньше) — виновник. Подкачка машины, а не главной, — с запасом в сторону жертвы. Пользователь 08.11 (alert-async):
// правило 03.11 «главная только читает — письмо только лиду» сужено до подкачки; фриз пробы 13:12:52 (387 МБ
// чтения при 6 МБ подкачки) уходил лиду.
func (c *Case) PagingVictim() bool {
	return c.Culprits.ReadMostly() && 2*(c.SwapIn+c.Majflt)*pageBytes >= c.Culprits.MainRead
}

// KernelOver — запись вне cgroup перевешивает главную cgroup и не меньше чужой; без обеих мер любой фон ядра виден.
func (c *Culprit) KernelOver() bool {
	return c != nil && (c.MainValue > 0 && c.KernelWrite > c.MainValue || c.Foreign.Value > 0 && c.KernelWrite > c.Foreign.Value || c.Main == "" && c.MainValue == 0 && c.KernelWrite > 0) && c.KernelWrite >= c.Foreign.Value
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
	series     *Series
	profiles   *Profiles
	live       *Live
	lastDetail time.Time
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

// Pressure reports whether a fast step currently belongs to an open case.
func (r *Recorder) Pressure() bool { return r.open != nil }

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
	covered := s.At.Add(-r.cfg.DetailIdle.D() - r.cfg.Detail.D() - step)
	if r.lastDetail.After(covered) {
		covered = r.lastDetail
	}
	return r.ready(covered)
}

// Detail кладёт детальную запись в буфер, в ряд (Dirty, Writeback, память), профили и живое состояние.
func (r *Recorder) Detail(s Step) {
	interval := r.cfg.Detail.D()
	if !r.lastDetail.IsZero() {
		interval = s.At.Sub(r.lastDetail)
	}
	r.lastDetail = s.At
	r.ring.Add(Record{Kind: KindDetail, detailInterval: interval, Step: s})
	if r.series != nil {
		r.series.detail(&s)
	}
	if r.profiles != nil {
		r.profiles.detail(&s, interval)
	}
	if r.live != nil {
		r.live.detail(&s, interval)
		if o := r.open; o != nil {
			// Берём предыдущую запись даже при покое длиннее detail: её интервал может пересечь начало случая.
			lookback := max(interval, r.cfg.DetailIdle.D())
			details := r.ring.Details(o.start.Add(-2*lookback), s.At)
			firstInterval := interval
			if len(details) > 0 {
				firstInterval = r.ring.DetailInterval(details[0].At, firstInterval)
			}
			v := cappedWaitFirstInterval(details, o.start, o.last, firstInterval, o.last.Sub(o.start).Milliseconds(), o.sum.ticks)
			r.live.setOpenCapped(o.start, v.Capped)
		}
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
	c.Quiet = quietCase(c, r.cfg.QuietBytes)
	from := c.Start.Add(-r.cfg.Context.D())
	// Closing is detected on a fast step after the gap threshold; the step boundary can add
	// one more step. The next idle detail can then land detail_idle after closure.
	details := r.ring.Details(from, c.End.Add(r.cfg.DetailIdle.D()+r.cfg.Gap.D()+2*r.cfg.Step.D()))
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
	main := sum.MainCgroup()
	mainProc := sum.MainProc(main)
	for _, top := range [][]Ranked{procWrite, procRead, {{Name: mainProc}}} {
		for _, p := range top {
			if info, ok := sum.ProcessDetails[p.Name]; ok {
				processDetails[p.Name] = info
			}
		}
	}
	cgroupMainProc := make(map[string]string)
	for _, tops := range [][]Ranked{Rank(sum.CgroupDirty, caseTop), Rank(sum.CgroupRead, caseTop), Rank(sum.CgroupIdle, caseTop)} {
		for _, item := range tops {
			path := sum.cgroupPath(item.Name)
			procName := sum.MainProc(path)
			if procName == "" {
				continue
			}
			cgroupMainProc[item.Name] = procName
			if info, ok := sum.ProcessDetails[procName]; ok {
				processDetails[procName] = info
			}
		}
	}
	cgroupPaths := make(map[string]string)
	for _, top := range [][]Ranked{Rank(sum.CgroupWrite, caseTop), Rank(sum.CgroupRead, caseTop), Rank(sum.CgroupDirty, caseTop), Rank(sum.CgroupIdle, caseTop)} {
		for _, item := range top {
			if path := sum.cgroupPath(item.Name); path != "" {
				cgroupPaths[item.Name] = path
			}
		}
	}
	c.Culprits = &Culprit{From: from, Main: main, MainValue: sum.cgroupTotal[main], MainWrite: sum.cgroupWrite[main], MainRead: sum.cgroupRead[main], MainProc: mainProc,
		CgroupMainProc: cgroupMainProc,
		CgroupPaths:    cgroupPaths,
		ProcWrite:      procWrite, ProcRead: procRead, ProcMajflt: Rank(sum.ProcMajflt, caseTop),
		ProcessDetails: processDetails,
		CgroupWrite:    Rank(sum.CgroupWrite, caseTop), CgroupRead: Rank(sum.CgroupRead, caseTop),
		CgroupSwapIn: Rank(sum.CgroupSwapIn, caseTop), CgroupIdle: Rank(sum.CgroupIdle, caseTop),
		CgroupDirty: Rank(sum.CgroupDirty, caseTop), CgroupOwn: map[string]int64{}}
	c.Culprits.Foreign, _ = sum.ForeignOver(c.Culprits.Main)
	firstInterval := r.cfg.Detail.D()
	if len(details) > 0 {
		firstInterval = r.ring.DetailInterval(details[0].At, firstInterval)
		c.Culprits.IdleWindowMS = details[len(details)-1].At.Sub(details[0].At.Add(-firstInterval)).Milliseconds()
	}
	v := cappedWaitFirstInterval(details, c.Start, c.End, firstInterval, c.DurMS, c.BusyMS)
	c.Capped, c.OutsideIdleMS, c.Culprits.CappedIdle = v.Capped, v.OutsideMS, v.Who
	for i := range details {
		c.Culprits.CleanerMS += details[i].CleanerMS
	}
	for _, g := range c.Culprits.CgroupWrite {
		if sum.Readable[g.Name] {
			c.Culprits.CgroupOwn[g.Name] = sum.CgroupDirty[g.Name]
		}
	}
	if len(details) > 0 { // детальная запись — дельта за интервал предыдущей записи
		c.Culprits.DeviceWrite = r.ring.DeviceWrite(details[0].At.Add(-firstInterval), details[len(details)-1].At)
		var cgroups int64
		for _, v := range sum.CgroupWrite {
			cgroups += v
		}
		c.Culprits.KernelWrite = max(0, c.Culprits.DeviceWrite-cgroups)
	}
	resolveCaseMain(c, &sum)
}

// resolveCaseMain assigns one cause to the case: kernel writeback, a cgroup waiting while the disk is mostly idle,
// or the cgroup with the most process writes plus reads. Reports and routing consume this same Main.
func resolveCaseMain(c *Case, sum *Culprits) {
	if c.Culprits == nil || sum == nil {
		return
	}
	c.Waiting = false
	main := c.Culprits.Main
	windowMS := c.Culprits.IdleWindowMS
	if windowMS <= 0 {
		// Старые строки журнала не хранят окно деталей; восстановим номинальные 5 с контекста и хвоста.
		windowMS = c.DurMS + 5_000 + 5_000 + 300 + 200
	}
	if !c.Quiet && !c.Capped && c.DurMS > 0 && c.BusyMS*100 < c.DurMS*waitDiskBusyPercent &&
		c.MemFullMS*100 < c.DurMS*waitMemoryFullPercent {
		if waiting := Rank(sum.CgroupIdle, 1); len(waiting) > 0 && waiting[0].Value*100 >= windowMS*waitIdleWindowPercent {
			main = sum.cgroupPath(waiting[0].Name)
			c.Waiting = main != ""
		}
	}
	writeFromIOStat := false
	if !c.Waiting && c.Culprits.KernelOver() {
		main, writeFromIOStat = kernelWritebackMain(c.Culprits, sum)
	}
	if main == c.Culprits.Main {
		if writeFromIOStat {
			c.Culprits.MainValue, _, c.Culprits.MainRead = cgroupMetricsByName(sum, main)
			c.Culprits.MainWrite = cgroupIOStatWrite(c.Culprits.CgroupWrite, main)
		}
		return
	}
	c.Culprits.Main = main
	c.Culprits.MainValue, c.Culprits.MainWrite, c.Culprits.MainRead = cgroupMetricsByName(sum, main)
	if writeFromIOStat {
		c.Culprits.MainWrite = cgroupIOStatWrite(c.Culprits.CgroupWrite, main)
	}
	c.Culprits.MainProc = sum.MainProc(main)
	if info, ok := sum.ProcessDetails[c.Culprits.MainProc]; ok {
		c.Culprits.ProcessDetails[c.Culprits.MainProc] = info
	}
}

func kernelWritebackMain(cul *Culprit, sum *Culprits) (string, bool) {
	if cul.SnapshotDeletion() {
		return "", false
	}
	dirty := Rank(sum.CgroupDirty, 2)
	if len(dirty) > 0 && dirty[0].Value >= writebackOwnerMin && dirty[0].Value >= 2*dirtyValue(dirty, 1) &&
		dirty[0].Value >= maxOtherCgroupWrite(cul.CgroupWrite, cul.CgroupOwn, dirty[0].Name) {
		return cgroupNameOrPath(sum, dirty[0].Name), false
	}
	if writer, ok := strongWritebackCgroup(cul.CgroupWrite, cul.CgroupOwn, dirtyValue(dirty, 0)); ok {
		return cgroupNameOrPath(sum, writer), true
	}
	return "", false
}

func cgroupIOStatWrite(items []Ranked, main string) int64 {
	name := CgroupName(main)
	for _, item := range items {
		if item.Name == name {
			return item.Value
		}
	}
	return 0
}

func cgroupMetricsByName(sum *Culprits, path string) (total, write, read int64) {
	if path == "" {
		return 0, 0, 0
	}
	name := CgroupName(path)
	for candidate, value := range sum.cgroupTotal {
		if CgroupName(candidate) == name {
			total += value
		}
	}
	for candidate, value := range sum.cgroupWrite {
		if CgroupName(candidate) == name {
			write += value
		}
	}
	for candidate, value := range sum.cgroupRead {
		if CgroupName(candidate) == name {
			read += value
		}
	}
	return total, write, read
}

func dirtyValue(items []Ranked, i int) int64 {
	if i >= 0 && i < len(items) {
		return items[i].Value
	}
	return 0
}

func maxOtherCgroupWrite(items []Ranked, own map[string]int64, owner string) int64 {
	var maxWrite int64
	for _, item := range items {
		if item.Name != owner {
			value := item.Value
			if FsyncOverage(value, own[item.Name]) {
				value = own[item.Name]
			}
			maxWrite = max(maxWrite, value)
		}
	}
	return maxWrite
}

// strongWritebackCgroup finds an io.stat writer only when its volume and margin support attribution.
// The effective value discounts known btrfs fsync overage using the same evidence as the report.
func strongWritebackCgroup(items []Ranked, own map[string]int64, dirtyTop int64) (string, bool) {
	var first, second Ranked
	for _, item := range items {
		value := item.Value
		if FsyncOverage(value, own[item.Name]) {
			value = own[item.Name]
		}
		if value <= 0 {
			continue
		}
		candidate := Ranked{Name: item.Name, Value: value}
		if first.Name == "" || candidate.Value > first.Value || candidate.Value == first.Value && candidate.Name < first.Name {
			second, first = first, candidate
		} else if second.Name == "" || candidate.Value > second.Value || candidate.Value == second.Value && candidate.Name < second.Name {
			second = candidate
		}
	}
	if first.Name == "" || first.Value < writebackOwnerMin || first.Value < 2*second.Value || first.Value < dirtyTop {
		return "", false
	}
	return first.Name, true
}

func cgroupNameOrPath(sum *Culprits, name string) string {
	if path := sum.cgroupPath(name); path != "" {
		return path
	}
	// Старые записи хранят только отображаемые имена, без достаточных данных для восстановления пути.
	return name
}

// FsyncOverage is the known btrfs transaction commit that io.stat charges to an fsync caller.
// The report uses the same predicate to annotate the excess in its cgroup write list.
func FsyncOverage(write, own int64) bool {
	return own > 0 && write > ioFsyncShare*own && write-own > ioFsyncMin
}

// ReattributeCases reapplies current attribution rules to stored summaries, whose detailed steps may have expired.
// It reads only fields persisted in the case journal and does not modify the journal.
func ReattributeCases(cases []Case) {
	for i := range cases {
		c := &cases[i]
		if c.Culprits == nil {
			continue
		}
		sum := Culprits{CgroupDirty: map[string]int64{}, CgroupRead: map[string]int64{}, CgroupIdle: map[string]int64{}, cgroupPaths: map[string]string{},
			cgroupTotal: map[string]int64{}, cgroupWrite: map[string]int64{}, cgroupRead: map[string]int64{},
			procCgroup: map[string]string{}, procIO: map[string]int64{}, ProcessDetails: c.Culprits.ProcessDetails}
		pathFor := func(name string) string {
			if path := c.Culprits.CgroupPaths[name]; path != "" {
				return path
			}
			if previous := c.Culprits.Main; previous != "" && CgroupName(previous) == name {
				return previous
			}
			// Имя cgroup не является путём; не выдумываем user-uid, slice или pid.
			return name
		}
		for _, item := range c.Culprits.CgroupDirty {
			sum.CgroupDirty[item.Name] = item.Value
			path := pathFor(item.Name)
			sum.cgroupWrite[path] = item.Value
			sum.cgroupTotal[path] = item.Value
			sum.cgroupPaths[item.Name] = path
		}
		for name, procName := range c.Culprits.CgroupMainProc {
			path := sum.cgroupPaths[name]
			if path == "" {
				path = pathFor(name)
				sum.cgroupPaths[name] = path
			}
			sum.procCgroup[procName] = path
			sum.procIO[procName] = 1
		}
		if len(c.Culprits.CgroupMainProc) == 0 && len(c.Culprits.ProcWrite) > 0 {
			if dirty := Rank(sum.CgroupDirty, 1); len(dirty) > 0 {
				path := sum.cgroupPaths[dirty[0].Name]
				procName := c.Culprits.ProcWrite[0].Name
				sum.procCgroup[procName] = path
				sum.procIO[procName] = c.Culprits.ProcWrite[0].Value
			}
		}
		for _, item := range c.Culprits.CgroupRead {
			sum.CgroupRead[item.Name] = item.Value
			path := sum.cgroupPaths[item.Name]
			if path == "" {
				path = pathFor(item.Name)
				sum.cgroupPaths[item.Name] = path
			}
			sum.cgroupRead[path] = item.Value
			sum.cgroupTotal[path] += item.Value
		}
		for _, item := range c.Culprits.CgroupIdle {
			sum.CgroupIdle[item.Name] = item.Value
			if sum.cgroupPaths[item.Name] == "" {
				sum.cgroupPaths[item.Name] = pathFor(item.Name)
			}
		}
		maps.Copy(sum.cgroupPaths, c.Culprits.CgroupPaths)
		resolveCaseMain(c, &sum)
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

// quietCase — простой без обмена с диском и не простой памяти (Case.Quiet).
func quietCase(c *Case, quietBytes int64) bool {
	return c.ReadB+c.WriteB < quietBytes && 2*c.MemFullMS < c.DurMS
}
