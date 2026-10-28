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
	"github.com/aiseeq/aguard/internal/night"
)

func TestNightDeliveryRetriesOnlyFailedChannel(t *testing.T) {
	dir := t.TempDir()
	count := filepath.Join(dir, "count")
	seen := filepath.Join(dir, "seen")
	script := filepath.Join(dir, "tgctl")
	body := fmt.Sprintf("#!/bin/sh\nn=$(cat %q 2>/dev/null || echo 0)\nn=$((n+1))\necho $n > %q\n[ \"$1\" = sendhtml ] || exit 2\ncat \"$3\" > %q\n[ $n -ge 2 ]\n", count, count, seen)
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := night.Default(dir)
	cfg.TelegramCmd = script
	state := nightState{}
	signals := []night.Signal{{Key: "dream:2026-10-28", Text: "Сон <нет> & ждать"}}
	now := time.Now()
	emails := 0
	send := func(channel, msg string) error {
		if channel == "lead" {
			emails++
			return nil
		}
		return nightTelegram(&cfg, msg)
	}
	var failures []nightFailure
	state, failures = dispatchNight(state, signals, now, send)
	if len(failures) != 1 || failures[0].Channel != "telegram" || emails != 1 {
		t.Fatalf("первый проход: ошибки=%+v письма=%d", failures, emails)
	}
	state, failures = dispatchNight(state, signals, now.Add(time.Minute), send)
	if len(failures) != 0 || emails != 1 {
		t.Fatalf("повтор: ошибки=%+v письма=%d", failures, emails)
	}
	if _, ok := state[signals[0].Key].Channels["telegram"]; !ok {
		t.Fatal("Telegram не отмечен после успеха")
	}
	if _, ok := state[signals[0].Key].Channels["lead"]; !ok {
		t.Fatal("письмо не отмечено")
	}
	b, err := os.ReadFile(seen)
	if err != nil || string(b) != "Сон &lt;нет&gt; &amp; ждать" {
		t.Fatalf("HTML-файл %q, err %v", b, err)
	}
}

func TestNightDryRunDoesNotWriteState(t *testing.T) {
	dir := t.TempDir()
	var out strings.Builder
	a := &app{home: dir, stateDir: filepath.Join(dir, "state"), log: slog.New(slog.NewTextHandler(os.Stderr, nil)), out: &out}
	cfg := night.Default(dir)
	cfg.Mode = night.ModeAlert
	cfg.DreamBy = "00:00"
	cfg.DreamMarker = filepath.Join(dir, "absent")
	cfg.DigestBy = "23:59"
	if err := a.nightRound(nil, &cfg, true); err != nil {
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
	cfg.DreamMarker = filepath.Join(dir, "absent")
	cfg.DigestMarker = filepath.Join(dir, "digest")
	// Дайджест прошёл сегодня и завтра: сигнал один и в 23:59, и после полуночи
	now := time.Now()
	if err := os.WriteFile(cfg.DigestMarker, []byte(now.Format("2006-01-02")+" "+now.AddDate(0, 0, 1).Format("2006-01-02")), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := a.nightRound(nil, &cfg, false); err != nil {
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
