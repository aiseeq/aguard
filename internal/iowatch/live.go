package iowatch

import (
	"runtime"
	"slices"
	"sync"
	"time"
)

// Live — моментальное состояние диска для запроса now: горутина замера пишет его на каждом шаге без
// аллокаций, соединения сокета читают под мьютексом, не заходя в главный цикл. Окна 1, 5 и 30 с
// считаются при запросе по кольцу последних быстрых шагов.
type Live struct {
	mu               sync.Mutex
	steps            [liveSteps]liveStep
	head, count      int
	last             detailLast
	open             time.Time // начало идущего случая; ноль — случая нет
	openCapped       bool      // идущий случай по своим детальным записям — ожидание под потолком (Recorder.Detail)
	details          [liveDetails]liveDetail
	dHead, nDetail   int
	owners           []capOwner // буфер capParts
	cases            [liveCases]liveCase
	casesHead, nCase int
	writers          [nowTop]Writer
	nWriters         int
	swappers         [nowTop]Swapper
	nSwappers        int
	profiles         map[string]ProfileStats
}

const (
	// liveSteps — быстрые шаги за окно 30 с при самом частом шаге 50 мс (Validate), с запасом.
	liveSteps = 640
	// liveCases — последние закрытые случаи: за 60 с их больше не бывает (случай — от 100 мс, gap между ними).
	liveCases = 128
	// nowTop — сколько cgroup в writers и swappers.
	nowTop = 5
	// liveDetails — детальные записи для признания окон: окно 30 с при детальном проходе 1 с, с запасом.
	liveDetails = 40
)

// liveDetail — детальная запись (from, at], сведённая capParts: простой по потолкам и вне их.
type liveDetail struct {
	from, at int64 // UnixNano
	parts    []capPart
}

type liveStep struct {
	at, from                                                 int64 // UnixNano
	ioFull, memFull, stall, rd, wr, wms, wios, flush, swapIn int64 // мкс и счётчики шага
	cpuSome, majflt, ticks                                   int64 // ticks — мс занятости устройства
	inflight, runnable                                       int64 // моментальные
}

type liveCase struct{ start, end time.Time }

func NewLive() *Live { return &Live{} }

// step — быстрый шаг: stallUS — его длительность, если он с простоем; open — начало идущего случая
// (ноль — нет); closed — случай, закрытый этим шагом (нулевой end — нет).
func (l *Live) step(s *Step, stallUS int64, interval time.Duration, open time.Time, closed liveCase) {
	l.mu.Lock()
	defer l.mu.Unlock()
	i := (l.head + l.count) % liveSteps
	if l.count == liveSteps {
		l.head = (l.head + 1) % liveSteps
	} else {
		l.count++
	}
	l.steps[i] = liveStep{at: s.At.UnixNano(), from: s.At.Add(-interval).UnixNano(), ioFull: s.IOFullUS, memFull: s.MemFullUS, stall: stallUS, rd: s.ReadBytes,
		wr: s.WriteBytes, wms: s.WMS, wios: s.WIOS, flush: s.FIOS, swapIn: s.SwapIn, cpuSome: s.CPUSomeUS, majflt: s.Majflt,
		inflight: s.InFlight, runnable: s.Runnable, ticks: s.IOTicks}
	if !open.Equal(l.open) {
		l.openCapped = false // новый случай или конец прежнего: признание — с его детальных записей
	}
	l.open = open
	if !closed.end.IsZero() {
		j := (l.casesHead + l.nCase) % liveCases
		if l.nCase == liveCases {
			l.casesHead = (l.casesHead + 1) % liveCases
		} else {
			l.nCase++
		}
		l.cases[j] = closed
	}
}

// detail — детальный проход длительностью interval: Dirty, Writeback, память и своп машины, до
// nowTop cgroup по записи и по swap-in.
func (l *Live) detail(s *Step, interval time.Duration) {
	wTop, nw := topCgroups(s.Cgroups, func(g *Cgroup) int64 { return g.WriteBytes })
	var writers [nowTop]Writer
	for i, g := range wTop[:nw] {
		writers[i] = Writer{Name: CgroupName(g.Path), Path: g.Path, WriteBps: perSecond(g.WriteBytes, interval)}
	}
	sTop, ns := topCgroups(s.Cgroups, func(g *Cgroup) int64 { return g.SwapIn })
	var swappers [nowTop]Swapper
	for i, g := range sTop[:ns] {
		swappers[i] = Swapper{Name: CgroupName(g.Path), Path: g.Path, SwapInPS: perSecond(g.SwapIn, interval), MajfltPS: perSecond(g.Majflt, interval)}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	at := s.At.UnixNano()
	from := at - int64(interval)
	if l.nDetail > 0 { // как cappedWait: запись — дельта с прошлого прохода, не короче detail
		from = min(from, l.details[(l.dHead+l.nDetail-1)%liveDetails].at)
	}
	i := (l.dHead + l.nDetail) % liveDetails
	if l.nDetail == liveDetails {
		l.dHead = (l.dHead + 1) % liveDetails
	} else {
		l.nDetail++
	}
	d := &l.details[i]
	d.from, d.at = from, at
	d.parts, l.owners = capParts(s, time.Duration(at-from), d.parts[:0], l.owners)
	l.last = detailLast{dirty: s.Dirty, writeback: s.Writeback, memAvail: s.MemAvail, swapUsed: s.SwapUsed}
	l.writers, l.nWriters = writers, nw
	l.swappers, l.nSwappers = swappers, ns
}

// topCgroups — до nowTop cgroup с наибольшим ненулевым key по убыванию, без аллокаций.
func topCgroups(cgroups []Cgroup, key func(*Cgroup) int64) (top [nowTop]*Cgroup, n int) {
	for i := range cgroups {
		g := &cgroups[i]
		v := key(g)
		if v == 0 || n == nowTop && v <= key(top[n-1]) {
			continue
		}
		pos := min(n, nowTop-1)
		if n < nowTop {
			n++
		}
		for pos > 0 && key(top[pos-1]) < v {
			top[pos] = top[pos-1]
			pos--
		}
		top[pos] = g
	}
	return top, n
}

// setOpenCapped — признание идущего случая, начавшегося в start; случай уже другой — не трогает.
func (l *Live) setOpenCapped(start time.Time, capped bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.open.Equal(start) {
		l.openCapped = capped
	}
}

// capWindow — признание окна (last − d, last] по детальным записям, пропорционально их доле внутри окна.
func (l *Live) capWindow(last int64, d time.Duration, busyMS int64) NowCapped {
	acc := newCapAcc()
	from := last - int64(d)
	for i := range l.nDetail {
		rec := &l.details[(l.dHead+i)%liveDetails]
		if share := overlapShare(rec.from, rec.at, from, last); share > 0 {
			acc.add(rec.parts, share)
		}
	}
	v := acc.verdict(d.Milliseconds(), busyMS)
	return NowCapped{Capped: v.Capped, OutsideMS: v.OutsideMS, CappedMS: v.CappedMS}
}

// SetProfiles — свод профилей по видам (служба пересчитывает, когда профили меняются).
func (l *Live) SetProfiles(stats map[string]ProfileStats) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.profiles = stats
}

// Writer — cgroup из последнего детального прохода по записи.
type Writer struct {
	Name     string `json:"name"` // CgroupName
	Path     string `json:"path"`
	WriteBps int64  `json:"wr_bps"`
}

// Swapper — cgroup из последнего детального прохода по swap-in: жертва вытеснения, её страницы
// возвращаются из свопа (в случае 29.10 22:56 — Chrome).
type Swapper struct {
	Name     string `json:"name"` // CgroupName
	Path     string `json:"path"`
	SwapInPS int64  `json:"swapin_ps"` // страниц/с
	MajfltPS int64  `json:"majflt_ps"` // major faults/с
}

// NowWindow — быстрые шаги за последние N секунд до At.
type NowWindow struct {
	Steps       int   `json:"steps"`
	IOFullMS    int64 `json:"io_full_ms"`
	MemFullMS   int64 `json:"mem_full_ms"`
	StallMS     int64 `json:"stall_ms"` // мс шагов с простоем (Stalled)
	WriteBps    int64 `json:"write_bps"`
	ReadBps     int64 `json:"read_bps"`
	WAwaitMS    int64 `json:"w_await_ms"` // среднее ожидание операции записи: WMS/WIOS окна
	InFlightMax int64 `json:"inflight_max"`
	Flushes     int64 `json:"flushes"`
	SwapIn      int64 `json:"swapin"` // страниц
	Majflt      int64 `json:"majflt"`
	CPUSomeMS   int64 `json:"cpu_some_ms"`  // PSI some cpu окна
	RunnableMax int64 `json:"runnable_max"` // наибольшее число готовых к работе за окно
	BusyMS      int64 `json:"busy_ms"`      // занятость устройства (io_ticks) за окно
	// Capped — окно по детальным записям — ожидание под потолком io.max (то же правило, что у закрытых случаев).
	// Нет поля — сторож старее этой меры. Детальная запись запаздывает за быстрым шагом до прохода (1 с).
	Capped NowCapped `json:"capped"`
}

// NowCapped — признание окна: под потолком, наибольший простой cgroup вне потолка и простой упиравшихся, мс.
type NowCapped struct {
	Capped    bool  `json:"capped"`
	OutsideMS int64 `json:"outside_ms"`
	CappedMS  int64 `json:"capped_ms"`
}

// NowOpen — идущий случай фриза.
type NowOpen struct {
	Open bool  `json:"open"`
	MS   int64 `json:"ms,omitempty"` // от начала первого шага с простоем до последнего шага
	// Capped — идущий случай по своим детальным записям — ожидание под потолком; деталей ещё нет — false.
	Capped bool `json:"capped"`
}

// NowCases — закрытые случаи по концу за последние 30 и 60 с: от 100 и от 200 мс.
type NowCases struct {
	From100ms30s int        `json:"ge100ms_30s"`
	From200ms30s int        `json:"ge200ms_30s"`
	From100ms60s int        `json:"ge100ms_60s"`
	From200ms60s int        `json:"ge200ms_60s"`
	LastEnd      *time.Time `json:"last_end,omitempty"` // конец последнего закрытого случая
}

// Now — моментальное состояние на последний быстрый шаг. At давний — замер не идёт: клиент сверяет сам.
type Now struct {
	At           time.Time     `json:"at"`
	W1s          NowWindow     `json:"1s"`
	W5s          NowWindow     `json:"5s"`
	W30s         NowWindow     `json:"30s"`
	NProc        int           `json:"nproc"`         // потоков процессора (runtime.NumCPU)
	Dirty        int64         `json:"dirty"`         // байт, последний детальный проход
	Writeback    int64         `json:"writeback"`     // байт
	MemAvailable int64         `json:"mem_available"` // MemAvailable, байт, последний детальный проход
	SwapUsed     int64         `json:"swap_used"`     // байт
	Open         NowOpen       `json:"open"`
	Cases        NowCases      `json:"cases"`
	Writers      []Writer      `json:"writers"`
	Swappers     []Swapper     `json:"swappers"`
	Profile      *ProfileStats `json:"profile,omitempty"` // свод профилей вида из запроса; нет таких — пусто
}

// Now — снимок состояния; kind — вид для свода профилей (пусто — без него). ok = false — шагов ещё не было.
func (l *Live) Now(kind string) (Now, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.count == 0 {
		return Now{}, false
	}
	last := l.steps[(l.head+l.count-1)%liveSteps].at
	n := Now{At: time.Unix(0, last), W1s: l.window(last, time.Second), W5s: l.window(last, 5*time.Second),
		W30s: l.window(last, 30*time.Second), NProc: runtime.NumCPU(), Dirty: l.last.dirty, Writeback: l.last.writeback,
		MemAvailable: l.last.memAvail, SwapUsed: l.last.swapUsed,
		Writers: slices.Clone(l.writers[:l.nWriters]), Swappers: slices.Clone(l.swappers[:l.nSwappers])}
	for _, w := range [...]struct {
		win *NowWindow
		d   time.Duration
	}{{&n.W1s, time.Second}, {&n.W5s, 5 * time.Second}, {&n.W30s, 30 * time.Second}} {
		w.win.Capped = l.capWindow(last, w.d, w.win.BusyMS)
	}
	if n.Writers == nil {
		n.Writers = []Writer{}
	}
	if n.Swappers == nil {
		n.Swappers = []Swapper{}
	}
	if !l.open.IsZero() {
		n.Open = NowOpen{Open: true, MS: n.At.Sub(l.open).Milliseconds(), Capped: l.openCapped}
	}
	for i := range l.nCase {
		c := l.cases[(l.casesHead+i)%liveCases]
		age, dur := n.At.Sub(c.end), c.end.Sub(c.start)
		for _, w := range [...]struct {
			within, min time.Duration
			count       *int
		}{{30 * time.Second, 100 * time.Millisecond, &n.Cases.From100ms30s}, {30 * time.Second, 200 * time.Millisecond, &n.Cases.From200ms30s},
			{60 * time.Second, 100 * time.Millisecond, &n.Cases.From100ms60s}, {60 * time.Second, 200 * time.Millisecond, &n.Cases.From200ms60s}} {
			if age <= w.within && dur >= w.min {
				*w.count++
			}
		}
		if n.Cases.LastEnd == nil || c.end.After(*n.Cases.LastEnd) {
			end := c.end
			n.Cases.LastEnd = &end
		}
	}
	if st, ok := l.profiles[kind]; kind != "" && ok {
		n.Profile = &st
	}
	return n, true
}

// window — интервальные суммы пропорциональны пересечению шага с (last − d, last]; счётчики
// неинтервальные учитываются целиком, если конец шага попал в окно. Байты/с считаются по d.
func (l *Live) window(last int64, d time.Duration) NowWindow {
	var w NowWindow
	var stall, ioFull, memFull, cpuSome, rd, wr, wms, wios, flush, swapIn, majflt, ticks int64
	from := last - int64(d)
	for i := l.count - 1; i >= 0; i-- {
		s := &l.steps[(l.head+i)%liveSteps]
		if s.at <= from {
			break
		}
		w.Steps++
		start, end := max(s.from, from), min(s.at, last)
		if end > start && s.at > s.from {
			ratio := float64(end-start) / float64(s.at-s.from)
			scale := func(v int64) int64 { return int64(float64(v) * ratio) }
			ioFull += scale(s.ioFull)
			memFull += scale(s.memFull)
			cpuSome += scale(s.cpuSome)
			stall += scale(s.stall)
			rd += scale(s.rd)
			wr += scale(s.wr)
			wms += scale(s.wms)
			wios += scale(s.wios)
			flush += scale(s.flush)
			swapIn += scale(s.swapIn)
			majflt += scale(s.majflt)
			ticks += scale(s.ticks)
		} else if s.at > from {
			ioFull += s.ioFull
			memFull += s.memFull
			cpuSome += s.cpuSome
			stall += s.stall
			rd += s.rd
			wr += s.wr
			wms += s.wms
			wios += s.wios
			flush += s.flush
			swapIn += s.swapIn
			majflt += s.majflt
			ticks += s.ticks
		}
		w.InFlightMax = max(w.InFlightMax, s.inflight)
		w.RunnableMax = max(w.RunnableMax, s.runnable)
	}
	w.IOFullMS, w.MemFullMS, w.CPUSomeMS, w.StallMS = ioFull/1000, memFull/1000, cpuSome/1000, stall/1000
	w.ReadBps, w.WriteBps = perSecond(rd, d), perSecond(wr, d)
	w.WAwaitMS = AwaitMS(wms, wios)
	w.Flushes, w.SwapIn, w.Majflt, w.BusyMS = flush, swapIn, majflt, ticks
	return w
}
