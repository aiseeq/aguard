package queue

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
)

type Config struct {
	Mode      string          `toml:"mode"`
	Live      []string        `toml:"live"`
	Interval  policy.Duration `toml:"interval"`
	FreeAfter policy.Duration `toml:"free_after"`
	Morning   string          `toml:"morning"`
	GitRoots  []string        `toml:"git_roots"`
	Dirs      []string        `toml:"dirs"`
	Skip      []string        `toml:"skip"`
	// WeekendCrit — папки проектов, где в субботу и воскресенье из строк идут только CRIT. Список — в конфиге машины.
	WeekendCrit []string `toml:"weekend_crit"`
	// Spaces — корень пространства и сдвиг его планки в ступенях приоритета; вне корней планка общих ворот. Список — в конфиге машины.
	Spaces   map[string]int `toml:"spaces"`
	Table    string         `toml:"table"`
	Override string         `toml:"override"`
	Gate     string         `toml:"gate"`
	History  string         `toml:"history"`
	Tk       string         `toml:"tk"`
}

func Default(home string) Config {
	expand := func(paths ...string) []string {
		out := make([]string, len(paths))
		for i, path := range paths {
			out[i] = policy.ExpandHome(path, home)
		}
		return out
	}
	paths := expand("~/git", "~/work", "~/docs", "~/.config/queue/gate.tsv", "~/.config/queue/override.tsv", "~/.local/state/queue/gate.json", "~/.local/state/chooks/limits-history.jsonl", "~/bin/tk")
	return Config{Mode: ModeOn, Live: []string{"glint", "graft"}, Interval: policy.Duration(30 * time.Second), FreeAfter: policy.Duration(2 * time.Minute), Morning: "07:00",
		GitRoots: paths[:2], Dirs: paths[2:3], Table: paths[3], Override: paths[4], Gate: paths[5], History: paths[6], Tk: paths[7]}
}

func (c *Config) Validate(home string) error {
	if c.Mode != "off" && c.Mode != ModeLog && c.Mode != ModeOn {
		return fmt.Errorf("queue.mode: off, log или on")
	}
	if c.Interval.D() <= 0 || c.FreeAfter.D() < 0 {
		return fmt.Errorf("queue.interval > 0, queue.free_after >= 0")
	}
	if _, err := time.Parse("15:04", c.Morning); err != nil {
		return fmt.Errorf("queue.morning: %w", err)
	}
	for _, ps := range [][]string{c.GitRoots, c.Dirs, c.Skip, c.WeekendCrit} {
		for i := range ps {
			ps[i] = policy.ExpandHome(ps[i], home)
		}
	}
	for i := range c.WeekendCrit {
		c.WeekendCrit[i] = filepath.Clean(c.WeekendCrit[i])
	}
	spaces := make(map[string]int, len(c.Spaces))
	for root, shift := range c.Spaces {
		if shift < 0 || shift > MaxSpaceShift {
			return fmt.Errorf("queue.spaces[%s]: сдвиг 0..%d, а не %d", root, MaxSpaceShift, shift)
		}
		path := filepath.Clean(policy.ExpandHome(root, home))
		if !filepath.IsAbs(path) {
			return fmt.Errorf("queue.spaces[%s]: корень — полный путь или от ~", root)
		}
		if _, dup := spaces[path]; dup {
			return fmt.Errorf("queue.spaces: корень %s записан дважды", path)
		}
		spaces[path] = shift
	}
	c.Spaces = spaces
	for _, p := range []*string{&c.Table, &c.Override, &c.Gate, &c.History, &c.Tk} {
		*p = policy.ExpandHome(*p, home)
	}
	for i := range c.Live {
		c.Live[i] = filepath.Base(filepath.Clean(c.Live[i]))
	}
	return nil
}

func (c *Config) MorningOffset() (time.Duration, error) {
	t, err := time.Parse("15:04", c.Morning)
	if err != nil {
		return 0, fmt.Errorf("queue.morning: %w", err)
	}
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute, nil
}
