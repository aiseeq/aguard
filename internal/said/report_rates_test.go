package said

import (
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/price"

	"github.com/aiseeq/aguard/internal/rates"
)

func TestSummaryPrintsProviderPercentagesSeparately(t *testing.T) {
	at := time.Date(2026, 11, 12, 10, 0, 0, 0, time.Local)
	recs := []*Record{
		{At: at, Model: "gpt-6-luna", Usage: price.Usage{Input: 1_000_000}},
		{At: at.Add(time.Minute), Model: "claude-opus-5-5", Usage: price.Usage{Input: 1_000_000}},
		{At: at.Add(2 * time.Minute), Model: "unknown", Usage: price.Usage{Input: 1_000_000}},
	}
	days, _, err := SummaryWithRates(func(fn func(*Record)) (int, error) {
		for _, r := range recs {
			fn(r)
		}
		return 0, nil
	}, map[string]rates.Rate{
		"openai": {APIWeek: 800, PlanUSD: 100}, "anthropic": {APIWeek: 1539, PlanUSD: 200},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 1 || days[0].Unpriced != 1 {
		t.Fatalf("days: %+v", days)
	}
	var out strings.Builder
	if err := Print(&out, days); err != nil {
		t.Fatal(err)
	}
	line := out.String()
	if !strings.Contains(line, "anthropic 0.52 openai 0.01") || !strings.Contains(line, "  1  ") || strings.Contains(line, "$0.") {
		t.Fatalf("report:\n%s", line)
	}
}

func TestSummaryAtMissingRates(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	at := time.Date(2026, 11, 12, 10, 0, 0, 0, time.Local)
	rec := &Record{At: at, Model: "gpt-6-luna", Usage: price.Usage{Input: 1_000_000}}
	days, bad, rateErr, err := SummaryAt(func(fn func(*Record)) (int, error) { fn(rec); return 0, nil }, at)
	if err != nil || rateErr == nil {
		t.Fatalf("days=%+v bad=%d rateErr=%v err=%v", days, bad, rateErr, err)
	}
	if len(days) != 1 || days[0].Calls != 1 || days[0].Unpriced != days[0].Calls {
		t.Fatalf("missing-rate summary: %+v", days)
	}
}

func TestPrintEmptyPrice(t *testing.T) {
	var out strings.Builder
	if err := Print(&out, []*Day{{Day: "2026-11-12", Cost: map[string]float64{}}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "—") {
		t.Fatalf("empty price should be an em dash:\n%s", out.String())
	}
}
