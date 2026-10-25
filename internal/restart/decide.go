package restart

import (
	"fmt"
	"strings"
	"time"
)

// Reason — довод против перезапуска: Code — для сравнения проверок между собой (без чисел), Text — для журнала.
type Reason struct {
	Code string `json:"code"`
	Text string `json:"text"`
}

// Verdict — решение о перезапуске сессии и его доводы.
type Verdict struct {
	Restart bool `json:"restart"`
	// Fresh — версия процесса последняя: перезапускать незачем, остальное не проверялось.
	Fresh   bool     `json:"fresh"`
	Against []Reason `json:"against"`
}

// Key — решение без чисел: журнал пишет строку, только когда оно меняется.
func (v Verdict) Key() string {
	codes := make([]string, 0, len(v.Against)+1)
	codes = append(codes, fmt.Sprint(v.Restart, v.Fresh))
	for _, r := range v.Against {
		codes = append(codes, r.Code)
	}
	return strings.Join(codes, ",")
}

// ttyGranularity — шаг, с которым ядро обновляет atime и mtime терминала (tty_update_time).
const ttyGranularity = 8 * time.Second

// allowance — что сессии позволено, кроме общих правил: порог простоя, таймеры роли, аргументы запуска.
type allowance struct {
	idle time.Duration
	role Role
	// argsProblem — почему перезапуск потерял бы аргументы запуска; пусто — аргументы восстановимы.
	argsProblem string
}

// decide — можно ли перезапускать сессию сейчас. Перезапуск — только когда ни один довод против не сработал:
// версия процесса старее установленной, ход закончен и сессия простояла idle, нет ввода в терминал (и после конца хода), детей-процессов,
// идущих инструментов, фоновых задач, суб-агентов и таймеров, которые пропали бы. Любое непрочитанное — против.
func decide(f facts, p allowance) Verdict {
	var v Verdict
	against := func(code, format string, args ...any) {
		v.Against = append(v.Against, Reason{Code: code, Text: fmt.Sprintf(format, args...)})
	}
	for _, e := range f.Errors {
		against("error", "не прочитал: %s", e)
	}
	if len(f.Errors) > 0 {
		return v
	}
	if old, err := older(f.Running, f.Installed); err != nil || !old {
		v.Fresh = err == nil
		if err != nil {
			against("version", "%v", err)
		}
		return v
	}
	if f.Kind != "interactive" {
		against("kind", "сессия не интерактивная: %q", f.Kind)
	}
	if f.Status != "idle" {
		against("status", "Claude Code: %s", orDash(f.Status))
	}
	hookChecks(f, p, against)
	if f.Quiet < p.idle {
		against("quiet", "тихо %s из %s", minutes(f.Quiet), minutes(p.idle))
	}
	if f.TTYQuiet < p.idle {
		against("tty", "ввод в терминал %s назад", minutes(f.TTYQuiet))
	}
	// Ввод после последней записи хода — в поле может лежать набранный, но не отправленный текст: перезапуск его
	// потеряет, и порог простоя тут ни при чём. Ядро обновляет atime pts раз в 8 с, поэтому ввод в пределах 8 с до
	// конца хода тоже считается: лишний раз не перезапустить дешевле, чем потерять черновик.
	if f.TTYQuiet <= f.TurnQuiet+ttyGranularity {
		against("draft", "ввод в терминал после конца хода (%s назад): может быть набранный текст", minutes(f.TTYQuiet))
	}
	if len(f.Children) > 0 {
		against("children", "дети процесса: %s", strings.Join(f.Children, ", "))
	}
	t := f.Transcript
	if len(t.Pending) > 0 {
		against("pending", "инструмент без результата: %s", strings.Join(t.Pending, ", "))
	}
	if len(t.Tasks) > 0 {
		against("tasks", "фоновые задачи и суб-агенты: %s", strings.Join(t.Tasks, ", "))
	}
	if lost := lostCrons(t.Crons, p.role); len(lost) > 0 {
		against("crons", "таймеры пропали бы: %s", strings.Join(lost, ", "))
	}
	if len(t.Wakeups) > 0 {
		against("wakeup", "%s", strings.Join(t.Wakeups, ", "))
	}
	if p.argsProblem != "" {
		against("args", "%s", p.argsProblem)
	}
	v.Restart = len(v.Against) == 0
	return v
}

// hookChecks — состояние от claude-hooks. done — ход закончен без фоновых задач и таймеров (по списку Claude Code на
// Stop). waiting — минутное напоминание после done или вопрос человеку; вопрос виден идущим инструментом (Pending).
// paused — Stop при фоновых задачах или таймерах: годится, только если задач нет (файла .tasks нет), а таймеры по
// транскрипту есть и все восстановимы ролью.
func hookChecks(f facts, p allowance, against func(code, format string, args ...any)) {
	switch f.HookState {
	case "done", "waiting":
	case "paused":
		if f.HookTasks {
			against("hook-tasks", "claude-hooks: на последнем Stop были фоновые задачи")
		}
		if !f.HookTasks && len(f.Transcript.Crons) == 0 {
			against("hook-paused", "claude-hooks: paused, а таймеров в транскрипте нет — непонятно, чего ждёт")
		}
	default:
		against("hook", "claude-hooks: %s", orDash(f.HookState))
	}
}

// lostCrons — таймеры, которые роль не ставит заново сама.
func lostCrons(crons []cronJob, r Role) []string {
	var lost []string
	for _, c := range crons {
		if !r.restorable(c.Prompt) {
			lost = append(lost, c.ID+" «"+short(c.Prompt)+"»")
		}
	}
	return lost
}

func minutes(d time.Duration) string {
	return fmt.Sprintf("%d мин", int(d.Minutes()))
}

func orDash(s string) string {
	if s == "" {
		return "нет записи"
	}
	return s
}
