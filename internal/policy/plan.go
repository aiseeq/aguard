package policy

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// Target — сессия-адресат сводки.
type Target struct {
	Name   string
	Pid    int
	Socket string
}

// Key — имя сессии или pid безымянной.
func (t Target) Key() string {
	if t.Name != "" {
		return t.Name
	}
	return "pid " + strconv.Itoa(t.Pid)
}

// Live — что user-часть знает о живых сессиях (в тестах подменяется).
type Live interface {
	// ByPid — живая сессия с этим pid и временем старта.
	ByPid(pid int, procStart string) (Target, bool)
	// ByName — живая сессия с этим именем.
	ByName(name string) (Target, bool)
}

// AlertState — сколько сводок уже ушло о записи и кому последняя.
type AlertState struct {
	Sent       int       `json:"sent"`
	LastTarget string    `json:"last_target"`
	LastSent   time.Time `json:"last_sent"`
}

// UserState — состояние эскалации, хранится в ~/.local/state/aguard/tmp/state.json.
type UserState struct {
	Mode   string                 `json:"mode"`
	Alerts map[string]*AlertState `json:"alerts"`
}

// EntryKey — запись определяется путём и временем рождения.
func EntryKey(it *Item) string {
	return it.Entry.Path + "|" + strconv.FormatInt(it.Entry.Born.Unix(), 10)
}

// Relation — почему запись попала именно этому адресату.
type Relation int

// Отношения адресата к записи.
const (
	RelOwn       Relation = iota // создала сама сессия-адресат
	RelFighter                   // создал боец этой сессии
	RelDeadOwner                 // сессия-создатель завершилась, адресат — ступень выше
	RelIgnored                   // создатель жив, но не убрал за несколько сводок
	RelNonAgent                  // создатель вне сессий Claude
	RelOtherUID                  // другая учётка
	RelNoCreator                 // запись старше сторожа
)

// Line — строка сводки.
type Line struct {
	Item        Item
	Rel         Relation
	CreatorName string // имя сессии-создателя (для эскалации)
	Sent        int    // сколько сводок о записи уже было
}

// Summary — одна сводка одному адресату.
type Summary struct {
	Target   Target
	Lines    []Line
	Pressure bool
	Factor   float64
	// NextAt — для отложенной сводки: когда верхней ступени можно повторить
	// (самый ранний срок среди её строк).
	NextAt time.Time
}

type step struct {
	t       Target
	creator bool
}

// ladder — живые ступени для записи: сессия-создатель, админ её
// пространства, лид. Не агенты и прочие — сразу лид.
func (c *Config) ladder(it *Item, live Live) (steps []step, creatorName string, creatorAlive bool) {
	seen := map[string]bool{}
	add := func(t Target, creator bool) {
		if seen[t.Key()] {
			return
		}
		seen[t.Key()] = true
		steps = append(steps, step{t: t, creator: creator})
	}
	if it.Class == ClassSession || it.Class == ClassFighter {
		s := it.Entry.Owner.Session
		creatorName = s.Name
		if t, ok := live.ByPid(s.Pid, s.ProcStart); ok {
			creatorAlive = true
			add(t, true)
		}
		if admin := c.adminFor(s.Cwd); admin != "" {
			if t, ok := live.ByName(admin); ok {
				add(t, false)
			}
		}
	}
	if t, ok := live.ByName(c.Lead); ok {
		add(t, false)
	}
	return steps, creatorName, creatorAlive
}

// adminFor — админ пространства по самому длинному подходящему префиксу cwd.
func (c *Config) adminFor(cwd string) string {
	best, name := -1, ""
	for prefix, n := range c.Escalation.Admins {
		if (cwd == prefix || strings.HasPrefix(cwd, prefix+"/")) && len(prefix) > best {
			best, name = len(prefix), n
		}
	}
	return name
}

// Plan раскладывает записи владельцев сверх порога по адресатам с учётом
// эскалации и обновляет счётчики в st. Вызывающий сохраняет st после
// успешной доставки (в observe — сразу: имитация). Убранные записи забывает
// Forget, не Plan: запись ниже порога (давление памяти спало) не убрана.
//
// send — сводки этого прохода. deferred — строки, которые верхняя ступень
// уже получала и получит повтором не раньше top_repeat; счётчики их не
// меняются, они нужны, чтобы dry-run и журнал показывали, что записи сверх
// порога есть, но сводка о них отложена.
func (c *Config) Plan(now time.Time, groups []Group, live Live, st *UserState, factor float64) (send, deferred []Summary) {
	if st.Alerts == nil {
		st.Alerts = map[string]*AlertState{}
	}
	sendB, deferB := newSummaries(factor), newSummaries(factor)
	for _, g := range groups {
		for i := range g.Items {
			it := &g.Items[i]
			key := EntryKey(it)
			steps, cname, calive := c.ladder(it, live)
			if len(steps) == 0 {
				continue // некому писать: ни создателя, ни лида
			}
			as := st.Alerts[key]
			if as == nil {
				as = &AlertState{}
				st.Alerts[key] = as
			}
			idx := min(as.Sent/c.Escalation.After, len(steps)-1)
			s := steps[idx]
			line := Line{Item: *it, Rel: relation(it, s, calive), CreatorName: cname, Sent: as.Sent}
			if idx == len(steps)-1 && as.Sent >= c.Escalation.After && as.LastTarget == s.t.Key() &&
				now.Sub(as.LastSent) < c.Escalation.TopRepeat.D() {
				deferB.add(s.t, line, as.LastSent.Add(c.Escalation.TopRepeat.D())) // верхняя ступень уже знает
				continue
			}
			sendB.add(s.t, line, time.Time{})
			as.Sent++
			as.LastTarget = s.t.Key()
			as.LastSent = now
		}
	}
	return sendB.list(now), deferB.list(now)
}

// Forget забывает эскалацию записей, которых больше нет в /tmp (items — все
// живые записи прохода, не только сверх порога). Раньше забывалась и запись,
// просто не прошедшая порог: 27.10 02:41–02:56 давление памяти то включало
// множитель ×0.5, то снимало, и те же 4 записи уходили лиду и git-admin
// каждым проходом под давлением как новые, мимо top_repeat.
func (st *UserState) Forget(items []Item) {
	keep := make(map[string]bool, len(items))
	for i := range items {
		keep[EntryKey(&items[i])] = true
	}
	for k := range st.Alerts {
		if !keep[k] {
			delete(st.Alerts, k)
		}
	}
}

// summaries — сводки по адресатам в порядке появления.
type summaries struct {
	factor float64
	by     map[string]*Summary
	order  []string
}

func newSummaries(factor float64) *summaries {
	return &summaries{factor: factor, by: map[string]*Summary{}}
}

func (b *summaries) add(t Target, l Line, next time.Time) {
	s := b.by[t.Key()]
	if s == nil {
		s = &Summary{Target: t, Pressure: b.factor < 1, Factor: b.factor}
		b.by[t.Key()] = s
		b.order = append(b.order, t.Key())
	}
	s.Lines = append(s.Lines, l)
	if !next.IsZero() && (s.NextAt.IsZero() || next.Before(s.NextAt)) {
		s.NextAt = next
	}
}

func (b *summaries) list(now time.Time) []Summary {
	out := make([]Summary, 0, len(b.order))
	for _, k := range b.order {
		s := b.by[k]
		sort.SliceStable(s.Lines, func(i, j int) bool {
			if s.Lines[i].Rel != s.Lines[j].Rel {
				return s.Lines[i].Rel < s.Lines[j].Rel
			}
			return s.Lines[i].Item.GBMin(now) > s.Lines[j].Item.GBMin(now)
		})
		out = append(out, *s)
	}
	return out
}

// Count — число записей в сводках.
func Count(sums []Summary) int {
	n := 0
	for i := range sums {
		n += len(sums[i].Lines)
	}
	return n
}

func relation(it *Item, s step, creatorAlive bool) Relation {
	switch it.Class {
	case ClassSession, ClassFighter:
		switch {
		case s.creator && it.Class == ClassFighter:
			return RelFighter
		case s.creator:
			return RelOwn
		case !creatorAlive:
			return RelDeadOwner
		}
		return RelIgnored
	case ClassNonAgent:
		return RelNonAgent
	case ClassOtherUID:
		return RelOtherUID
	}
	return RelNoCreator
}
