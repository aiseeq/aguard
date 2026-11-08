// Package ioroute — кому и как сообщить о случае фриза: письмо хозяину виновника, когда вред идёт сейчас, иначе
// тикет в его проект; лиду — только без хозяина, при общем вреде машины и при одном классе у разных хозяев.
// Причина (пользователь 08.11, тикет alert-async): письмо будит сессию, та заново читает свой контекст; лид в
// цепочке только пересылал. Маршрут — функция последовательности случаев и времени: состояние после перезапуска
// восстанавливается прогоном сохранённых случаев (Seed), им же считается сводка за неделю.
package ioroute

import (
	"maps"
	"slices"
	"time"
)

// Config — пороги маршрута.
type Config struct {
	UrgentMin     time.Duration // случай от такой длительности — вред сейчас
	SeriesN       int           // столько случаев одного хозяина за SeriesWindow — серия, вред сейчас
	SeriesWindow  time.Duration
	Repeat        time.Duration // письмо одному адресату не чаще; случаи в паузе — по её концу
	MachineN      int           // столько случаев за SeriesWindow от MachineOwners разных хозяев — общий вред, письмо лиду
	MachineOwners int
	CrossWindow   time.Duration // класс у двух разных хозяев за столько — письмо лиду, раз за CrossWindow на класс
	Lead          string
}

// Case — случай для маршрута. Owner — сессия-хозяин виновника, "" — хозяина нет; Class — что фризило (служба,
// главный процесс), общий для разных хозяев; Generic — класс без имени процесса (session, worker): в сверку
// «класс у разных хозяев» не идёт.
type Case struct {
	Start   time.Time
	End     time.Time
	DurMS   int64
	Owner   string
	Class   string
	Generic bool
	// Victim — главная cgroup в основном читала при подкачке машины не меньше половины чтения: может быть жертвой
	// вытеснения (правило 03.11 io-culprit-victim, сужено 08.11) — хозяину только тикет, срочное — лиду.
	Victim bool
	Text   string // строка случая для письма и тикета
	// Offline — хозяин был не в сети, когда маршрут решал (сохранено со случаем): только для Seed.
	Offline bool
	// ticketed — тикет хозяину уже заведён при маршруте (жертва, хозяин не в сети): по концу паузы письма — не в тикет.
	ticketed bool
}

// Kind — вид действия.
type Kind string

const (
	Letter Kind = "letter" // письмо сессии To
	Ticket Kind = "ticket" // тикет хозяина Owner класса Class: открытый — заметка, иначе новый
	// Summary — несрочный случай без хозяина: ни письма, ни тикета (некому), только в сводку (aguard io route, io report).
	Summary Kind = "summary"
)

// Why — причина письма.
type Why string

const (
	WhyUrgent  Why = "срочно"           // вред сейчас у хозяина
	WhyOffline Why = "хозяин не в сети" // вред сейчас, хозяин не отвечает — лиду
	WhyVictim  Why = "главная читала"   // вред сейчас, хозяин может быть жертвой — лиду
	WhyNoOwner Why = "без хозяина"
	WhyMachine Why = "общий вред машины"
	WhyCross   Why = "класс у разных хозяев"
	WhyAfter   Why = "после паузы, вред идёт"
)

// Action — что сделать.
type Action struct {
	Kind  Kind
	To    string // адресат письма
	Owner string // хозяин тикета ("" — лиду)
	Class string
	Why   Why
	Cases []Case
}

// Router — состояние маршрута. Не потокобезопасен: один владелец (горутина наблюдателя).
type Router struct {
	cfg     Config
	series  map[string][]time.Time          // хозяин (без хозяина — класс) → концы случаев в SeriesWindow
	machine []Case                          // случаи машины в SeriesWindow
	told    map[string]time.Time            // адресат → последнее письмо
	pending map[string][]Case               // адресат → срочное в паузе
	cross   map[string]map[string]time.Time // класс → хозяин → последний случай
	crossAt map[string]time.Time            // класс → письмо лиду
	machAt  time.Time
}

func New(cfg Config) *Router {
	return &Router{cfg: cfg, series: map[string][]time.Time{}, told: map[string]time.Time{}, pending: map[string][]Case{},
		cross: map[string]map[string]time.Time{}, crossAt: map[string]time.Time{}}
}

// SetConfig — новые пороги без потери состояния.
func (r *Router) SetConfig(cfg Config) { r.cfg = cfg }

// Route — действия по закрытому случаю. alive — жива ли сессия.
func (r *Router) Route(c Case, alive func(string) bool) []Action {
	now := c.End
	var out []Action
	recent := r.addSeries(c)
	urgent := c.DurMS >= r.cfg.UrgentMin.Milliseconds() || recent >= r.cfg.SeriesN
	switch {
	case urgent && c.Owner == "":
		out = append(out, r.letter(r.cfg.Lead, WhyNoOwner, c, now)...)
	case urgent && c.Victim:
		out = append(out, Action{Kind: Ticket, Owner: c.Owner, Class: c.Class, Why: WhyVictim, Cases: []Case{c}})
		c.ticketed = true
		out = append(out, r.letter(r.cfg.Lead, WhyVictim, c, now)...)
	case urgent && c.Owner == r.cfg.Lead || urgent && alive(c.Owner):
		out = append(out, r.letter(c.Owner, WhyUrgent, c, now)...)
	case urgent:
		out = append(out, Action{Kind: Ticket, Owner: c.Owner, Class: c.Class, Why: WhyOffline, Cases: []Case{c}})
		c.ticketed = true
		out = append(out, r.letter(r.cfg.Lead, WhyOffline, c, now)...)
	case c.Owner == "":
		out = append(out, Action{Kind: Summary, Class: c.Class, Cases: []Case{c}})
	default:
		out = append(out, Action{Kind: Ticket, Owner: c.Owner, Class: c.Class, Cases: []Case{c}})
	}
	if !urgent {
		out = append(out, r.machineCheck(now)...)
	}
	return append(out, r.crossCheck(c, now)...)
}

// Tick — по концу паузы адресата: вред идёт (последний случай моложе SeriesWindow) — письмо, иначе — заметки в тикеты.
func (r *Router) Tick(now time.Time) []Action {
	var out []Action
	for _, to := range slices.Sorted(maps.Keys(r.pending)) {
		cases := r.pending[to]
		if now.Sub(r.told[to]) < r.cfg.Repeat {
			continue
		}
		delete(r.pending, to)
		if now.Sub(cases[len(cases)-1].End) < r.cfg.SeriesWindow {
			r.told[to] = now
			out = append(out, Action{Kind: Letter, To: to, Why: WhyAfter, Cases: cases})
			continue
		}
		out = append(out, Tickets(cases)...)
	}
	return out
}

// Seed — состояние по прошлым случаям (по времени) без действий: после перезапуска серии, паузы и сверка классов
// продолжаются; срочное, ждавшее конца паузы, остаётся ждать. Пауза, кончившаяся до now, — сделанное прежним
// демоном: не повторяется (цена — демон стоял через конец паузы: заметки её случаев не уйдут, случаи — в сводке).
// Хозяин в сети — по Offline случая, как решал прежний демон.
func (r *Router) Seed(cases []Case, now time.Time) {
	for _, c := range cases {
		r.Tick(c.End)
		r.Route(c, func(string) bool { return !c.Offline })
	}
	r.Tick(now)
}

// Pending — сколько срочных случаев ждёт конца паузы.
func (r *Router) Pending() int {
	n := 0
	for _, v := range r.pending {
		n += len(v)
	}
	return n
}

func (r *Router) letter(to string, why Why, c Case, now time.Time) []Action {
	if last, ok := r.told[to]; ok && now.Sub(last) < r.cfg.Repeat {
		r.pending[to] = append(r.pending[to], c)
		return nil
	}
	r.told[to] = now
	return []Action{{Kind: Letter, To: to, Why: why, Cases: []Case{c}}}
}

// addSeries — случай в серию хозяина (без хозяина — класса: шторм службы) и машины; число случаев серии в окне, с этим.
func (r *Router) addSeries(c Case) int {
	from := c.End.Add(-r.cfg.SeriesWindow)
	r.machine = append(slices.DeleteFunc(r.machine, func(x Case) bool { return x.End.Before(from) }), c)
	key := c.Owner
	if key == "" {
		key = "\x00" + c.Class // не совпадает с именем сессии
	}
	s := append(slices.DeleteFunc(r.series[key], func(t time.Time) bool { return t.Before(from) }), c.End)
	r.series[key] = s
	return len(s)
}

// machineCheck — много несрочных случаев от разных хозяев: машина фризит, ни один хозяин не видит целого.
func (r *Router) machineCheck(now time.Time) []Action {
	if r.cfg.MachineN <= 0 || len(r.machine) < r.cfg.MachineN || !r.machAt.IsZero() && now.Sub(r.machAt) < r.cfg.Repeat {
		return nil
	}
	owners := map[string]bool{}
	for _, x := range r.machine {
		owners[x.Owner] = true
	}
	if len(owners) < r.cfg.MachineOwners {
		return nil
	}
	r.machAt = now
	return []Action{{Kind: Letter, To: r.cfg.Lead, Why: WhyMachine, Cases: slices.Clone(r.machine)}}
}

// crossCheck — один класс у разных хозяев за CrossWindow: общая причина, лиду раз за окно на класс.
func (r *Router) crossCheck(c Case, now time.Time) []Action {
	if c.Owner == "" || c.Generic || c.Class == "" || r.cfg.CrossWindow <= 0 {
		return nil
	}
	m := r.cross[c.Class]
	if m == nil {
		m = map[string]time.Time{}
		r.cross[c.Class] = m
	}
	m[c.Owner] = now
	from := now.Add(-r.cfg.CrossWindow)
	for o, t := range m {
		if t.Before(from) {
			delete(m, o)
		}
	}
	if len(m) < 2 {
		return nil
	}
	if at, ok := r.crossAt[c.Class]; ok && now.Sub(at) < r.cfg.CrossWindow {
		return nil
	}
	r.crossAt[c.Class] = now
	return []Action{{Kind: Letter, To: r.cfg.Lead, Class: c.Class, Why: WhyCross, Cases: []Case{c}}}
}

// Owners — хозяева класса в окне сверки, по имени.
func (r *Router) Owners(class string) []string { return slices.Sorted(maps.Keys(r.cross[class])) }

// Tickets — случаи по тикетам (хозяин, класс), в порядке первого случая; без хозяина — сводка; уже ушедшие в
// тикет — мимо.
func Tickets(cases []Case) []Action {
	var out []Action
	idx := map[[2]string]int{}
	for _, c := range cases {
		if c.ticketed {
			continue
		}
		k := [2]string{c.Owner, c.Class}
		i, ok := idx[k]
		if !ok {
			i = len(out)
			idx[k] = i
			kind := Ticket
			if c.Owner == "" {
				kind = Summary
			}
			out = append(out, Action{Kind: kind, Owner: c.Owner, Class: c.Class, Why: WhyUrgent})
		}
		out[i].Cases = append(out[i].Cases, c)
	}
	return out
}

// BatchLetters — писем лиду при прежней рассылке io (до 08.11): первый случай после паузы every — письмо сразу, случаи
// в паузе — одним письмом по её концу. Для сравнения в сводке маршрута.
func BatchLetters(cases []Case, every time.Duration) int {
	var last time.Time
	n, pending := 0, 0
	for _, c := range cases {
		if pending > 0 && c.End.Sub(last) >= every {
			n++
			last, pending = last.Add(every), 0
		}
		if last.IsZero() || c.End.Sub(last) >= every {
			n++
			last = c.End
			continue
		}
		pending++
	}
	if pending > 0 {
		n++
	}
	return n
}
