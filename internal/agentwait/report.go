package agentwait

import (
	"cmp"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
)

// Version — версия формата Summary; dreamstats проверяет её. Новые поля — без смены версии.
const Version = 1

// Labels — классы целей ожидания по-русски, в порядке показа.
var Labels = []struct{ Class, Label string }{
	{"test", "тест или гейт"}, {"build", "сборка"}, {"deploy", "деплой"}, {"graft", "очередь graft"},
	{"agent", "другой агент"}, {"human", "человек"}, {"external", "внешний сервис или CI"}, {"other", "прочее"},
}

func label(class string) string {
	for _, l := range Labels {
		if l.Class == class {
			return l.Label
		}
	}
	return class
}

// Windows — окна сводки: сутки и неделя назад от момента сводки.
var Windows = []struct {
	Name string
	D    time.Duration
}{{"day", 24 * time.Hour}, {"week", 7 * 24 * time.Hour}}

// Summary — сводка для отчёта и страницы Сна (aguard agentwaits report -json).
type Summary struct {
	V  int       `json:"v"`
	At time.Time `json:"at"`
	// CoveredFrom — самое раннее ожидание в хранилище: окно, начатое раньше, видно не целиком.
	CoveredFrom time.Time          `json:"covered_from,omitzero"`
	Windows     map[string]*Window `json:"windows"`
	Bad         int                `json:"bad,omitempty"`       // битых строк в хранилище и журнале graft
	BadFirst    string             `json:"bad_first,omitempty"` // первая битая строка журнала graft с причиной
}

// Window — ожидания за окно: итог, по классам цели, примеры «прочего», очередь graft.
type Window struct {
	From    time.Time `json:"from"`
	Total   Row       `json:"total"`
	Classes []*Row    `json:"classes"` // по убыванию времени ожидания
	// Other — самые частые команды класса other: по ним решается, чему ещё учить агентов.
	Other []*Example `json:"other"`
	Graft *Graft     `json:"graft"`
}

// Row — ожидания одного класса (или все — в Total).
type Row struct {
	Class    string         `json:"class,omitempty"`
	Label    string         `json:"label,omitempty"`
	Waits    int            `json:"waits"`
	Calls    int            `json:"calls"`
	Repeats  int            `json:"repeats"`
	Tokens   int64          `json:"tokens"`
	WaitS    float64        `json:"wait_s"`
	MedianS  float64        `json:"median_s"`
	MaxS     float64        `json:"max_s"`
	Sessions int            `json:"sessions"`
	How      map[string]int `json:"how"`      // ожиданий по способу
	Runners  map[string]int `json:"runners"`  // по исполнителю: claude, codex
	Outcomes map[string]int `json:"outcomes"` // по исходу
	ms       []int64
	sessions map[string]bool
}

// Example — команда «прочего»: сколько раз ею ждали.
type Example struct {
	Command string  `json:"command"`
	How     string  `json:"how"`
	Runner  string  `json:"runner"`
	Waits   int     `json:"waits"`
	Calls   int     `json:"calls"`
	Tokens  int64   `json:"tokens"`
	WaitS   float64 `json:"wait_s"`
}

// otherShown — сколько примеров «прочего» в окне.
const otherShown = 12

func newRow(class string) *Row {
	return &Row{Class: class, Label: label(class), How: map[string]int{}, Runners: map[string]int{},
		Outcomes: map[string]int{}, sessions: map[string]bool{}}
}

func (r *Row) add(w *Wait) {
	r.Waits++
	r.Calls += w.Calls
	r.Repeats += w.Repeats
	r.Tokens += w.Tokens
	r.WaitS += float64(w.Ms) / 1000
	r.ms = append(r.ms, w.Ms)
	r.sessions[w.Session+"/"+w.Agent] = true
	r.How[w.How]++
	r.Runners[w.Runner]++
	r.Outcomes[w.Outcome]++
}

func (r *Row) finish() {
	slices.Sort(r.ms)
	if len(r.ms) > 0 {
		r.MedianS, r.MaxS = float64(r.ms[len(r.ms)/2])/1000, float64(r.ms[len(r.ms)-1])/1000
	}
	r.Sessions = len(r.sessions)
}

// Collect — сводка на момент now: ожидания из read (обычно Store.Read с начала недели) и очередь graft из
// журналов graftFiles.
func Collect(now time.Time, read func(fn func(*Wait)) (int, error), graftFiles []string) (*Summary, error) {
	s := &Summary{V: Version, At: now, Windows: map[string]*Window{}}
	type acc struct {
		w       *Window
		classes map[string]*Row
		other   map[string]*Example
	}
	accs := make([]acc, len(Windows))
	for i, win := range Windows {
		w := &Window{From: now.Add(-win.D), Total: *newRow("")}
		s.Windows[win.Name] = w
		accs[i] = acc{w: w, classes: map[string]*Row{}, other: map[string]*Example{}}
	}
	bad, err := read(func(w *Wait) {
		if s.CoveredFrom.IsZero() || w.At.Before(s.CoveredFrom) {
			s.CoveredFrom = w.At
		}
		for _, a := range accs {
			if w.At.Before(a.w.From) || !w.At.Before(now) {
				continue
			}
			a.w.Total.add(w)
			row := a.classes[w.Class]
			if row == nil {
				row = newRow(w.Class)
				a.classes[w.Class] = row
			}
			row.add(w)
			if w.Class == "other" {
				addExample(a.other, w)
			}
		}
	})
	s.Bad += bad
	if err != nil {
		return s, err
	}
	for _, a := range accs {
		a.w.Total.finish()
		for _, row := range a.classes {
			row.finish()
			a.w.Classes = append(a.w.Classes, row)
		}
		slices.SortFunc(a.w.Classes, func(x, y *Row) int {
			return cmp.Or(cmp.Compare(y.WaitS, x.WaitS), cmp.Compare(x.Class, y.Class))
		})
		a.w.Other = topExamples(a.other)
	}
	froms := make([]time.Time, len(accs))
	for i, a := range accs {
		froms[i] = a.w.From
	}
	gs, gbad, err := ReadGraft(graftFiles, froms, now)
	s.Bad += gbad.N
	s.BadFirst = gbad.First
	for i, a := range accs {
		a.w.Graft = gs[i]
	}
	return s, err
}

func addExample(m map[string]*Example, w *Wait) {
	k := w.Runner + "\x00" + w.How + "\x00" + w.Command
	e := m[k]
	if e == nil {
		e = &Example{Command: w.Command, How: w.How, Runner: w.Runner}
		m[k] = e
	}
	e.Waits++
	e.Calls += w.Calls
	e.Tokens += w.Tokens
	e.WaitS += float64(w.Ms) / 1000
}

func topExamples(m map[string]*Example) []*Example {
	out := slices.SortedFunc(maps.Values(m), func(a, b *Example) int {
		return cmp.Or(cmp.Compare(b.Waits, a.Waits), cmp.Compare(b.WaitS, a.WaitS), cmp.Compare(a.Command, b.Command))
	})
	return out[:min(len(out), otherShown)]
}

// Print печатает недельное окно: по классам, примеры «прочего», очередь graft.
func (s *Summary) Print(w io.Writer, window string) error {
	win := s.Windows[window]
	if win == nil {
		return fmt.Errorf("окна %q нет", window)
	}
	const at = "02.01 15:04"
	fmt.Fprintf(w, "Ожидания, которые агенты заводят сами, с %s по %s (местное время).\n", win.From.Local().Format(at), s.At.Local().Format(at))
	if s.CoveredFrom.After(win.From) {
		fmt.Fprintf(w, "Записи есть только с %s.\n", s.CoveredFrom.Local().Format(at))
	}
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "чего ждали\tожиданий\tвызовов\tповторов\tтокенов, тыс.\tждали всего\tмедиана / макс\tкак")
	for _, r := range slices.Concat(win.Classes, []*Row{&win.Total}) {
		name := r.Label
		if r == &win.Total {
			name = "всего"
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%s\t%s / %s\t%s\n", name, r.Waits, r.Calls, r.Repeats, r.Tokens/1000,
			span(r.WaitS), span(r.MedianS), span(r.MaxS), counts(r.How))
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("сводка: %w", err)
	}
	if len(win.Other) > 0 {
		fmt.Fprintln(w, "\nПрочее — чем ждали чаще всего:")
		for _, e := range win.Other {
			fmt.Fprintf(w, "  %d× %s %s, ждали %s: %s\n", e.Waits, e.Runner, e.How, span(e.WaitS), e.Command)
		}
	}
	g := win.Graft
	fmt.Fprintf(w, "\nОчередь graft: из %d задач ждали %d, всего %s, самая долгая %s", g.Tasks, g.Waited, span(g.WaitS), span(g.MaxS))
	if len(g.Why) > 0 {
		fmt.Fprintf(w, "; чего: %s", seconds(g.Why))
	}
	if g.CoveredFrom.After(win.From) {
		fmt.Fprintf(w, " (журнал graft с %s)", g.CoveredFrom.Local().Format(at))
	}
	fmt.Fprintln(w)
	if s.Bad > 0 {
		fmt.Fprintf(w, "\nбитых строк: %d %s\n", s.Bad, s.BadFirst)
	}
	return nil
}

// span — секунды коротко: 45 с, 12 мин, 3,5 ч.
func span(s float64) string {
	switch {
	case s < 90:
		return fmt.Sprintf("%.0f с", s)
	case s < 90*60:
		return fmt.Sprintf("%.0f мин", s/60)
	}
	return strings.Replace(fmt.Sprintf("%.1f ч", s/3600), ".", ",", 1)
}

// counts — «loop 12, wfor 3» по убыванию.
func counts(m map[string]int) string {
	keys := slices.SortedFunc(maps.Keys(m), func(a, b string) int { return cmp.Or(cmp.Compare(m[b], m[a]), cmp.Compare(a, b)) })
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %d", k, m[k])
	}
	return strings.Join(parts, ", ")
}

// seconds — «queue 12 мин, swap 6 с» по убыванию.
func seconds(m map[string]float64) string {
	keys := slices.SortedFunc(maps.Keys(m), func(a, b string) int { return cmp.Or(cmp.Compare(m[b], m[a]), cmp.Compare(a, b)) })
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + " " + span(m[k])
	}
	return strings.Join(parts, ", ")
}
