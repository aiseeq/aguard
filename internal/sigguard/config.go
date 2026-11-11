package sigguard

import (
	"fmt"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
)

const (
	ModeAlert = "alert"
	ModeLog   = "log"
)

type Config struct {
	Mode     string          `toml:"mode"`
	Window   policy.Duration `toml:"window"`
	Stale    policy.Duration `toml:"stale"`
	Lead     string          `toml:"-"`
	SendAuth bool            `toml:"-"`
}

func Default() Config {
	return Config{Mode: "alert", Window: policy.Duration(3 * time.Second), Stale: policy.Duration(10 * time.Minute)}
}
func (c *Config) Validate() error {
	if c.Mode != ModeAlert && c.Mode != ModeLog {
		return fmt.Errorf("mode = %q: ожидается alert или log", c.Mode)
	}
	if c.Window.D() <= 0 || c.Stale.D() <= 0 {
		return fmt.Errorf("window и stale должны быть положительными")
	}
	return nil
}
