// Package config — единый конфиг aguard (~/.config/aguard/config.toml):
// общие поля (лид, админы пространств) и разделы наблюдателей [orphans], [tmp],
// [memory], [restart], [waits], [agentwaits], [compact], [disk], [said], [bgtask], [cpu], [remote], [codex]. Перечитывается каждый проход.
package config

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/aiseeq/aguard/internal/agentwait"
	"github.com/aiseeq/aguard/internal/bgtask"
	"github.com/aiseeq/aguard/internal/codexwatch"
	"github.com/aiseeq/aguard/internal/cpuwatch"
	"github.com/aiseeq/aguard/internal/disk"
	"github.com/aiseeq/aguard/internal/guiwatch"
	"github.com/aiseeq/aguard/internal/iowatch"
	"github.com/aiseeq/aguard/internal/memwatch"
	"github.com/aiseeq/aguard/internal/night"
	"github.com/aiseeq/aguard/internal/orphans"
	"github.com/aiseeq/aguard/internal/policy"
	"github.com/aiseeq/aguard/internal/queue"
	"github.com/aiseeq/aguard/internal/record"
	"github.com/aiseeq/aguard/internal/remotebroken"
	"github.com/aiseeq/aguard/internal/restart"
	"github.com/aiseeq/aguard/internal/said"
	"github.com/aiseeq/aguard/internal/waits"
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
	Waits   waits.Config    `toml:"waits"`   // отказы и ожидания агентов по транскриптам
	// Agentwaits — ожидания, которые агенты заводят сами: циклы sleep, wfor, фоновые команды, ответы сессий.
	Agentwaits agentwait.Config `toml:"agentwaits"`
	// Compact — своё сжатие сессий в простое, пока кэш подсказки жив.
	Compact restart.CompactConfig `toml:"compact"`
	Disk    disk.Config           `toml:"disk"`   // рост папок кэшей и состояний, остатки временного
	Said    said.Config           `toml:"said"`   // указатель сказанного пользователем
	Bgtask  bgtask.Config         `toml:"bgtask"` // забытые фоновые задачи Bash: письмо владельцу
	Cpu     cpuwatch.Config       `toml:"cpu"`    // перегрузка процессора ноута и PC: письмо тем, чья она
	IO      iowatch.Config        `toml:"io"`     // телеметрия диска, свопа и фризов
	GUI     guiwatch.Config       `toml:"gui"`    // потоки графического интерфейса
	Remote  remotebroken.Config   `toml:"remote"` // сломанный путь graft на PC: письмо владельцу и сводка лиду
	Codex   codexwatch.Config     `toml:"codex"`  // недельный лимит Codex: письмо лиду перед досрочным 100% и при нём
	Queue   queue.Config          `toml:"queue"`
	Night   night.Config          `toml:"night"` // Сон, Дайджест и окна доверия
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
		Orphans:    orphans.Default(home),
		Tmp:        policy.Default(home),
		Memory:     memwatch.Default(),
		Restart:    restart.Default(home, record.RuntimeDir()),
		Waits:      waits.Default(),
		Agentwaits: agentwait.Default(),
		Compact:    restart.DefaultCompact(),
		Disk:       disk.Default(),
		Said:       said.Default(),
		Bgtask:     bgtask.Default(),
		Cpu:        cpuwatch.Default(home),
		IO:         ioDefault(),
		GUI:        guiwatch.Default(),
		Remote:     remotebroken.Default(home),
		Codex:      codexwatch.Default(home),
		Queue:      queue.Default(home),
		Night:      night.Default(home),
	}
}

// ioDefault — раздел [io] по умолчанию с сокетом в $XDG_RUNTIME_DIR.
func ioDefault() iowatch.Config {
	c := iowatch.Default()
	c.Socket = iowatch.DefaultSocket(record.RuntimeDir())
	return c
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
	c.Bgtask.SendAuth = c.SendAuth
	c.Cpu.Lead, c.Cpu.SendAuth = c.Lead, c.SendAuth
	c.Disk.Lead, c.Disk.SendAuth = c.Lead, c.SendAuth
	c.IO.Lead, c.IO.SendAuth = c.Lead, c.SendAuth
	c.IO.Socket = policy.ExpandHome(c.IO.Socket, home)
	c.Remote.Lead, c.Remote.SendAuth = c.Lead, c.SendAuth
	c.Codex.Lead, c.Codex.SendAuth = c.Lead, c.SendAuth
	c.Night.Lead, c.Night.SendAuth = c.Lead, c.SendAuth
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
	if err := c.Waits.Validate(); err != nil {
		return fmt.Errorf("waits: %w", err)
	}
	if err := c.Agentwaits.Validate(); err != nil {
		return fmt.Errorf("agentwaits: %w", err)
	}
	if err := c.Disk.Validate(home); err != nil {
		return err
	}
	if err := c.Said.Validate(home); err != nil {
		return err
	}
	if err := c.Bgtask.Validate(); err != nil {
		return err
	}
	if err := c.Cpu.Validate(home); err != nil {
		return err
	}
	if err := c.IO.Validate(); err != nil {
		return fmt.Errorf("io: %w", err)
	}
	if err := c.GUI.Validate(); err != nil {
		return fmt.Errorf("gui: %w", err)
	}
	if err := c.Remote.Validate(home); err != nil {
		return fmt.Errorf("remote: %w", err)
	}
	if err := c.Codex.Validate(home); err != nil {
		return fmt.Errorf("codex: %w", err)
	}
	if err := c.Queue.Validate(home); err != nil {
		return err
	}
	if err := c.Night.Validate(home); err != nil {
		return err
	}
	return c.Compact.Validate()
}
