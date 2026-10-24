package main

import (
	"fmt"
	"strings"
	"time"
)

// reason — довод против перезапуска: code — для сравнения проверок между собой (без чисел), text — для журнала.
type reason struct {
	code string
	text string
}

// verdict — решение о перезапуске сессии и его доводы.
type verdict struct {
	Restart bool
	// Fresh — версия процесса последняя: перезапускать незачем, остальное не проверялось.
	Fresh   bool
	Against []reason
}

// key — решение без чисел: журнал пишет строку, только когда оно меняется.
func (v verdict) key() string {
	codes := make([]string, 0, len(v.Against)+1)
	codes = append(codes, fmt.Sprint(v.Restart, v.Fresh))
	for _, r := range v.Against {
		codes = append(codes, r.code)
	}
	return strings.Join(codes, ",")
}

// policy — что сессии позволено, кроме общих правил: порог простоя, таймеры роли, аргументы запуска.
type policy struct {
	idle time.Duration
	role role
	// argsProblem — почему перезапуск потерял бы аргументы запуска; пусто — аргументы восстановимы.
	argsProblem string
}

// decide — можно ли перезапускать сессию сейчас. Перезапуск — только когда ни один довод против не сработал:
// версия процесса старее установленной, ход закончен и сессия простояла idle, нет ввода в терминал, детей-процессов,
// идущих инструментов, фоновых задач, суб-агентов и таймеров, которые пропали бы. Любое непрочитанное — против.
func decide(f facts, p policy) verdict {
	var v verdict
	against := func(code, format string, args ...any) {
		v.Against = append(v.Against, reason{code: code, text: fmt.Sprintf(format, args...)})
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
func hookChecks(f facts, p policy, against func(code, format string, args ...any)) {
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
func lostCrons(crons []cronJob, r role) []string {
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
