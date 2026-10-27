package restart

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
)

// Сжатие в простое. Кэш подсказки живёт час от начала запроса. Беседа, брошенная с большим контекстом, после часа
// пишет весь контекст в кэш заново. Сжатие до конца часа читает контекст из живого кэша, и следующий запрос идёт уже
// от короткой сводки. Claude Code делает это сам около 54-й минуты (настройка idleCompaction), но не всегда вовремя;
// aguard смотрит сам. Сжимает cl через перезапуск (решение пользователя 25.10 18:01 CET): останавливает claude,
// `claude -p --resume <сессия> /compact`, поднимает беседу снова. Поэтому сессия должна быть свободна, как для
// перезапуска, и ничего не терять: таймеры, будильники, аргументы запуска. В терминал никто не печатает.

// Режимы сжатия в aguard: CompactLog — вердикт cl и журнал «сжал бы» со сверкой исхода; CompactOff — вердикт всегда
// «нет». Сжимать или только писать — решает режим cl.
const (
	CompactOff = "off"
	CompactLog = "log"
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

// Validate проверяет раздел [compact].
func (c *CompactConfig) Validate() error {
	switch c.Mode {
	case CompactOff, CompactLog:
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
// сессия свободна и перезапуск ничего не потеряет (как для перезапуска, без версии и порога простоя), человек не
// печатает.
func decideCompact(f facts, c *CompactConfig, p allowance, now time.Time) CompactVerdict {
	against, in := idleChecks(f, c, p, idleWindow{c.MinTokens, c.After.D(), c.Before.D(), "окно", false}, now)
	return CompactVerdict{Compact: len(against) == 0, Against: against, InWindow: in}
}

// idleWindow — когда решать о беседе в простое: контекст от minTokens, последний запрос from…to назад (to 0 — без
// верхней границы); what — слово для доводов; afterCompaction — действие имеет смысл и после сжатия беседы (сброс).
type idleWindow struct {
	minTokens       int64
	from, to        time.Duration
	what            string
	afterCompaction bool
}

// idleChecks — доводы против действия над беседой в простое (сжатия или сброса) и в окне ли она: контекст и окно,
// после запроса не сжимали, сессия свободна, человек не печатает, ничего не идёт и ничего не потеряется.
func idleChecks(f facts, c *CompactConfig, p allowance, w idleWindow, now time.Time) ([]Reason, bool) {
	var list []Reason
	against := func(code, format string, args ...any) {
		list = append(list, Reason{Code: code, Text: fmt.Sprintf(format, args...)})
	}
	for _, e := range f.Errors {
		against("error", "не прочитал: %s", e)
	}
	if len(f.Errors) > 0 {
		return list, false
	}
	ctx := f.Transcript.Context
	age, in := now.Sub(ctx.Request), false
	switch {
	case ctx.Request.IsZero():
		against("context", "в транскрипте нет запросов")
	case ctx.Tokens < w.minTokens:
		against("tokens", "контекст %d тыс. из %d тыс.", ctx.Tokens/1000, w.minTokens/1000)
	case age < w.from:
		against("early", "запрос %s назад, %s с %s", minutes(age), w.what, minutes(w.from))
	case w.to > 0 && age >= w.to:
		against("late", "запрос %s назад, %s до %s", minutes(age), w.what, minutes(w.to))
	default:
		in = true
	}
	if !w.afterCompaction && !ctx.Compacted.IsZero() && !ctx.Compacted.Before(ctx.Request) {
		against("compacted", "уже сжата (%s) после последнего запроса", OrDash(ctx.Trigger))
	}
	sessionChecks(f, against)
	if f.TTYQuiet < c.TTYQuiet.D() {
		against("tty", "ввод в терминал %s назад", minutes(f.TTYQuiet))
	}
	inputChecks(f, against)
	workChecks(f, against)
	lossChecks(f, p, against)
	return list, in
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
	// Requests — последние запросы: исход решения смотрит первый после решения, а не последний.
	Requests  []RequestMark `json:"-"`
	Compacted time.Time     `json:"compacted"` // последнее сжатие
	Trigger   string        `json:"trigger"`
	// Pid — процесс сессии: /clear меняет беседу у того же процесса.
	Pid int `json:"pid"`
	// Model, Reply, Prompt, PromptAt — модель, итог последнего хода, последняя реплика человека (contextFacts).
	Model    string    `json:"model"`
	Reply    string    `json:"reply"`
	Prompt   string    `json:"prompt"`
	PromptAt time.Time `json:"prompt_at"`
	// Reset — сбросить беседу: cl поднимает сессию новой беседой (режим act — в cl), aguard пишет «сбросил бы».
	Reset ResetVerdict `json:"reset"`
}

// Compact — решение о сжатии сессии процесса req.Pid; cfg — раздел [restart] (пути chooks и claude, роли),
// req.ArgsProblem — почему перезапуск потерял бы аргументы (знает только cl; пусто — восстановимы), req.Typed — cl
// наберёт команду в живой сессии.
func (j *Judge) Compact(cfg *Config, c *CompactConfig, req Request, now time.Time) CompactCheck {
	pid, allow := req.Pid, allowance{argsProblem: req.ArgsProblem, typed: req.Typed}
	ch := j.cache(pid, now)
	ch.mu.Lock()
	var f facts
	p := pathsFor(j.Home, cfg)
	s, err := readSession(p.sessions, pid)
	if err != nil {
		f.Errors = append(f.Errors, err.Error())
	} else {
		f.Info = s
		state(p, &ch.scan, pid, now, &f)
	}
	ch.mu.Unlock()
	ctx := f.Transcript.Context
	off := []Reason{{Code: "off", Text: "сжатие выключено в конфиге aguard"}}
	v, r := CompactVerdict{Against: off}, ResetVerdict{Against: off}
	if c.Mode != CompactOff {
		v = decideCompact(f, c, allow, now)
		if r = decideReset(f, c, allow, now); r.Reset {
			treeCheck(&r, p.tree, f.Cwd)
		}
		preferReset(&v, r)
	}
	return CompactCheck{
		CompactVerdict: v, SessionID: f.SessionID, Name: f.Name, SessionKind: f.Kind,
		Request: ctx.Request, Tokens: ctx.Tokens, CacheRead: ctx.CacheRead, Requests: f.Transcript.Requests,
		Compacted: ctx.Compacted, Trigger: ctx.Trigger,
		Pid: pid, Model: ctx.Model, Reply: ctx.Reply, Prompt: ctx.Prompt, PromptAt: ctx.PromptAt, Reset: r,
	}
}

// preferReset — где можно сбросить, не сжимать: сжатие — компромисс (пользователь 27.10 10:48 CET, decideReset).
func preferReset(v *CompactVerdict, r ResetVerdict) {
	if r.Reset && v.Compact {
		v.Compact = false
		v.Against = append(v.Against, Reason{Code: "reset", Text: "сброс вместо сжатия: работа закрыта, беседа не нужна"})
	}
}

// Key — решение без чисел: cl пишет строку журнала, только когда оно меняется.
func (v CompactVerdict) Key() string {
	return verdictKey(v.Compact, v.Against)
}

// verdictKey — решение и коды доводов против одной строкой.
func verdictKey(yes bool, against []Reason) string {
	codes := []string{fmt.Sprint(yes)}
	for _, r := range against {
		codes = append(codes, r.Code)
	}
	return strings.Join(codes, ",")
}
