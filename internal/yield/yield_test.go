package yield

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/pressure"
)

// Занятая машина: обход ждёт; остановка демона обрывает паузу сразу (f62029e).
func TestPausesOnBusyMachine(t *testing.T) {
	y := newWith(busyMeter(t), func(err error) { t.Errorf("замер: %v", err) })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	start := time.Now()
	paused, err := y.Wait(ctx)
	if !errors.Is(err, context.Canceled) || !paused || y.Pauses != 1 {
		t.Fatalf("пауз на занятой машине: %d, ждал 1", y.Pauses)
	}
	if took := time.Since(start); took >= Pause/2 {
		t.Fatalf("отменённая пауза длилась %v", took)
	}
	paused, err = y.Wait(ctx)
	if !errors.Is(err, context.Canceled) || paused || y.Pauses != 1 { // второй вызов в ту же секунду — без замера и паузы
		t.Fatalf("пауз после второго вызова: %d", y.Pauses)
	}
}

func TestWaitSafePausesOnBusyAndFailsClosedOnMeasureError(t *testing.T) {
	y := newWith(busyMeter(t), func(err error) { t.Errorf("замер: %v", err) })
	y.check = 0
	y.pause = time.Millisecond
	if err := y.WaitSafe(t.Context()); err != nil || y.Pauses != 1 {
		t.Fatalf("busy: err=%v pauses=%d", err, y.Pauses)
	}
	y = newWith(pressure.NewMeter(t.TempDir()), func(error) {})
	y.check = 0
	y.pause = time.Millisecond
	configDir := os.Getenv("XDG_CONFIG_HOME")
	if err := os.MkdirAll(filepath.Join(configDir, "akit"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "akit", "pressure.yaml"), []byte("runnable: broken\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := y.WaitSafe(t.Context()); err == nil || y.Pauses != 1 {
		t.Fatalf("measurement error must pause and fail closed: err=%v pauses=%d", err, y.Pauses)
	}
}

// Disk and track use Wait: a broken pressure config now pauses their scan before it proceeds.
func TestDiskYielderPausesWhenPressureMeasurementFails(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeBrokenPressureConfig(t)
	var warned error
	y := newWith(pressure.NewMeter(t.TempDir()), func(err error) { warned = err })
	y.check, y.pause = 0, time.Millisecond
	paused, err := y.Wait(t.Context())
	if err != nil || !paused || y.Pauses != 1 {
		t.Fatalf("disk-facing Wait did not pause and allow one work portion: err=%v pauses=%d", err, y.Pauses)
	}
	if warned == nil {
		t.Fatal("measurement error was not reported")
	}
}

// Пауза конечна: без отмены — pause, и после неё машина снова меряется не раньше чем через check от конца паузы
// (пауза длиннее check, как Pause и Check), то есть на постоянно занятой машине обход идёт урывками и всё же кончается.
func TestPauseIsFinite(t *testing.T) {
	y := newWith(busyMeter(t), func(err error) { t.Errorf("замер: %v", err) })
	y.check, y.pause = 200*time.Millisecond, 300*time.Millisecond
	start := time.Now()
	paused, err := y.Wait(t.Context())
	if err != nil || !paused {
		t.Fatal("занятая машина без паузы")
	}
	if took := time.Since(start); took < y.pause || took > 5*time.Second {
		t.Fatalf("пауза %v, ждал %v", took, y.pause)
	}
	if y.Paused < y.pause {
		t.Fatalf("время пауз %v меньше паузы %v", y.Paused, y.pause)
	}
	paused, err = y.Wait(t.Context())
	if err != nil || paused {
		t.Fatal("после паузы новый замер раньше Check: обход не продвинулся бы")
	}
}

// Пороги пользователя из akit (pressure.yaml) — те же, что у busy и очереди graft; битый файл — ошибка, не умолчания.
func TestUserLimits(t *testing.T) {
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
	y := newWith(m, func(err error) { t.Errorf("замер: %v", err) })
	paused, err := y.Wait(ctx)
	if !errors.Is(err, context.Canceled) || paused || y.Pauses != 0 {
		t.Fatalf("порог пользователя 100 готовых, а пауза была: %d", y.Pauses)
	}
	writeBrokenPressureConfig(t)
	var warned error
	warns := 0
	y = newWith(m, func(err error) { warned = err; warns++ })
	y.check = 0 // каждый вызов — замер
	y.pause = time.Millisecond
	firstPaused, firstErr := y.Wait(t.Context())
	secondPaused, secondErr := y.Wait(t.Context())
	if warned == nil || !strings.Contains(warned.Error(), "pressure.yaml") || firstErr != nil || secondErr != nil || !firstPaused || !secondPaused || y.Pauses != 2 || warns != 1 {
		t.Fatalf("битый файл порогов: ошибка %v (раз %d), вызовы %v/%v err=%v/%v, пауз %d", warned, warns, firstPaused, secondPaused, firstErr, secondErr, y.Pauses)
	}
}

func writeBrokenPressureConfig(t *testing.T) {
	t.Helper()
	file := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "akit", "pressure.yaml")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("runnable: broken\n"), 0o600); err != nil {
		t.Fatal(err)
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
