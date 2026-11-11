package queue

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
)

// LiveAll в queue.live — все проекты с папкой задач под git_roots и dirs, кроме skip.
const LiveAll = "*"

type Config struct {
	Mode      string          `toml:"mode"`
	Live      []string        `toml:"live"`
	Interval  policy.Duration `toml:"interval"`
	FreeAfter policy.Duration `toml:"free_after"`
	BootGrace policy.Duration `toml:"boot_grace"`
	GitRoots  []string        `toml:"git_roots"`
	Dirs      []string        `toml:"dirs"`
	Skip      []string        `toml:"skip"`
	// RestCrit — папки, где в дни отдыха (суббота, воскресенье, праздники RestHolidays; дата — в поясе RestZone) из
	// новой работы идёт только CRIT: ворота, будильник и «HIGH не взят». Список — в конфиге машины.
	RestCrit     []string `toml:"rest_crit"`
	RestZone     string   `toml:"rest_zone"`
	RestHolidays string   `toml:"rest_holidays"`
	rest         Rest
	// Spaces — корень пространства и его ступень (Level); проект вне корней — по Level. Список — в конфиге машины.
	Spaces map[string]Level `toml:"spaces"`
	// Level — ступень вне корней Spaces; по умолчанию HIGH: нет слова пользователя — открыты только CRIT и HIGH.
	Level  Level  `toml:"level"`
	Frozen string `toml:"frozen"`
	Gate   string `toml:"gate"`
	// History — история лимитов Claude для aguard limit-reset-check; ворота её не читают.
	History string `toml:"history"`
	Tk      string `toml:"tk"`
}

func Default(home string) Config {
	expand := func(paths ...string) []string {
		out := make([]string, len(paths))
		for i, path := range paths {
			out[i] = policy.ExpandHome(path, home)
		}
		return out
	}
	paths := expand("~/git", "~/work", "~/docs", "~/.local/state/queue/gate.json", "~/.local/state/chooks/limits-history.jsonl", "~/bin/tk")
	return Config{Mode: ModeOn, Live: []string{"glint", "graft"}, Interval: policy.Duration(30 * time.Second), FreeAfter: policy.Duration(2 * time.Minute), BootGrace: policy.Duration(time.Hour),
		GitRoots: paths[:2], Dirs: paths[2:3], Gate: paths[3], History: paths[4], Tk: paths[5], Level: LevelHigh, Frozen: policy.ExpandHome("~/.config/queue/frozen", home)}
}

func (c *Config) Validate(home string) error {
	if c.Mode != "off" && c.Mode != ModeLog && c.Mode != ModeOn {
		return fmt.Errorf("queue.mode: off, log или on")
	}
	if c.Interval.D() <= 0 || c.FreeAfter.D() < 0 || c.BootGrace.D() < 0 {
		return fmt.Errorf("queue.interval > 0, queue.free_after и queue.boot_grace >= 0")
	}
	for _, ps := range [][]string{c.GitRoots, c.Dirs, c.Skip, c.RestCrit} {
		for i := range ps {
			ps[i] = policy.ExpandHome(ps[i], home)
		}
	}
	for i, root := range c.RestCrit {
		c.RestCrit[i] = filepath.Clean(root)
		if !filepath.IsAbs(c.RestCrit[i]) {
			return fmt.Errorf("queue.rest_crit[%s]: корень — полный путь или от ~", root)
		}
	}
	rest, err := NewRest(c.RestCrit, c.RestZone, c.RestHolidays)
	if err != nil {
		return err
	}
	c.rest = rest
	if _, err := ParseLevel(string(c.Level)); err != nil {
		return fmt.Errorf("queue.level: %w", err)
	}
	spaces := make(map[string]Level, len(c.Spaces))
	for root, level := range c.Spaces {
		if _, err := ParseLevel(string(level)); err != nil {
			return fmt.Errorf("queue.spaces[%s]: %w", root, err)
		}
		path := filepath.Clean(policy.ExpandHome(root, home))
		if !filepath.IsAbs(path) {
			return fmt.Errorf("queue.spaces[%s]: корень — полный путь или от ~", root)
		}
		if _, dup := spaces[path]; dup {
			return fmt.Errorf("queue.spaces: корень %s записан дважды", path)
		}
		spaces[path] = level
	}
	c.Spaces = spaces
	for _, p := range []*string{&c.Frozen, &c.Gate, &c.History, &c.Tk} {
		*p = policy.ExpandHome(*p, home)
	}
	for i := range c.Live {
		c.Live[i] = filepath.Base(filepath.Clean(c.Live[i]))
	}
	return nil
}

// Rest — дни отдыха после Validate; до него нулевое значение: дней отдыха нет.
func (c *Config) Rest() Rest { return c.rest }
