package main

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/night"
)

// fakeUnits — подмена systemd: запуски, состояние unit и снятые.
type fakeUnits struct {
	started  []string
	startErr error
	st       unitStatus
	cleaned  []string
}

func (f *fakeUnits) start(unit, _, _ string) error {
	f.started = append(f.started, unit)
	return f.startErr
}

func (f *fakeUnits) status(string) (unitStatus, error) { return f.st, nil }

func (f *fakeUnits) tail(string) string { return "шаг 9: готово" }

func (f *fakeUnits) clean(unit string) error {
	f.cleaned = append(f.cleaned, unit)
	return nil
}

func fallbackApp(t *testing.T, units *fakeUnits) (*app, *night.Config) {
	t.Helper()
	dir := t.TempDir()
	a := &app{home: dir, stateDir: filepath.Join(dir, "state"), log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), units: units}
	cfg := night.Default(dir)
	cfg.Mode = night.ModeAlert
	cfg.DreamTo = []string{"dream-master"}
	return a, &cfg
}

func at(clock string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", "2026-10-31 "+clock, time.Local)
	if err != nil {
		panic(err)
	}
	return t
}

// Запуск — раз за дату: ни до срока, ни при маркере, ни после окна; второй проход и новый демон (состояние с диска)
// не запускают снова.
func TestNightFallbackOnce(t *testing.T) {
	units := &fakeUnits{st: unitStatus{Active: "active", Sub: "running"}}
	a, cfg := fallbackApp(t, units)
	for _, c := range []struct {
		clock  string
		marker bool
	}{{"03:09", false}, {"03:20", true}, {"06:11", false}} {
		if _, err := a.fallbackStep(cfg, at(c.clock), night.Facts{DreamMarker: c.marker}, false); err != nil {
			t.Fatal(err)
		}
	}
	if len(units.started) != 0 {
		t.Fatalf("запуск вне окна или при маркере: %v", units.started)
	}
	for _, clock := range []string{"03:10", "03:11"} {
		signals, err := a.fallbackStep(cfg, at(clock), night.Facts{}, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(signals) != 1 || signals[0].Key != "dream-fallback:2026-10-31" || !strings.Contains(signals[0].Text, "запустил на Terra") ||
			signals[0].To[0] != "dream-master" {
			t.Fatalf("сигнал запуска: %+v", signals)
		}
	}
	a.nightFb = nil
	if _, err := a.fallbackStep(cfg, at("03:30"), night.Facts{}, false); err != nil {
		t.Fatal(err)
	}
	if strings.Join(units.started, ",") != "dream-terra-2026-10-31" {
		t.Fatalf("запусков: %v", units.started)
	}
}

// systemd-run упал — сигнал с ошибкой, повтора нет.
func TestNightFallbackStartFailed(t *testing.T) {
	units := &fakeUnits{startErr: errors.New("systemd-run: нет шины")}
	a, cfg := fallbackApp(t, units)
	for _, clock := range []string{"03:10", "03:11", "03:40"} {
		signals, err := a.fallbackStep(cfg, at(clock), night.Facts{}, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(signals) != 1 || !strings.Contains(signals[0].Text, "нет шины") || !strings.Contains(signals[0].Text, "запусти сам") {
			t.Fatalf("сигнал ошибки: %+v", signals)
		}
	}
	if len(units.started) != 1 {
		t.Fatalf("запусков: %v", units.started)
	}
}

// Кончился — сигнал конца с кодом и хвостом журнала, unit снят.
func TestNightFallbackEnd(t *testing.T) {
	units := &fakeUnits{st: unitStatus{Active: "active", Sub: "running"}}
	a, cfg := fallbackApp(t, units)
	if _, err := a.fallbackStep(cfg, at("03:10"), night.Facts{}, false); err != nil {
		t.Fatal(err)
	}
	units.st = unitStatus{Active: "failed", Sub: "failed", Result: "exit-code", Code: 3}
	signals, err := a.fallbackStep(cfg, at("08:30"), night.Facts{DreamMarker: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(signals) != 2 || signals[1].Key != "dream-fallback-end:2026-10-31" || !strings.Contains(signals[1].Text, "код 3, exit-code") ||
		!strings.Contains(signals[1].Text, "шаг 9: готово") {
		t.Fatalf("сигнал конца: %+v", signals)
	}
	if strings.Join(units.cleaned, ",") != "dream-terra-2026-10-31" {
		t.Fatalf("снятые unit: %v", units.cleaned)
	}
	a.nightFb = nil
	if _, err := a.fallbackStep(cfg, at("08:31"), night.Facts{}, false); err != nil {
		t.Fatal(err)
	}
	if len(units.cleaned) != 1 {
		t.Fatalf("кончившийся снова снят: %v", units.cleaned)
	}
}

// mode=log — не запускает; выключенный (пустая команда) — ничего.
func TestNightFallbackLogOff(t *testing.T) {
	units := &fakeUnits{}
	a, cfg := fallbackApp(t, units)
	cfg.Mode = night.ModeLog
	if _, err := a.fallbackStep(cfg, at("03:10"), night.Facts{}, false); err != nil {
		t.Fatal(err)
	}
	cfg.Mode = night.ModeAlert
	cfg.DreamFallback = ""
	if _, err := a.fallbackStep(cfg, at("03:11"), night.Facts{}, false); err != nil {
		t.Fatal(err)
	}
	if len(units.started) != 0 {
		t.Fatalf("запуск: %v", units.started)
	}
}

// Живой systemd пользователя: unit с кодом 3 — конец, код, хвост журнала, снятие. Только с AGUARD_LIVE_UNITS=1.
func TestSystemdUnitsLive(t *testing.T) {
	if os.Getenv("AGUARD_LIVE_UNITS") != "1" {
		t.Skip("AGUARD_LIVE_UNITS=1 — проба на systemd пользователя")
	}
	u := systemdUnits{}
	unit := fmt.Sprintf("aguard-probe-%d", os.Getpid())
	if err := u.start(unit, t.TempDir(), "echo проба-$HOME; exit 3"); err != nil {
		t.Fatal(err)
	}
	var st unitStatus
	for range 50 {
		var err error
		if st, err = u.status(unit); err != nil {
			t.Fatal(err)
		}
		if st.finished() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !st.finished() || st.Code != 3 {
		t.Fatalf("состояние: %+v", st)
	}
	if tail := u.tail(unit); !strings.Contains(tail, "проба-/") {
		t.Fatalf("хвост: %q", tail)
	}
	if err := u.clean(unit); err != nil {
		t.Fatal(err)
	}
	if st, err := u.status(unit); err != nil || st.Active != "inactive" {
		t.Fatalf("после снятия: %+v %v", st, err)
	}
	if err := u.clean(unit); err != nil {
		t.Fatalf("повторное снятие: %v", err)
	}
}
