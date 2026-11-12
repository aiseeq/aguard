package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/cllog"
	"github.com/aiseeq/aguard/internal/config"
	"github.com/aiseeq/aguard/internal/night"
	"github.com/aiseeq/akit/session"
)

func TestNightDryRunDoesNotWriteState(t *testing.T) {
	dir := t.TempDir()
	var out strings.Builder
	a := &app{home: dir, stateDir: filepath.Join(dir, "state"), log: slog.New(slog.NewTextHandler(os.Stderr, nil)), out: &out}
	cfg := night.Default(dir)
	cfg.Mode = night.ModeAlert
	cfg.DreamBy = "00:00"
	cfg.DreamFallback = ""
	cfg.DreamFallbackBy = pastFallbackBy()
	cfg.DreamMarker = filepath.Join(dir, "absent")
	cfg.DigestBy = "23:59"
	if err := a.nightRound(&cfg, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(a.stateDir, "night", "state.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry-run изменил состояние: %v", err)
	}
	if strings.TrimSpace(cfg.DreamMarker) == "" {
		t.Fatal("путь маркера потерян")
	}
	if !strings.Contains(out.String(), "Сон 00:00: маркер нет") || !strings.Contains(out.String(), "→ dream:") {
		t.Fatalf("dry-run не показал факты и сигнал:\n%s", out.String())
	}
}

// Снятый Сон не отправляет письмо и не запускает запасной unit; запись в журнале дедуплицирована состоянием night.
func TestNightPausedSilencesDreamAndFallback(t *testing.T) {
	dir := t.TempDir()
	pause := filepath.Join(dir, "night-paused")
	if err := os.WriteFile(pause, []byte("dream-master: сегодня не запускать\nподробности"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	units := &fakeUnits{}
	a := &app{home: dir, stateDir: filepath.Join(dir, "state"), log: slog.New(slog.NewTextHandler(&logs, nil)), units: units}
	mail := liveLead(t, a)
	cfg := night.Default(dir)
	cfg.Mode = night.ModeAlert
	cfg.Lead = "lead" // сессия liveLead: без неё письмо не дойдёт до сокета и проверка молчания ничего не ловит
	cfg.DreamBy = "00:00"
	cfg.DreamFallbackBy = pastFallbackBy()
	cfg.DreamMarker = filepath.Join(dir, "absent-marker")
	cfg.DreamPaused = pause
	cfg.DreamPauseCheck = fakePauseCheck(t, dir, "legacy", 0)
	cfg.DigestBy = "23:59"
	cfg.DigestMarker = filepath.Join(dir, "digest")
	if err := os.WriteFile(cfg.DigestMarker, []byte(time.Now().Format("2006-01-02")), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := a.nightRound(&cfg, false); err != nil {
			t.Fatal(err)
		}
	}
	// перезапуск демона: новый app на том же состоянии — строка о снятом Сне не повторяется
	restarted := &app{home: dir, stateDir: a.stateDir, log: a.log, units: units, reg: a.reg}
	if err := restarted.nightRound(&cfg, false); err != nil {
		t.Fatal(err)
	}
	if len(units.started) != 0 {
		t.Fatalf("запасной unit запущен: %v", units.started)
	}
	if len(mail) != 0 {
		t.Fatalf("получено письмо: %q", <-mail)
	}
	if n := strings.Count(logs.String(), "aguard night: Сон снят, письма и запасного Сна нет"); n != 1 {
		t.Fatalf("строк о снятом Сне %d, ждали 1:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "dream-master: сегодня не запускать") {
		t.Fatalf("в журнале нет первой строки файла: %s", logs.String())
	}
}

// Сон снят, а файл состояния запасного Сна битый: письмо «не знаю, запускал ли» тоже молчит — Сна сегодня не ждут.
func TestNightPausedSilencesFallbackStateLetter(t *testing.T) {
	dir := t.TempDir()
	pause := filepath.Join(dir, "night-paused")
	if err := os.WriteFile(pause, []byte("dream-master: сегодня не запускать\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &app{home: dir, stateDir: filepath.Join(dir, "state"), log: slog.New(slog.NewTextHandler(&strings.Builder{}, nil)), units: &fakeUnits{}}
	mail := liveLead(t, a)
	if err := os.MkdirAll(a.dir("night"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.dir("night"), "fallback.json"), []byte("{битый"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := night.Default(dir)
	cfg.Mode = night.ModeAlert
	cfg.Lead = "lead"
	cfg.DreamBy = "00:00"
	cfg.DreamFallbackBy = pastFallbackBy()
	cfg.DreamMarker = filepath.Join(dir, "absent-marker")
	cfg.DreamPaused = pause
	cfg.DreamPauseCheck = fakePauseCheck(t, dir, "legacy", 0)
	cfg.DigestBy = "23:59"
	if err := a.nightRound(&cfg, false); err != nil {
		t.Fatal(err)
	}
	if len(mail) != 0 {
		t.Fatalf("получено письмо: %q", <-mail)
	}
}

func TestNightPausedStatErrorFailsRound(t *testing.T) {
	dir := t.TempDir()
	cfg := night.Default(dir)
	restricted := filepath.Join(dir, "restricted")
	if err := os.Mkdir(restricted, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg.DreamPaused = filepath.Join(restricted, "night-paused")
	a := &app{home: dir, stateDir: filepath.Join(dir, "state"), log: slog.New(slog.NewTextHandler(&strings.Builder{}, nil))}
	if err := os.Chmod(restricted, 0); err != nil {
		t.Fatal(err)
	}
	statErr := a.nightRound(&cfg, false)
	if err := os.Chmod(restricted, 0o700); err != nil {
		t.Fatal(err)
	}
	if statErr == nil && os.Geteuid() != 0 {
		t.Fatal("папка без прав не прервала проход")
	}
	if statErr == nil { // root обходит права доступа; проверяем другую ошибку stat.
		blocker := filepath.Join(dir, "file")
		if err := os.WriteFile(blocker, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		cfg.DreamPaused = filepath.Join(blocker, "night-paused")
		statErr = a.nightRound(&cfg, false)
	}
	if statErr == nil || !strings.Contains(statErr.Error(), cfg.DreamPaused) {
		t.Fatalf("ошибка прохода без пути паузы: %v", statErr)
	}
}

// Закрытая консоль (cl умер) — не «стоит на окне»; строка без pid проверке не поддаётся и остаётся.
func TestOpenTrust(t *testing.T) {
	events := []cllog.TrustEvent{{Name: "old"}, {Name: "closed", PID: 11}, {Name: "stuck", PID: 12}}
	got := openTrust(events, func(pid int) bool { return pid == 12 })
	if len(got) != 2 || got[0].Name != "old" || got[1].Name != "stuck" {
		t.Fatalf("%+v", got)
	}
}

// mode=log: «написал бы» по разу на сигнал и канал, а не каждый проход.
func TestNightLogModeOncePerSignal(t *testing.T) {
	dir := t.TempDir()
	var logs strings.Builder
	a := &app{home: dir, stateDir: filepath.Join(dir, "state"), log: slog.New(slog.NewTextHandler(&logs, nil))}
	cfg := night.Default(dir)
	cfg.Mode = night.ModeLog
	cfg.DreamBy = "00:00"
	cfg.DreamFallback = ""
	cfg.DreamFallbackBy = pastFallbackBy()
	cfg.DreamMarker = filepath.Join(dir, "absent")
	cfg.DigestMarker = filepath.Join(dir, "digest")
	// Дайджест прошёл сегодня и завтра: сигнал один и в 23:59, и после полуночи
	now := time.Now()
	if err := os.WriteFile(cfg.DigestMarker, []byte(now.Format("2006-01-02")+" "+now.AddDate(0, 0, 1).Format("2006-01-02")), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := a.nightRound(&cfg, false); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(logs.String(), "написал бы"); n != 1 {
		t.Fatalf("строк «написал бы» %d, ждали 1:\n%s", n, logs.String())
	}
	if _, err := os.Stat(filepath.Join(a.stateDir, "night", "state.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mode=log записал состояние: переход на alert не отправил бы сегодняшнее (%v)", err)
	}
}

// Письмо не ушло — повтор в следующий проход; ушло — больше не шлётся.
func TestNightDeliveryRetriesFailed(t *testing.T) {
	state := nightState{}
	signals := []night.Signal{{Key: "dream:2026-10-28", Text: "Сон не стартовал"}}
	now := time.Now()
	tries := 0
	send := func(channel, msg, key string) error {
		tries++
		if tries == 1 {
			return errors.New("лид не в реестре")
		}
		return nil
	}
	state, failures := dispatchNight(state, signals, []string{"lead"}, now, send)
	if len(failures) != 1 || failures[0].Channel != "lead" {
		t.Fatalf("первый проход: %+v", failures)
	}
	for i := 1; i <= 2; i++ {
		if state, failures = dispatchNight(state, signals, []string{"lead"}, now.Add(time.Duration(i)*time.Minute), send); len(failures) != 0 {
			t.Fatalf("проход %d: %+v", i+1, failures)
		}
	}
	if _, ok := state[signals[0].Key].Channels["lead"]; !ok || tries != 2 {
		t.Fatalf("письмо: попыток %d, состояние %+v", tries, state)
	}
}

// Лид не поднялся: письмо ему не уходит, запасному адресату — уходит и не повторяется; лиду — повтор каждый проход
// (аудит процесса 29.10, D2: сигнал «Лид жив?» уходил только лиду).
func TestNightBackupRecipientWhenLeadDown(t *testing.T) {
	signals := []night.Signal{{Key: "dream:2026-10-29", Text: "Сон не стартовал"}}
	sent := map[string]int{}
	send := func(to, msg, key string) error {
		if to == "lead" {
			return errors.New("сессии lead нет")
		}
		sent[to]++
		return nil
	}
	state := nightState{}
	now := time.Now()
	for i := range 3 {
		var failures []nightFailure
		state, failures = dispatchNight(state, signals, []string{"lead", "git-admin"}, now.Add(time.Duration(i)*time.Minute), send)
		if len(failures) != 1 || failures[0].Channel != "lead" {
			t.Fatalf("проход %d: %+v", i+1, failures)
		}
	}
	if sent["git-admin"] != 1 {
		t.Fatalf("git-admin получил %d писем, ждали 1", sent["git-admin"])
	}
	if _, ok := state[signals[0].Key].Channels["git-admin"]; !ok {
		t.Fatalf("доставка git-admin не учтена: %+v", state)
	}
}

func TestNightRecipients(t *testing.T) {
	cfg := night.Default(t.TempDir())
	cfg.Lead = "lead"
	cfg.AlsoTo = []string{"git-admin", "lead", "work-admin", "git-admin"}
	if got := strings.Join(cfg.Recipients(), ","); got != "lead,git-admin,work-admin" {
		t.Fatal(got)
	}
	cfg.AlsoTo = []string{""}
	if err := cfg.Validate(t.TempDir()); err == nil {
		t.Fatal("пустое имя в also_to прошло проверку")
	}
}

// Живая проба доставки: AGUARD_NIGHT_PROBE=<живая сессия> — лид несуществующий, сигнал Сна должен дойти до пробной
// сессии через её сокет. Без переменной — пропуск.
func TestNightProbeLive(t *testing.T) {
	to := os.Getenv("AGUARD_NIGHT_PROBE")
	if to == "" {
		t.Skip("AGUARD_NIGHT_PROBE не задан")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	full, err := config.Load(config.Path(home), home, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	var logs strings.Builder
	a := &app{home: dir, stateDir: filepath.Join(dir, "state"), reg: session.ForHome(home), log: slog.New(slog.NewTextHandler(&logs, nil))}
	cfg := full.Night
	cfg.Mode = night.ModeAlert
	cfg.Lead = "night-probe-absent-lead"
	cfg.AlsoTo = []string{to}
	cfg.DreamBy = "00:00"
	cfg.DreamFallback = ""
	cfg.DreamFallbackBy = pastFallbackBy()
	cfg.DreamMarker = filepath.Join(dir, "absent")
	cfg.DigestBy = "23:59"
	cfg.DigestMarker = filepath.Join(dir, "absent-digest")
	if err := a.nightRound(&cfg, false); err != nil {
		t.Fatal(err)
	}
	state, err := loadNightState(filepath.Join(a.stateDir, "night", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	key := "dream:" + time.Now().Format("2006-01-02")
	if _, ok := state[key].Channels[to]; !ok {
		t.Fatalf("письмо %s не дошло: %+v\n%s", to, state, logs.String())
	}
	if _, ok := state[key].Channels[cfg.Lead]; ok {
		t.Fatalf("несуществующему лиду «дошло»: %+v", state)
	}
	t.Log(logs.String())
}

// Сон ведёт dream-master: сигнал о Сне идёт и ему и называет следующий шаг, сигнал о Дайджесте — только лиду и
// also_to (обход design Сна 30.10).
func TestNightDreamOwnerGetsDreamSignal(t *testing.T) {
	cfg := night.Default(t.TempDir())
	cfg.Lead = "lead"
	cfg.DreamFallbackBy = "07:00"
	now := time.Date(2026, 10, 30, 8, 0, 0, 0, time.Local)
	signals := night.Check(now, cfg, night.Facts{})
	if len(signals) != 2 {
		t.Fatalf("ждали сигналы Сна и Дайджеста: %+v", signals)
	}
	for _, s := range signals {
		if !strings.Contains(s.Text, "запусти ") {
			t.Errorf("%s: в тексте нет следующего шага: %s", s.Key, s.Text)
		}
	}
	if !strings.Contains(signals[0].Text, "dream-master: сначала проверь") || !strings.Contains(signals[0].Text, "dream-run-1030") {
		t.Errorf("текст Сна: %s", signals[0].Text)
	}
	got := map[string][]string{}
	dispatchNight(nightState{}, signals, cfg.Recipients(), now, func(to, msg, signalKey string) error {
		key, _, _ := strings.Cut(msg, " ")
		got[key] = append(got[key], to)
		return nil
	})
	if strings.Join(got["Сон"], ",") != "lead,git-admin,dream-master" || strings.Join(got["Дайджест"], ",") != "lead,git-admin" {
		t.Fatalf("адресаты: %v", got)
	}
}

// Сигнал уходит только письмом сессиям — лиду, also_to и своим адресатам сигнала; другого канала нет (Telegram снят
// 28.10, пользователь: «не надо так», a46a53c).
func TestNightAlertOnlySessionLetters(t *testing.T) {
	dir := t.TempDir()
	a := &app{home: dir, stateDir: filepath.Join(dir, "state"), log: slog.New(slog.NewTextHandler(&strings.Builder{}, nil))}
	got := liveLead(t, a)
	cfg := night.Default(dir)
	cfg.Mode = night.ModeAlert
	cfg.Lead = "lead"
	cfg.AlsoTo, cfg.DreamTo, cfg.DreamFallback = nil, nil, ""
	cfg.DreamBy = "00:00"
	cfg.DreamFallbackBy = pastFallbackBy()
	cfg.DreamMarker = filepath.Join(dir, "absent")
	cfg.DigestMarker = filepath.Join(dir, "digest")
	now := time.Now() // Дайджест прошёл сегодня и завтра: сигнал один — о Сне, и после полуночи тоже
	if err := os.WriteFile(cfg.DigestMarker, []byte(now.Format("2006-01-02")+" "+now.AddDate(0, 0, 1).Format("2006-01-02")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.nightRound(&cfg, false); err != nil {
		t.Fatal(err)
	}
	if msg := <-got; !strings.Contains(msg, "Сон") {
		t.Fatalf("лиду пришло: %q", msg)
	}
	state, err := loadNightState(filepath.Join(a.stateDir, "night", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(state) != 1 {
		t.Fatalf("сигналов %d, ждали 1 (Сон): %+v", len(state), state)
	}
	for key, sent := range state {
		for channel := range sent.Channels {
			if channel != "lead" {
				t.Errorf("%s: доставка по каналу %q — сверх писем сессиям", key, channel)
			}
		}
		if _, ok := sent.Channels["lead"]; !ok {
			t.Errorf("%s: письмо лиду не учтено: %+v", key, sent)
		}
	}
}

// Режим log не читает состояние доставок: битый state.json не мешает «написал бы» и строке о снятом Сне.
func TestNightLogModeIgnoresBrokenState(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(filepath.Join(stateDir, "night"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "night", "state.json"), []byte("{битый"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, paused := range []bool{false, true} {
		var logs strings.Builder
		a := &app{home: dir, stateDir: stateDir, log: slog.New(slog.NewTextHandler(&logs, nil)), units: &fakeUnits{}}
		cfg := night.Default(dir)
		cfg.Mode = night.ModeLog
		cfg.DreamBy = "00:00"
		cfg.DreamFallback = ""
		cfg.DreamFallbackBy = "23:59"
		cfg.DreamMarker = filepath.Join(dir, "absent-marker")
		cfg.DreamPaused = filepath.Join(dir, "night-paused")
		if paused {
			if err := os.WriteFile(cfg.DreamPaused, []byte("пользователь\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		cfg.DigestMarker = filepath.Join(dir, "digest")
		for range 2 {
			if err := a.nightRound(&cfg, false); err != nil {
				t.Fatalf("пауза %v: %v", paused, err)
			}
		}
		want := map[bool]string{false: "aguard night: написал бы", true: "aguard night: Сон снят"}[paused]
		if n := strings.Count(logs.String(), want); n < 1 {
			t.Fatalf("пауза %v: нет %q:\n%s", paused, want, logs.String())
		}
		if paused {
			// перезапуск в режиме log: новый app на той же папке состояния — строки за дату больше нет
			restarted := &app{home: dir, stateDir: stateDir, log: a.log, units: &fakeUnits{}}
			if err := restarted.nightRound(&cfg, false); err != nil {
				t.Fatal(err)
			}
			if n := strings.Count(logs.String(), "aguard night: Сон снят"); n != 1 {
				t.Fatalf("строк о снятом Сне %d, ждали 1 и после перезапуска:\n%s", n, logs.String())
			}
		}
	}
}

// fakePauseCheck — подставной `dreamstats -pause-check`: печатает answer и выходит с code (2 — битый файл, текст в
// stderr); аргументы вызова — в dir/pause-args.
func fakePauseCheck(t *testing.T, dir, answer string, code int) []string {
	t.Helper()
	script := filepath.Join(dir, "pause-check")
	body := fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %s/pause-args\n[ %d -ne 0 ] && { echo 'dreamstats: неверная дата \"2026-13-01\"' >&2; exit %d; }\necho %s\n",
		dir, code, code, answer)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return []string{script, "-night", "{date}", "-pause-check"}
}

// Пауза по датам ночей (night-pause-dates): paused — ни письма, ни unit; outside — как без файла (письмо «Сон не
// стартовал» и запасной unit); битый файл — письмо об ошибке, unit нет. Команде уходит местная дата ночи.
func TestNightPauseByDates(t *testing.T) {
	for _, tc := range []struct {
		answer            string
		code              int
		unit              bool
		letter, notLetter string
	}{
		{answer: "paused"},
		{answer: "outside", unit: true, letter: "Сон не стартовал", notLetter: "Файл паузы"},
		{answer: "", code: 2, letter: "неверная дата", notLetter: "Звенья:"},
		{answer: "что-то", letter: "неизвестный статус паузы", notLetter: "Звенья:"},
	} {
		t.Run(tc.answer+fmt.Sprint(tc.code), func(t *testing.T) {
			dir := t.TempDir()
			pause := filepath.Join(dir, "night-paused")
			if err := os.WriteFile(pause, []byte("пауза\nnights: 2026-11-07\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			units := &fakeUnits{}
			a := &app{home: dir, stateDir: filepath.Join(dir, "state"), log: slog.New(slog.DiscardHandler), units: units}
			mail := liveLead(t, a)
			cfg := night.Default(dir)
			cfg.Mode, cfg.Lead, cfg.DreamBy, cfg.DigestBy = night.ModeAlert, "lead", "00:00", "23:59"
			cfg.DreamFallbackBy = pastFallbackBy()
			cfg.DreamMarker, cfg.DreamPaused = filepath.Join(dir, "absent-marker"), pause
			cfg.DreamPauseCheck = fakePauseCheck(t, dir, tc.answer, tc.code)
			if err := a.nightRound(&cfg, false); err != nil {
				t.Fatal(err)
			}
			if got := len(units.started) == 1; got != tc.unit {
				t.Fatalf("запасной unit: %v, ждали %v", units.started, tc.unit)
			}
			var letters []string
			for len(mail) > 0 {
				letters = append(letters, <-mail)
			}
			all := strings.Join(letters, "\n")
			if tc.letter == "" && len(letters) != 0 || tc.letter != "" && !strings.Contains(all, tc.letter) ||
				tc.notLetter != "" && strings.Contains(all, tc.notLetter) {
				t.Fatalf("письма (%d):\n%s", len(letters), all)
			}
			args, err := os.ReadFile(filepath.Join(dir, "pause-args"))
			if err != nil || !strings.Contains(string(args), "-night "+time.Now().Format("2006-01-02")+" ") {
				t.Fatalf("аргументы команды статуса: %q %v", args, err)
			}
		})
	}
}

// pastFallbackBy — срок запасного Сна, уже наступивший сегодня и с открытым окном (3 ч от срока): час назад, а в первый
// час суток — 00:00 (час назад — вчера, срок сегодняшней даты ещё впереди: тест падал с 00:00 до 01:00).
func pastFallbackBy() string {
	now := time.Now()
	if now.Hour() == 0 {
		return "00:00"
	}
	return now.Add(-time.Hour).Format("15:04")
}
