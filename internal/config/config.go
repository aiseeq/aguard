// Package config — единый конфиг aguard (~/.config/aguard/config.toml):
// общие поля (лид, админы пространств) и разделы наблюдателей [orphans], [tmp],
// [memory], [restart]. Перечитывается каждый проход.
package config

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/aiseeq/aguard/internal/memwatch"
	"github.com/aiseeq/aguard/internal/orphans"
	"github.com/aiseeq/aguard/internal/policy"
	"github.com/aiseeq/aguard/internal/restart"
)

// Config — весь конфиг.
type Config struct {
	Lead     string            `toml:"lead"`      // имя сессии лида
	SendAuth bool              `toml:"send_auth"` // слать peerToken первой строкой
	Admins   map[string]string `toml:"admins"`    // префикс cwd сессии -> имя админа пространства

	Orphans orphans.Config  `toml:"orphans"` // процессы умерших сессий (бывший procguard)
	Tmp     policy.Config   `toml:"tmp"`     // записи в /tmp (бывший tmpguard)
	Memory  memwatch.Config `toml:"memory"`  // пики памяти сессий
	Restart restart.Config  `toml:"restart"` // можно ли cl перезапустить сессию
}

// Path — путь конфига в домашней папке.
func Path(home string) string { return filepath.Join(home, ".config", "aguard", "config.toml") }

// Default — значения по умолчанию.
func Default(home string) Config {
	return Config{
		Lead: "lead",
		Admins: map[string]string{
			filepath.Join(home, "work"): "work-admin",
			filepath.Join(home, "git"):  "git-admin",
		},
		Orphans: orphans.Default(home),
		Tmp:     policy.Default(home),
		Memory:  memwatch.Default(),
		Restart: restart.Default(home, runtimeDir()),
	}
}

// Load читает конфиг поверх значений по умолчанию; нет файла — значения по
// умолчанию. Неизвестный ключ — ошибка: опечатка не должна молча менять режим.
func Load(path, home string, uid int) (Config, error) {
	c := Default(home)
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return c, fmt.Errorf("конфиг: %w", err)
	}
	if err == nil {
		admins := c.Admins
		c.Admins = nil // [admins] в файле заменяет список по умолчанию, а не дополняет его
		md, err := toml.Decode(string(b), &c)
		if c.Admins == nil {
			c.Admins = admins
		}
		if err != nil {
			return c, fmt.Errorf("%s: %w", path, err)
		}
		if und := md.Undecoded(); len(und) > 0 {
			keys := make([]string, len(und))
			for i, k := range und {
				keys[i] = k.String()
			}
			return c, fmt.Errorf("%s: неизвестные ключи: %s", path, strings.Join(keys, ", "))
		}
	}
	return c, c.finish(home, uid)
}

// finish раздаёт общие поля разделам и проверяет их.
func (c *Config) finish(home string, uid int) error {
	admins := make(map[string]string, len(c.Admins))
	for k, v := range c.Admins {
		admins[policy.ExpandHome(k, home)] = v
	}
	c.Admins = admins
	c.Orphans.Lead, c.Orphans.SendAuth, c.Orphans.Admins = c.Lead, c.SendAuth, maps.Clone(admins)
	c.Tmp.Lead, c.Tmp.SendAuth, c.Tmp.Escalation.Admins = c.Lead, c.SendAuth, maps.Clone(admins)
	if err := c.Orphans.Compile(home); err != nil {
		return fmt.Errorf("orphans: %w", err)
	}
	if err := c.Tmp.Validate(home); err != nil {
		return fmt.Errorf("tmp: %w", err)
	}
	if err := c.Memory.Validate(uid); err != nil {
		return fmt.Errorf("memory: %w", err)
	}
	if err := c.Restart.Validate(home); err != nil {
		return err
	}
	return nil
}

// runtimeDir — $XDG_RUNTIME_DIR; без него (запуск не из сеанса) — /run/user/<uid>, как его ставит systemd.
func runtimeDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return d
	}
	return filepath.Join("/run/user", strconv.Itoa(os.Getuid()))
}
