package restart

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
)

// Сжатие в простое. Кэш подсказки живёт час от начала запроса. Беседа, брошенная с большим контекстом, после часа
// пишет весь контекст в кэш заново. Сжатие до конца часа читает контекст из живого кэша, и следующий запрос идёт уже
// от короткой сводки. Claude Code делает это сам около 54-й минуты (настройка idleCompaction), но не всегда вовремя;
// aguard смотрит сам. Сессия должна быть свободна, как для перезапуска.

// Режимы сжатия: CompactLog — aguard пишет «сжал бы» и потом сверяет с тем, что случилось; CompactAct — шлёт /compact
// в терминал сессии (пока не сделан).
const (
	CompactOff = "off"
	CompactLog = "log"
	CompactAct = "act"
)

// CompactConfig — раздел [compact] конфига aguard.
type CompactConfig struct {
	Mode string `toml:"mode"`
	// MinTokens — контекст последнего ответа, с которого сжимать.
	MinTokens int64 `toml:"min_tokens"`
	// After и Before — окно от начала последнего запроса. Раньше After человек ещё может вернуться к беседе. После
	// Before кэш может умереть раньше, чем сжатие его прочитает.
	After  policy.Duration `toml:"after"`
	Before policy.Duration `toml:"before"`
	// TTYQuiet — сколько не было ввода в терминал: человек не печатает прямо сейчас.
	TTYQuiet policy.Duration `toml:"tty_quiet"`
	Interval policy.Duration `toml:"interval"`
	// Keep — сколько хранить записи «сжал бы» и их сверку.
	Keep policy.Duration `toml:"keep"`
}

// cacheTTL1h — сколько живёт кэш подсказки у Claude Code (TTL 1 ч).
const cacheTTL1h = time.Hour

// DefaultCompact — только журнал: порог 100 тыс., окно 53–57 мин.
func DefaultCompact() CompactConfig {
	return CompactConfig{
		Mode:      CompactLog,
		MinTokens: 100_000,
		After:     policy.Duration(53 * time.Minute),
		Before:    policy.Duration(57 * time.Minute),
		TTYQuiet:  policy.Duration(2 * time.Minute),
		Interval:  policy.Duration(time.Minute),
		Keep:      policy.Duration(60 * 24 * time.Hour),
	}
}

// Validate проверяет раздел [compact]. act пока не сделан: сначала журнал и сверка.
func (c *CompactConfig) Validate() error {
	switch c.Mode {
	case CompactOff, CompactLog:
	case CompactAct:
		return errors.New("compact.mode = act: отправка /compact ещё не сделана, только off или log")
	default:
		return fmt.Errorf("compact.mode = %q: ожидается %s или %s", c.Mode, CompactOff, CompactLog)
	}
	if c.MinTokens <= 0 {
		return fmt.Errorf("compact.min_tokens = %d: больше нуля", c.MinTokens)
	}
	if c.After.D() <= 0 || c.After.D() >= c.Before.D() || c.Before.D() >= cacheTTL1h {
		return fmt.Errorf("compact: окно %v–%v: after < before < %v", c.After.D(), c.Before.D(), cacheTTL1h)
	}
	if c.Interval.D() < 10*time.Second || c.Interval.D() > c.Before.D()-c.After.D() {
		return fmt.Errorf("compact.interval = %v: от 10 с до ширины окна", c.Interval.D())
	}
	if c.TTYQuiet.D() < 0 || c.Keep.D() < 24*time.Hour {
		return errors.New("compact: tty_quiet не меньше нуля, keep не меньше суток")
	}
	return nil
}

// CompactVerdict — решение о сжатии сессии и его доводы.
type CompactVerdict struct {
	Compact bool     `json:"compact"`
	Against []Reason `json:"against"`
	// InWindow — последний запрос в окне After–Before и контекст не меньше порога: решение стоит записать.
	InWindow bool `json:"in_window"`
}

// decideCompact — сжимать ли сессию сейчас: большой контекст, последний запрос в окне, после него не сжимали,
// сессия свободна (как для перезапуска, без версии и порога простоя), человек не печатает.
func decideCompact(f facts, c *CompactConfig, now time.Time) CompactVerdict {
	var v CompactVerdict
	against := func(code, format string, args ...any) {
		v.Against = append(v.Against, Reason{Code: code, Text: fmt.Sprintf(format, args...)})
	}
	for _, e := range f.Errors {
		against("error", "не прочитал: %s", e)
	}
	if len(f.Errors) > 0 {
		return v
	}
	ctx := f.Transcript.Context
	age := now.Sub(ctx.Request)
	switch {
	case ctx.Request.IsZero():
		against("context", "в транскрипте нет запросов")
	case ctx.Tokens < c.MinTokens:
		against("tokens", "контекст %d тыс. из %d тыс.", ctx.Tokens/1000, c.MinTokens/1000)
	case age < c.After.D():
		against("early", "запрос %s назад, окно с %s", minutes(age), minutes(c.After.D()))
	case age >= c.Before.D():
		against("late", "запрос %s назад, окно до %s", minutes(age), minutes(c.Before.D()))
	default:
		v.InWindow = true
	}
	if !ctx.Compacted.IsZero() && !ctx.Compacted.Before(ctx.Request) {
		against("compacted", "уже сжата (%s) после последнего запроса", orDash(ctx.Trigger))
	}
	sessionChecks(f, against)
	if f.TTYQuiet < c.TTYQuiet.D() {
		against("tty", "ввод в терминал %s назад", minutes(f.TTYQuiet))
	}
	inputChecks(f, against)
	workChecks(f, against)
	v.Compact = len(v.Against) == 0
	return v
}

// CompactCheck — решение о сжатии и то, что о сессии пишет журнал.
type CompactCheck struct {
	CompactVerdict
	SessionID string `json:"session_id"`
	Name      string `json:"name"`
	// SessionKind — interactive, …: сжимаем только интерактивные.
	SessionKind string    `json:"session_kind"`
	Request     time.Time `json:"request"`    // начало последнего запроса
	Tokens      int64     `json:"tokens"`     // контекст последнего ответа
	CacheRead   int64     `json:"cache_read"` // сколько последний ответ прочитал из кэша
	Compacted   time.Time `json:"compacted"`  // последнее сжатие
	Trigger     string    `json:"trigger"`
}

// Compact — решение о сжатии сессии процесса pid; cfg — раздел [restart] (пути claude-hooks и claude).
func (j *Judge) Compact(cfg *Config, c *CompactConfig, pid int, now time.Time) CompactCheck {
	ch := j.cache(pid, now)
	ch.mu.Lock()
	var f facts
	p := pathsFor(j.Home, cfg)
	s, err := readSession(filepath.Join(p.sessions, strconv.Itoa(pid)+".json"))
	if err != nil {
		f.Errors = append(f.Errors, err.Error())
	} else {
		f.session = s
		state(p, &ch.scan, pid, now, &f)
	}
	ch.mu.Unlock()
	ctx := f.Transcript.Context
	return CompactCheck{
		CompactVerdict: decideCompact(f, c, now), SessionID: f.SessionID, Name: f.Name, SessionKind: f.Kind,
		Request: ctx.Request, Tokens: ctx.Tokens, CacheRead: ctx.CacheRead, Compacted: ctx.Compacted, Trigger: ctx.Trigger,
	}
}
