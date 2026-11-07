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
	onStart  func() // проверка в момент запуска
	st       unitStatus
	cleaned  []string
}

func (f *fakeUnits) start(unit, _, _ string) error {
	f.started = append(f.started, unit)
	if f.onStart != nil {
		f.onStart()
	}
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
	for _, s := range signals {
		if strings.Join(s.To, ",") != "dream-master" {
			t.Fatalf("адресаты %s: %v", s.Key, s.To)
		}
	}
}

// Успешный конец с RemainAfterExit — active/exited, код 0; пропавший unit (not-found: show отдаёт inactive, success, 0) —
// «пропал», без кода и без снятия.
func TestNightFallbackExitedLost(t *testing.T) {
	units := &fakeUnits{st: unitStatus{Load: "loaded", Active: "active", Sub: "running"}}
	a, cfg := fallbackApp(t, units)
	if _, err := a.fallbackStep(cfg, at("03:10"), night.Facts{}, false); err != nil {
		t.Fatal(err)
	}
	units.st = unitStatus{Load: "loaded", Active: "active", Sub: "exited", Result: "success"}
	signals, err := a.fallbackStep(cfg, at("08:30"), night.Facts{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(signals) != 2 || !strings.Contains(signals[1].Text, "кончился в 08:30: код 0, success") {
		t.Fatalf("конец с кодом 0: %+v", signals)
	}
	units = &fakeUnits{st: unitStatus{Load: "loaded", Active: "active", Sub: "running"}}
	a, cfg = fallbackApp(t, units)
	if _, err := a.fallbackStep(cfg, at("03:10"), night.Facts{}, false); err != nil {
		t.Fatal(err)
	}
	units.st = unitStatus{Load: "not-found", Active: "inactive", Sub: "dead", Result: "success"}
	signals, err = a.fallbackStep(cfg, at("05:00"), night.Facts{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(signals) != 2 || !strings.Contains(signals[1].Text, "пропал из systemd") || strings.Contains(signals[1].Text, "код 0") ||
		strings.Join(signals[1].To, ",") != "dream-master" {
		t.Fatalf("пропавший unit: %+v", signals)
	}
	if len(units.cleaned) != 0 {
		t.Fatalf("пропавший снят: %v", units.cleaned)
	}
}

// Запись о запуске — на диске до запуска; не записалась — запуска нет, письмо «запусти сам», ключа в памяти нет.
func TestNightFallbackSaveFirst(t *testing.T) {
	units := &fakeUnits{st: unitStatus{Load: "loaded", Active: "active", Sub: "running"}}
	a, cfg := fallbackApp(t, units)
	path := filepath.Join(a.dir("night"), "fallback.json")
	units.onStart = func() {
		state, err := loadFallbackState(path)
		if err != nil || state["2026-10-31"].Unit != "dream-terra-2026-10-31" {
			t.Errorf("до запуска на диске: %+v %v", state, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil { // путь занят папкой — запись не выйдет
		t.Fatal(err)
	}
	a.nightFb = fallbackState{}
	signals, err := a.fallbackStep(cfg, at("03:10"), night.Facts{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(units.started) != 0 || len(signals) != 1 || signals[0].Key != "dream-fallback-blocked:2026-10-31" ||
		!strings.Contains(signals[0].Text, "запущу сам, как только запись пройдёт") || strings.Join(signals[0].To, ",") != "dream-master" {
		t.Fatalf("запуск без записи: %v %+v", units.started, signals)
	}
	if tmp, err := filepath.Glob(path + ".*.tmp"); err != nil || len(tmp) != 0 {
		t.Fatalf("временные файлы после неудачной записи: %v %v", tmp, err)
	}
	if _, ok := a.nightFb["2026-10-31"]; ok {
		t.Fatal("ключ без записи остался в памяти")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	signals, err = a.fallbackStep(cfg, at("03:11"), night.Facts{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(units.started) != 1 || len(signals) != 1 || !strings.Contains(signals[0].Text, "запустил на Terra") {
		t.Fatalf("после починки: %v %+v", units.started, signals)
	}
}

// Битый fallback.json — запуска нет, письмо о выключенном шаге.
func TestNightFallbackBrokenState(t *testing.T) {
	units := &fakeUnits{}
	a, cfg := fallbackApp(t, units)
	path := filepath.Join(a.dir("night"), "fallback.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{обрезан"), 0o600); err != nil {
		t.Fatal(err)
	}
	signals, err := a.fallbackStep(cfg, at("03:10"), night.Facts{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(units.started) != 0 || len(signals) != 1 || signals[0].Key != "dream-fallback-state:2026-10-31" ||
		!strings.Contains(signals[0].Text, "выключен") || strings.Join(signals[0].To, ",") != "dream-master" {
		t.Fatalf("битое состояние: %v %+v", units.started, signals)
	}
}

// Запись атомарна: файл меняется целиком через переименование временного, старое содержимое не обрезается на месте.
func TestSaveFallbackStateAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fallback.json")
	if err := saveFallbackState(path, fallbackState{"2026-10-30": {Unit: "dream-terra-2026-10-30"}}); err != nil {
		t.Fatal(err)
	}
	old, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveFallbackState(path, fallbackState{"2026-10-31": {Unit: "dream-terra-2026-10-31"}}); err != nil {
		t.Fatal(err)
	}
	cur, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(old, cur) {
		t.Fatal("файл переписан на месте, а не заменён")
	}
	state, err := loadFallbackState(path)
	if err != nil || len(state) != 1 || state["2026-10-31"].Unit != "dream-terra-2026-10-31" {
		t.Fatalf("прочитано: %+v %v", state, err)
	}
	if tmp, err := filepath.Glob(path + ".*.tmp"); err != nil || len(tmp) != 0 {
		t.Fatalf("временные файлы: %v %v", tmp, err)
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

// Живой systemd пользователя: unit с кодом 3 и с кодом 0 (RemainAfterExit держит его active/exited) — конец, код, хвост
// журнала, снятие, после снятия — not-found. Только с AGUARD_LIVE_UNITS=1.
func TestSystemdUnitsLive(t *testing.T) {
	if os.Getenv("AGUARD_LIVE_UNITS") != "1" {
		t.Skip("AGUARD_LIVE_UNITS=1 — проба на systemd пользователя")
	}
	for _, code := range []int{3, 0} {
		liveUnit(t, code)
	}
}

func liveUnit(t *testing.T, code int) {
	t.Helper()
	u := systemdUnits{}
	unit := fmt.Sprintf("aguard-probe-%d-%d", os.Getpid(), code)
	if err := u.start(unit, t.TempDir(), fmt.Sprintf("echo проба-$HOME; exit %d", code)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { // упавшая на середине проба не оставляет unit (проверка git-admin 30.10)
		if err := u.clean(unit); err != nil {
			t.Error(err)
		}
	})
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
	if !st.finished() || st.lost() || st.Code != code || code == 0 && st.Sub != "exited" {
		t.Fatalf("состояние: %+v", st)
	}
	tail := u.tail(unit)
	for i := 0; i < 20 && !strings.Contains(tail, "проба-/"); i++ { // журнал доходит с задержкой
		time.Sleep(100 * time.Millisecond)
		tail = u.tail(unit)
	}
	if !strings.Contains(tail, "проба-/") {
		t.Fatalf("хвост: %q", tail)
	}
	if err := u.clean(unit); err != nil {
		t.Fatal(err)
	}
	if st, err := u.status(unit); err != nil || !st.lost() {
		t.Fatalf("после снятия: %+v %v", st, err)
	}
	if err := u.clean(unit); err != nil {
		t.Fatalf("повторное снятие: %v", err)
	}
}

// Битый fallback.json после окна запасного Сна (06:10) — письма нет: о несостоявшемся Сне уже поздно.
func TestNightFallbackBrokenStateAfterWindow(t *testing.T) {
	units := &fakeUnits{}
	a, cfg := fallbackApp(t, units)
	path := filepath.Join(a.dir("night"), "fallback.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{обрезан"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		clock string
		want  int
	}{{"06:09", 1}, {"06:10", 0}, {"10:34", 0}} {
		signals, err := a.fallbackStep(cfg, at(c.clock), night.Facts{}, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(signals) != c.want || len(units.started) != 0 {
			t.Fatalf("%s: сигналов %d, ждали %d; запусков %v", c.clock, len(signals), c.want, units.started)
		}
	}
}

// Сон снят: ни битое состояние, ни ранее упавший запуск не дают писем о запасном Сне.
func TestNightFallbackPausedNoSignals(t *testing.T) {
	units := &fakeUnits{}
	a, cfg := fallbackApp(t, units)
	path := filepath.Join(a.dir("night"), "fallback.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	paused := night.Facts{DreamPaused: true}
	if err := os.WriteFile(path, []byte("{обрезан"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := a.nightFallback(cfg, at("03:10"), paused, false); len(got) != 0 {
		t.Fatalf("битое состояние при паузе: %+v", got)
	}
	a.nightFb = nil
	if err := saveFallbackState(path, fallbackState{"2026-10-31": {Unit: night.FallbackUnit(at("03:10")), At: at("03:10"),
		Err: "systemd-run: отказ"}}); err != nil {
		t.Fatal(err)
	}
	if got := a.nightFallback(cfg, at("03:20"), paused, false); len(got) != 0 {
		t.Fatalf("упавший запуск при паузе: %+v", got)
	}
	if len(units.started) != 0 {
		t.Fatalf("запуск при паузе: %v", units.started)
	}
	a.nightFb = nil
	if got := a.nightFallback(cfg, at("03:20"), night.Facts{}, false); len(got) == 0 {
		t.Fatal("без паузы упавший запуск не дал письма — тест не проверяет то, что должен")
	}
}
