// Package policy — чистая логика наблюдателя tmp (бывший tmpguard): конфиг, классы
// создателей, учёт «размер × время» по владельцам, адресат и эскалация,
// сборка текста сводки.
package policy

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/record"
)

// Режимы работы.
const (
	ModeObserve = "observe" // ничего не шлёт, только считает и пишет, что отправил бы
	ModeAlert   = "alert"   // шлёт сводки в сессии
)

// Size — размер в конфиге: "500K", "5MB", "1G" (двоичные единицы).
type Size int64

// UnmarshalText разбирает размер с суффиксом.
func (s *Size) UnmarshalText(b []byte) error {
	v, err := ParseSize(string(b))
	*s = Size(v)
	return err
}

// ParseSize: число с необязательным суффиксом K/M/G/T (B и iB допустимы).
func ParseSize(str string) (int64, error) {
	t := strings.ToUpper(strings.TrimSpace(str))
	t = strings.TrimSuffix(strings.TrimSuffix(t, "B"), "I")
	mult := int64(1)
	if t != "" {
		switch t[len(t)-1] {
		case 'K':
			mult = 1 << 10
		case 'M':
			mult = 1 << 20
		case 'G':
			mult = 1 << 30
		case 'T':
			mult = 1 << 40
		}
		if mult > 1 {
			t = t[:len(t)-1]
		}
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("размер %q: ожидается число с K/M/G", str)
	}
	return int64(f * float64(mult)), nil
}

// Duration — длительность в конфиге ("5m", "72h").
type Duration time.Duration

// UnmarshalText разбирает длительность Go.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	*d = Duration(v)
	return err
}

// D — значение как time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Config — раздел [tmp] конфига aguard. Lead, SendAuth и Escalation.Admins —
// общие для всего конфига, их подставляет загрузчик aguard.
type Config struct {
	Mode        string   `toml:"mode"`
	Interval    Duration `toml:"interval"`
	TrackDir    string   `toml:"track_state_dir"` // owners.jsonl и долговременный снимок
	RunDir      string   `toml:"track_run_dir"`   // частый снимок state.json
	TmpfilesDir string   `toml:"-"`               // пользовательские правила systemd-tmpfiles
	Lead        string   `toml:"-"`               // имя сессии лида
	Fighters    []string `toml:"fighters"`        // comm бойцов в цепочке до сессии
	Ignore      []string `toml:"ignore"`          // шаблоны путей, которые не учитываются
	SendAuth    bool     `toml:"-"`               // слать токен peerToken первой строкой

	Policy     LimitsConfig     `toml:"policy"`
	Pressure   PressureConfig   `toml:"pressure"`
	Escalation EscalationConfig `toml:"escalation"`
	Cache      CacheConfig      `toml:"cache"`
}

// LimitsConfig — пороги учёта.
type LimitsConfig struct {
	MinEntry  Size    `toml:"min_entry"`     // записи меньше не учитываются вовсе
	MinOwner  Size    `toml:"min_owner"`     // владелец с суммой меньше не получает сводку
	BudgetGBM float64 `toml:"budget_gb_min"` // порог «ГБ × минуты» на владельца
	MaxListed int     `toml:"max_listed"`    // строк в одной сводке
}

// PressureConfig — когда память под давлением, порог умножается на Factor.
type PressureConfig struct {
	PSISomeAvg60 float64 `toml:"psi_some_avg60"` // /proc/pressure/memory, %
	SwapUsedPct  float64 `toml:"swap_used_pct"`  // занятый своп, % от размера
	Factor       float64 `toml:"factor"`
}

// EscalationConfig — лестница адресатов.
type EscalationConfig struct {
	After     int               `toml:"after_summaries"` // не убрано за столько сводок — ступень выше
	TopRepeat Duration          `toml:"top_repeat"`      // как часто повторять верхней ступени
	Admins    map[string]string `toml:"-"`               // префикс cwd сессии -> имя админа пространства
}

// CacheConfig — общие кеши без владельца.
type CacheConfig struct {
	Paths      []string `toml:"paths"`
	Max        Size     `toml:"max"`         // при превышении — чистка старого содержимого
	CleanOlder Duration `toml:"clean_older"` // что считать старым
	Clean      bool     `toml:"clean"`       // чистить в режиме alert
}

// Default — начальные значения. Порог 12 ГБ·мин: 1 ГБ на 12 минут
// ≈ 100 МБ на 2 часа ≈ 10 МБ на сутки.
func Default(home string) Config {
	return Config{
		Mode:        ModeObserve,
		Interval:    Duration(5 * time.Minute),
		TrackDir:    record.TrackStateDir(home, "track"),
		RunDir:      record.TrackRunDir("track"),
		TmpfilesDir: filepath.Join(home, ".config", "user-tmpfiles.d"),
		Lead:        "lead",
		Fighters:    []string{"opencode", "opencode.exe", "codex", "claude"},
		Ignore:      []string{"/tmp/.X11-unix", "/tmp/.ICE-unix", "/tmp/.font-unix", "/tmp/cc-socks"},
		Policy:      LimitsConfig{MinEntry: 1 << 20, MinOwner: 5 << 20, BudgetGBM: 12, MaxListed: 15},
		Pressure:    PressureConfig{PSISomeAvg60: 5, SwapUsedPct: 50, Factor: 0.25},
		Escalation: EscalationConfig{
			After: 2, TopRepeat: Duration(30 * time.Minute),
			Admins: map[string]string{filepath.Join(home, "work"): "work-admin", filepath.Join(home, "git"): "git-admin"},
		},
		Cache: CacheConfig{
			Paths:      []string{"/tmp/.cache", "/tmp/node-compile-cache", "/tmp/playwright-transform-cache-*", "/tmp/jest_rs"},
			Max:        1 << 30,
			CleanOlder: Duration(72 * time.Hour),
			Clean:      true,
		},
	}
}

// Validate проверяет раздел и раскрывает ~ в префиксах админов.
func (c *Config) Validate(home string) error {
	admins := map[string]string{}
	for k, v := range c.Escalation.Admins {
		admins[ExpandHome(k, home)] = v
	}
	c.Escalation.Admins = admins
	return c.validate()
}

// ExpandHome раскрывает ~ и ~/ в начале пути.
func ExpandHome(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

func (c *Config) validate() error {
	if c.Mode != ModeObserve && c.Mode != ModeAlert {
		return fmt.Errorf("mode = %q: ожидается observe или alert", c.Mode)
	}
	for _, p := range append(slices.Clone(c.Ignore), c.Cache.Paths...) {
		if _, err := filepath.Match(p, ""); err != nil {
			return fmt.Errorf("шаблон пути %q: %w", p, err)
		}
	}
	if c.Interval.D() < time.Minute {
		return fmt.Errorf("interval = %v: не чаще раза в минуту", c.Interval.D())
	}
	if c.Escalation.After < 1 {
		return errors.New("escalation.after_summaries должно быть >= 1")
	}
	if c.Policy.BudgetGBM <= 0 || c.Pressure.Factor <= 0 || c.Pressure.Factor > 1 {
		return errors.New("policy.budget_gb_min > 0 и 0 < pressure.factor <= 1")
	}
	if c.Policy.MaxListed < 1 {
		c.Policy.MaxListed = 15
	}
	return nil
}
