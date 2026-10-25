package memwatch

import (
	"cmp"
	"fmt"
	"io"
	"slices"
	"text/tabwriter"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
)

// Thresholds — пороги, для которых сводка считает, сколько раз их бы перешли.
var Thresholds = []int64{2 << 30, 4 << 30, 6 << 30, 8 << 30, 10 << 30, 12 << 30, 16 << 30}

// roleStat — сводка по имени сессии (роль: lead, git-admin, …) за все её запуски.
type roleStat struct {
	Name     string
	Scopes   map[string]bool
	Samples  int
	First    time.Time
	Last     time.Time
	Peaks    []int64
	Max      Sample // запись с наибольшим пиком
	Before   int64  // наибольший пик за жизнь группы до начала записи
	HighHits int64  // события memory.high (рост за период)
	OOM      int64
}

// Report — сводка записей для выбора порога.
type Report struct {
	From, To time.Time
	Bad      int
	Roles    []*roleStat
	Over     []thresholdHits
}

type thresholdHits struct {
	Limit    int64
	Samples  int             // интервалов с пиком выше порога
	Sessions map[string]bool // scope, у которых такой интервал был
}

// Collect собирает сводку из записей.
func Collect(samples func(fn func(*Sample)) (int, error)) (*Report, error) {
	roles := map[string]*roleStat{}
	over := make([]thresholdHits, len(Thresholds))
	for i, t := range Thresholds {
		over[i] = thresholdHits{Limit: t, Sessions: map[string]bool{}}
	}
	lastEvents := map[string]map[string]int64{}
	r := &Report{}
	bad, err := samples(func(s *Sample) {
		if r.From.IsZero() || s.At.Before(r.From) {
			r.From = s.At
		}
		if s.At.After(r.To) {
			r.To = s.At
		}
		rs := roles[s.Name]
		if rs == nil {
			rs = &roleStat{Name: s.Name, Scopes: map[string]bool{}, First: s.At}
			roles[s.Name] = rs
		}
		rs.Scopes[s.Scope] = true
		rs.Samples++
		rs.Last = s.At
		prev, seen := lastEvents[s.Scope]
		lastEvents[s.Scope] = s.Events
		if s.First {
			// пик за жизнь группы до начала записи: когда он был — неизвестно
			rs.Before = max(rs.Before, s.Peak)
			return
		}
		rs.Peaks = append(rs.Peaks, s.Peak)
		if s.Peak > rs.Max.Peak {
			rs.Max = *s
		}
		if seen { // счётчики событий растут за всю жизнь группы: считаем прирост внутри окна
			rs.HighHits += max(0, s.Events["high"]-prev["high"])
			rs.OOM += max(0, s.Events["oom_kill"]-prev["oom_kill"])
		}
		for i := range over {
			if s.Peak > over[i].Limit {
				over[i].Samples++
				over[i].Sessions[s.Scope] = true
			}
		}
	})
	r.Bad = bad
	if err != nil {
		return r, err
	}
	for _, rs := range roles {
		slices.Sort(rs.Peaks)
		r.Roles = append(r.Roles, rs)
	}
	slices.SortFunc(r.Roles, func(a, b *roleStat) int { return cmp.Compare(b.Max.Peak, a.Max.Peak) })
	r.Over = over
	return r, nil
}

func quantile(sorted []int64, q float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[int(q*float64(len(sorted)-1))]
}

// Print печатает сводку.
func (r *Report) Print(w io.Writer) error {
	if len(r.Roles) == 0 {
		_, err := fmt.Fprintln(w, "записей нет")
		return err
	}
	const at = "02.01 15:04"
	fmt.Fprintf(w, "Память сессий с %s по %s (пик группы cgroup за интервал, с файловым кешем).\n\n",
		r.From.Format(at), r.To.Format(at))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "сессия\tзапусков\tчасов\tp50\tp95\tp99\tмакс\tкогда\tкрупнейший процесс в тот интервал\tmemory.high\toom\tдо записи")
	for _, rs := range r.Roles {
		top := "—"
		if len(rs.Max.Top) > 0 {
			p := rs.Max.Top[0]
			top = fmt.Sprintf("%s %s: %s", p.Comm, policy.HumanBytes(p.RSS), p.Args)
		}
		fmt.Fprintf(tw, "%s\t%d\t%.0f\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%s\n", rs.Name, len(rs.Scopes), rs.Last.Sub(rs.First).Hours(),
			policy.HumanBytes(quantile(rs.Peaks, 0.5)), policy.HumanBytes(quantile(rs.Peaks, 0.95)),
			policy.HumanBytes(quantile(rs.Peaks, 0.99)), policy.HumanBytes(rs.Max.Peak), rs.Max.At.Format(at),
			top, rs.HighHits, rs.OOM, policy.HumanBytes(rs.Before))
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("сводка: %w", err)
	}
	fmt.Fprintln(w, "\nСколько раз порог был бы перейдён (интервалов с пиком выше; запусков сессий):")
	for _, o := range r.Over {
		fmt.Fprintf(w, "  %s: интервалов %d, запусков %d\n", policy.HumanBytes(o.Limit), o.Samples, len(o.Sessions))
	}
	if r.Bad > 0 {
		fmt.Fprintf(w, "\nбитых строк: %d\n", r.Bad)
	}
	return nil
}
