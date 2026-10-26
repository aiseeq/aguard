package said

import (
	"cmp"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/aiseeq/akit/price"

	"github.com/aiseeq/aguard/internal/daily"
)

// NewStore — дневные файлы журнала разметки в dir.
func NewStore(dir string) daily.Store[Record] {
	return daily.Store[Record]{Dir: dir, What: "журнал said", At: func(r *Record) time.Time { return r.At }}
}

// Day — сутки журнала: реплик, вызовов модели, находок по видам, отброшенных, сбоев, токенов и цены.
type Day struct {
	Day                    string
	Replies, Calls, Failed int // реплик размечено (без сбоя), вызовов всего, вызовов со сбоем
	Kinds                  map[string]int
	Dropped, Written       int
	Usage                  price.Usage
	Cost                   float64 // $ по таблице akit/price; подписка, а не счёт — для сравнения
	Ms                     int64
}

// Summary — сводка журнала по местным суткам.
func Summary(read func(fn func(*Record)) (int, error)) ([]*Day, int, error) {
	days := map[string]*Day{}
	bad, err := read(func(r *Record) {
		k := r.At.Local().Format("2006-01-02")
		d := days[k]
		if d == nil {
			d = &Day{Day: k, Kinds: map[string]int{}}
			days[k] = d
		}
		d.Calls++
		d.Usage.Input += r.Usage.Input
		d.Usage.CacheRead += r.Usage.CacheRead
		d.Usage.Output += r.Usage.Output
		if c, ok := price.Cost(r.Model, r.Usage); ok {
			d.Cost += c
		}
		d.Ms += r.Ms
		if r.Err != "" {
			d.Failed++
			return
		}
		d.Replies++
		for i := range r.Items {
			d.Kinds[r.Items[i].Kind]++
		}
		d.Dropped += len(r.Dropped)
		if r.Written {
			d.Written += len(r.Items)
		}
	})
	out := make([]*Day, 0, len(days))
	for _, d := range days {
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b *Day) int { return cmp.Compare(a.Day, b.Day) })
	return out, bad, err
}

// Print — сводка таблицей.
func Print(w io.Writer, days []*Day) error {
	if _, err := fmt.Fprintln(w, "сутки       реплик  rule decision promise  отброш записано  сбоев  вход/реплику  выход/реплику  токенов всего     $  с/вызов"); err != nil {
		return fmt.Errorf("вывод: %w", err)
	}
	for _, d := range days {
		in, out := d.Usage.Input+d.Usage.CacheRead, d.Usage.Output
		per := func(n int64) int64 { return n / int64(max(1, d.Calls)) }
		if _, err := fmt.Fprintf(w, "%s  %6d  %4d %8d %7d  %6d %8d  %5d  %12d  %13d  %13d  %5.2f  %7.1f\n",
			d.Day, d.Replies, d.Kinds[KindRule], d.Kinds[KindDecision], d.Kinds[KindPromise], d.Dropped, d.Written,
			d.Failed, per(in), per(out), in+out, d.Cost, float64(d.Ms)/1000/float64(max(1, d.Calls))); err != nil {
			return fmt.Errorf("вывод: %w", err)
		}
	}
	return nil
}
