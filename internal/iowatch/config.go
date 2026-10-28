// Package iowatch — телеметрия диска, свопа и PSI для наблюдателя io: быстрые шаги машины и
// детальные записи по cgroup и процессам (замер), кольцевой буфер и снимки фризов (событие и
// снимок), чтение снимков по окну, секунды и фризы (разбор).
package iowatch

import (
	"errors"
	"fmt"
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
	Mode           string          `toml:"mode"`            // off | log (снимки и разбор, письма — в журнал) | alert
	Step           policy.Duration `toml:"step"`            // быстрый шаг: PSI, vmstat, diskstats
	Detail         policy.Duration `toml:"detail"`          // детальная запись: cgroup и процессы
	Ring           policy.Duration `toml:"ring"`            // сколько держать в памяти
	Trigger        float64         `toml:"trigger"`         // доля шага простоя PSI full, открывающая событие
	Calm           policy.Duration `toml:"calm"`            // событие закрывается после стольких спокойных шагов подряд
	Keep           policy.Duration `toml:"keep"`            // срок файлов снимков
	AnalyzeMin     policy.Duration `toml:"analyze_min"`     // простой события, с которого зовётся разбор
	AnalyzeEvery   policy.Duration `toml:"analyze_every"`   // разбор не чаще; события в паузе копятся
	AnalyzeTimeout policy.Duration `toml:"analyze_timeout"` // предел команды разбора
	Device         string          `toml:"device"`          // диск в /proc/diskstats и /sys/block
	AwaitTrigger   policy.Duration `toml:"await_trigger"`   // среднее ожидание операции устройства, открывающее событие
	Analyze        []string        `toml:"analyze"`         // команда разбора; {prompt} — текст
	Lead           string          `toml:"-"`
	SendAuth       bool            `toml:"-"`
}

func Default() Config {
	return Config{
		Mode:           ModeAlert,
		Step:           policy.Duration(100 * time.Millisecond),
		Detail:         policy.Duration(time.Second),
		Ring:           policy.Duration(5 * time.Minute),
		Trigger:        .5,
		Calm:           policy.Duration(30 * time.Second),
		Keep:           policy.Duration(720 * time.Hour),
		AnalyzeMin:     policy.Duration(300 * time.Millisecond),
		AnalyzeEvery:   policy.Duration(15 * time.Minute),
		AnalyzeTimeout: policy.Duration(10 * time.Minute),
		Device:         "nvme0n1",
		AwaitTrigger:   policy.Duration(20 * time.Millisecond),
		Analyze:        []string{"worker", "ask", "--model", "luna", "--class", "разбор-данных", "--msg", "{prompt}"},
	}
}

func (c *Config) Validate() error {
	if c.Mode != ModeOff && c.Mode != ModeLog && c.Mode != ModeAlert {
		return fmt.Errorf("mode: off, log или alert")
	}
	step, detail := c.Step.D(), c.Detail.D()
	if step < 50*time.Millisecond || detail < step || detail%step != 0 || c.Ring.D() < time.Minute || c.Trigger <= 0 || c.Trigger > 1 {
		return errors.New("step >= 50ms, detail >= step и кратен ему, ring >= 1m, trigger в (0, 1]")
	}
	if c.Calm.D() <= 0 || c.Keep.D() <= 0 || c.AnalyzeTimeout.D() <= 0 || c.AwaitTrigger.D() <= 0 || c.AnalyzeEvery.D() < 0 {
		return errors.New("calm, keep, analyze_timeout и await_trigger должны быть положительными, analyze_every — не отрицательным")
	}
	if c.Device == "" || len(c.Analyze) == 0 {
		return errors.New("device и analyze не должны быть пустыми")
	}
	return nil
}

// ShouldTrigger — шаг открывает или продлевает событие: простой PSI full не меньше доли шага
// или заметное среднее ожидание операций устройства.
func ShouldTrigger(s Step, c Config) bool {
	threshold := int64(float64(c.Step.D().Microseconds()) * c.Trigger)
	return Idle(s) >= threshold ||
		awaitExceeded(s.WMS, s.WIOS, c.AwaitTrigger.D()) || awaitExceeded(s.RMS, s.RIOS, c.AwaitTrigger.D())
}

func awaitExceeded(ms, operations int64, threshold time.Duration) bool {
	return operations >= 10 && time.Duration(ms)*time.Millisecond >= threshold*time.Duration(operations)
}

// Idle — простой шага, мкс: больший из PSI full io и memory. Одна мера для событий, freezes и разбора;
// ожидание устройства событие открывает, но в простой не входит.
func Idle(s Step) int64 { return max(s.IOFullUS, s.MemFullUS) }

// AwaitMS — среднее ожидание одной операции устройства, мс.
func AwaitMS(ms, operations int64) int64 {
	if operations == 0 {
		return 0
	}
	return ms / operations
}
