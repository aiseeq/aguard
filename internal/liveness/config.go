package liveness

import (
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"github.com/aiseeq/aguard/internal/bgtask"
	"github.com/aiseeq/aguard/internal/policy"
)

const (
	ModeOff   = bgtask.ModeOff
	ModeLog   = bgtask.ModeLog
	ModeAlert = bgtask.ModeAlert
)

type Watch struct {
	Name   string          `toml:"name"`
	Path   string          `toml:"path"`
	MaxAge policy.Duration `toml:"max_age"`
	Grace  policy.Duration `toml:"grace"`
	To     []string        `toml:"to"`
}

type Config struct {
	Mode     string          `toml:"mode"`
	Interval policy.Duration `toml:"interval"`
	Watch    []Watch         `toml:"watch"`
	Lead     string          `toml:"-"`
	SendAuth bool            `toml:"-"`
}

func Default() Config { return Config{Mode: ModeLog, Interval: policy.Duration(time.Minute)} }

func (c *Config) Validate(home string) error {
	if c.Mode != ModeOff && c.Mode != ModeLog && c.Mode != ModeAlert {
		return fmt.Errorf("liveness.mode = %q: off, log или alert", c.Mode)
	}
	if c.Interval.D() < 10*time.Second {
		return fmt.Errorf("liveness.interval >= 10s")
	}
	seen := map[string]bool{}
	for i := range c.Watch {
		w := &c.Watch[i]
		if w.Name == "" || seen[w.Name] {
			return fmt.Errorf("liveness.watch: пустое или повторное имя %q", w.Name)
		}
		seen[w.Name] = true
		if w.Path == "" || w.MaxAge.D() <= 0 || w.Grace.D() < 0 {
			return fmt.Errorf("liveness.watch.%s: path задан, max_age > 0 и grace >= 0", w.Name)
		}
		if w.Grace.D() == 0 {
			w.Grace = policy.Duration(2 * time.Minute)
		}
		if slices.Contains(w.To, "") {
			return fmt.Errorf("liveness.watch.%s.to: пустое имя сессии", w.Name)
		}
		w.Path = policy.ExpandHome(w.Path, home)
		if !filepath.IsAbs(w.Path) {
			return fmt.Errorf("liveness.watch.%s.path должен быть абсолютным", w.Name)
		}
	}
	return nil
}
