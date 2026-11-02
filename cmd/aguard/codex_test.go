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
// бы» один раз.
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
	noArming(a)
	a.codexRead = func(_ context.Context, _ string, at time.Time) (codexwatch.Reading, error) {
		return codexwatch.Reading{At: at, Used: used, ResetsAt: now.Add(5 * 24 * time.Hour), Allowed: true, Available: 1}, nil
	}
	cfg := codexwatch.Default(dir)
	cfg.Lead = "lead"
	ctx := context.Background()
	if err := a.codexRound(ctx, &cfg, false); err != nil {
		t.Fatal(err)
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
			noArming(a)
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

// noArming — проход без взвода: одобрений нет, consume и тикет — ошибка теста.
func noArming(a *app) {
	a.codexApproved = func(string, time.Time) (string, error) { return "", nil }
	a.codexConsume = func(context.Context, string, string) (string, error) {
		panic("consume без взвода")
	}
	a.codexNote = func(context.Context, string, string, string, string) error { panic("тикет без взвода") }
}

// Взвод на подставном app-server: строка «да» на окно (секунды в файле — :00, у сервера — :23), при 100% — один
// consume с ключом окна; первый вызов без ответа — повтор тем же ключом через fast; исход — строка в тикет; письмо
// лиду не ушло (лида нет) — повтор письма, но не consume.
func TestCodexArmedFakeAppServer(t *testing.T) {
	fake, err := filepath.Abs("testdata/fake-codex")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	var logs strings.Builder
	a := &app{home: dir, stateDir: filepath.Join(dir, "state"), uid: 1000,
		reg: session.Registry{Dir: filepath.Join(dir, "sessions"), ProcRoot: dir}, log: slog.New(slog.NewTextHandler(&logs, nil)),
		codexConsume: consumeCodex, codexApproved: approvedCodex}
	if err := os.MkdirAll(a.dir("codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	resets := time.Date(2026, 11, 8, 14, 39, 23, 0, time.UTC)
	answer, calls, approvals := filepath.Join(dir, "answer.json"), filepath.Join(dir, "calls"), filepath.Join(dir, "ok.tsv")
	t.Setenv("FAKE_CODEX_ANSWER", answer)
	t.Setenv("FAKE_CODEX_CALLS", calls)
	t.Setenv("FAKE_CODEX_OUTCOME", "hang")
	if err := os.WriteFile(approvals, []byte("2026-11-08T15:39:00+01:00\te7192203:278 «Да»\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"rateLimits":{"primary":{"usedPercent":100,"windowDurationMins":10080,"resetsAt":%d},"planType":"pro"},`+
		`"accountId":"acc","ordinaryUsageAllowed":false,"rateLimitResetCredits":{"availableCount":1,"credits":[]}}`, resets.Unix())
	if err := os.WriteFile(answer, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 2, 19, 0, 0, 0, time.UTC)
	a.codexRead = func(ctx context.Context, codex string, _ time.Time) (codexwatch.Reading, error) {
		return readCodex(ctx, codex, at)
	}
	var notes []string
	a.codexNote = func(_ context.Context, _, _, ticket, text string) error {
		notes = append(notes, ticket+": "+text)
		return nil
	}
	cfg := codexwatch.Default(dir)
	cfg.Codex, cfg.Approvals, cfg.Lead = fake, approvals, "lead"
	ctx := context.Background()
	if err := a.codexRound(ctx, &cfg, false); err != nil { // consume без ответа
		t.Fatal(err)
	}
	if a.codexEvery(&cfg) != cfg.Fast.D() {
		t.Fatalf("шаг после consume без ответа: %v", a.codexEvery(&cfg))
	}
	t.Setenv("FAKE_CODEX_OUTCOME", "reset")
	for range 2 {
		at = at.Add(2 * time.Minute)
		if err := a.codexRound(ctx, &cfg, false); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	st, err := codexwatch.LoadState(filepath.Join(a.dir("codex"), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || !strings.Contains(lines[0], st.Reset.Key) || !strings.Contains(lines[1], st.Reset.Key) {
		t.Fatalf("вызовы consume (ключ %s):\n%s", st.Reset.Key, raw)
	}
	if st.Reset.Outcome != "reset" || len(notes) != 1 || !strings.Contains(notes[0], "codex-limit-reset: aguard: consume") {
		t.Fatalf("исход %q, тикет %q", st.Reset.Outcome, notes)
	}
	if len(st.Pending) != 1 || st.Pending[0].Lead || !st.Pending[0].Ticket ||
		strings.Count(logs.String(), "исход не отправлен лиду") != 2 {
		t.Fatalf("доставка: %+v\n%s", st.Pending, logs.String())
	}
}

// Блокировка: второй проход при занятой — ошибка, consume не зовётся.
func TestCodexLockExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json.lock")
	release, err := codexLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codexLock(path); err == nil || !strings.Contains(err.Error(), "уже идёт") {
		t.Fatalf("вторая блокировка: %v", err)
	}
	release()
	again, err := codexLock(path)
	if err != nil {
		t.Fatal(err)
	}
	again()
}

// dry-run (проверка 02.11: мутации «dry сохраняет», «dry шлёт» выживали): состояние не меняется, писем и consume нет,
// решение — в выводе.
func TestCodexDryRunNoSideEffects(t *testing.T) {
	dir := t.TempDir()
	var logs, out strings.Builder
	a := &app{home: dir, stateDir: filepath.Join(dir, "state"), log: slog.New(slog.NewTextHandler(&logs, nil)), out: &out}
	if err := os.MkdirAll(a.dir("codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	noArming(a)
	a.codexApproved = func(string, time.Time) (string, error) { return "x", nil } // взведено, но dry не зовёт consume
	now := time.Now()
	a.codexRead = func(_ context.Context, _ string, at time.Time) (codexwatch.Reading, error) {
		return codexwatch.Reading{At: at, Used: 100, ResetsAt: now.Add(5 * 24 * time.Hour), Available: 1}, nil
	}
	statePath := filepath.Join(a.dir("codex"), "state.json")
	before := codexwatch.State{ResetsAt: now.Add(5 * 24 * time.Hour), Warned: true}
	if err := before.Save(statePath); err != nil {
		t.Fatal(err)
	}
	was, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := codexwatch.Default(dir)
	cfg.Lead = "lead"
	if err := a.codexRound(context.Background(), &cfg, true); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(was) || strings.Contains(logs.String(), "написал") ||
		!strings.Contains(out.String(), "consume: вызвал бы") {
		t.Fatalf("состояние:\n%s\n→\n%s\nжурнал:\n%s\nвывод:\n%s", was, after, logs.String(), out.String())
	}
}

// armedApp — проход со взводом при 100% и кредите: замер подставной, одобрение есть, тикет и consume задаёт тест.
func armedApp(t *testing.T) (*app, *strings.Builder, codexwatch.Config) {
	t.Helper()
	dir := t.TempDir()
	logs := &strings.Builder{}
	a := &app{home: dir, stateDir: filepath.Join(dir, "state"), uid: 1000,
		reg: session.Registry{Dir: filepath.Join(dir, "sessions"), ProcRoot: dir}, log: slog.New(slog.NewTextHandler(logs, nil))}
	if err := os.MkdirAll(a.dir("codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	noArming(a)
	a.codexApproved = func(string, time.Time) (string, error) { return "e7192203:278", nil }
	resets := time.Now().Add(5 * 24 * time.Hour)
	a.codexRead = func(_ context.Context, _ string, at time.Time) (codexwatch.Reading, error) {
		return codexwatch.Reading{At: at, Used: 100, ResetsAt: resets, Available: 1}, nil
	}
	cfg := codexwatch.Default(dir)
	cfg.Lead = "lead"
	return a, logs, cfg
}

// Проверка взвода 02.11, мутация m1: mode=log — consume не зовётся, только строка «вызвал бы».
func TestCodexLogModeNoConsume(t *testing.T) {
	a, logs, cfg := armedApp(t)
	cfg.Mode = codexwatch.ModeLog
	if err := a.codexRound(context.Background(), &cfg, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "вызвал бы consume") {
		t.Fatalf("журнал:\n%s", logs.String())
	}
}

// m2: проход берёт блокировку состояния — при занятой (ручной tick идёт) consume не зовётся.
func TestCodexRoundTakesLock(t *testing.T) {
	a, _, cfg := armedApp(t)
	release, err := codexLock(filepath.Join(a.dir("codex"), "state.json.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := a.codexRound(context.Background(), &cfg, false); err == nil || !strings.Contains(err.Error(), "уже идёт") {
		t.Fatalf("проход при занятой блокировке: %v", err)
	}
}

// m3, m4 и доставка лиду: ключ и Called на диске до вызова; исход — письмо лиду (уходит один раз) и строка в тикет:
// tk упал — строка повторяется следующим проходом, письмо и consume — нет.
func TestCodexOutcomeDelivered(t *testing.T) {
	a, logs, cfg := armedApp(t)
	got := liveLead(t, a)
	statePath := filepath.Join(a.dir("codex"), "state.json")
	consumes := 0
	a.codexConsume = func(_ context.Context, _, key string) (string, error) {
		consumes++
		st, err := codexwatch.LoadState(statePath)
		if err != nil || !st.Reset.Called || st.Reset.Key != key || key == "" {
			t.Errorf("до вызова на диске: %+v, ключ %q, %v", st.Reset, key, err)
		}
		return "noCredit", nil
	}
	var notes []string
	fail := true
	a.codexNote = func(_ context.Context, _, _, _, text string) error {
		notes = append(notes, text)
		if fail {
			fail = false
			return fmt.Errorf("tk упал")
		}
		return nil
	}
	ctx := context.Background()
	for range 2 {
		if err := a.codexRound(ctx, &cfg, false); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case msg := <-got:
		if !strings.Contains(msg, "outcome=noCredit") || !strings.Contains(msg, "кредитов сброса нет") {
			t.Fatalf("письмо лиду: %q", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("письма лиду нет")
	}
	st, err := codexwatch.LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	// повторная проверка (мутация «письмо каждый проход» выживала): второй попытки письма нет — ни успешной, ни упавшей
	if n := strings.Count(logs.String(), `msg="aguard codex: написал"`); n != 1 || strings.Contains(logs.String(), "исход не отправлен") {
		t.Fatalf("писем лиду %d, журнал:\n%s", n, logs.String())
	}
	if consumes != 1 || len(notes) != 2 || notes[0] != notes[1] || len(st.Pending) != 0 {
		t.Fatalf("consume %d, тикет %q, к доставке %+v", consumes, notes, st.Pending)
	}
}
