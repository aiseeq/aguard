package activity

import (
	"cmp"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
)

// Version — версия формата `aguard report -json`: смена смысла полей — новая версия.
const Version = 1

// Windows — окна отчёта: сутки, неделя, месяц до момента отчёта.
var Windows = []struct {
	Name string
	Len  time.Duration
}{{"day", 24 * time.Hour}, {"week", 7 * 24 * time.Hour}, {"month", 30 * 24 * time.Hour}}

// Line — срабатывания одного вида за окно.
type Line struct {
	Kind   string    `json:"kind"`
	What   string    `json:"what,omitempty"`
	Target string    `json:"to_whom,omitempty"`
	Count  int       `json:"count"`
	Max    int64     `json:"max,omitempty"`
	Unit   string    `json:"unit,omitempty"` // единица Max: bytes, ms, entries, procs, trees
	Last   time.Time `json:"last,omitzero"`
}

// Window — наблюдатель за одно окно.
type Window struct {
	From time.Time `json:"from"`
	// Covered — с какого момента в окне есть учёт: до первой записи учёта aguard ещё не стоял или не вёл учёт.
	Covered       time.Time `json:"covered_from,omitzero"`
	Passes        int       `json:"passes"`
	Failed        int       `json:"failed"`
	Panics        int       `json:"panics"`
	ConfigSkipped int       `json:"config_skipped"`
	Warnings      int       `json:"warnings"`
	// SilentHours — целых часов учёта без единого прохода: демон стоял или наблюдатель завис.
	SilentHours int    `json:"silent_hours"`
	Lines       []Line `json:"lines"`
}

// Watcher — один наблюдатель по окнам.
type Watcher struct {
	Name    string             `json:"name"`
	Mode    string             `json:"mode,omitempty"`
	Windows map[string]*Window `json:"windows"`
}

// Daemon — сам демон по окнам: сколько раз запускался.
type Daemon struct {
	Starts map[string]int `json:"starts"`
}

// Report — `aguard report -json`.
type Report struct {
	V        int        `json:"v"`
	At       time.Time  `json:"at"`
	Daemon   Daemon     `json:"daemon"`
	Watchers []*Watcher `json:"watchers"`
	Bad      int        `json:"bad_records,omitempty"` // битых строк в записях
}

// Spec — что известно о наблюдателе вне учёта.
type Spec struct {
	Name string
	Mode string
	// Looped — наблюдатель ходит по кругу: часы без проходов — сбой. У restart проходы — вопросы cl, их может не быть.
	Looped bool
	// Extra — строки из своих записей наблюдателя (memory, waits) за окно [from, to).
	Extra func(from, to time.Time) ([]Line, error)
}

// Build собирает отчёт на момент at из записей учёта recs (все записи за месяц).
func Build(at time.Time, recs []Record, specs []Spec) (*Report, error) {
	r := &Report{V: Version, At: at, Daemon: Daemon{Starts: map[string]int{}}}
	var first time.Time
	for i := range recs {
		if first.IsZero() || recs[i].From.Before(first) {
			first = recs[i].From
		}
	}
	for _, w := range Windows {
		from := at.Add(-w.Len)
		for i := range recs {
			if recs[i].Kind == KindStart && recs[i].To.After(from) && !recs[i].To.After(at) {
				r.Daemon.Starts[w.Name] += recs[i].Count
			}
		}
	}
	for _, s := range specs {
		wr := &Watcher{Name: s.Name, Mode: s.Mode, Windows: map[string]*Window{}}
		for _, w := range Windows {
			win, err := window(at, at.Add(-w.Len), first, recs, s)
			if err != nil {
				return r, fmt.Errorf("%s %s: %w", s.Name, w.Name, err)
			}
			wr.Windows[w.Name] = win
		}
		r.Watchers = append(r.Watchers, wr)
	}
	return r, nil
}

// window — наблюдатель за окно [from, at). Запись учёта входит в окно по концу своего периода.
func window(at, from, first time.Time, recs []Record, s Spec) (*Window, error) {
	w := &Window{From: from, Lines: []Line{}}
	if !first.IsZero() {
		w.Covered = later(from, first)
	}
	lines := map[key]*Line{}
	covered := map[time.Time]bool{}
	for i := range recs {
		rec := &recs[i]
		if rec.Watcher != s.Name || !rec.To.After(from) || rec.To.After(at) {
			continue
		}
		switch rec.Kind {
		case KindPass:
			w.Passes += rec.Count
			for h := rec.From.Truncate(time.Hour); !h.After(rec.To); h = h.Add(time.Hour) {
				covered[h] = true
			}
			continue
		case KindPassFailed:
			w.Failed += rec.Count
		case KindPanic:
			w.Panics += rec.Count
		case KindConfigSkipped:
			w.ConfigSkipped += rec.Count
		case KindWarn:
			w.Warnings += rec.Count
		}
		k := key{s.Name, rec.Kind, rec.What, rec.Target}
		l := lines[k]
		if l == nil {
			l = &Line{Kind: rec.Kind, What: rec.What, Target: rec.Target, Unit: rec.Unit}
			lines[k] = l
		}
		l.Count += rec.Count
		l.Max = max(l.Max, rec.Max)
		l.Last = later(l.Last, rec.Last)
	}
	if s.Looped && !w.Covered.IsZero() {
		// целые часы от начала учёта до часа отчёта (текущий час не закончен)
		for h := w.Covered.Truncate(time.Hour).Add(time.Hour); !h.Add(time.Hour).After(at); h = h.Add(time.Hour) {
			if !covered[h] {
				w.SilentHours++
			}
		}
	}
	for _, l := range lines {
		w.Lines = append(w.Lines, *l)
	}
	if s.Extra != nil {
		extra, err := s.Extra(from, at)
		if err != nil {
			return w, err
		}
		w.Lines = append(w.Lines, extra...)
	}
	slices.SortStableFunc(w.Lines, func(a, b Line) int {
		return cmp.Or(cmp.Compare(b.Count, a.Count), cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.What, b.What),
			cmp.Compare(a.Target, b.Target))
	})
	return w, nil
}

// Print — отчёт для человека: по наблюдателю — сутки и неделя, месяц только числом проходов.
func (r *Report) Print(out io.Writer) error {
	var b strings.Builder
	const at = "02.01 15:04 MST"
	fmt.Fprintf(&b, "aguard на %s; запусков демона: сутки %d, неделя %d, месяц %d.\n", r.At.Local().Format(at),
		r.Daemon.Starts["day"], r.Daemon.Starts["week"], r.Daemon.Starts["month"])
	for _, wr := range r.Watchers {
		fmt.Fprintf(&b, "\n%s", wr.Name)
		if wr.Mode != "" {
			fmt.Fprintf(&b, " (%s)", wr.Mode)
		}
		b.WriteString("\n")
		for _, name := range []string{"day", "week"} {
			w := wr.Windows[name]
			fmt.Fprintf(&b, "  %s: проходов %d, сбоев %d, паник %d, пропусков по конфигу %d, предупреждений %d, часов без проходов %d\n",
				name, w.Passes, w.Failed, w.Panics, w.ConfigSkipped, w.Warnings, w.SilentHours)
			for _, l := range w.Lines {
				fmt.Fprintf(&b, "    %s", l.Kind)
				if l.What != "" {
					fmt.Fprintf(&b, " %s", l.What)
				}
				if l.Target != "" {
					fmt.Fprintf(&b, " → %s", l.Target)
				}
				fmt.Fprintf(&b, ": %d", l.Count)
				if l.Max > 0 {
					b.WriteString(", макс " + valueText(l.Max, l.Unit))
				}
				b.WriteString("\n")
			}
		}
	}
	if r.Bad > 0 {
		fmt.Fprintf(&b, "\nбитых строк в записях: %d\n", r.Bad)
	}
	if _, err := io.WriteString(out, b.String()); err != nil {
		return fmt.Errorf("отчёт: %w", err)
	}
	return nil
}

// valueText — величина для человека по единице.
func valueText(v int64, unit string) string {
	switch unit {
	case "bytes":
		return policy.HumanBytes(v)
	case "ms":
		return (time.Duration(v) * time.Millisecond).Round(100 * time.Millisecond).String()
	case "entries":
		return fmt.Sprintf("%d записей", v)
	case "procs":
		return fmt.Sprintf("%d процессов", v)
	case "trees":
		return fmt.Sprintf("%d деревьев", v)
	case "tokens":
		return fmt.Sprintf("%d тыс. токенов", v/1000)
	}
	return strconv.FormatInt(v, 10)
}
