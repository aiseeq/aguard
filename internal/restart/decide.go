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

// Texts — тексты доводов по порядку.
func Texts(rs []Reason) []string {
	texts := make([]string, len(rs))
	for i, r := range rs {
		texts[i] = r.Text
	}
	return texts
}

// Verdict — решение о перезапуске сессии и его доводы.
type Verdict struct {
	Restart bool `json:"restart"`
	// Lost — таймеры, унаследованные через /clear: --resume их не вернёт. Перезапуск они не держат: cl поднимает
	// сессию со строкой роли (роль ставит свои таймеры сама) и пишет каждый в журнал (тикет 20261108-2117,
	// пользователь 08.11: «вся эта инструкция либо вредна, либо бесполезна. Перезапуск cl сам бы выдал роль»).
	Lost []LostCron `json:"lost,omitempty"`
	// Fresh — версия процесса последняя и аргументы не устарели: перезапускать незачем, против не проверялось.
	Fresh bool `json:"fresh"`
	// ArgsStale — решалось ради устаревших аргументов запуска при свежей версии (Request.Stale).
	ArgsStale bool     `json:"args_stale,omitempty"`
	Against   []Reason `json:"against"`
}

// Key — решение без чисел: журнал пишет строку, только когда оно меняется.
func (v Verdict) Key() string {
	codes := make([]string, 0, len(v.Against)+1)
	codes = append(codes, fmt.Sprint(v.Restart, v.Fresh))
	if v.ArgsStale {
		codes = append(codes, "stale")
	}
	for _, r := range v.Against {
		codes = append(codes, r.Code)
	}
	return strings.Join(codes, ",")
}

// ttyGranularity — шаг, с которым ядро обновляет atime и mtime терминала (tty_update_time).
const ttyGranularity = 8 * time.Second

// allowance — что сессии позволено и что cl сказал о ней, кроме общих правил: порог простоя, аргументы запуска.
type allowance struct {
	idle time.Duration
	// argsProblem — почему перезапуск потерял бы аргументы запуска; пусто — аргументы восстановимы.
	argsProblem string
	// typed — cl может набирать команды в живой сессии под pty.
	typed bool
	// stale — чем процесс устарел, кроме версии (Request.Stale); не пусто — перезапуск и при свежей версии.
	stale string
}

// action — что делают с живым claude: от этого зависит, что пропадёт.
type action int

const (
	// restart — SIGTERM и подъём (новая версия, сжатие и сброс без pty): умрёт всё запущенное.
	restart action = iota
	// typedCompact — /compact набором под pty: процесс тот же, фоновые задачи, суб-агенты и дети живут дальше.
	typedCompact
	// typedClear — /clear набором под pty: фоновая команда живёт, и уведомление о её конце приходит в новую беседу
	// (пользователь 26.10 16:25 CET в chooks-master: /clear при живой задаче bq4yr5j4i, уведомление в новой беседе
	// 0369ecf0 в 16:53 CET). Суб-агенты держат сброс: «запущены субагенты, они реально работают, я бы такое разрешил
	// только компактить» (пользователь 26.10 16:33 CET там же).
	typedClear
)

// decide — можно ли перезапускать сессию сейчас. Перезапуск — только когда ни один довод против не сработал:
// версия процесса старее установленной (или аргументы запуска устарели — p.stale), ход закончен и сессия простояла idle, нет ввода в терминал (и после конца хода), детей-процессов,
// идущих инструментов, фоновых задач, суб-агентов, будильников и аргументов, которые пропали бы. Таймеры,
// унаследованные через /clear, не держат: уходят в Verdict.Lost. Любое непрочитанное — против.
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
	old, err := older(f.Running, f.Installed)
	if err != nil {
		against("version", "%v", err)
		return v
	}
	for _, job := range inherited(f.Transcript.Crons) { // и при свежей версии: подъём после сжатия тоже идёт через --resume
		v.Lost = append(v.Lost, LostCron{ID: job.ID, Cron: job.Cron, Prompt: job.Prompt, Recurring: job.Recurring})
	}
	if !old && p.stale == "" {
		v.Fresh = true
		return v
	}
	v.ArgsStale = !old
	sessionChecks(f, restart, against)
	if f.Quiet < p.idle {
		against("quiet", "тихо %s из %s", minutes(f.Quiet), minutes(p.idle))
	}
	if f.TTYQuiet < p.idle {
		against("tty", "ввод в терминал %s назад", minutes(f.TTYQuiet))
	}
	inputChecks(f, against)
	workChecks(f, restart, against)
	lossChecks(f, p, against)
	v.Restart = len(v.Against) == 0
	return v
}

// LostCron — таймер CronCreate, который перезапуск не вернёт (Verdict.Lost).
type LostCron struct {
	ID        string `json:"id"`
	Cron      string `json:"cron"`
	Prompt    string `json:"prompt"`
	Recurring bool   `json:"recurring"`
}

// lossChecks — новый процесс поднимется со всем, что было: будильники, аргументы запуска. Таймеры CronCreate --resume
// возвращает только свои — созданные в этой беседе и `tasks` записи session_cron_carry; унаследованные через /clear
// (`otherArmed`) теряет: лид 30.10 02:09 CET, 2.1.312→2.1.314, CronList после подъёма пуст; проба aguard-master
// 30.10 на 2.1.314 — свой таймер после --resume на месте. Их перезапуск не держит: они уходят в Verdict.Lost, и cl
// поднимает сессию со строкой роли — роль ставит свои таймеры заново сама.
func lossChecks(f facts, p allowance, against againstFunc) {
	if len(f.Transcript.Wakeups) > 0 {
		against("wakeup", "%s", strings.Join(f.Transcript.Wakeups, ", "))
	}
	if p.argsProblem != "" {
		against("args", "%s", p.argsProblem)
	}
}

// inherited — таймеры прошлой беседы: --resume их не вернёт.
// Унаследованный с двойником среди своих (тот же cron, prompt и recurring) уже поставлен заново: набор прошёл, а
// CronDelete старого — нет. Он не потеря, иначе набор повторялся бы и копил копии (проверка git-admin 30.10).
func inherited(crons []cronJob) []cronJob {
	own := map[cronKey]bool{}
	for _, job := range crons {
		if job.Own {
			own[cronTwin(job)] = true
		}
	}
	var jobs []cronJob
	for _, job := range crons {
		if !job.Own && !own[cronTwin(job)] {
			jobs = append(jobs, job)
		}
	}
	return jobs
}

type cronKey struct {
	cron, prompt string
	recurring    bool
}

func cronTwin(job cronJob) cronKey {
	return cronKey{job.Cron, strings.Join(strings.Fields(job.Prompt), " "), job.Recurring}
}

// againstFunc — записать довод против.
type againstFunc func(code, format string, args ...any)

// sessionChecks — интерактивная сессия закончила ход; a — что с ней сделают.
func sessionChecks(f facts, a action, against againstFunc) {
	if f.Kind != "interactive" {
		against("kind", "сессия не интерактивная: %q", f.Kind)
	}
	if f.Status != "idle" {
		against("status", "Claude Code: %s", OrDash(f.Status))
	}
	hookChecks(f, a, against)
}

// inputChecks — в поле ввода нет набранного текста: то, что aguard или cl пошлют в сессию, его затёрло бы.
func inputChecks(f facts, against againstFunc) {
	// Ввод после последней записи хода — в поле может лежать набранный, но не отправленный текст: перезапуск его
	// потеряет, и порог простоя тут ни при чём. Ядро обновляет atime pts раз в 8 с, поэтому ввод в пределах 8 с до
	// конца хода тоже считается: лишний раз не перезапустить дешевле, чем потерять черновик. Экран прочитан и поле
	// пусто — черновика нет, а ввод — байты фокуса: Claude Code включает отчёт о фокусе (ESC[?1004h), и Konsole при
	// переключении на вкладку шлёт его в терминал (решение пользователя 26.10 20:54 CET, вариант 2).
	if !f.Clean && f.TTYQuiet <= f.TurnQuiet+ttyGranularity && (f.PromptErr != "" || f.Prompt != screen.Empty) {
		against("draft", "ввод в терминал после конца хода (%s назад): может быть набранный текст", minutes(f.TTYQuiet))
	}
	// Текст, набранный во время хода, старше конца хода, и проверка выше его не видит: поле ввода — по экрану.
	switch {
	case f.PromptErr != "":
		against("screen", "экран терминала: %s", f.PromptErr)
	case f.Prompt == screen.Draft && f.Suggestion:
		// подсказка Claude Code: набранного нет, /compact и /clear её заменяют
	case f.Prompt == screen.Draft:
		against("screen", "в поле ввода набранный текст")
	case f.Prompt != screen.Empty:
		against("screen", "поля ввода на экране не видно")
	}
}

// workChecks — ничего не идёт, что действие a убило бы: дети процесса, инструменты, фоновые задачи и суб-агенты.
// Набор под pty процесс не трогает: дети и фоновые задачи живут, против — только суб-агенты у /clear.
func workChecks(f facts, a action, against againstFunc) {
	if len(f.Children) > 0 && a == restart {
		against("children", "дети процесса: %s", strings.Join(f.Children, ", "))
	}
	if len(f.Transcript.Pending) > 0 {
		against("pending", "инструмент без результата: %s", strings.Join(f.Transcript.Pending, ", "))
	}
	switch {
	case len(f.Transcript.Tasks) > 0 && a == restart:
		against("tasks", "фоновые задачи и суб-агенты: %s", strings.Join(f.Transcript.Tasks, ", "))
	case len(f.Transcript.Agents) > 0 && a == typedClear:
		against("agents", "суб-агенты работают: %s", strings.Join(f.Transcript.Agents, ", "))
	}
}

// hookChecks — состояние от chooks (матрица пользователя 26.10 16:33–16:35 CET в транскрипте chooks-master: пауза —
// сжимать и сбрасывать можно, работают суб-агенты — только сжимать, ждёт ответа — «сброс должен быть запрещён»).
// done и idle — ход закончен без фоновых задач и таймеров (по списку Claude Code на Stop), idle — человек после
// этого открыл вкладку. waiting — вопрос человеку (минутное напоминание с chooks ae7ceb3 его не ставит): против,
// как working и error. paused — Stop при фоновых задачах или таймерах: годится, только если задач в .tasks нет
// (монитор артефакта не в счёт — см. HookTasks), а пауза объяснена таймерами (по транскрипту или по .tasks) или
// монитором артефакта; живые фоновые команды, мониторы и суб-агенты перезапуск cl (сжатие и сброс без pty — тоже
// перезапуск) убил бы. Набор под pty (a) их не трогает, кроме суб-агентов у /clear. Записи нет, а запросов в беседе
// не было (после /clear хода не было — и Stop тоже): ждать нечего, простой видно по статусу реестра.
func hookChecks(f facts, a action, against againstFunc) {
	switch f.HookState {
	case "done", "idle":
	case "":
		if !f.Transcript.Context.Request.IsZero() {
			against("hook", "chooks: нет записи")
		}
	case "paused":
		switch {
		case f.HookTasks && a == restart:
			against("hook-tasks", "chooks: на последнем Stop были фоновые задачи")
		case f.HookAgents && a == typedClear:
			against("hook-agents", "chooks: на последнем Stop работали суб-агенты")
		}
		if !f.HookTasks && !f.HookWaits && len(f.Transcript.Crons) == 0 {
			against("hook-paused", "chooks: paused, а таймеров в транскрипте нет — непонятно, чего ждёт")
		}
	default:
		against("hook", "chooks: %s", OrDash(f.HookState))
	}
}

// cronList — таймеры для довода: id и начало текста.
func cronList(crons []cronJob) string {
	list := make([]string, len(crons))
	for i, c := range crons {
		list[i] = c.ID + " «" + short(c.Prompt) + "»"
	}
	return strings.Join(list, ", ")
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
