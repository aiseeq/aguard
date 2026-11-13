// Package iowatch — телеметрия диска, свопа и PSI для наблюдателя io: быстрые шаги машины и
// детальные записи по cgroup и процессам (замер), кольцевой буфер и сводки случаев фриза (случай),
// их хранение и итог за период (сводка), ручной снимок буфера и его чтение по окну (снимок).
package iowatch

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/aiseeq/aguard/internal/bgtask"
	"github.com/aiseeq/aguard/internal/ioroute"
	"github.com/aiseeq/aguard/internal/policy"
)

const (
	ModeOff   = bgtask.ModeOff
	ModeLog   = bgtask.ModeLog
	ModeAlert = bgtask.ModeAlert
)

// Config — параметры наблюдателя [io].
type Config struct {
	Mode       string          `toml:"mode"`        // off | log (случаи копятся, письма — в журнал) | alert
	Step       policy.Duration `toml:"step"`        // быстрый шаг: PSI, vmstat, diskstats
	Detail     policy.Duration `toml:"detail"`      // детальная запись: cgroup и процессы
	DetailIdle policy.Duration `toml:"detail_idle"` // детальная запись в покое
	Ring       policy.Duration `toml:"ring"`        // сколько сырых записей держать в памяти
	Trigger    float64         `toml:"trigger"`     // доля шага простоя PSI full: шаг с простоем
	Gap        policy.Duration `toml:"gap"`         // спокойный промежуток до стольких не делит случай
	CaseMin    policy.Duration `toml:"case_min"`    // случай от такой длительности — с виновниками
	Context    policy.Duration `toml:"context"`     // виновники ищутся и за столько до начала случая
	AlertMin   policy.Duration `toml:"alert_min"`   // случай от такой длительности — в маршрут (ioroute): письмо или тикет
	AlertEvery policy.Duration `toml:"alert_every"` // письмо одному адресату не чаще; срочное в паузе — по её концу
	// Маршрут случая от alert_min (тикет alert-async): вред сейчас — письмо хозяину виновника, иначе тикет в его проект.
	UrgentMin     policy.Duration   `toml:"urgent_min"`     // случай от такой длительности — вред сейчас
	SeriesN       int               `toml:"series_n"`       // столько случаев одного хозяина за series_window — вред сейчас
	SeriesWindow  policy.Duration   `toml:"series_window"`  // окно серии и общего вреда машины
	MachineN      int               `toml:"machine_n"`      // столько случаев за series_window от machine_owners хозяев — письмо лиду
	MachineOwners int               `toml:"machine_owners"` // 0 в machine_n — общий вред не сверяется
	CrossWindow   policy.Duration   `toml:"cross_window"`   // класс у двух хозяев за столько — письмо лиду, раз за окно на класс
	Owners        map[string]string `toml:"owners"`         // служба (unit) → сессия-хозяин; agent-*.scope и бойцы worker — сами
	Tk            string            `toml:"tk"`             // путь к tk: тикеты хозяевам
	QuietBytes    int64             `toml:"quiet_bytes"`    // обмен с диском меньше — случай тихий (простой без трафика)
	CasesKeep     policy.Duration   `toml:"cases_keep"`     // срок сводок случаев
	Keep          policy.Duration   `toml:"keep"`           // срок файлов ручных снимков буфера
	Device        string            `toml:"device"`         // диск в /proc/diskstats и /sys/block
	// Посекундный ряд машины и профили завершившихся cgroup — в памяти службы; файлы — их копия при
	// остановке (профили — и раз в час, как сводки).
	SeriesKeep   policy.Duration `toml:"series_keep"`   // срок посекундного ряда в памяти
	ProfilesKeep policy.Duration `toml:"profiles_keep"` // срок профилей cgroup
	// Socket — сокет моментального состояния диска (запросы now, series, profiles, cases); смена — с перезапуском
	// службы. По умолчанию $XDG_RUNTIME_DIR/io-state.sock (DefaultSocket).
	Socket   string `toml:"socket"`
	Lead     string `toml:"-"`
	SendAuth bool   `toml:"-"`
}

// Route — пороги маршрута случаев.
func (c *Config) Route() ioroute.Config {
	return ioroute.Config{UrgentMin: c.UrgentMin.D(), SeriesN: c.SeriesN, SeriesWindow: c.SeriesWindow.D(), Repeat: c.AlertEvery.D(),
		MachineN: c.MachineN, MachineOwners: c.MachineOwners, CrossWindow: c.CrossWindow.D(), Lead: c.Lead}
}

func Default() Config {
	return Config{
		Mode:          ModeAlert,
		Step:          policy.Duration(100 * time.Millisecond),
		Detail:        policy.Duration(time.Second),
		DetailIdle:    policy.Duration(5 * time.Second),
		Ring:          policy.Duration(5 * time.Minute),
		Trigger:       .5,
		Gap:           policy.Duration(300 * time.Millisecond),
		CaseMin:       policy.Duration(500 * time.Millisecond),
		Context:       policy.Duration(5 * time.Second),
		AlertMin:      policy.Duration(2 * time.Second),
		AlertEvery:    policy.Duration(time.Hour),
		UrgentMin:     policy.Duration(10 * time.Second),
		SeriesN:       3,
		SeriesWindow:  policy.Duration(15 * time.Minute),
		MachineN:      6,
		MachineOwners: 3,
		CrossWindow:   policy.Duration(24 * time.Hour),
		Tk:            "~/bin/tk",
		QuietBytes:    1 << 20,
		CasesKeep:     policy.Duration(14 * 24 * time.Hour),
		Keep:          policy.Duration(7 * 24 * time.Hour),
		Device:        "nvme0n1",
		SeriesKeep:    policy.Duration(24 * time.Hour),
		ProfilesKeep:  policy.Duration(30 * 24 * time.Hour),
	}
}

// DefaultSocket — сокет моментального состояния в папке runtime ($XDG_RUNTIME_DIR). Имя без aguard:
// клиент (akit) знает нейтральный контракт «источник моментального состояния диска», а не службу.
func DefaultSocket(runtime string) string { return filepath.Join(runtime, "io-state.sock") }

func (c *Config) Validate() error {
	if c.Mode != ModeOff && c.Mode != ModeLog && c.Mode != ModeAlert {
		return fmt.Errorf("mode: off, log или alert")
	}
	if err := c.validateIntervals(); err != nil {
		return err
	}
	if c.Ring.D() < time.Minute || c.Trigger <= 0 || c.Trigger > 1 {
		return errors.New("ring >= 1m, trigger в (0, 1]")
	}
	if c.Gap.D() < 0 || c.CaseMin.D() < 0 || c.Context.D() < 0 {
		return errors.New("gap, case_min и context не отрицательны")
	}
	if c.Context.D()+c.DetailIdle.D() >= c.Ring.D() {
		return errors.New("context + detail_idle должны быть меньше ring: виновники берутся из буфера")
	}
	if c.CasesKeep.D() <= 0 || c.Keep.D() <= 0 || c.QuietBytes < 0 {
		return errors.New("cases_keep и keep должны быть положительными, quiet_bytes — не отрицательным")
	}
	if c.Device == "" {
		return errors.New("device не должен быть пустым")
	}
	if err := c.validateRoute(); err != nil {
		return err
	}
	return c.validateLive()
}

func (c *Config) validateIntervals() error {
	step, detail, idle := c.Step.D(), c.Detail.D(), c.DetailIdle.D()
	if step < 50*time.Millisecond || detail < step || detail%step != 0 || idle < detail || idle%detail != 0 {
		return errors.New("step >= 50ms, detail >= step и кратен ему, detail_idle >= detail и кратен ему")
	}
	return nil
}

// validateRoute — пороги маршрута случаев и таблица хозяев служб.
func (c *Config) validateRoute() error {
	if c.AlertMin.D() <= 0 || c.AlertEvery.D() < 0 {
		return errors.New("alert_every не отрицателен, alert_min положителен")
	}
	if c.UrgentMin.D() < c.AlertMin.D() || c.SeriesN < 2 || c.SeriesWindow.D() <= 0 || c.MachineN < 0 ||
		c.MachineN > 0 && c.MachineOwners < 2 || c.CrossWindow.D() < 0 || c.Tk == "" {
		return errors.New("urgent_min >= alert_min, series_n >= 2, series_window > 0, machine_n >= 0 (machine_owners >= 2 при machine_n > 0), cross_window >= 0, tk не пуст")
	}
	for unit, owner := range c.Owners {
		if unit == "" || owner == "" {
			return fmt.Errorf("owners: пустая служба или хозяин (%q = %q)", unit, owner)
		}
	}
	return nil
}

// validateLive — ряд, профили и сокет моментального состояния.
func (c *Config) validateLive() error {
	if c.SeriesKeep.D() < time.Minute || c.SeriesKeep.D() > 7*24*time.Hour || c.ProfilesKeep.D() <= 0 {
		return errors.New("series_keep от 1m до 168h (ряд держится в памяти), profiles_keep положителен")
	}
	if c.Socket == "" {
		return errors.New("socket не должен быть пустым")
	}
	return nil
}

// Stalled — шаг с простоем: PSI full не меньше доли интервала. Частые шаги не
// уменьшают порог ниже номинального шага.
func Stalled(s Step, c Config, interval time.Duration) bool {
	interval = max(interval, c.Step.D())
	return Idle(s) >= int64(float64(interval.Microseconds())*c.Trigger)
}

// Idle — простой шага, мкс: больший из PSI full io и memory. Одна мера для случаев и окна `at`.
func Idle(s Step) int64 { return max(s.IOFullUS, s.MemFullUS) }

// AwaitMS — среднее ожидание одной операции устройства, мс.
func AwaitMS(ms, operations int64) int64 {
	if operations == 0 {
		return 0
	}
	return ms / operations
}
