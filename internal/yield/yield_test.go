package yield

import (
	"context"
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
	if !y.Wait(ctx) || y.Pauses != 1 {
		t.Fatalf("пауз на занятой машине: %d, ждал 1", y.Pauses)
	}
	if took := time.Since(start); took >= Pause/2 {
		t.Fatalf("отменённая пауза длилась %v", took)
	}
	if y.Wait(ctx) || y.Pauses != 1 { // второй вызов в ту же секунду — без замера и паузы
		t.Fatalf("пауз после второго вызова: %d", y.Pauses)
	}
}

// Пауза конечна: без отмены — pause, и после неё машина снова меряется не раньше чем через check от конца паузы
// (пауза длиннее check, как Pause и Check), то есть на постоянно занятой машине обход идёт урывками и всё же кончается.
func TestPauseIsFinite(t *testing.T) {
	y := newWith(busyMeter(t), func(err error) { t.Errorf("замер: %v", err) })
	y.check, y.pause = 200*time.Millisecond, 300*time.Millisecond
	start := time.Now()
	if !y.Wait(t.Context()) {
		t.Fatal("занятая машина без паузы")
	}
	if took := time.Since(start); took < y.pause || took > 5*time.Second {
		t.Fatalf("пауза %v, ждал %v", took, y.pause)
	}
	if y.Paused < y.pause {
		t.Fatalf("время пауз %v меньше паузы %v", y.Paused, y.pause)
	}
	if y.Wait(t.Context()) {
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
	if y.Wait(ctx) || y.Pauses != 0 {
		t.Fatalf("порог пользователя 100 готовых, а пауза была: %d", y.Pauses)
	}
	if err := os.WriteFile(file, []byte("runnable 16\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var warned error
	warns := 0
	y = newWith(m, func(err error) { warned = err; warns++ })
	y.check = 0 // каждый вызов — замер
	y.Wait(ctx)
	y.Wait(ctx)
	if warned == nil || !strings.Contains(warned.Error(), "pressure.yaml") || y.Pauses != 0 || warns != 1 {
		t.Fatalf("битый файл порогов: ошибка %v (раз %d), пауз %d — ждал одну ошибку с путём и без паузы", warned, warns, y.Pauses)
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
