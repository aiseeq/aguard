// Package agentwait — ожидания, которые агенты заводят сами: циклы `until … sleep`, `sleep N`, wfor, фоновые
// команды до уведомления, Monitor, письмо другой сессии до её ответа, ScheduleWakeup, у бойцов codex — пустые
// опросы write_stdin. Чего ждали (тест, сборка, деплой, другой агент, человек…), сколько длилось, сколько вызовов
// и токенов ушло на опрос. Что считать ожиданием, решает распознаватель akit/transcript/waits — один на aguard и
// dreamstats, чтобы мера была одна; здесь — чтение транскриптов кусками, хранение по дням и сводка. Очередь graft — из его
// журнала затрат (~/.cache/graft/stats.jsonl), не из транскриптов. Решение пользователя 27.10 — agent-waits-slice
// в ~/.claude/decisions.md.
package agentwait

import (
	"errors"
	"fmt"
	"time"

	"github.com/aiseeq/aguard/internal/daily"
	"github.com/aiseeq/aguard/internal/policy"
)

// Исполнители.
const (
	RunnerClaude = "claude"
	RunnerCodex  = "codex"
)

// Wait — ожидание с источником, как оно хранится.
type Wait struct {
	At      time.Time `json:"at"`
	End     time.Time `json:"end,omitzero"`
	Ms      int64     `json:"ms"`
	Runner  string    `json:"runner"`
	Session string    `json:"session"`
	Agent   string    `json:"agent,omitempty"`
	Project string    `json:"project"`
	Name    string    `json:"name,omitempty"`
	How     string    `json:"how"`
	Class   string    `json:"class"`
	Command string    `json:"command,omitempty"`
	Calls   int       `json:"calls"`
	Repeats int       `json:"repeats,omitempty"`
	Tokens  int64     `json:"tokens,omitempty"`
	Outcome string    `json:"outcome"`
}

// Config — раздел [agentwaits] конфига aguard.
type Config struct {
	Interval   policy.Duration `toml:"interval"`    // как часто читать дописанное
	Backfill   policy.Duration `toml:"backfill"`    // транскрипты, изменённые за этот срок до первого прохода, читаются с начала
	BackfillMB int64           `toml:"backfill_mb"` // сколько МБ читать за проход (больший файл — кусками)
	MaxOpen    policy.Duration `toml:"max_open"`    // дольше открытое ожидание закрывается с исходом none
	Keep       policy.Duration `toml:"keep"`        // сколько хранить дневные файлы
}

// Default — раз в 5 минут, неделя назад по 300 МБ за проход, открытое — до 6 часов, хранение 60 дней.
func Default() Config {
	return Config{
		Interval: policy.Duration(5 * time.Minute), Backfill: policy.Duration(7 * 24 * time.Hour), BackfillMB: 300,
		MaxOpen: policy.Duration(6 * time.Hour), Keep: policy.Duration(60 * 24 * time.Hour),
	}
}

// Validate проверяет раздел.
func (c *Config) Validate() error {
	if c.Interval.D() < time.Minute {
		return fmt.Errorf("interval = %v: не чаще раза в минуту", c.Interval.D())
	}
	if c.Backfill.D() < 0 || c.BackfillMB <= 0 {
		return errors.New("backfill не может быть отрицательным, backfill_mb — больше нуля")
	}
	if c.MaxOpen.D() < time.Hour {
		return fmt.Errorf("max_open = %v: не меньше часа", c.MaxOpen.D())
	}
	if c.Keep.D() < 7*24*time.Hour {
		return fmt.Errorf("keep = %v: не меньше недели", c.Keep.D())
	}
	return nil
}

// NewStore — дневные файлы ожиданий в dir.
func NewStore(dir string) daily.Store[Wait] {
	return daily.Store[Wait]{Dir: dir, What: "ожидания агентов", At: func(w *Wait) time.Time { return w.At }}
}
