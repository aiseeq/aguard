// Package orphans — логика наблюдателя orphans (бывший procguard): процессы, оставшиеся от умерших
// сессий Claude Code (CLAUDE_PID в окружении указывает на сессию, которой
// больше нет), выбор, что снимать, и само снятие.
package orphans

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
)

// Режимы работы.
const (
	ModeObserve = "observe" // ничего не снимает и не шлёт: журнал «снял бы»
	ModeAct     = "act"     // снимает и шлёт сводки
)

// Config — раздел [orphans] конфига aguard. Lead, SendAuth и Admins — общие
// для всего конфига, их подставляет загрузчик aguard.
type Config struct {
	Mode      string            `toml:"mode"`
	Interval  policy.Duration   `toml:"interval"`
	MinDead   policy.Duration   `toml:"min_dead"`   // сколько процесс должен прожить после смерти сессии
	TermWait  policy.Duration   `toml:"term_wait"`  // от SIGTERM до SIGKILL (и -t у podman stop)
	MuteUntil map[string]string `toml:"mute_until"` // префикс cwd -> дата, до которой сводок нет (только журнал)
	Exclude   []Exclude         `toml:"exclude"`
	Lead      string            `toml:"-"` // имя сессии лида
	SendAuth  bool              `toml:"-"` // слать peerToken первой строкой
	Admins    map[string]string `toml:"-"` // префикс cwd сессии -> имя админа пространства
}

// Exclude — исключение: все заданные поля должны совпасть с процессом дерева.
type Exclude struct {
	Comm    string `toml:"comm"`    // регулярное выражение по comm, целиком
	Args    string `toml:"args"`    // регулярное выражение по командной строке (поиск)
	Cwd     string `toml:"cwd"`     // префикс cwd процесса
	Session string `toml:"session"` // CLAUDE_CODE_SESSION_ID
	Until   string `toml:"until"`   // после этой даты исключение не действует
	Reason  string `toml:"reason"`

	comm, args *regexp.Regexp
	until      time.Time
}

const (
	defaultInterval = 5 * time.Minute
	defaultMinDead  = time.Hour
	defaultTermWait = 30 * time.Second
)

// Default — значения по умолчанию.
func Default(home string) Config {
	return Config{
		Mode:     ModeObserve,
		Interval: policy.Duration(defaultInterval),
		MinDead:  policy.Duration(defaultMinDead),
		TermWait: policy.Duration(defaultTermWait),
		Lead:     "lead",
		Admins: map[string]string{
			filepath.Join(home, "work"): "work-admin",
			filepath.Join(home, "git"):  "git-admin",
		},
	}
}

// Compile проверяет раздел, раскрывает ~ в путях и компилирует исключения.
func (c *Config) Compile(home string) error {
	if c.Mode != ModeObserve && c.Mode != ModeAct {
		return fmt.Errorf("mode = %q: ожидается observe или act", c.Mode)
	}
	if c.Interval.D() < time.Minute {
		return fmt.Errorf("interval = %v: не чаще раза в минуту", c.Interval.D())
	}
	if c.MinDead.D() < time.Minute || c.TermWait.D() < time.Second {
		return errors.New("min_dead >= 1m и term_wait >= 1s")
	}
	c.Admins = expandKeys(c.Admins, home)
	c.MuteUntil = expandKeys(c.MuteUntil, home)
	for k, v := range c.MuteUntil {
		if _, err := ParseUntil(v); err != nil {
			return fmt.Errorf("orphans.mute_until %q: %w", k, err)
		}
	}
	for i := range c.Exclude {
		if err := c.Exclude[i].compile(home); err != nil {
			return fmt.Errorf("orphans.exclude[%d]: %w", i, err)
		}
	}
	return nil
}

func (e *Exclude) compile(home string) error {
	if e.Comm == "" && e.Args == "" && e.Cwd == "" && e.Session == "" {
		return errors.New("нужно хотя бы одно из comm, args, cwd, session")
	}
	var err error
	if e.Comm != "" {
		if e.comm, err = regexp.Compile("^(?:" + e.Comm + ")$"); err != nil {
			return fmt.Errorf("comm: %w", err)
		}
	}
	if e.Args != "" {
		if e.args, err = regexp.Compile(e.Args); err != nil {
			return fmt.Errorf("args: %w", err)
		}
	}
	e.Cwd = policy.ExpandHome(e.Cwd, home)
	if e.Until != "" {
		if e.until, err = ParseUntil(e.Until); err != nil {
			return fmt.Errorf("until: %w", err)
		}
	}
	return nil
}

// ParseUntil — дата "2006-01-02" (начало дня по местному времени) или RFC3339.
func ParseUntil(s string) (time.Time, error) {
	if t, err := time.ParseInLocation(time.DateOnly, s, time.Local); err == nil {
		return t, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return t, fmt.Errorf("%q: ожидается 2006-01-02 или RFC3339", s)
	}
	return t, nil
}

// matches — исключение действует сейчас и все его поля совпали с p.
func (e *Exclude) matches(p *Proc, now time.Time) bool {
	if !e.until.IsZero() && !now.Before(e.until) {
		return false
	}
	if e.comm != nil && !e.comm.MatchString(p.Comm) {
		return false
	}
	if e.args != nil && !e.args.MatchString(p.Args) {
		return false
	}
	if e.Cwd != "" && !underPath(p.Cwd, e.Cwd) {
		return false
	}
	return e.Session == "" || e.Session == p.SessionID
}

// excluded — первое действующее исключение, под которое попал p.
func (c *Config) excluded(p *Proc, now time.Time) *Exclude {
	for i := range c.Exclude {
		if c.Exclude[i].matches(p, now) {
			return &c.Exclude[i]
		}
	}
	return nil
}

func expandKeys(m map[string]string, home string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[policy.ExpandHome(k, home)] = v
	}
	return out
}

// underPath — p совпадает с dir или лежит внутри.
func underPath(p, dir string) bool {
	dir = strings.TrimSuffix(dir, "/")
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// LongestPrefix — значение по самому длинному префиксу-папке, под которым лежит p.
func LongestPrefix(m map[string]string, p string) (key, val string) {
	for k, v := range m {
		if underPath(p, k) && (len(k) > len(key) || len(k) == len(key) && k < key) {
			key, val = k, v
		}
	}
	return key, val
}
