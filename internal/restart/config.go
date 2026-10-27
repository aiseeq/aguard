// Package restart — решение «можно ли сейчас перезапустить сессию Claude Code на новую версию» для cl. Перезапускает
// cl (он родитель сессии), aguard только наблюдает: собирает факты о сессии и отвечает вердиктом в сокет.
package restart

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
)

// Режимы перезапуска: ModeLog — cl только пишет «перезапустил бы / не стал бы», ModeAct — перезапускает всерьёз.
const (
	ModeLog = "log"
	ModeAct = "act"
)

// Config — раздел [restart] конфига aguard.
type Config struct {
	Mode   string          `toml:"mode"`
	Idle   policy.Duration `toml:"idle"`   // сколько сессия должна простоять, чтобы её можно было перезапустить
	Socket string          `toml:"socket"` // где aguard ждёт вопросы cl
	Claude string          `toml:"claude"` // claude из PATH сессий: версию по этой ссылке запустит перезапуск
	// HookStates — состояния chooks (internal/core/state.go: $XDG_RUNTIME_DIR/chooks/sessions).
	HookStates string `toml:"hook_states"`
}

// Default — начальные значения: только журнал, без ролей. runtime — $XDG_RUNTIME_DIR.
func Default(home, runtime string) Config {
	return Config{
		Mode:       ModeLog,
		Idle:       policy.Duration(10 * time.Minute),
		Socket:     filepath.Join(runtime, "aguard", "aguard.sock"),
		Claude:     filepath.Join(home, ".local", "bin", "claude"),
		HookStates: filepath.Join(runtime, "chooks", "sessions"),
	}
}

// Validate проверяет раздел и раскрывает ~ в путях.
func (c *Config) Validate(home string) error {
	if c.Mode != ModeLog && c.Mode != ModeAct {
		return fmt.Errorf("restart.mode = %q: ожидается %s или %s", c.Mode, ModeLog, ModeAct)
	}
	if c.Idle.D() < time.Minute {
		return fmt.Errorf("restart.idle = %v: не меньше минуты", c.Idle.D())
	}
	if c.Socket == "" || c.Claude == "" || c.HookStates == "" {
		return errors.New("restart: socket, claude и hook_states не могут быть пустыми")
	}
	c.Socket = policy.ExpandHome(c.Socket, home)
	c.Claude = policy.ExpandHome(c.Claude, home)
	c.HookStates = policy.ExpandHome(c.HookStates, home)
	return nil
}
