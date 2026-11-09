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
	a.codexRead = func(_ context.Context, _, _ string, at time.Time) (codexwatch.Reading, error) {
		return codexwatch.Reading{At: at, Used: used, ResetsAt: now.Add(5 * 24 * time.Hour), Allowed: true, Available: 1}, nil
	}
	cfg := oneAccount(dir)
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
	fake := fakeCodexPath(t)
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
			a.codexRead = func(ctx context.Context, codex, home string, _ time.Time) (codexwatch.Reading, error) {
				return readCodex(ctx, codex, home, at)
			}
			cfg := oneAccount(dir)
			cfg.Mode, cfg.Codex, cfg.Lead = codexwatch.ModeLog, fake, "lead"
			for ; at.Before(start.Add(8 * time.Hour)); at = at.Add(10 * time.Minute) {
				writeFakeAnswer(t, answer, min(100, float64(int(70+tc.perHour*at.Sub(start).Hours()))), resets, tc.credits)
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

// Подставной app-server, живой лид: письмо 1 о быстром расходе и «пора» приходят в сокет по одному разу и по порядку.
func TestCodexWarnAndFullDelivered(t *testing.T) {
	dir := t.TempDir()
	var logs strings.Builder
	a := &app{home: dir, stateDir: filepath.Join(dir, "state"), log: slog.New(slog.NewTextHandler(&logs, nil))}
	if err := os.MkdirAll(a.dir("codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	got := liveLead(t, a)
	answer := filepath.Join(dir, "answer.json")
	t.Setenv("FAKE_CODEX_ANSWER", answer)
	start := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	resets := start.Add(6 * 24 * time.Hour)
	at := start
	noArming(a)
	a.codexRead = func(ctx context.Context, codex, home string, _ time.Time) (codexwatch.Reading, error) {
		return readCodex(ctx, codex, home, at)
	}
	cfg := oneAccount(dir)
	cfg.Mode, cfg.Codex, cfg.Lead = codexwatch.ModeAlert, fakeCodexPath(t), "lead"
	for ; at.Before(start.Add(8 * time.Hour)); at = at.Add(10 * time.Minute) {
		writeFakeAnswer(t, answer, min(100, float64(int(70+4.5*at.Sub(start).Hours()))), resets, 1)
		if err := a.codexRound(context.Background(), &cfg, false); err != nil {
			t.Fatal(err)
		}
	}
	msgs := leadLetters(got)
	if len(msgs) != 2 || !strings.Contains(msgs[0], "100% около") || !strings.Contains(msgs[1], "пора") {
		t.Fatalf("письма лиду: %q, журнал:\n%s", msgs, logs.String())
	}
}

// Взвод есть, кредита нет: лиду — одно письмо «consume не вызываю», consume не зовётся.
func TestCodexArmedNoCreditDelivered(t *testing.T) {
	a, logs, cfg := armedApp(t)
	cfg.Mode, cfg.Codex = codexwatch.ModeAlert, fakeCodexPath(t)
	got := liveLead(t, a)
	answer := filepath.Join(t.TempDir(), "answer.json")
	t.Setenv("FAKE_CODEX_ANSWER", answer)
	writeFakeAnswer(t, answer, 100, time.Now().Add(5*24*time.Hour), 0)
	a.codexRead = readCodex
	a.codexConsume = func(context.Context, string, string, string) (string, error) {
		t.Error("consume вызван при нулевом числе кредитов")
		return "", nil
	}
	for range 2 {
		if err := a.codexRound(context.Background(), &cfg, false); err != nil {
			t.Fatal(err)
		}
	}
	msgs := leadLetters(got)
	if len(msgs) != 1 || !strings.Contains(msgs[0], "consume не вызываю") {
		t.Fatalf("письма лиду: %q, журнал:\n%s", msgs, logs.String())
	}
}

// fakeCodexPath — подставной app-server.
func fakeCodexPath(t *testing.T) string {
	t.Helper()
	fake, err := filepath.Abs("testdata/fake-codex")
	if err != nil {
		t.Fatal(err)
	}
	return fake
}

// writeFakeAnswer — ответ подставного app-server на чтение лимита; обычная работа разрешена, пока used < 100.
func writeFakeAnswer(t *testing.T, path string, used float64, resets time.Time, credits int) {
	t.Helper()
	body := fmt.Sprintf(`{"rateLimits":{"primary":{"usedPercent":%v,"windowDurationMins":10080,"resetsAt":%d},`+
		`"planType":"pro"},"accountId":"acc","ordinaryUsageAllowed":%v,`+
		`"rateLimitResetCredits":{"availableCount":%d,"credits":[]}}`, used, resets.Unix(), used < 100, credits)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// leadLetters — все письма, принятые лидом к концу прохода: liveLead кладёт письмо в канал до закрытия соединения,
// а отправка ждёт закрытия (до 2 с, дольше — ложное падение теста), поэтому считать можно без ожидания.
func leadLetters(got <-chan string) []string {
	var msgs []string
	for {
		select {
		case msg, ok := <-got:
			if !ok {
				return msgs
			}
			msgs = append(msgs, msg)
		default:
			return msgs
		}
	}
}

// noArming — проход без взвода: одобрений нет, consume и тикет — ошибка теста.
func noArming(a *app) {
	a.codexApproved = func(string, time.Time) (string, error) { return "", nil }
	a.codexConsume = func(context.Context, string, string, string) (string, error) {
		panic("consume без взвода")
	}
	a.codexNote = func(context.Context, string, string, string, string) error { panic("тикет без взвода") }
}

// Взвод на подставном app-server: строка «да» на окно (секунды в файле — :00, у сервера — :23), при 100% — один
// consume с ключом окна; первый вызов без ответа — повтор тем же ключом через fast; исход — строка в тикет; письмо
// лиду не ушло (лида нет) — повтор письма, но не consume.
func TestCodexArmedFakeAppServer(t *testing.T) {
	fake := fakeCodexPath(t)
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
	writeFakeAnswer(t, answer, 100, resets, 1)
	at := time.Date(2026, 11, 2, 19, 0, 0, 0, time.UTC)
	a.codexRead = func(ctx context.Context, codex, home string, _ time.Time) (codexwatch.Reading, error) {
		return readCodex(ctx, codex, home, at)
	}
	var notes []string
	a.codexNote = func(_ context.Context, _, _, ticket, text string) error {
		notes = append(notes, ticket+": "+text)
		return nil
	}
	cfg := oneAccount(dir)
	cfg.Codex, cfg.Accounts[0].Approvals, cfg.Lead = fake, approvals, "lead"
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
	st, err := codexwatch.LoadState(filepath.Join(a.dir("codex"), "work.json"))
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
	a.codexRead = func(_ context.Context, _, _ string, at time.Time) (codexwatch.Reading, error) {
		return codexwatch.Reading{At: at, Used: 100, ResetsAt: now.Add(5 * 24 * time.Hour), Available: 1}, nil
	}
	statePath := filepath.Join(a.dir("codex"), "work.json")
	before := codexwatch.State{ResetsAt: now.Add(5 * 24 * time.Hour), Warned: true}
	if err := before.Save(statePath); err != nil {
		t.Fatal(err)
	}
	was, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := oneAccount(dir)
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
	a.codexRead = func(_ context.Context, _, _ string, at time.Time) (codexwatch.Reading, error) {
		return codexwatch.Reading{At: at, Used: 100, ResetsAt: resets, Available: 1}, nil
	}
	cfg := oneAccount(dir)
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
	release, err := codexLock(filepath.Join(a.dir("codex"), "pass.lock"))
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
	statePath := filepath.Join(a.dir("codex"), "work.json")
	consumes := 0
	a.codexConsume = func(_ context.Context, _, _, key string) (string, error) {
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

// «да» на окно через границу минуты: в файле 15:39:00 (+01:00), секунды сервера дрожат на обе стороны —
// 14:38:58 и 14:39:59 UTC — то же окно; ровно минута и дальше — другое окно, взвода нет.
func TestApprovedCodexAcrossMinute(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ok.tsv")
	if err := os.WriteFile(path, []byte("2026-11-08T15:39:00+01:00\te7192203:278 «Да»\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		at   time.Time
		want string
	}{
		{time.Date(2026, 11, 8, 14, 38, 58, 0, time.UTC), "e7192203:278 «Да»"},
		{time.Date(2026, 11, 8, 14, 39, 59, 0, time.UTC), "e7192203:278 «Да»"},
		{time.Date(2026, 11, 8, 14, 40, 0, 0, time.UTC), ""},
		{time.Date(2026, 11, 8, 14, 38, 0, 0, time.UTC), ""},
	} {
		got, err := approvedCodex(path, c.at)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("approvedCodex(%s) = %q, want %q", c.at.Format(time.RFC3339), got, c.want)
		}
	}
}

// oneAccount — настройки с одной учёткой work: проходы, где важна логика окна, а не учётки.
func oneAccount(dir string) codexwatch.Config {
	cfg := codexwatch.Default(dir)
	cfg.Accounts = cfg.Accounts[:1]
	return cfg
}

// twoAccounts — проход по work и personal с подставным замером по CODEX_HOME и живым лидом в письмах.
func twoAccounts(t *testing.T, read map[string]func(time.Time) (codexwatch.Reading, error)) (*app, *strings.Builder, codexwatch.Config) {
	t.Helper()
	a, logs, _ := armedApp(t)
	cfg := codexwatch.Default(a.home)
	cfg.Mode, cfg.Lead = codexwatch.ModeLog, "lead"
	a.codexApproved = func(string, time.Time) (string, error) { return "", nil }
	a.codexRead = func(_ context.Context, _, home string, at time.Time) (codexwatch.Reading, error) {
		for _, acc := range cfg.Accounts {
			if acc.Home == home {
				return read[acc.Name](at)
			}
		}
		t.Fatalf("замер неизвестной учётки CODEX_HOME=%s", home)
		return codexwatch.Reading{}, nil
	}
	return a, logs, cfg
}

func limit(used float64, reset time.Time, credits int) func(time.Time) (codexwatch.Reading, error) {
	return func(at time.Time) (codexwatch.Reading, error) {
		return codexwatch.Reading{At: at, Used: used, ResetsAt: reset, Allowed: used < 100, Available: credits}, nil
	}
}

// app-server каждой учётки запускается со своим CODEX_HOME, в чтении и в consume: иначе чужой лимит и чужой кредит.
func TestCodexHomeReachesAppServer(t *testing.T) {
	dir := t.TempDir()
	seen := filepath.Join(dir, "home")
	fake := filepath.Join(dir, "codex")
	script := "#!/bin/bash\nprintf '%s\\n' \"$CODEX_HOME\" >> " + seen + "\nexit 3\n"
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := readCodex(ctx, fake, "/h/personal", time.Now()); err == nil || !strings.Contains(err.Error(), "CODEX_HOME=/h/personal") {
		t.Fatalf("readCodex: %v", err)
	}
	if _, err := consumeCodex(ctx, fake, "/h/work", "k"); err == nil || !strings.Contains(err.Error(), "CODEX_HOME=/h/work") {
		t.Fatalf("consumeCodex: %v", err)
	}
	got, err := os.ReadFile(seen)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "/h/personal\n/h/work\n" {
		t.Fatalf("CODEX_HOME у app-server: %q", got)
	}
}

// Взвод одной учётки: consume — только её домом и своим «да»; у второй 100% и кредит, но «да» нет — письмо, не consume.
func TestCodexArmedOnlyOwnAccount(t *testing.T) {
	reset := time.Now().Add(5 * 24 * time.Hour)
	a, logs, cfg := twoAccounts(t, map[string]func(time.Time) (codexwatch.Reading, error){
		"work": limit(100, reset, 1), "personal": limit(100, reset, 1),
	})
	cfg.Mode = codexwatch.ModeAlert
	liveLead(t, a)
	personal := cfg.Accounts[1]
	a.codexApproved = func(path string, _ time.Time) (string, error) {
		if path == personal.Approvals {
			return "да на personal", nil
		}
		return "", nil
	}
	var homes []string
	a.codexConsume = func(_ context.Context, _, home, _ string) (string, error) {
		homes = append(homes, home)
		return "reset", nil
	}
	a.codexNote = func(_ context.Context, _, _, _, _ string) error { return nil }
	if err := a.codexRound(context.Background(), &cfg, false); err != nil {
		t.Fatal(err)
	}
	if len(homes) != 1 || homes[0] != personal.Home {
		t.Fatalf("consume: %q, журнал:\n%s", homes, logs.String())
	}
	out := logs.String() // письма — в журнале; «все без лимита» нет: замер снят до сброса
	for _, want := range []string{"учётка work (CODEX_HOME=" + cfg.Accounts[0].Home + ")", "пора", "Другие учётки: personal 100%",
		"учётка personal (CODEX_HOME=" + personal.Home + ")", "outcome=reset"} {
		if !strings.Contains(out, want) {
			t.Fatalf("нет %q в письмах:\n%s", want, out)
		}
	}
	if strings.Contains(out, "все учётки") {
		t.Fatalf("письмо «все без лимита» в проходе с consume:\n%s", out)
	}
}

// Сбой замера одной учётки не мешает второй: её письмо уходит с «замера нет», проход возвращает ошибку с именем, письма
// «все без лимита» нет — про первую неизвестно.
func TestCodexReadFailureIsolated(t *testing.T) {
	reset := time.Now().Add(5 * 24 * time.Hour)
	a, logs, cfg := twoAccounts(t, map[string]func(time.Time) (codexwatch.Reading, error){
		"work": func(time.Time) (codexwatch.Reading, error) {
			return codexwatch.Reading{}, fmt.Errorf("app-server упал")
		},
		"personal": limit(100, reset, 1),
	})
	err := a.codexRound(context.Background(), &cfg, false)
	if err == nil || !strings.Contains(err.Error(), "учётки work") {
		t.Fatalf("ошибка прохода: %v", err)
	}
	out := logs.String()
	if !strings.Contains(out, "учётка personal") || !strings.Contains(out, "work — замера нет") || strings.Contains(out, "все учётки") {
		t.Fatalf("журнал:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(a.dir("codex"), "work.json")); !os.IsNotExist(err) {
		t.Fatalf("состояние учётки без замера: %v", err)
	}
	if st, err := codexwatch.LoadState(filepath.Join(a.dir("codex"), "personal.json")); err != nil || !st.Full {
		t.Fatalf("состояние personal: %+v, %v", st, err)
	}
}

// Письмо «все без лимита» — один раз на переход; лимит появился и снова кончился — снова. Шаг — меньший из учёток.
func TestCodexAllOutOncePerTransition(t *testing.T) {
	reset := time.Now().Add(5 * 24 * time.Hour)
	personal := 100.0
	a, logs, cfg := twoAccounts(t, map[string]func(time.Time) (codexwatch.Reading, error){
		"work": limit(100, reset, 0),
		"personal": func(at time.Time) (codexwatch.Reading, error) {
			return limit(personal, reset.Add(time.Hour), 0)(at)
		},
	})
	ctx := context.Background()
	count := func() int { return strings.Count(logs.String(), "все учётки Codex без лимита") }
	for _, step := range []struct {
		used float64
		want int
	}{{100, 1}, {100, 1}, {10, 1}, {100, 2}} {
		personal = step.used
		if err := a.codexRound(ctx, &cfg, false); err != nil {
			t.Fatal(err)
		}
		if count() != step.want {
			t.Fatalf("used %v: писем %d, ждали %d, журнал:\n%s", step.used, count(), step.want, logs.String())
		}
	}
}

// «Все учётки без лимита» до живого лида: одно письмо на переход, повторный проход без перехода — без письма.
func TestCodexAllOutDelivered(t *testing.T) {
	reset := time.Now().Add(5 * 24 * time.Hour)
	a, logs, cfg := twoAccounts(t, nil)
	cfg.Mode, cfg.Codex = codexwatch.ModeAlert, fakeCodexPath(t)
	a.codexRead = readCodex
	got := liveLead(t, a)
	work, personal := cfg.Accounts[0].Home, cfg.Accounts[1].Home
	for _, home := range []string{work, personal} {
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, used := range []float64{100, 100, 10, 100} {
		writeFakeAnswer(t, filepath.Join(work, "answer.json"), 100, reset, 0)
		writeFakeAnswer(t, filepath.Join(personal, "answer.json"), used, reset.Add(time.Hour), 0)
		if err := a.codexRound(context.Background(), &cfg, false); err != nil {
			t.Fatal(err)
		}
	}
	msgs := leadLetters(got)
	if len(msgs) != 2 || !strings.Contains(msgs[0], "все учётки Codex без лимита") || !strings.Contains(msgs[1], "все учётки Codex без лимита") {
		t.Fatalf("письма лиду: %q, журнал:\n%s", msgs, logs.String())
	}
}

// Письмо лиду не ушло (лида нет): следующий проход пробует снова, а не молчит до следующего перехода.
func TestCodexAllOutRetriedWhenUnsent(t *testing.T) {
	reset := time.Now().Add(5 * 24 * time.Hour)
	a, logs, cfg := twoAccounts(t, map[string]func(time.Time) (codexwatch.Reading, error){
		"work": limit(100, reset, 0), "personal": limit(100, reset, 0),
	})
	cfg.Mode = codexwatch.ModeAlert
	for range 2 {
		if err := a.codexRound(context.Background(), &cfg, false); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(logs.String(), "kind=all_out"); n != 2 {
		t.Fatalf("попыток письма %d, журнал:\n%s", n, logs.String())
	}
}

// Шаг прохода — меньший из учёток, в любом порядке: близкая к 100% учётка держит шаг fast, хотя другая спокойна.
func TestCodexStepIsShortest(t *testing.T) {
	for _, near := range []string{"work", "personal"} {
		t.Run(near, func(t *testing.T) {
			reset := time.Now().Add(5 * 24 * time.Hour)
			read := map[string]func(time.Time) (codexwatch.Reading, error){"work": limit(5, reset, 0), "personal": limit(5, reset, 0)}
			read[near] = limit(99, reset, 1)
			a, _, cfg := twoAccounts(t, read)
			seed := codexwatch.State{ResetsAt: reset, Samples: []codexwatch.Sample{{At: time.Now().Add(-time.Hour), Used: 70}}}
			if err := seed.Save(filepath.Join(a.dir("codex"), near+".json")); err != nil {
				t.Fatal(err)
			}
			if err := a.codexRound(context.Background(), &cfg, false); err != nil {
				t.Fatal(err)
			}
			if a.codexEvery(&cfg) != cfg.Fast.D() {
				t.Fatalf("шаг: %v", a.codexEvery(&cfg))
			}
		})
	}
}
