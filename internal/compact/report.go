package compact

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/cllog"
	"github.com/aiseeq/aguard/internal/rates"
)

// Version — версия формата Summary для sleepstats.
const Version = 1

// Summary — сводки за окна до At (aguard compact report -json): day, week, month.
type Summary struct {
	V       int                `json:"v"`
	At      time.Time          `json:"at"`
	Windows map[string]*Report `json:"windows"`
}

// Report — сводка журнала: решения, доводы против и исходы.
type Report struct {
	Decisions map[string]int `json:"decisions"` // would, would_not, would_reset, would_not_reset
	// Held — по решению: сколько контекста держали беседы и во что обошёлся бы следующий запрос при мёртвом кэше.
	Held map[string]Tally `json:"held"`
	// Against — would_not по доводу: сколько раз довод был среди причин.
	Against map[string]int `json:"against"`
	// ResetAgainst — то же для would_not_reset.
	ResetAgainst map[string]int `json:"reset_against"`
	// Outcomes — по «решение/исход»: сколько и сколько токенов контекста.
	Outcomes map[string]Tally `json:"outcomes"`
	Bad      int              `json:"bad"` // нечитаемые строки журнала
	// Done — что cl сделал на деле по своему журналу: по «вид/итог» (cllog.Kind*, cllog.Result*).
	Done map[string]Tally `json:"done"`
	// Sessions — по имени сессии: решения aguard (одно на запрос, в момент входа в окно) и что cl сделал на деле.
	Sessions map[string]*Session `json:"sessions"`
}

// Session — решения aguard и сделанное cl по одной сессии.
type Session struct {
	Decisions map[string]int   `json:"decisions"`
	Done      map[string]Tally `json:"done"`
}

// session — запись сессии name, при надобности новая.
func (r *Report) session(name string) *Session {
	s := r.Sessions[name]
	if s == nil {
		s = &Session{Decisions: map[string]int{}, Done: map[string]Tally{}}
		r.Sessions[name] = s
	}
	return s
}

// AddDone добавляет сделанное cl.
func (r *Report) AddDone(actions []cllog.Action) {
	for _, a := range actions {
		key := a.Kind + "/" + a.Result
		for _, m := range []map[string]Tally{r.Done, r.session(a.Name).Done} {
			t := m[key]
			t.Count++
			t.Tokens += a.Tokens
			m[key] = t
		}
	}
}

// Tally — число записей, сумма контекста в них и оценка запись кэша заново (rewrite_micro_usd записей) в миллионных
// долях доллара.
type Tally struct {
	Count    int   `json:"count"`
	Tokens   int64 `json:"tokens"`
	MicroUSD int64 `json:"micro_usd"`
}

func (t *Tally) add(rec *Record) {
	t.Count++
	t.Tokens += rec.Tokens
	t.MicroUSD += rec.RewriteMicroUSD
}

// dollars — миллионные доли доллара в долларах для печати.
func dollars(micro int64) float64 { return float64(micro) / 1e6 }

// Collect — сводка по записям, которые read отдаёт в fn.
func Collect(read func(fn func(*Record)) (int, error)) (*Report, error) {
	r := &Report{Decisions: map[string]int{}, Held: map[string]Tally{}, Against: map[string]int{}, ResetAgainst: map[string]int{},
		Outcomes: map[string]Tally{}, Done: map[string]Tally{}, Sessions: map[string]*Session{}}
	bad, err := read(func(rec *Record) {
		switch rec.Kind {
		case KindWould, KindWouldNot, KindWouldReset, KindWouldNotReset:
			r.Decisions[rec.Kind]++
			r.session(rec.Name).Decisions[rec.Kind]++
			t := r.Held[rec.Kind]
			t.add(rec)
			r.Held[rec.Kind] = t
			against := r.Against
			if rec.Kind == KindWouldNotReset {
				against = r.ResetAgainst
			}
			for _, a := range rec.Against {
				against[a]++
			}
		case KindOutcome:
			key := rec.Decision + "/" + rec.Outcome
			t := r.Outcomes[key]
			t.add(rec)
			r.Outcomes[key] = t
		}
	})
	r.Bad = bad
	return r, err
}

// Print — сводка для человека.
func (r *Report) Print(w io.Writer) error {
	return r.PrintRates(w, rates.Rate{}, false)
}

// PrintRates — человекочитаемая сводка с курсом anthropic.
func (r *Report) PrintRates(w io.Writer, rate rates.Rate, rateAvailable bool) error {
	var b strings.Builder
	price := func(micro int64) string {
		if !rateAvailable || rate.APIWeek <= 0 || rate.PlanUSD <= 0 {
			return "цена ?"
		}
		return rates.FormatPct(rate.WeekPct(dollars(micro))) + " % недели $100 Claude"
	}
	fmt.Fprintf(&b, "сжал бы: %d, не стал бы: %d\n", r.Decisions[KindWould], r.Decisions[KindWouldNot])
	fmt.Fprintf(&b, "сбросил бы: %d (контекст %d тыс., запись заново %s), не стал бы: %d\n", r.Decisions[KindWouldReset],
		r.Held[KindWouldReset].Tokens/1000, price(r.Held[KindWouldReset].MicroUSD), r.Decisions[KindWouldNotReset])
	for _, code := range slices.Sorted(maps.Keys(r.Against)) {
		fmt.Fprintf(&b, "  против сжатия %s: %d\n", code, r.Against[code])
	}
	for _, code := range slices.Sorted(maps.Keys(r.ResetAgainst)) {
		fmt.Fprintf(&b, "  против сброса %s: %d\n", code, r.ResetAgainst[code])
	}
	for _, key := range slices.Sorted(maps.Keys(r.Outcomes)) {
		t := r.Outcomes[key]
		fmt.Fprintf(&b, "исход %s: %d, контекст %d тыс. токенов, %s\n", key, t.Count, t.Tokens/1000, price(t.MicroUSD))
	}
	if r.Bad > 0 {
		fmt.Fprintf(&b, "нечитаемых строк: %d\n", r.Bad)
	}
	fmt.Fprintf(&b, "cl сделал: %s\n", doneList(r.Done, true))
	for _, name := range slices.Sorted(maps.Keys(r.Sessions)) {
		s := r.Sessions[name]
		if len(s.Done) == 0 && s.Decisions[KindWould]+s.Decisions[KindWouldReset] == 0 {
			continue
		}
		fmt.Fprintf(&b, "  %s: aguard сжал бы %d, сбросил бы %d, не стал бы сжимать %d; cl: %s\n", name, s.Decisions[KindWould],
			s.Decisions[KindWouldReset], s.Decisions[KindWouldNot], doneList(s.Done, false))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// doneText — «вид/итог» сделанного словами.
var doneText = map[string]string{
	cllog.KindReset + "/" + cllog.ResultDone:          "сбросил",
	cllog.KindReset + "/" + cllog.ResultFailed:        "сброс не состоялся",
	cllog.KindCompact + "/" + cllog.ResultDone:        "сжал",
	cllog.KindCompact + "/" + cllog.ResultFailed:      "не сжалось",
	cllog.KindCompact + "/" + cllog.ResultStopped:     "сжатие прервано сигналом",
	cllog.KindCompact + "/" + cllog.ResultNone:        "сжатие без итога",
	cllog.KindTypeCompact + "/" + cllog.ResultDone:    "набрал /compact",
	cllog.KindTypeCompact + "/" + cllog.ResultSkipped: "не набрал /compact: человек вводил",
	cllog.KindTypeCompact + "/" + cllog.ResultFailed:  "набор /compact не удался",
	cllog.KindTypeCompact + "/" + cllog.ResultNone:    "набор /compact без итога",
	cllog.KindTypeClear + "/" + cllog.ResultDone:      "набрал /clear",
	cllog.KindTypeClear + "/" + cllog.ResultSkipped:   "не набрал /clear: человек вводил",
	cllog.KindTypeClear + "/" + cllog.ResultFailed:    "набор /clear не удался",
	cllog.KindTypeClear + "/" + cllog.ResultNone:      "набор /clear без итога",
}

// doneList — сделанное через запятую; tokens — и контекст, который был у бесед.
func doneList(done map[string]Tally, tokens bool) string {
	if len(done) == 0 {
		return "ничего"
	}
	parts := make([]string, 0, len(done))
	for _, key := range slices.Sorted(maps.Keys(done)) {
		text := doneText[key]
		if text == "" {
			text = key
		}
		t := done[key]
		if tokens {
			parts = append(parts, fmt.Sprintf("%s %d (контекст %d тыс.)", text, t.Count, t.Tokens/1000))
		} else {
			parts = append(parts, fmt.Sprintf("%s %d", text, t.Count))
		}
	}
	return strings.Join(parts, ", ")
}
