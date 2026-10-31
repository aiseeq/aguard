package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/config"
	"github.com/aiseeq/aguard/internal/disk"
	"github.com/aiseeq/akit/pressure"
	"github.com/aiseeq/akit/session"
)

// Сигнал о свободном месте: отметки по адресатам — в free.json сразу после писем; дошедшему следующее через сутки,
// недошедшему (сессии нет) — повтор через час (8e8571b).
func TestDiskFreeMarksEachRecipient(t *testing.T) {
	a, _ := testIOApp(t)
	if err := os.MkdirAll(a.dir("disk"), 0o700); err != nil { // папку заводит run демона
		t.Fatal(err)
	}
	got := liveLead(t, a)
	cfg := disk.Default()
	cfg.Lead, cfg.FreeTell, cfg.FreeMode, cfg.FreeMin = "lead", []string{"git-admin"}, disk.FreeAlert, 150<<30
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.Local)
	if err := a.diskFree(&cfg, &disk.State{}, now, 140<<30); err != nil {
		t.Fatal(err)
	}
	if msg := <-got; !strings.Contains(msg, "свободно 140") {
		t.Fatalf("письмо лиду: %q", msg)
	}
	told, err := disk.LoadFreeTold(filepath.Join(a.dir("disk"), "free.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !told["lead"].Equal(now.Add(24*time.Hour)) || !told["git-admin"].Equal(now.Add(time.Hour)) {
		t.Fatalf("отметки в free.json: %v", told)
	}
}

// Занятая машина: обход ждёт; остановка демона обрывает паузу сразу (f62029e).
func TestDiskYieldPausesOnBusyMachine(t *testing.T) {
	m := busyMeter(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	y := &diskYielder{ctx: ctx, meter: m, warn: func(err error) { t.Errorf("замер: %v", err) }}
	y.wait()
	if y.pauses != 1 {
		t.Fatalf("пауз на занятой машине: %d, ждал 1", y.pauses)
	}
	y.wait() // второй вызов в ту же секунду — без замера и паузы
	if y.pauses != 1 {
		t.Fatalf("пауз после второго вызова: %d", y.pauses)
	}
}

// Пороги пользователя из akit (pressure.yaml) — те же, что у busy и очереди graft; битый файл — ошибка, не умолчания.
func TestDiskYieldUserLimits(t *testing.T) {
	m := busyMeter(t)
	file := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "akit", "pressure.yaml")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("runnable: 100\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	y := &diskYielder{ctx: ctx, meter: m, warn: func(err error) { t.Errorf("замер: %v", err) }}
	y.wait()
	if y.pauses != 0 {
		t.Fatalf("порог пользователя 100 готовых, а пауза была: %d", y.pauses)
	}
	if err := os.WriteFile(file, []byte("runnable 16\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var warned error
	y = &diskYielder{ctx: ctx, meter: m, warn: func(err error) { warned = err }}
	y.wait()
	if warned == nil || !strings.Contains(warned.Error(), "pressure.yaml") || y.pauses != 0 {
		t.Fatalf("битый файл порогов: ошибка %v, пауз %d — ждал ошибку с путём и без паузы", warned, y.pauses)
	}
}

// busyMeter — замер машины, занятой по процессору; пороги пользователя — пустой каталог настроек.
func busyMeter(t *testing.T) *pressure.Meter {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir()) // нет сокета состояния диска
	t.Setenv("XDG_STATE_HOME", t.TempDir())  // нет тихого режима
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // нет pressure.yaml пользователя
	root := t.TempDir()
	for name, body := range map[string]string{
		"stat":   "cpu  1\ncpu0 1\ncpu1 1\nprocs_running 9\n", // 8 готовых на 2 потока — занята по процессору
		"vmstat": "pswpin 1\npswpout 1\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m := pressure.NewMeter(root)
	// первый замер ждёт окно свопа: здесь — короткое, дальше своп считается с него
	if _, err := m.Check(t.Context(), pressure.Limits{SwapWindow: time.Millisecond}, "", false); err != nil {
		t.Fatal(err)
	}
	return m
}

// Проход disk спрашивает о машине по ходу обхода (yield.wait передан в MeasureAll, f62029e), а оборванный остановкой
// обход помечен в журнале (2f23463). Замер машины здесь падает на файле тихого режима — без чтения /proc.
func TestDiskRoundYieldsAndMarksInterrupted(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	if err := os.MkdirAll(filepath.Join(state, "akit"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "akit", "quiet-until"), []byte("не время\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, _ := testIOApp(t)
	var logged bytes.Buffer
	a.log = slog.New(slog.NewTextHandler(&logged, nil))
	a.reg = session.Registry{Dir: filepath.Join(t.TempDir(), "sessions"), ProcRoot: "/proc"}
	if err := os.MkdirAll(a.dir("disk"), 0o700); err != nil { // папку заводит run демона
		t.Fatal(err)
	}
	cache := filepath.Join(a.home, ".cache", "big")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range 1100 {
		if err := os.WriteFile(filepath.Join(cache, fmt.Sprintf("f%d", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Default(a.home)
	cfg.Disk.Roots, cfg.Disk.Containers, cfg.Disk.Track = []string{filepath.Join(a.home, ".cache")}, nil, nil
	cfg.Disk.FreeMode = disk.FreeOff
	stopped, cancel := context.WithCancel(t.Context())
	cancel()
	if err := a.diskRound(stopped, &cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logged.String(), "interrupted=true") {
		t.Fatalf("оборванный обход не помечен:\n%s", logged.String())
	}
	logged.Reset()
	if err := a.diskRound(t.Context(), &cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logged.String(), "замер машины для паузы обхода") || !strings.Contains(logged.String(), "interrupted=false") {
		t.Fatalf("обход не спросил о машине:\n%s", logged.String())
	}
}
