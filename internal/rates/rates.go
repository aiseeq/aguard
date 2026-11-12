// Package rates reads subscription capacities and converts API dollars to a
// percentage of one provider's weekly limit for a $100 plan.
package rates

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Rate struct {
	APIWeek float64
	PlanUSD float64
}

func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home: %w", err)
	}
	return filepath.Join(home, ".claude", "improve", "rates.tsv"), nil
}

// Read selects each provider's latest applicable row; equal timestamps select
// the later row in the file.
func Read(path string, at time.Time) (map[string]Rate, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 4096), 1<<20)
	var header []string
	cols := map[string]int{}
	chosenAt := map[string]time.Time{}
	chosen := map[string]Rate{}
	for n := 1; s.Scan(); n++ {
		line := strings.TrimSuffix(s.Text(), "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cells := strings.Split(line, "\t")
		if header == nil {
			var err error
			header, cols, err = readHeader(path, cells)
			if err != nil {
				return nil, err
			}
			continue
		}
		if len(cells) != len(header) {
			return nil, fmt.Errorf("%s:%d: колонок %d, в шапке %d", path, n, len(cells), len(header))
		}
		provider, valid, rate, applicable, err := parseRateRow(path, n, cells, cols, chosenAt, at)
		if err != nil {
			return nil, err
		}
		if !applicable {
			continue
		}
		chosenAt[provider] = valid
		chosen[provider] = rate
	}
	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("чтение %s: %w", path, err)
	}
	if header == nil {
		return nil, fmt.Errorf("%s: нет заголовка", path)
	}
	return chosen, nil
}

func readHeader(path string, cells []string) ([]string, map[string]int, error) {
	cols := make(map[string]int, len(cells))
	for i, name := range cells {
		cols[name] = i
	}
	for _, name := range []string{"valid_from", "provider", "plan_usd_month", "api_usd_week"} {
		if _, ok := cols[name]; !ok {
			return nil, nil, fmt.Errorf("%s: шапка без колонки %s", path, name)
		}
	}
	return cells, cols, nil
}

func parseRateRow(path string, line int, cells []string, cols map[string]int, latest map[string]time.Time, at time.Time) (string, time.Time, Rate, bool, error) {
	provider := cells[cols["provider"]]
	valid, err := parseDate(cells[cols["valid_from"]])
	if err != nil {
		return "", time.Time{}, Rate{}, false, fmt.Errorf("%s:%d: valid_from %q: %w", path, line, cells[cols["valid_from"]], err)
	}
	if valid.After(at) || latest[provider].After(valid) {
		return provider, valid, Rate{}, false, nil
	}
	plan, err := positive(path, line, "plan_usd_month", cells[cols["plan_usd_month"]])
	if err != nil {
		return "", time.Time{}, Rate{}, false, err
	}
	week, err := positive(path, line, "api_usd_week", cells[cols["api_usd_week"]])
	if err != nil {
		return "", time.Time{}, Rate{}, false, err
	}
	return provider, valid, Rate{APIWeek: week, PlanUSD: plan}, true, nil
}

func positive(path string, line int, column, value string) (float64, error) {
	n, err := strconv.ParseFloat(value, 64)
	if err != nil || n <= 0 || math.IsInf(n, 0) || math.IsNaN(n) {
		return 0, fmt.Errorf("%s:%d: %s %q должен быть числом больше нуля", path, line, column, value)
	}
	return n, nil
}

func Load(at time.Time) (map[string]Rate, error) {
	path, err := DefaultPath()
	if err != nil {
		return nil, err
	}
	return Read(path, at)
}

func parseDate(s string) (time.Time, error) {
	if t, err := time.ParseInLocation(time.DateOnly, s, time.Local); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, s)
}

func (r Rate) WeekPct(usd float64) float64 {
	return usd * r.PlanUSD / r.APIWeek
}

func Provider(model string) string {
	switch {
	case strings.HasPrefix(model, "claude-"):
		return "anthropic"
	case strings.HasPrefix(model, "gpt-"):
		return "openai"
	case strings.HasPrefix(model, "deepseek-"):
		return "ollama"
	case strings.HasPrefix(model, "waibee/"):
		return "waibee"
	default:
		return ""
	}
}

// FormatPct uses cents below ten percent and whole percentages from ten up.
func FormatPct(pct float64) string {
	rounded := math.Round(pct*100) / 100
	if rounded < 10 && rounded > -10 {
		return fmt.Sprintf("%.2f", rounded)
	}
	return fmt.Sprintf("%.0f", rounded)
}
