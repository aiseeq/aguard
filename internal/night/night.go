// Package night проверяет, что Сон, Дайджест и поднятие после окна доверия начались вовремя.
package night

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/bgtask"
	"github.com/aiseeq/aguard/internal/cllog"
	"github.com/aiseeq/aguard/internal/policy"
)

// Config — раздел [night] конфига aguard.
type Config struct {
	Mode         string          `toml:"mode"`
	Interval     policy.Duration `toml:"interval"`
	DreamBy      string          `toml:"dream_by"`
	DreamMarker  string          `toml:"dream_marker"`
	DigestBy     string          `toml:"digest_by"`
	DigestMarker string          `toml:"digest_marker"`
	TrustWait    policy.Duration `toml:"trust_wait"`
	AlsoTo       []string        `toml:"also_to"` // кроме лида: сигнал «лид не поднялся» лиду же не поможет
	// DreamTo — хозяева ночного прогона: сигнал о Сне идёт и им (лид только шлёт таймер; Сон ведёт dream-master,
	// ~/git/dream/CLAUDE.md, «Хозяин»).
	DreamTo  []string `toml:"dream_to"`
	Lead     string   `toml:"-"`
	SendAuth bool     `toml:"-"`
}

const (
	ModeOff   = bgtask.ModeOff
	ModeLog   = bgtask.ModeLog
	ModeAlert = bgtask.ModeAlert
)

// Default returns ночной сторож по умолчанию.
func Default(home string) Config {
	return Config{Mode: ModeAlert, Interval: policy.Duration(time.Minute), DreamBy: "03:00",
		DreamMarker: filepath.Join(home, ".claude/improve/runs/{date}/started"), DigestBy: "07:00",
		DigestMarker: filepath.Join(home, ".local/state/digest/started"), TrustWait: policy.Duration(5 * time.Minute),
		AlsoTo: []string{"git-admin"}, DreamTo: []string{"dream-master"}}
}

// Recipients — лид и also_to без повторов, лид первым.
func (c Config) Recipients() []string {
	out := []string{c.Lead}
	for _, to := range c.AlsoTo {
		if !slices.Contains(out, to) {
			out = append(out, to)
		}
	}
	return out
}

// Validate проверяет настройки и раскрывает домашнюю папку в путях.
func (c *Config) Validate(home string) error {
	if c.Mode != ModeOff && c.Mode != ModeLog && c.Mode != ModeAlert {
		return fmt.Errorf("night.mode = %q: off, log или alert", c.Mode)
	}
	if c.Interval.D() < 10*time.Second || c.TrustWait.D() < 0 {
		return fmt.Errorf("night.interval >= 10s, night.trust_wait >= 0")
	}
	if !validClock(c.DreamBy) || !validClock(c.DigestBy) {
		return fmt.Errorf("night.dream_by и night.digest_by должны быть ЧЧ:ММ")
	}
	if slices.Contains(c.AlsoTo, "") || slices.Contains(c.DreamTo, "") {
		return fmt.Errorf("night.also_to, night.dream_to: пустое имя сессии")
	}
	c.DreamMarker = policy.ExpandHome(c.DreamMarker, home)
	c.DigestMarker = policy.ExpandHome(c.DigestMarker, home)
	return nil
}

func validClock(s string) bool { _, err := time.Parse("15:04", s); return err == nil }

// Facts — уже прочитанные наблюдателем данные, пригодные для чистой проверки.
type Facts struct {
	DreamMarker bool
	DigestText  string
	TrustEvents []cllog.TrustEvent
}

// Signal — срабатывание с ключом дедупликации и текстом для человека; To — адресаты сверх Recipients.
type Signal struct {
	Key, Text string
	To        []string
}

// Check возвращает события, срок которых наступил для местной даты now.
func Check(now time.Time, cfg Config, facts Facts) []Signal {
	date := now.Format("2006-01-02")
	var out []Signal
	if due(now, cfg.DreamBy) && !facts.DreamMarker {
		out = append(out, Signal{Key: "dream:" + date, To: cfg.DreamTo, Text: fmt.Sprintf("Сон не стартовал к %s: нет %s. "+
			"Звенья: таймер Сна у %s (CronList; окно доверия, лимит, упавшая консоль) и сессия %s. "+
			"%s: запусти Сон сам по скиллу dream.", cfg.DreamBy, DreamPath(cfg, now), cfg.Lead, strings.Join(cfg.DreamTo, ", "),
			strings.Join(cfg.DreamTo, ", "))})
	}
	if due(now, cfg.DigestBy) && !strings.Contains(facts.DigestText, date) {
		out = append(out, Signal{Key: "digest:" + date, Text: fmt.Sprintf("Дайджест не стартовал к %s: нет сегодняшней даты в %s. "+
			"Звено: таймер Дайджеста у %s (CronList; окно доверия, лимит, упавшая консоль). %s: запусти Дайджест по скиллу digest.",
			cfg.DigestBy, cfg.DigestMarker, cfg.Lead, cfg.Lead)})
	}
	for _, e := range facts.TrustEvents {
		if now.Sub(e.At) > 24*time.Hour || now.Sub(e.At) < cfg.TrustWait.D() || !e.Recovered.IsZero() {
			continue
		}
		key := "trust:" + e.Name + ":" + e.At.Format(time.RFC3339)
		text := fmt.Sprintf("%s стоит на окне доверия к %s с %s — подтвердить в его консоли: стрелка вниз, Enter", e.Name, e.Dir, e.At.Format("15:04"))
		if e.Confirmed {
			text = fmt.Sprintf("%s не поднялась за %s после окна доверия к %s: cl подтвердил его в %s — посмотреть её консоль", e.Name, cfg.TrustWait.D(), e.Dir, e.At.Format("15:04"))
		}
		out = append(out, Signal{Key: key, Text: text})
	}
	return out
}

// DreamPath — маркер старта Сна за местную дату now.
func DreamPath(cfg Config, now time.Time) string {
	return strings.ReplaceAll(cfg.DreamMarker, "{date}", now.Format("2006-01-02"))
}

// due — местное время now не раньше clock (ЧЧ:ММ, проверено Validate) того же дня.
func due(now time.Time, clock string) bool {
	t, err := time.Parse("15:04", clock)
	if err != nil {
		panic(fmt.Sprintf("night: время %q прошло Validate: %v", clock, err))
	}
	return now.Hour()*60+now.Minute() >= t.Hour()*60+t.Minute()
}
