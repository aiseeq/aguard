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
	Ring       policy.Duration `toml:"ring"`        // сколько сырых записей держать в памяти
	Trigger    float64         `toml:"trigger"`     // доля шага простоя PSI full: шаг с простоем
	Gap        policy.Duration `toml:"gap"`         // спокойный промежуток до стольких не делит случай
	CaseMin    policy.Duration `toml:"case_min"`    // случай от такой длительности — с виновниками
	Context    policy.Duration `toml:"context"`     // виновники ищутся и за столько до начала случая
	AlertMin   policy.Duration `toml:"alert_min"`   // случай от такой длительности — письмо лиду
	AlertEvery policy.Duration `toml:"alert_every"` // письмо не чаще; случаи в паузе склеиваются в следующее
	QuietBytes int64           `toml:"quiet_bytes"` // обмен с диском меньше — случай тихий (простой без трафика)
	CasesKeep  policy.Duration `toml:"cases_keep"`  // срок сводок случаев
	Keep       policy.Duration `toml:"keep"`        // срок файлов ручных снимков буфера
	Device     string          `toml:"device"`      // диск в /proc/diskstats и /sys/block
	// Посекундный ряд машины и профили завершившихся cgroup — в памяти службы; файлы — их копия при
	// остановке и по SIGUSR1 (профили — и раз в час, как сводки).
	SeriesKeep   policy.Duration `toml:"series_keep"`   // срок посекундного ряда в памяти
	ProfilesKeep policy.Duration `toml:"profiles_keep"` // срок профилей cgroup
	// Socket — сокет моментального состояния диска (запросы now, series, profiles); смена — с перезапуском
	// службы. По умолчанию $XDG_RUNTIME_DIR/io-state.sock (DefaultSocket).
	Socket   string `toml:"socket"`
	Lead     string `toml:"-"`
	SendAuth bool   `toml:"-"`
}

func Default() Config {
	return Config{
		Mode:         ModeAlert,
		Step:         policy.Duration(100 * time.Millisecond),
		Detail:       policy.Duration(time.Second),
		Ring:         policy.Duration(5 * time.Minute),
		Trigger:      .5,
		Gap:          policy.Duration(300 * time.Millisecond),
		CaseMin:      policy.Duration(500 * time.Millisecond),
		Context:      policy.Duration(5 * time.Second),
		AlertMin:     policy.Duration(2 * time.Second),
		AlertEvery:   policy.Duration(15 * time.Minute),
		QuietBytes:   1 << 20,
		CasesKeep:    policy.Duration(14 * 24 * time.Hour),
		Keep:         policy.Duration(7 * 24 * time.Hour),
		Device:       "nvme0n1",
		SeriesKeep:   policy.Duration(24 * time.Hour),
		ProfilesKeep: policy.Duration(30 * 24 * time.Hour),
	}
}

// DefaultSocket — сокет моментального состояния в папке runtime ($XDG_RUNTIME_DIR). Имя без aguard:
// клиент (akit) знает нейтральный контракт «источник моментального состояния диска», а не службу.
func DefaultSocket(runtime string) string { return filepath.Join(runtime, "io-state.sock") }

func (c *Config) Validate() error {
	if c.Mode != ModeOff && c.Mode != ModeLog && c.Mode != ModeAlert {
		return fmt.Errorf("mode: off, log или alert")
	}
	step, detail := c.Step.D(), c.Detail.D()
	if step < 50*time.Millisecond || detail < step || detail%step != 0 || c.Ring.D() < time.Minute || c.Trigger <= 0 || c.Trigger > 1 {
		return errors.New("step >= 50ms, detail >= step и кратен ему, ring >= 1m, trigger в (0, 1]")
	}
	if c.Gap.D() < 0 || c.CaseMin.D() < 0 || c.Context.D() < 0 || c.AlertMin.D() <= 0 || c.AlertEvery.D() < 0 {
		return errors.New("gap, case_min, context и alert_every не отрицательны, alert_min положителен")
	}
	if c.Context.D()+c.Detail.D() >= c.Ring.D() {
		return errors.New("context + detail должны быть меньше ring: виновники берутся из буфера")
	}
	if c.CasesKeep.D() <= 0 || c.Keep.D() <= 0 || c.QuietBytes < 0 {
		return errors.New("cases_keep и keep должны быть положительными, quiet_bytes — не отрицательным")
	}
	if c.Device == "" {
		return errors.New("device не должен быть пустым")
	}
	return c.validateLive()
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
