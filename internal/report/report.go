// Package report строит `aguard tmp report`: распределение времени жизни
// записей по корзинам размера и по классам создателей — по нему лид
// выбирает пороги политики.
package report

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
	"github.com/aiseeq/aguard/internal/record"
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
	for i := range events {
		e := &events[i]
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
	items := cfg.Items(st.Entries, myUID)
	for i := range items {
		it := &items[i]
		d.Live = append(d.Live, Rec{
			Path: it.Entry.Path, Class: it.Class, Creator: creatorOf(it.Class, it.Entry.Owner), Life: now.Sub(it.Entry.Born),
			MaxBytes: it.Entry.MaxBytes, Bytes: it.Entry.Bytes, Measured: !it.Entry.MeasuredAt.IsZero(), GBMin: it.Entry.ByteSec / policy.GiBMin, Live: true,
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
func Print(w io.Writer, d Data) error {
	fmt.Fprintf(w, "aguard tmp report: закрытых записей %d (с %s по %s), живых %d (снимок %s)\n",
		len(d.Closed), d.From.Format("2006-01-02 15:04"), d.To.Format("2006-01-02 15:04"), len(d.Live), d.TrackSeen.Format("15:04:05"))
	if d.BadLines > 0 {
		fmt.Fprintf(w, "битых строк журнала: %d\n", d.BadLines)
	}
	fmt.Fprintln(w, "\n1. Закрытые записи: наибольший размер × время жизни (число записей).")
	fmt.Fprintln(w, "   «не замерено» — умерли раньше первого замера (молодые записи замеряются раз в минуту);")
	fmt.Fprintln(w, "   на политику не влияют: за минуту порог набирает только запись больше порога в ГБ.")
	err := matrix(w, d.Closed)
	fmt.Fprintln(w, "\n2. Живые записи: наибольший размер × возраст сейчас.")
	err = errors.Join(err, matrix(w, d.Live))
	fmt.Fprintln(w, "\n3. Мелочь числом: живые записи меньше 1 МиБ, по шаблону имени (топ 10).")
	err = errors.Join(err, printLitter(w, d.Live))
	fmt.Fprintln(w, "\n4. По классам создателей (жизнь — медиана и 90-й процентиль закрытых; мера — ГБ·мин).")
	err = errors.Join(err, byClass(w, d))
	fmt.Fprintln(w, "\n5. Создатели с наибольшей мерой (закрытые + живые), топ 15.")
	return errors.Join(err, topCreators(w, append(append([]Rec{}, d.Closed...), d.Live...)))
}

const litterMaxBytes = 1 << 20

// NoticeablePatternCount — 2 тыс.: в живом снимке шумные серии worker-тестов
// дают около 1 тыс. на шаблон, glint — 3 976, а Chrome — десятки тысяч.
const NoticeablePatternCount = 2000

type LitterGroup struct {
	Name         string
	Count        int
	Bytes        int64
	P50          time.Duration
	OlderThanDay int
	Classes      map[policy.Class]int
	ages         []time.Duration
}

// NameMask схлопывает уникальные шестисимвольные хвосты и нумерованные имена.
func NameMask(name string) string {
	base := filepath.Base(name)
	if start, ok := randomSuffixStart(base); ok {
		return base[:start] + "XXXXXX"
	}
	i := len(base)
	for i > 0 && base[i-1] >= '0' && base[i-1] <= '9' {
		i--
	}
	if i < len(base) {
		return base[:i] + "N"
	}
	return base
}

func randomSuffixStart(base string) (int, bool) {
	const knownPrefix = "scoped_dir"
	if strings.HasPrefix(base, knownPrefix) && randomToken(base[len(knownPrefix):]) {
		return len(knownPrefix), true
	}
	sep := strings.LastIndexAny(base, ".-_")
	if sep < 0 || sep == len(base)-1 {
		return 0, false
	}
	tail := base[sep+1:]
	if !randomToken(tail) {
		return 0, false
	}
	// glint-cmd-testN — известная схема нумерации; короткий хвост иначе
	// может выглядеть как случайный шестисимвольный токен (test10).
	if base[sep] != '.' && numberedTestTail(tail) {
		return 0, false
	}
	// Слова в нижнем регистре после точки обычно являются расширениями
	// или именами сокетов (.config, .socket); числовые и смешанные хвосты случайны.
	if base[sep] == '.' && lowercaseWord(tail) {
		return 0, false
	}
	return sep + 1, true
}

func randomToken(s string) bool {
	if len(s) < 6 {
		return false
	}
	letter := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
			letter = true
			continue
		}
		if c < '0' || c > '9' {
			return false
		}
	}
	return letter
}

func lowercaseWord(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 'a' || s[i] > 'z' {
			return false
		}
	}
	return true
}

func numberedTestTail(s string) bool {
	if !strings.HasPrefix(s, "test") {
		return false
	}
	digits := s[len("test"):]
	if digits == "" {
		return false
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return false
		}
	}
	return true
}

// LitterSummary считает маленькие живые записи по родительскому каталогу и маске.
func LitterSummary(recs []Rec) []LitterGroup {
	groups := make(map[string]*LitterGroup)
	for _, rec := range recs {
		if rec.Bytes >= litterMaxBytes {
			continue
		}
		name := NameMask(filepath.Base(rec.Path))
		key := filepath.Dir(rec.Path) + string(filepath.Separator) + name
		g := groups[key]
		if g == nil {
			g = &LitterGroup{Name: key, Classes: make(map[policy.Class]int)}
			groups[key] = g
		}
		g.Count++
		g.Bytes += rec.Bytes
		g.ages = append(g.ages, rec.Life)
		g.Classes[rec.Class]++
		if rec.Life > 24*time.Hour {
			g.OlderThanDay++
		}
	}
	out := make([]LitterGroup, 0, len(groups))
	for _, g := range groups {
		slices.Sort(g.ages)
		g.P50 = g.ages[(len(g.ages)-1)/2]
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func printLitter(w io.Writer, recs []Rec) error {
	groups := LitterSummary(recs)
	if len(groups) == 0 {
		_, err := fmt.Fprintln(w, "   нет")
		return err
	}
	limit := min(len(groups), 10)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "шаблон\tчисло\tвозраст p50\tстарше 1 сут\tразмер\tкласс создателя")
	for _, g := range groups[:limit] {
		classes := make([]string, 0, len(policy.Classes))
		for _, class := range policy.Classes {
			if count := g.Classes[class]; count > 0 {
				classes = append(classes, fmt.Sprintf("%s %d", class, count))
			}
		}
		name := g.Name
		if g.Count >= NoticeablePatternCount {
			name += " (заметно)"
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%d\t%s\t%s\n", name, g.Count, policy.HumanAge(g.P50),
			g.OlderThanDay, policy.HumanBytes(g.Bytes), strings.Join(classes, ", "))
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("отчёт мелких записей: %w", err)
	}
	if len(groups) > limit && groups[limit].Count == groups[limit-1].Count {
		tieEnd := limit
		for tieEnd < len(groups) && groups[tieEnd].Count == groups[limit-1].Count {
			tieEnd++
		}
		fmt.Fprintf(w, "   ещё на границе топ-10 (%d записей, шаблонов %d):", groups[limit-1].Count, tieEnd-limit)
		for _, g := range groups[limit:min(tieEnd, limit+10)] {
			fmt.Fprintf(w, " %s", g.Name)
		}
		if tieEnd > limit+10 {
			fmt.Fprintf(w, " и ещё %d", tieEnd-limit-10)
		}
		fmt.Fprintln(w)
	}
	return nil
}

func matrix(w io.Writer, recs []Rec) error {
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
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("отчёт: %w", err)
	}
	return nil
}

func dash(v int) string {
	if v == 0 {
		return "·"
	}
	return fmt.Sprint(v)
}

func byClass(w io.Writer, d Data) error {
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
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("отчёт: %w", err)
	}
	return nil
}

func pct(ds []time.Duration, q float64) string {
	if len(ds) == 0 {
		return "—"
	}
	s := append([]time.Duration{}, ds...)
	slices.Sort(s)
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

func topCreators(w io.Writer, recs []Rec) error {
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
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]].gb != m[keys[j]].gb {
			return m[keys[i]].gb > m[keys[j]].gb
		}
		return keys[i] < keys[j]
	})
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "создатель\tкласс\tзаписей\tмера, ГБ·мин\tсамая большая")
	for i, k := range keys {
		if i >= 15 {
			break
		}
		a := m[k]
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", k, a.cl, a.n, policy.FormatGBMin(a.gb), policy.HumanBytes(a.max))
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("отчёт: %w", err)
	}
	return nil
}
