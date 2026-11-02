package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/codexwatch"
	"github.com/aiseeq/akit/session"
)

// Проход codex: лида нет — письмо «100%» не ушло, флаг снят, следующий проход пробует снова; mode=log — «написал
// бы» один раз; шаг у 99% — быстрый.
func TestCodexRoundRetriesUnsent(t *testing.T) {
	dir := t.TempDir()
	var logs strings.Builder
	a := &app{home: dir, stateDir: filepath.Join(dir, "state"), uid: 1000,
		reg: session.Registry{Dir: filepath.Join(dir, "sessions"), ProcRoot: dir}, log: slog.New(slog.NewTextHandler(&logs, nil))}
	if err := os.MkdirAll(a.dir("codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	used := 99.0
	a.codexRead = func(_ context.Context, _ string, at time.Time) (codexwatch.Reading, error) {
		return codexwatch.Reading{At: at, Used: used, ResetsAt: now.Add(5 * 24 * time.Hour), Allowed: true, Available: 1}, nil
	}
	cfg := codexwatch.Default(dir)
	cfg.Lead = "lead"
	ctx := context.Background()
	if err := a.codexRound(ctx, &cfg, false); err != nil {
		t.Fatal(err)
	}
	if a.codexEvery(&cfg) != cfg.Fast.D() {
		t.Fatalf("шаг у 99%%: %v", a.codexEvery(&cfg))
	}
	used = 100
	for range 2 {
		if err := a.codexRound(ctx, &cfg, false); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(logs.String(), "письмо не отправлено"); n != 2 {
		t.Fatalf("попыток письма %d, журнал:\n%s", n, logs.String())
	}
	cfg.Mode = codexwatch.ModeLog
	for range 2 {
		if err := a.codexRound(ctx, &cfg, false); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(logs.String(), "написал бы"); n != 1 {
		t.Fatalf("«написал бы» %d раз, журнал:\n%s", n, logs.String())
	}
	if a.codexEvery(&cfg) != cfg.Interval.D() {
		t.Fatalf("шаг после 100%%: %v", a.codexEvery(&cfg))
	}
}

// Сквозной проход на подставном app-server (testdata/fake-codex): readCodex через akit/codexlimit, три окна —
// быстрый расход (письмо 1, затем «100%»), медленный (писем нет), без досрочного сброса (строка журнала, писем нет).
func TestCodexFakeAppServer(t *testing.T) {
	fake, err := filepath.Abs("testdata/fake-codex")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	resets := start.Add(6 * 24 * time.Hour)
	cases := []struct {
		name    string
		perHour float64
		credits int
		want    []string // строки журнала по порядку
		absent  []string
	}{
		{"быстрый", 4.5, 1, []string{"написал бы", "100% около", "написал бы", "пора"}, nil},
		{"медленный", 0.3, 1, nil, []string{"написал бы", "досрочных сбросов нет"}},
		{"без сброса", 4.5, 0, []string{"досрочных сбросов нет"}, []string{"написал бы"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			var logs strings.Builder
			a := &app{home: dir, stateDir: filepath.Join(dir, "state"), log: slog.New(slog.NewTextHandler(&logs, nil))}
			if err := os.MkdirAll(a.dir("codex"), 0o700); err != nil {
				t.Fatal(err)
			}
			answer := filepath.Join(dir, "answer.json")
			t.Setenv("FAKE_CODEX_ANSWER", answer)
			at := start
			a.codexRead = func(ctx context.Context, codex string, _ time.Time) (codexwatch.Reading, error) {
				return readCodex(ctx, codex, at)
			}
			cfg := codexwatch.Default(dir)
			cfg.Mode, cfg.Codex, cfg.Lead = codexwatch.ModeLog, fake, "lead"
			for ; at.Before(start.Add(8 * time.Hour)); at = at.Add(10 * time.Minute) {
				used := min(100, float64(int(70+tc.perHour*at.Sub(start).Hours())))
				body := fmt.Sprintf(`{"rateLimits":{"primary":{"usedPercent":%v,"windowDurationMins":10080,"resetsAt":%d},`+
					`"planType":"pro"},"accountId":"acc","ordinaryUsageAllowed":%v,`+
					`"rateLimitResetCredits":{"availableCount":%d,"credits":[]}}`, used, resets.Unix(), used < 100, tc.credits)
				if err := os.WriteFile(answer, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := a.codexRound(context.Background(), &cfg, false); err != nil {
					t.Fatal(err)
				}
			}
			out := logs.String()
			rest := out
			for _, w := range tc.want {
				i := strings.Index(rest, w)
				if i < 0 {
					t.Fatalf("нет %q по порядку, журнал:\n%s", w, out)
				}
				rest = rest[i+len(w):]
			}
			for _, w := range tc.absent {
				if strings.Contains(out, w) {
					t.Fatalf("лишнее %q, журнал:\n%s", w, out)
				}
			}
			if n := strings.Count(out, "написал бы"); n > 2 {
				t.Fatalf("«написал бы» %d раз", n)
			}
		})
	}
}
