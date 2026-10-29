package iowatch

import (
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
	dirty, writeback int64
	open             time.Time // начало идущего случая; ноль — случая нет
	cases            [liveCases]liveCase
	casesHead, nCase int
	writers          [nowWriters]Writer
	nWriters         int
	profiles         map[string]ProfileStats
}

const (
	// liveSteps — быстрые шаги за окно 30 с при самом частом шаге 50 мс (Validate), с запасом.
	liveSteps = 640
	// liveCases — последние закрытые случаи: за 60 с их больше не бывает (случай — от 100 мс, gap между ними).
	liveCases  = 128
	nowWriters = 5
)

type liveStep struct {
	at                                                       int64 // UnixNano
	ioFull, memFull, stall, rd, wr, wms, wios, flush, swapIn int64 // мкс и счётчики шага
	inflight                                                 int64
}

type liveCase struct{ start, end time.Time }

func NewLive() *Live { return &Live{} }

// step — быстрый шаг: stallUS — его длительность, если он с простоем; open — начало идущего случая
// (ноль — нет); closed — случай, закрытый этим шагом (нулевой end — нет).
func (l *Live) step(s *Step, stallUS int64, open time.Time, closed liveCase) {
	l.mu.Lock()
	defer l.mu.Unlock()
	i := (l.head + l.count) % liveSteps
	if l.count == liveSteps {
		l.head = (l.head + 1) % liveSteps
	} else {
		l.count++
	}
	l.steps[i] = liveStep{at: s.At.UnixNano(), ioFull: s.IOFullUS, memFull: s.MemFullUS, stall: stallUS, rd: s.ReadBytes,
		wr: s.WriteBytes, wms: s.WMS, wios: s.WIOS, flush: s.FIOS, swapIn: s.SwapIn, inflight: s.InFlight}
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

// detail — детальный проход длительностью interval: Dirty, Writeback и до nowWriters cgroup по записи.
func (l *Live) detail(s *Step, interval time.Duration) {
	var top [nowWriters]Writer
	n := 0
	for i := range s.Cgroups {
		g := &s.Cgroups[i]
		if g.WriteBytes == 0 || n == nowWriters && g.WriteBytes <= top[n-1].WriteB {
			continue
		}
		pos := min(n, nowWriters-1)
		if n < nowWriters {
			n++
		}
		for pos > 0 && top[pos-1].WriteB < g.WriteBytes {
			top[pos] = top[pos-1]
			pos--
		}
		top[pos] = Writer{Path: g.Path, WriteB: g.WriteBytes}
	}
	for i := range top[:n] {
		top[i].Name, top[i].WriteBps = CgroupName(top[i].Path), perSecond(top[i].WriteB, interval)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.dirty, l.writeback = s.Dirty, s.Writeback
	l.writers, l.nWriters = top, n
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
	WriteB   int64  `json:"-"`
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
}

// NowOpen — идущий случай фриза.
type NowOpen struct {
	Open bool  `json:"open"`
	MS   int64 `json:"ms,omitempty"` // от начала первого шага с простоем до последнего шага
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
	At        time.Time     `json:"at"`
	W1s       NowWindow     `json:"1s"`
	W5s       NowWindow     `json:"5s"`
	W30s      NowWindow     `json:"30s"`
	Dirty     int64         `json:"dirty"`     // байт, последний детальный проход
	Writeback int64         `json:"writeback"` // байт
	Open      NowOpen       `json:"open"`
	Cases     NowCases      `json:"cases"`
	Writers   []Writer      `json:"writers"`
	Profile   *ProfileStats `json:"profile,omitempty"` // свод профилей вида из запроса; нет таких — пусто
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
		W30s: l.window(last, 30*time.Second), Dirty: l.dirty, Writeback: l.writeback, Writers: slices.Clone(l.writers[:l.nWriters])}
	if n.Writers == nil {
		n.Writers = []Writer{}
	}
	if !l.open.IsZero() {
		n.Open = NowOpen{Open: true, MS: n.At.Sub(l.open).Milliseconds()}
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

// window — шаги с концом в (last − d, last]; байт/с — по d.
func (l *Live) window(last int64, d time.Duration) NowWindow {
	var w NowWindow
	var stall, ioFull, memFull, rd, wr, wms, wios int64
	from := last - int64(d)
	for i := l.count - 1; i >= 0; i-- {
		s := &l.steps[(l.head+i)%liveSteps]
		if s.at <= from {
			break
		}
		w.Steps++
		ioFull += s.ioFull
		memFull += s.memFull
		stall += s.stall
		rd += s.rd
		wr += s.wr
		wms += s.wms
		wios += s.wios
		w.Flushes += s.flush
		w.SwapIn += s.swapIn
		w.InFlightMax = max(w.InFlightMax, s.inflight)
	}
	w.IOFullMS, w.MemFullMS, w.StallMS = ioFull/1000, memFull/1000, stall/1000
	w.ReadBps, w.WriteBps = perSecond(rd, d), perSecond(wr, d)
	w.WAwaitMS = AwaitMS(wms, wios)
	return w
}
