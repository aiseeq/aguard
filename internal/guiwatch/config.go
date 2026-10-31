package guiwatch

import (
	"errors"
	"fmt"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
)

const (
	ModeOff = "off"
	ModeLog = "log"
)

type Config struct {
	Mode          string          `toml:"mode"`
	Interval      policy.Duration `toml:"interval"`
	RendererEvery policy.Duration `toml:"renderer_every"`
	Rediscover    policy.Duration `toml:"rediscover"`
	MinStall      policy.Duration `toml:"min_stall"`
	Gap           policy.Duration `toml:"gap"`
	Context       policy.Duration `toml:"context"`
	Ring          policy.Duration `toml:"ring"`
	Keep          policy.Duration `toml:"keep"`
	MaxPerHour    int             `toml:"max_per_hour"`
	MaxBytes      int64           `toml:"max_bytes"`
}

func Default() Config {
	return Config{Mode: ModeLog, Interval: policy.Duration(50 * time.Millisecond), RendererEvery: policy.Duration(250 * time.Millisecond), Rediscover: policy.Duration(5 * time.Second), MinStall: policy.Duration(time.Second), Gap: policy.Duration(300 * time.Millisecond), Context: policy.Duration(5 * time.Second), Ring: policy.Duration(30 * time.Second), Keep: policy.Duration(14 * 24 * time.Hour), MaxPerHour: 60, MaxBytes: 200 << 20}
}

func (c *Config) Validate() error {
	if c.Mode != ModeOff && c.Mode != ModeLog {
		return fmt.Errorf("gui.mode: off или log")
	}
	if c.Interval.D() <= 0 || c.RendererEvery.D() <= 0 || c.Rediscover.D() <= 0 || c.MinStall.D() <= 0 || c.Gap.D() < 0 || c.Context.D() < 0 || c.Ring.D() < c.MinStall.D()+c.Context.D() || c.Keep.D() <= 0 || c.MaxPerHour < 1 || c.MaxBytes < 1 {
		return errors.New("gui: интервалы, ring, keep, max_per_hour и max_bytes должны быть положительными; gap/context могут быть нулём")
	}
	return nil
}
