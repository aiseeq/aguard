package compact

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"
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
	Decisions map[string]int `json:"decisions"` // would, would_not
	// Against — would_not по доводу: сколько раз довод был среди причин.
	Against map[string]int `json:"against"`
	// Outcomes — по «решение/исход»: сколько и сколько токенов контекста.
	Outcomes map[string]Tally `json:"outcomes"`
	Bad      int              `json:"bad"` // нечитаемые строки журнала
}

// Tally — число исходов и сумма контекста в них.
type Tally struct {
	Count  int   `json:"count"`
	Tokens int64 `json:"tokens"`
}

// Collect — сводка по записям, которые read отдаёт в fn.
func Collect(read func(fn func(*Record)) (int, error)) (*Report, error) {
	r := &Report{Decisions: map[string]int{}, Against: map[string]int{}, Outcomes: map[string]Tally{}}
	bad, err := read(func(rec *Record) {
		switch rec.Kind {
		case KindWould, KindWouldNot:
			r.Decisions[rec.Kind]++
			for _, a := range rec.Against {
				r.Against[a]++
			}
		case KindOutcome:
			key := rec.Decision + "/" + rec.Outcome
			t := r.Outcomes[key]
			t.Count++
			t.Tokens += rec.Tokens
			r.Outcomes[key] = t
		}
	})
	r.Bad = bad
	return r, err
}

// Print — сводка для человека.
func (r *Report) Print(w io.Writer) error {
	var b strings.Builder
	fmt.Fprintf(&b, "сжал бы: %d, не стал бы: %d\n", r.Decisions[KindWould], r.Decisions[KindWouldNot])
	for _, code := range slices.Sorted(maps.Keys(r.Against)) {
		fmt.Fprintf(&b, "  против %s: %d\n", code, r.Against[code])
	}
	for _, key := range slices.Sorted(maps.Keys(r.Outcomes)) {
		t := r.Outcomes[key]
		fmt.Fprintf(&b, "исход %s: %d, контекст %d тыс. токенов\n", key, t.Count, t.Tokens/1000)
	}
	if r.Bad > 0 {
		fmt.Fprintf(&b, "нечитаемых строк: %d\n", r.Bad)
	}
	_, err := io.WriteString(w, b.String())
	return err
}
