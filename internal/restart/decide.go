package restart

import (
	"fmt"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/screen"
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
	sessionChecks(f, against)
	if f.Quiet < p.idle {
		against("quiet", "тихо %s из %s", minutes(f.Quiet), minutes(p.idle))
	}
	if f.TTYQuiet < p.idle {
		against("tty", "ввод в терминал %s назад", minutes(f.TTYQuiet))
	}
	inputChecks(f, against)
	workChecks(f, against)
	lossChecks(f, p, against)
	v.Restart = len(v.Against) == 0
	return v
}

// lossChecks — новый процесс поднимется со всем, что было: таймеры, будильники, аргументы запуска.
func lossChecks(f facts, p allowance, against againstFunc) {
	if lost := lostCrons(f.Transcript.Crons, p.role); len(lost) > 0 {
		against("crons", "таймеры пропали бы: %s", strings.Join(lost, ", "))
	}
	if len(f.Transcript.Wakeups) > 0 {
		against("wakeup", "%s", strings.Join(f.Transcript.Wakeups, ", "))
	}
	if p.argsProblem != "" {
		against("args", "%s", p.argsProblem)
	}
}

// againstFunc — записать довод против.
type againstFunc func(code, format string, args ...any)

// sessionChecks — интерактивная сессия закончила ход.
func sessionChecks(f facts, against againstFunc) {
	if f.Kind != "interactive" {
		against("kind", "сессия не интерактивная: %q", f.Kind)
	}
	if f.Status != "idle" {
		against("status", "Claude Code: %s", OrDash(f.Status))
	}
	hookChecks(f, against)
}

// inputChecks — в поле ввода нет набранного текста: то, что aguard или cl пошлют в сессию, его затёрло бы.
func inputChecks(f facts, against againstFunc) {
	// Ввод после последней записи хода — в поле может лежать набранный, но не отправленный текст: перезапуск его
	// потеряет, и порог простоя тут ни при чём. Ядро обновляет atime pts раз в 8 с, поэтому ввод в пределах 8 с до
	// конца хода тоже считается: лишний раз не перезапустить дешевле, чем потерять черновик. Экран прочитан и поле
	// пусто — черновика нет, а ввод — байты фокуса: Claude Code включает отчёт о фокусе (ESC[?1004h), и Konsole при
	// переключении на вкладку шлёт его в терминал (решение пользователя 26.10 20:54 CET, вариант 2).
	if f.TTYQuiet <= f.TurnQuiet+ttyGranularity && (f.PromptErr != "" || f.Prompt != screen.Empty) {
		against("draft", "ввод в терминал после конца хода (%s назад): может быть набранный текст", minutes(f.TTYQuiet))
	}
	// Текст, набранный во время хода, старше конца хода, и проверка выше его не видит: поле ввода — по экрану.
	switch {
	case f.PromptErr != "":
		against("screen", "экран терминала: %s", f.PromptErr)
	case f.Prompt == screen.Draft:
		against("screen", "в поле ввода набранный текст")
	case f.Prompt != screen.Empty:
		against("screen", "поля ввода на экране не видно")
	}
}

// workChecks — ничего не идёт: дети процесса, инструменты, фоновые задачи и суб-агенты.
func workChecks(f facts, against againstFunc) {
	if len(f.Children) > 0 {
		against("children", "дети процесса: %s", strings.Join(f.Children, ", "))
	}
	if len(f.Transcript.Pending) > 0 {
		against("pending", "инструмент без результата: %s", strings.Join(f.Transcript.Pending, ", "))
	}
	if len(f.Transcript.Tasks) > 0 {
		against("tasks", "фоновые задачи и суб-агенты: %s", strings.Join(f.Transcript.Tasks, ", "))
	}
}

// hookChecks — состояние от chooks (матрица пользователя 26.10 16:33–16:35 CET в транскрипте chooks-master: пауза —
// сжимать и сбрасывать можно, работают суб-агенты — только сжимать, ждёт ответа — «сброс должен быть запрещён»).
// done и idle — ход закончен без фоновых задач и таймеров (по списку Claude Code на Stop), idle — человек после
// этого открыл вкладку. waiting — вопрос человеку (минутное напоминание с chooks ae7ceb3 его не ставит): против,
// как working и error. paused — Stop при фоновых задачах или таймерах: годится, только если задач нет (файла .tasks
// нет), а таймеры по транскрипту есть и все восстановимы ролью; живые фоновые команды, мониторы и суб-агенты
// перезапуск cl (сжатие и сброс — тоже перезапуск) убил бы.
func hookChecks(f facts, against againstFunc) {
	switch f.HookState {
	case "done", "idle":
	case "paused":
		if f.HookTasks {
			against("hook-tasks", "chooks: на последнем Stop были фоновые задачи")
		}
		if !f.HookTasks && len(f.Transcript.Crons) == 0 {
			against("hook-paused", "chooks: paused, а таймеров в транскрипте нет — непонятно, чего ждёт")
		}
	default:
		against("hook", "chooks: %s", OrDash(f.HookState))
	}
}

// lostCrons — таймеры, которые роль не ставит заново сама.
func lostCrons(crons []CronJob, r Role) []string {
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

// OrDash — пустое значение в строке журнала: «-».
func OrDash(s string) string {
	if s == "" {
		return "нет записи"
	}
	return s
}
