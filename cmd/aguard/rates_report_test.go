package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/price"

	"github.com/aiseeq/aguard/internal/compact"
	"github.com/aiseeq/aguard/internal/config"
	"github.com/aiseeq/aguard/internal/said"
)

const syntheticRates = "# test rates\nvalid_from\tprovider\tplan_usd_month\tapi_usd_week\n" +
	"2026-01-01\tanthropic\t200\t1539\n" +
	"2026-01-01\topenai\t100\t800\n"

func newRatesTestApp(t *testing.T, withRates bool) (*app, *bytes.Buffer, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if withRates {
		path := filepath.Join(home, ".claude", "improve", "rates.tsv")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(syntheticRates), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	return &app{home: home, cfgPath: config.Path(home), stateDir: filepath.Join(home, ".local", "state", "aguard"),
		uid: os.Getuid(), out: &out}, &out, home
}

func captureStderr(t *testing.T, run func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	defer func() { os.Stderr = old }()
	run()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLimitResetCheckSubscriptionRateAndMissingRates(t *testing.T) {
	const argsSince = "2026-11-03T20:22:11+01:00"
	at, err := time.Parse(time.RFC3339, "2026-11-04T00:03:09+01:00")
	if err != nil {
		t.Fatal(err)
	}
	history := filepath.Join(t.TempDir(), "limits.jsonl")
	if err := os.WriteFile(history, []byte(resetHistory[:strings.LastIndex(strings.TrimSuffix(resetHistory, "\n"), "\n")+1]), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--account", "waited@example.com", "--since", argsSince, "--history", history}

	t.Run("anthropic plan", func(t *testing.T) {
		a, out, _ := newRatesTestApp(t, true)
		var result error
		stderr := captureStderr(t, func() { result = a.limitResetCheck(args, at) })
		if !errors.Is(result, resetRefused) || !strings.Contains(out.String(), "расход 200 % недели $100 из 200") || stderr != "" {
			t.Fatalf("result=%v stdout=%s stderr=%s", result, out, stderr)
		}
	})

	t.Run("missing rates", func(t *testing.T) {
		a, out, _ := newRatesTestApp(t, false)
		var result error
		stderr := captureStderr(t, func() { result = a.limitResetCheck(args, at) })
		if !errors.Is(result, resetRefused) || !strings.Contains(out.String(), "расход 100 % своей учётки") || !strings.Contains(stderr, "rates.tsv") {
			t.Fatalf("result=%v stdout=%s stderr=%s", result, out, stderr)
		}
	})
}

func TestCompactReportUsesAnthropicRate(t *testing.T) {
	a, out, home := newRatesTestApp(t, true)
	at := time.Now()
	store := compact.NewStore(filepath.Join(home, ".local", "state", "aguard", "compact"))
	if err := store.Append([]compact.Record{{At: at, Kind: compact.KindWouldReset, Tokens: 1200, RewriteMicroUSD: 7_695_000}}); err != nil {
		t.Fatal(err)
	}
	if err := a.compactCmd([]string{"report", "-since", "168h"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "запись заново 1.00 % недели $100 Claude") {
		t.Fatalf("compact output:\n%s", out.String())
	}
}

func TestSaidReportUsesProviderRatesSeparately(t *testing.T) {
	a, out, home := newRatesTestApp(t, true)
	at := time.Now()
	store := said.NewStore(filepath.Join(home, ".local", "state", "aguard", "said"))
	recs := []said.Record{
		{At: at, Model: "gpt-6-luna", Usage: price.Usage{Input: 1_000_000}},
		{At: at.Add(time.Minute), Model: "claude-opus-5-5", Usage: price.Usage{Input: 1_000_000}},
	}
	if err := store.Append(recs); err != nil {
		t.Fatal(err)
	}
	if err := a.saidCmd(context.Background(), []string{"report"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "anthropic 0.52 openai 0.01") {
		t.Fatalf("said output:\n%s", out.String())
	}
}
