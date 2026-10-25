package waits

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"text/tabwriter"
	"time"

	"github.com/aiseeq/aguard/internal/daily"
	"github.com/aiseeq/aguard/internal/policy"
)

// Config — раздел [waits] конфига aguard.
type Config struct {
	Interval   policy.Duration `toml:"interval"`    // как часто читать дописанное
	Backfill   policy.Duration `toml:"backfill"`    // насколько назад догрузить транскрипты, бывшие до первого прохода
	BackfillMB int64           `toml:"backfill_mb"` // сколько МБ догрузки за проход
	Keep       policy.Duration `toml:"keep"`        // сколько хранить дневные файлы событий
}

// Default — начальные значения: раз в минуту, догрузка недели по 200 МБ за проход, хранение 60 дней.
func Default() Config {
	return Config{
		Interval: policy.Duration(time.Minute), Backfill: policy.Duration(7 * 24 * time.Hour),
		BackfillMB: 200, Keep: policy.Duration(60 * 24 * time.Hour),
	}
}

// Validate проверяет раздел.
func (c *Config) Validate() error {
	if c.Interval.D() < 10*time.Second {
		return fmt.Errorf("interval = %v: не чаще раза в 10 с", c.Interval.D())
	}
	if c.Backfill.D() < 0 || c.BackfillMB < 0 {
		return errors.New("backfill и backfill_mb не могут быть отрицательными")
	}
	if c.Keep.D() < 24*time.Hour {
		return fmt.Errorf("keep = %v: не меньше суток", c.Keep.D())
	}
	return nil
}

// Tail — проход по транскриптам в projects по разделу.
func (c *Config) Tail(projects string) *Tail {
	return &Tail{Projects: projects, Backfill: c.Backfill.D(), BackfillBytes: c.BackfillMB << 20}
}

// NewStore — дневные файлы событий в dir.
func NewStore(dir string) daily.Store[Event] {
	return daily.Store[Event]{Dir: dir, What: "события транскриптов", At: func(e *Event) time.Time { return e.At }}
}

// group — события одного вида и подробности.
type group struct {
	Kind, Detail string
	Count        int
	Sessions     map[string]bool
	Last         time.Time
	Ms           []int64
}

// Report — сводка событий за окно.
type Report struct {
	From, To time.Time
	Groups   []*group
	// Hours — по часам местного времени: таймауты хуков и ошибки API (ожидания, а не сжатия).
	Hours map[time.Time]int
	// Who — сессии с наибольшим числом таймаутов хуков и ошибок API.
	Who map[string]int
	Bad int
}

// Collect собирает сводку из read (обычно Store.Read с началом окна).
func Collect(read func(fn func(*Event)) (int, error)) (*Report, error) {
	r := &Report{Hours: map[time.Time]int{}, Who: map[string]int{}}
	groups := map[string]*group{}
	bad, err := read(func(e *Event) {
		if r.From.IsZero() || e.At.Before(r.From) {
			r.From = e.At
		}
		r.To = later(r.To, e.At)
		k := e.Kind + "\x00" + e.Detail
		g := groups[k]
		if g == nil {
			g = &group{Kind: e.Kind, Detail: e.Detail, Sessions: map[string]bool{}}
			groups[k] = g
		}
		g.Count++
		g.Sessions[e.Session] = true
		g.Last = later(g.Last, e.At)
		if e.Ms > 0 {
			g.Ms = append(g.Ms, e.Ms)
		}
		if e.Kind != KindCompact {
			r.Hours[e.At.Local().Truncate(time.Hour)]++
			r.Who[cmp.Or(e.Name, e.Project)]++
		}
	})
	r.Bad = bad
	if err != nil {
		return r, err
	}
	for _, k := range slices.Sorted(maps.Keys(groups)) {
		slices.Sort(groups[k].Ms)
		r.Groups = append(r.Groups, groups[k])
	}
	slices.SortStableFunc(r.Groups, func(a, b *group) int { return cmp.Compare(b.Count, a.Count) })
	return r, nil
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// msText — медиана и максимум длительностей.
func msText(ms []int64) string {
	if len(ms) == 0 {
		return "—"
	}
	d := func(v int64) string {
		return (time.Duration(v) * time.Millisecond).Round(100 * time.Millisecond).String()
	}
	return d(ms[len(ms)/2]) + " / " + d(ms[len(ms)-1])
}

// Print печатает сводку.
func (r *Report) Print(w io.Writer) error {
	if len(r.Groups) == 0 {
		_, err := fmt.Fprintln(w, "событий нет")
		return err
	}
	const at = "02.01 15:04"
	fmt.Fprintf(w, "Отказы и ожидания агентов с %s по %s (по транскриптам, местное время).\n\n", r.From.Local().Format(at), r.To.Local().Format(at))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "вид\tчто\tраз\tсессий\tпоследнее\tдлительность: медиана / макс")
	for _, g := range r.Groups {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\t%s\n", g.Kind, g.Detail, g.Count, len(g.Sessions), g.Last.Local().Format(at), msText(g.Ms))
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("сводка: %w", err)
	}
	if len(r.Hours) > 0 {
		fmt.Fprintln(w, "\nТаймауты хуков и ошибки API по часам:")
		for _, h := range slices.SortedFunc(maps.Keys(r.Hours), time.Time.Compare) {
			// с поясом: в ночь перехода на зимнее время час 02:00 бывает дважды
			fmt.Fprintf(w, "  %s  %d\n", h.Format(at+" MST"), r.Hours[h])
		}
		fmt.Fprintln(w, "\nПо сессиям:")
		who := slices.SortedStableFunc(maps.Keys(r.Who), func(a, b string) int { return cmp.Compare(r.Who[b], r.Who[a]) })
		for _, n := range who {
			fmt.Fprintf(w, "  %s  %d\n", n, r.Who[n])
		}
	}
	if r.Bad > 0 {
		fmt.Fprintf(w, "\nбитых строк в записях: %d\n", r.Bad)
	}
	return nil
}
