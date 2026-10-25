// Package report строит `tmpguard report`: распределение времени жизни
// записей по корзинам размера и по классам создателей — по нему лид
// выбирает пороги политики.
package report

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"tmpguard/internal/policy"
	"tmpguard/internal/record"
)

// Корзины размера и времени жизни.
var (
	sizeEdges  = []int64{1 << 20, 10 << 20, 100 << 20, 1 << 30}
	sizeLabels = []string{"<1 МБ", "1–10 МБ", "10–100 МБ", "100 МБ–1 ГБ", "≥1 ГБ"}
	lifeEdges  = []time.Duration{10 * time.Second, time.Minute, 10 * time.Minute, time.Hour, 6 * time.Hour, 24 * time.Hour}
	lifeLabels = []string{"<10с", "<1м", "<10м", "<1ч", "<6ч", "<1д", "≥1д"}
)

const unmeasured = "не замерено"

func sizeBucket(b int64) int {
	for i, e := range sizeEdges {
		if b < e {
			return i
		}
	}
	return len(sizeEdges)
}

func lifeBucket(d time.Duration) int {
	for i, e := range lifeEdges {
		if d < e {
			return i
		}
	}
	return len(lifeEdges)
}

// Rec — одна запись для отчёта (закрытая или живая).
type Rec struct {
	Path     string
	Class    policy.Class
	Creator  string // имя сессии или команда
	Life     time.Duration
	MaxBytes int64
	Measured bool
	GBMin    float64
	Bytes    int64 // живые: размер сейчас
	Live     bool
}

// Data — входные данные отчёта.
type Data struct {
	From, To  time.Time
	Closed    []Rec
	Live      []Rec
	BadLines  int
	TrackSeen time.Time // время снимка
}

// Collect собирает данные из событий журнала и снимка.
func Collect(cfg *policy.Config, events []record.Event, st *record.State, since time.Time, myUID int, now time.Time) Data {
	d := Data{To: now, TrackSeen: st.Updated}
	for _, e := range events {
		if !e.Closed() || e.T.Before(since) || cfg.Ignored(e.Path) {
			continue
		}
		if d.From.IsZero() || e.T.Before(d.From) {
			d.From = e.T
		}
		cl, _ := cfg.Classify(e.Path, e.Owner, e.UID, myUID)
		d.Closed = append(d.Closed, Rec{
			Path: e.Path, Class: cl, Creator: creatorOf(cl, e.Owner), Life: time.Duration(e.LifeS * float64(time.Second)),
			MaxBytes: e.MaxBytes, Measured: e.Measured, GBMin: e.ByteSec / policy.GiBMin,
		})
	}
	for _, it := range cfg.Items(st.Entries, myUID) {
		d.Live = append(d.Live, Rec{
			Path: it.Entry.Path, Class: it.Class, Creator: creatorOf(it.Class, it.Entry.Owner), Life: now.Sub(it.Entry.Born),
			MaxBytes: it.Entry.MaxBytes, Bytes: it.Entry.Bytes, Measured: !it.Entry.MeasuredAt.IsZero(), GBMin: it.GBMin(), Live: true,
		})
	}
	return d
}

// creatorOf — по кому группировать в топе: кеши отдельно от записей без создателя.
func creatorOf(cl policy.Class, o *record.Owner) string {
	if cl == policy.ClassCache {
		return "(общие кеши)"
	}
	return creatorName(o)
}

func creatorName(o *record.Owner) string {
	switch {
	case o == nil:
		return "(неизвестен)"
	case o.Session != nil && o.Session.Name != "":
		return "сессия " + o.Session.Name
	case o.Session != nil:
		return fmt.Sprintf("сессия pid %d", o.Session.Pid)
	case o.Proc.Comm != "":
		return o.Proc.Comm
	}
	return "?"
}

// Print печатает отчёт.
func Print(w io.Writer, d Data) {
	fmt.Fprintf(w, "tmpguard report: закрытых записей %d (с %s), живых %d (снимок %s)\n",
		len(d.Closed), d.From.Format("2006-01-02 15:04"), len(d.Live), d.TrackSeen.Format("15:04:05"))
	if d.BadLines > 0 {
		fmt.Fprintf(w, "битых строк журнала: %d\n", d.BadLines)
	}
	fmt.Fprintln(w, "\n1. Закрытые записи: наибольший размер × время жизни (число записей).")
	fmt.Fprintln(w, "   «не замерено» — умерли раньше первого замера (молодые записи замеряются раз в минуту);")
	fmt.Fprintln(w, "   на политику не влияют: за минуту порог набирает только запись больше порога в ГБ.")
	matrix(w, d.Closed)
	fmt.Fprintln(w, "\n2. Живые записи: наибольший размер × возраст сейчас.")
	matrix(w, d.Live)
	fmt.Fprintln(w, "\n3. По классам создателей (жизнь — медиана и 90-й процентиль закрытых; мера — ГБ·мин).")
	byClass(w, d)
	fmt.Fprintln(w, "\n4. Создатели с наибольшей мерой (закрытые + живые), топ 15.")
	topCreators(w, append(append([]Rec{}, d.Closed...), d.Live...))
}

func matrix(w io.Writer, recs []Rec) {
	rows := append(append([]string{}, sizeLabels...), unmeasured)
	m := make([][]int, len(rows))
	for i := range m {
		m[i] = make([]int, len(lifeLabels))
	}
	for _, r := range recs {
		row := len(rows) - 1
		if r.Measured {
			row = sizeBucket(r.MaxBytes)
		}
		m[row][lifeBucket(r.Life)]++
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintf(tw, "размер \\ жизнь\t%s\tвсего\t\n", strings.Join(lifeLabels, "\t"))
	for i, label := range rows {
		total := 0
		cells := make([]string, len(lifeLabels))
		for j, v := range m[i] {
			total += v
			cells[j] = dash(v)
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t\n", label, strings.Join(cells, "\t"), total)
	}
	_ = tw.Flush()
}

func dash(v int) string {
	if v == 0 {
		return "·"
	}
	return fmt.Sprint(v)
}

func byClass(w io.Writer, d Data) {
	type agg struct {
		lifes            []time.Duration
		closedGB, liveGB float64
		maxB, liveB      int64
		nClosed, nLive   int
	}
	m := map[policy.Class]*agg{}
	get := func(c policy.Class) *agg {
		if m[c] == nil {
			m[c] = &agg{}
		}
		return m[c]
	}
	for _, r := range d.Closed {
		a := get(r.Class)
		a.nClosed++
		a.lifes = append(a.lifes, r.Life)
		a.closedGB += r.GBMin
		a.maxB = max(a.maxB, r.MaxBytes)
	}
	for _, r := range d.Live {
		a := get(r.Class)
		a.nLive++
		a.liveGB += r.GBMin
		a.liveB += r.Bytes
		a.maxB = max(a.maxB, r.MaxBytes)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "класс\tзакрыто\tжизнь p50\tжизнь p90\tмера закр.\tживых\tразмер живых\tмера живых\tсамая большая")
	for _, c := range policy.Classes {
		a := m[c]
		if a == nil {
			continue
		}
		p50, p90 := pct(a.lifes, 0.5), pct(a.lifes, 0.9)
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n", c, a.nClosed, p50, p90, policy.FormatGBMin(a.closedGB),
			a.nLive, policy.HumanBytes(a.liveB), policy.FormatGBMin(a.liveGB), policy.HumanBytes(a.maxB))
	}
	_ = tw.Flush()
}

func pct(ds []time.Duration, q float64) string {
	if len(ds) == 0 {
		return "—"
	}
	s := append([]time.Duration{}, ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return lifeText(s[int(q*float64(len(s)-1))])
}

// lifeText — как HumanAge, но короче минуты с десятыми: go-build и mktemp
// живут доли секунды, и «0 с» читалось бы как обнулённое время жизни.
func lifeText(d time.Duration) string {
	if d < time.Minute {
		return strconv.FormatFloat(d.Seconds(), 'f', 1, 64) + " с"
	}
	return policy.HumanAge(d)
}

func topCreators(w io.Writer, recs []Rec) {
	type agg struct {
		n   int
		gb  float64
		max int64
		cl  policy.Class
	}
	m := map[string]*agg{}
	for _, r := range recs {
		a := m[r.Creator]
		if a == nil {
			a = &agg{cl: r.Class}
			m[r.Creator] = a
		}
		a.n++
		a.gb += r.GBMin
		a.max = max(a.max, r.MaxBytes)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return m[keys[i]].gb > m[keys[j]].gb })
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "создатель\tкласс\tзаписей\tмера, ГБ·мин\tсамая большая")
	for i, k := range keys {
		if i >= 15 {
			break
		}
		a := m[k]
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", k, a.cl, a.n, policy.FormatGBMin(a.gb), policy.HumanBytes(a.max))
	}
	_ = tw.Flush()
}
