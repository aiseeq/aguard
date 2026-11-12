package rates

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadSelectionAndWeekPct(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rates.tsv")
	body := "# synthetic\nvalid_from\tprovider\tplan_usd_month\tapi_usd_week\n" +
		"2026-01-01\tanthropic\t100\t1000\n" +
		"2026-02-01\tanthropic\t200\t2000\n" +
		"2026-02-01\tanthropic\t200\t1539\n" +
		"2027-01-01\tanthropic\t300\t3000\n" +
		"2026-02-01\topenai\t100\t800\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path, time.Date(2026, 6, 1, 0, 0, 0, 0, time.Local))
	if err != nil {
		t.Fatal(err)
	}
	r := got["anthropic"]
	if r.APIWeek != 1539 || r.PlanUSD != 200 {
		t.Fatalf("anthropic rate %+v", r)
	}
	if pct := r.WeekPct(7.695); pct < .999 || pct > 1.001 {
		t.Fatalf("WeekPct=%v, want 1", pct)
	}
	if r.WeekPct(7.695) >= 10 || FormatPct(r.WeekPct(7.695)) != "1.00" || FormatPct(12.6) != "13" {
		t.Fatal("format precision mismatch")
	}
}

func TestReadEqualAtAndLocalMidnight(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rates.tsv")
	body := "valid_from\tprovider\tplan_usd_month\tapi_usd_week\n2026-02-01\tanthropic\t200\t1539\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	oldLocal := time.Local
	time.Local = time.FixedZone("UTC+3", 3*60*60)
	t.Cleanup(func() { time.Local = oldLocal })
	midnight := time.Date(2026, 2, 1, 0, 0, 0, 0, time.Local)
	got, err := Read(path, midnight)
	if err != nil || got["anthropic"].APIWeek != 1539 {
		t.Fatalf("valid_from at equality: rates=%v err=%v", got, err)
	}
	before := time.Date(2026, 1, 31, 23, 30, 0, 0, time.Local)
	got, err = Read(path, before)
	if err != nil || len(got) != 0 {
		t.Fatalf("local date took effect before midnight: rates=%v err=%v", got, err)
	}
}

func TestReadBadRowHasPathAndLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rates.tsv")
	if err := os.WriteFile(path, []byte("valid_from\tprovider\tplan_usd_month\tapi_usd_week\n2026-01-01\tanthropic\t0\t3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Read(path, time.Now())
	if err == nil || !strings.Contains(err.Error(), path+":2:") {
		t.Fatalf("error = %v", err)
	}
}

func TestProvider(t *testing.T) {
	for model, want := range map[string]string{"claude-x": "anthropic", "gpt-x": "openai", "deepseek-x": "ollama", "waibee/deepseek-x": "waibee", "other": ""} {
		if got := Provider(model); got != want {
			t.Errorf("Provider(%q)=%q want %q", model, got, want)
		}
	}
}

func TestFormatPctRoundsBeforeThreshold(t *testing.T) {
	for in, want := range map[float64]string{9.99: "9.99", 9.996: "10", 10: "10"} {
		if got := FormatPct(in); got != want {
			t.Errorf("FormatPct(%v)=%q want %q", in, got, want)
		}
	}
}
