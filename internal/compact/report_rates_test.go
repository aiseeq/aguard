package compact

import (
	"strings"
	"testing"

	"github.com/aiseeq/aguard/internal/rates"
)

func TestPrintRatesAndUnavailableRate(t *testing.T) {
	r := &Report{Decisions: map[string]int{KindWouldReset: 1}, Held: map[string]Tally{KindWouldReset: {Tokens: 1200, MicroUSD: 7_695_000}},
		Against: map[string]int{}, ResetAgainst: map[string]int{}, Outcomes: map[string]Tally{"would_reset/cleared": {Count: 1, Tokens: 2000, MicroUSD: 1_539_000}}, Done: map[string]Tally{}, Sessions: map[string]*Session{}}
	var out strings.Builder
	if err := r.PrintRates(&out, rates.Rate{APIWeek: 1539, PlanUSD: 200}, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "запись заново 1.00 % недели $100 Claude") || !strings.Contains(out.String(), "исход would_reset/cleared: 1, контекст 2 тыс. токенов, 0.20 % недели $100 Claude") || strings.Contains(out.String(), "$7.70") {
		t.Fatalf("report:\n%s", out.String())
	}
	out.Reset()
	if err := r.PrintRates(&out, rates.Rate{APIWeek: 1539, PlanUSD: 200}, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "цена ?") {
		t.Fatalf("missing-rate output:\n%s", out.String())
	}
}
