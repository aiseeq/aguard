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
	"time"

	"github.com/BurntSushi/toml"

	"github.com/aiseeq/aguard/internal/agentwait"
	"github.com/aiseeq/aguard/internal/bgtask"
	"github.com/aiseeq/aguard/internal/btrfswatch"
	ceilingwatch "github.com/aiseeq/aguard/internal/ceiling"
	"github.com/aiseeq/aguard/internal/codexwatch"
	"github.com/aiseeq/aguard/internal/cpuwatch"
	"github.com/aiseeq/aguard/internal/disk"
	"github.com/aiseeq/aguard/internal/forkguard"
	"github.com/aiseeq/aguard/internal/guiwatch"
	"github.com/aiseeq/aguard/internal/iowatch"
	"github.com/aiseeq/aguard/internal/liveness"
	"github.com/aiseeq/aguard/internal/memwatch"
	"github.com/aiseeq/aguard/internal/night"
	"github.com/aiseeq/aguard/internal/orphans"
	"github.com/aiseeq/aguard/internal/policy"
	"github.com/aiseeq/aguard/internal/prune"
	"github.com/aiseeq/aguard/internal/queue"
	"github.com/aiseeq/aguard/internal/record"
	"github.com/aiseeq/aguard/internal/remotebroken"
	"github.com/aiseeq/aguard/internal/restart"
	"github.com/aiseeq/aguard/internal/said"
	"github.com/aiseeq/aguard/internal/sigguard"
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
	Compact   restart.CompactConfig `toml:"compact"`
	Disk      disk.Config           `toml:"disk"`       // рост папок кэшей и состояний, остатки временного
	Btrfs     btrfswatch.Config     `toml:"btrfs"`      // запас Btrfs, заполнение метаданных и квоты
	ClaudeTmp prune.Config          `toml:"claude_tmp"` // уборка старых scratchpad-сессий
	Said      said.Config           `toml:"said"`       // указатель сказанного пользователем
	Bgtask    bgtask.Config         `toml:"bgtask"`     // забытые фоновые задачи Bash: письмо владельцу
	Cpu       cpuwatch.Config       `toml:"cpu"`        // перегрузка процессора ноута и PC: письмо тем, чья она
	IO        iowatch.Config        `toml:"io"`         // телеметрия диска, свопа и фризов
	GUI       guiwatch.Config       `toml:"gui"`        // потоки графического интерфейса
	Remote    remotebroken.Config   `toml:"remote"`     // сломанный путь graft на PC: письмо владельцу и сводка лиду
	Codex     codexwatch.Config     `toml:"codex"`      // недельный лимит Codex: журнал прогноза 100%, сброс по «да» на окно
	Queue     queue.Config          `toml:"queue"`
	Night     night.Config          `toml:"night"` // Сон, Дайджест и окна доверия
	Liveness  liveness.Config       `toml:"liveness"`
	Sigguard  sigguard.Config       `toml:"sigguard"`
	Ceiling   ceilingwatch.Config   `toml:"ceiling"`
	Quiet     QuietConfig           `toml:"quiet"`
	Forkguard ForkguardConfig       `toml:"forkguard"`
}

type ForkguardConfig struct {
	Mode      string          `toml:"mode"`
	Interval  policy.Duration `toml:"interval"`
	Window    policy.Duration `toml:"window"`
	Hard      int64           `toml:"hard"`
	Floor     int64           `toml:"floor"`
	Rise      int64           `toml:"rise"`
	Slice     string          `toml:"slice"`
	SliceWarn int64           `toml:"slice_warn"`
}

func (c ForkguardConfig) Validate() error {
	if c.Mode == "act" {
		return errors.New("act — этап 2")
	}
	if c.Mode != "log" {
		return errors.New("forkguard.mode = log")
	}
	if c.Interval.D() <= 0 || c.Window.D() <= 0 || c.Hard <= 0 || c.Floor <= 0 || c.Rise <= 0 || c.Slice == "" || c.SliceWarn < 1 || c.SliceWarn > 100 {
		return errors.New("forkguard: interval, window, hard, floor, rise, slice заданы; slice_warn от 1 до 100")
	}
	return nil
}

func (c ForkguardConfig) EngineConfig() forkguard.Config {
	return forkguard.Config{Window: c.Window.D(), Hard: c.Hard, Floor: c.Floor, Rise: c.Rise, SliceWarn: c.SliceWarn}
}

// LoadForkguard reads only the forkguard table; unrelated observer settings cannot block its sampler.
func LoadForkguard(path string) (ForkguardConfig, error) {
	c := ForkguardConfig{Mode: "log", Interval: policy.Duration(time.Second), Window: policy.Duration(10 * time.Second), Hard: 3000, Floor: 1500, Rise: 1000, Slice: "agents.slice", SliceWarn: 80}
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return c, fmt.Errorf("config: %w", err)
	}
	if err == nil {
		var partial struct {
			Forkguard ForkguardConfig `toml:"forkguard"`
		}
		partial.Forkguard = c
		md, err := toml.Decode(string(b), &partial)
		if err != nil {
			return c, fmt.Errorf("%s [forkguard]: %w", path, err)
		}
		// опечатка в своём разделе — ошибка, как в общем конфиге; чужие разделы сторожа не касаются
		for _, k := range md.Undecoded() {
			if len(k) > 0 && k[0] == "forkguard" {
				return c, fmt.Errorf("%s: неизвестный ключ %s", path, k.String())
			}
		}
		c = partial.Forkguard
	}
	return c, c.Validate()
}

// QuietConfig — окно тишины после загрузки машины.
type QuietConfig struct {
	AfterBoot policy.Duration `toml:"after_boot"`
}

func (c QuietConfig) Validate() error {
	if c.AfterBoot.D() < 0 {
		return fmt.Errorf("quiet.after_boot должен быть не меньше нуля")
	}
	return nil
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
		Btrfs:      btrfswatch.Default(),
		ClaudeTmp:  prune.Config{Mode: prune.ModeLog, OutsideMode: prune.ModeLog, Age: policy.Duration(7 * 24 * time.Hour), Window: prune.DefaultWindow},
		Said:       said.Default(),
		Bgtask:     bgtask.Default(),
		Cpu:        cpuwatch.Default(home),
		IO:         ioDefault(),
		GUI:        guiwatch.Default(),
		Remote:     remotebroken.Default(home),
		Codex:      codexwatch.Default(home),
		Queue:      queue.Default(home),
		Night:      night.Default(home),
		Liveness:   liveness.Default(),
		Sigguard:   sigguard.Default(),
		Ceiling:    ceilingwatch.Default(),
		Quiet:      QuietConfig{AfterBoot: policy.Duration(45 * time.Minute)},
		Forkguard:  ForkguardConfig{Mode: "log", Interval: policy.Duration(time.Second), Window: policy.Duration(10 * time.Second), Hard: 3000, Floor: 1500, Rise: 1000, Slice: "agents.slice", SliceWarn: 80},
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
	if err := c.Quiet.Validate(); err != nil {
		return c, err
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
	c.Btrfs.Lead, c.Btrfs.SendAuth = c.Lead, c.SendAuth
	c.IO.Lead, c.IO.SendAuth = c.Lead, c.SendAuth
	c.IO.BtrfsSysfsRoot = c.Btrfs.SysfsRoot
	c.IO.Socket = policy.ExpandHome(c.IO.Socket, home)
	c.IO.Tk = policy.ExpandHome(c.IO.Tk, home)
	c.Disk.Tk = c.IO.Tk // тикеты хозяевам disk — тем же tk
	c.Remote.Lead, c.Remote.SendAuth = c.Lead, c.SendAuth
	c.Codex.Lead, c.Codex.SendAuth = c.Lead, c.SendAuth
	c.Night.Lead, c.Night.SendAuth = c.Lead, c.SendAuth
	c.Liveness.Lead, c.Liveness.SendAuth = c.Lead, c.SendAuth
	c.Sigguard.Lead, c.Sigguard.SendAuth = c.Lead, c.SendAuth
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
	if c.Btrfs.Interval.D() < time.Minute || c.Btrfs.UnallocatedMin <= 0 || c.Btrfs.MetadataUnallocatedMax <= c.Btrfs.UnallocatedMin || c.Btrfs.MetadataPercent < 1 || c.Btrfs.MetadataPercent > 99 || c.Btrfs.SysfsRoot == "" {
		return errors.New("btrfs: interval >= 1m, пороги места положительны, metadata_percent от 1 до 99, sysfs_root задан")
	}
	if err := c.finishClaudeTmp(home); err != nil {
		return err
	}
	return c.finishRemaining(home)
}

func (c *Config) finishRemaining(home string) error {
	if err := c.Forkguard.Validate(); err != nil {
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
	if err := c.Liveness.Validate(home); err != nil {
		return err
	}
	if err := c.Sigguard.Validate(); err != nil {
		return fmt.Errorf("sigguard: %w", err)
	}
	if err := c.Ceiling.Validate(); err != nil {
		return fmt.Errorf("ceiling: %w", err)
	}
	return c.Compact.Validate()
}

func (c *Config) finishClaudeTmp(home string) error {
	if c.ClaudeTmp.Mode != prune.ModeLog && c.ClaudeTmp.Mode != prune.ModeAct {
		return fmt.Errorf("claude_tmp.mode = %q: log или act", c.ClaudeTmp.Mode)
	}
	if c.ClaudeTmp.OutsideMode != prune.ModeLog && c.ClaudeTmp.OutsideMode != prune.ModeAct {
		return fmt.Errorf("claude_tmp.outside_mode = %q: log или act", c.ClaudeTmp.OutsideMode)
	}
	if c.ClaudeTmp.Age.D() < 24*time.Hour {
		return errors.New("claude_tmp.age >= 24h")
	}
	c.ClaudeTmp.Window = prune.DefaultWindow
	c.ClaudeTmp.Home = home
	c.ClaudeTmp.Root = filepath.Join(home, ".cache", "claude-tmp", "claude-1000")
	c.ClaudeTmp.Projects = filepath.Join(home, ".claude", "projects")
	return nil
}
