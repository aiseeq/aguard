// Package waits распознаёт в транскрипте ожидания, которые агент заводит сам: цикл с sleep, wfor, одиночный sleep,
// скрипт ожидания, фоновую команду до уведомления, Monitor, письмо до ответа, ScheduleWakeup, TaskOutput, у codex —
// серию опросов запущенной команды (write_stdin). Распознаватель идёт по событиям одного файла и отдаёт
// законченные ожидания; его состояние можно сохранить и продолжить после чтения дописанных строк. Это локальная копия
// akit для agentwait, которой нужно переживать проходы процесса. Общий для замеров ночного разбора (dreamstats) и чтобы
// числа ожиданий сходились.
package waits

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/aiseeq/akit/shellcmd"
	"github.com/aiseeq/akit/transcript"
)

// Способы ожидания (Wait.How).
const (
	Wfor       = "wfor"
	Loop       = "loop"        // while/until/for с sleep в теле
	Sleep      = "sleep"       // одиночный sleep от minSleep
	Script     = "scr" + "ipt" // wait*.sh, wait-file, inotifywait, worker wait
	Background = "background"  // run_in_background (или Bash, уведённый в фон) до task-notification
	Monitor    = "monitor"     // Monitor до первого уведомления
	Send       = "send"        // SendMessage до письма того же адресата
	Wakeup     = "wakeup"      // ScheduleWakeup до следующей реплики или уведомления
	StdinPoll  = "stdin_poll"  // codex: пустые write_stdin запущенной команды
	TaskOutput = "taskoutput"  // TaskOutput до его вывода
)

// Исходы (Wait.Outcome).
const (
	Done    = "done"    // дождался
	Timeout = "timeout" // вышел по своему таймауту, условие не наступило
	Stopped = "stopped" // прерван: человек, kill
	None    = "none"    // конца в транскрипте не видно
)

// Wait — одно ожидание. Повтор того же ожидания следом (вышел по таймауту и ждёт снова, без других вызовов между)
// — то же ожидание: Calls растёт, Repeats = Calls-1.
type Wait struct {
	Start, End time.Time // End пуст — конца не видно
	How        string
	Class      string // test, build, deploy, graft, agent, human, external, other — что ждали, по команде
	Command    string // короткий текст: команда ожидания, у stdin_poll — команда, которую опрашивали
	Calls      int
	Repeats    int
	// Tokens — расход ходов, которые завели ожидание и повторы: у Claude input+cache_creation+cache_read+output
	// ответа с вызовом (один раз на ответ), у codex — last_token_usage хода с вызовом.
	Tokens  int64
	Outcome string
	Line    int      // строка первого вызова
	CallIDs []string // id вызовов по порядку
}

// commandWidth — сколько рун команды держать в Wait.Command.
const commandWidth = 200

// minSleep — одиночный sleep короче этого — пауза, не ожидание.
const minSleep = 5 * time.Second

// pending — незаконченное ожидание.
type pending struct {
	w       Wait
	limit   time.Duration // timeout N вокруг цикла: вызов дольше limit-1с — вышел по таймауту
	callAt  time.Time     // начало последнего вызова: таймаут считает свой вызов, не всё ожидание
	replies map[string]bool
	to      string // Send: кому
	session string // сессия запущенной команды codex, которую опрашивают (StdinPoll или ожидание дольше 30 с)
	codex   bool   // начато вызовом codex: дольше 30 с его команду опрашивают пустым write_stdin
	outSeen bool   // Wakeup: вывод вызова уже был, ждём следующую реплику
}

// Recognizer — распознаватель одного файла.
type Recognizer struct {
	cur      *pending            // ожидание переднего плана, к которому может прийти повтор
	byCall   map[string]*pending // вызовы, ждущие вывода
	bg       map[string]*pending // ждут task-notification, по id вызова
	sends    []*pending
	wakeup   *pending
	tokensTo *pending          // codex: следующий token_count — ход этого ожидания
	tasks    map[string]string // id фоновой задачи Claude → её команда
	// polls — опросы codex по сессии: между опросами одной сессии боец смотрит ps и tail, ожидание длится до
	// правки, конца команды или конца файла.
	polls      map[string]*pending
	shellText  map[string]string // команда вызова, ждущего вывода: вывод «session N» привяжет её к сессии
	sessionCmd map[string]string // сессия codex → команда, которая её запустила
	prevCmd    string            // последняя команда не-ожидание: опрашивают её, если сессия не найдена
}

// New — распознаватель с начала файла или с любой строки (Open прошлого прохода).
func New() *Recognizer {
	return &Recognizer{byCall: map[string]*pending{}, bg: map[string]*pending{}, tasks: map[string]string{},
		polls: map[string]*pending{}, shellText: map[string]string{}, sessionCmd: map[string]string{}}
}

// Add принимает событие и отдаёт ожидания, которые им закончились.
func (r *Recognizer) Add(ev transcript.Event) []Wait {
	switch ev.Role {
	case transcript.Cmd:
		return r.cmd(ev)
	case transcript.Out:
		return r.out(ev)
	case transcript.Note:
		if ev.Who == "token_count" {
			if r.tokensTo != nil && ev.Tokens != nil && ev.Tokens.Last != nil {
				r.tokensTo.w.Tokens += ev.Tokens.Last.Total
			}
			r.tokensTo = nil
			return nil
		}
		done := r.wake(ev)
		if ev.Who == "task-notification" {
			done = append(done, r.notified(ev)...)
		}
		return done
	case transcript.User, transcript.Peer:
		done := r.wake(ev)
		if ev.Role == transcript.Peer {
			done = append(done, r.replied(ev)...)
		}
		return done
	}
	return nil
}

// Open — строка самого раннего незаконченного ожидания: с неё перечитать файл на следующем проходе.
func (r *Recognizer) Open() (line int, ok bool) {
	for _, p := range r.all() {
		if !ok || p.w.Line < line {
			line, ok = p.w.Line, true
		}
	}
	return line, ok
}

// Close — конец файла: незаконченные ожидания с исходом none (у переднего плана с выводом — его исход).
func (r *Recognizer) Close() []Wait {
	out := finishAll(r.all())
	*r = *New()
	return out
}

// finishAll — ожидания по порядку первого вызова (строка, затем id вызова: в одной записи бывает несколько).
func finishAll(ps []*pending) []Wait {
	slices.SortFunc(ps, func(a, b *pending) int {
		return cmp.Or(a.w.Line-b.w.Line, strings.Compare(a.w.CallIDs[0], b.w.CallIDs[0]))
	})
	out := make([]Wait, 0, len(ps))
	for _, p := range ps {
		out = append(out, finish(p))
	}
	return out
}

// all — все незаконченные ожидания без повторов.
func (r *Recognizer) all() []*pending {
	seen := map[*pending]bool{}
	var ps []*pending
	add := func(p *pending) {
		if p != nil && !seen[p] {
			seen[p] = true
			ps = append(ps, p)
		}
	}
	add(r.cur)
	for _, p := range r.byCall {
		add(p)
	}
	for _, p := range r.bg {
		add(p)
	}
	for _, p := range r.sends {
		add(p)
	}
	add(r.wakeup)
	for _, p := range r.polls {
		add(p)
	}
	return ps
}

// endPolls — правка: опросы codex закончены (агент перестал ждать и правит).
func (r *Recognizer) endPolls() []Wait {
	var ps []*pending
	for _, sid := range slices.Sorted(maps.Keys(r.polls)) {
		p := r.polls[sid]
		delete(r.polls, sid)
		for id, q := range r.byCall {
			if q == p {
				delete(r.byCall, id)
			}
		}
		ps = append(ps, p)
	}
	return finishAll(ps)
}

func finish(p *pending) Wait {
	if p.w.Outcome == "" {
		p.w.Outcome = None
	}
	p.w.Repeats = p.w.Calls - 1
	return p.w
}

// breakChain — вызов не-ожидание или ожидание другого рода: ожидание переднего плана закончено.
func (r *Recognizer) breakChain() []Wait {
	p := r.cur
	r.cur = nil
	if p == nil {
		return nil
	}
	for id, q := range r.byCall {
		if q == p {
			delete(r.byCall, id) // вывода не дождались, а агент уже делает другое
		}
	}
	return []Wait{finish(p)}
}

func (r *Recognizer) cmd(ev transcript.Event) []Wait {
	r.tokensTo = nil
	how, text := kind(ev)
	bg := ev.Who == "Bash" && background(ev.Raw)
	if how == "" && bg {
		how, text = Background, ev.Text
	}
	if how == "" {
		var done []Wait
		if edit(ev) {
			done = r.endPolls()
		}
		if shells[ev.Who] {
			r.prevCmd = ev.Text
			r.shellText[ev.CallID] = ev.Text
		}
		return append(done, r.breakChain()...)
	}
	if how == StdinPoll {
		return r.poll(ev)
	}
	foreground := !bg && (how == Wfor || how == Loop || how == Sleep || how == Script || how == TaskOutput)
	if foreground && r.repeat(how, ev) {
		return nil
	}
	done := r.breakChain()
	p := &pending{w: Wait{Start: ev.Time, How: how, Class: r.class(how, text), Command: transcript.Short(text, commandWidth),
		Calls: 1, Line: ev.Line, CallIDs: []string{ev.CallID}}, limit: limitOf(ev.Text), callAt: ev.Time,
		replies: map[string]bool{}, codex: CodexShell(ev.Who)}
	r.count(p, ev)
	switch {
	case foreground:
		r.cur = p
		r.byCall[ev.CallID] = p
	case how == Send:
		p.to = sendTo(ev.Raw)
		r.sends = append(r.sends, p)
	case how == Wakeup:
		if r.wakeup != nil {
			done = append(done, finish(r.wakeup))
		}
		r.wakeup = p
		r.byCall[ev.CallID] = p
	default: // фон и Monitor ждут task-notification; вывод вызова Bash в фоне даёт id задачи
		r.bg[ev.CallID] = p
		r.byCall[ev.CallID] = p
	}
	return done
}

// repeat — повтор ожидания переднего плана тем же способом, когда прошлый вызов не дождался (дождавшись, ждут
// уже другое): то же ожидание, вызов прибавлен к нему.
func (r *Recognizer) repeat(how string, ev transcript.Event) bool {
	p := r.cur
	if p == nil || p.w.How != how || p.w.Outcome == Done {
		return false
	}
	p.w.Calls++
	p.w.CallIDs = append(p.w.CallIDs, ev.CallID)
	p.w.End, p.w.Outcome = time.Time{}, ""
	p.limit, p.callAt = limitOf(ev.Text), ev.Time
	r.count(p, ev)
	r.byCall[ev.CallID] = p
	return true
}

// poll — опрос запущенной команды codex: продолжает ожидание своей сессии или начинает новое.
func (r *Recognizer) poll(ev transcript.Event) []Wait {
	done := r.breakChain()
	sid := SessionOf(ev)
	if p := r.polls[sid]; p != nil {
		p.w.Calls++
		p.w.CallIDs = append(p.w.CallIDs, ev.CallID)
		p.w.End, p.w.Outcome = time.Time{}, ""
		r.count(p, ev)
		r.byCall[ev.CallID] = p
		return done
	}
	text, ok := r.sessionCmd[sid]
	if !ok {
		text = r.prevCmd
	}
	p := &pending{w: Wait{Start: ev.Time, How: StdinPoll, Class: r.class(StdinPoll, text), Command: transcript.Short(text, commandWidth),
		Calls: 1, Line: ev.Line, CallIDs: []string{ev.CallID}}, session: sid, replies: map[string]bool{}}
	r.count(p, ev)
	r.polls[sid] = p
	r.byCall[ev.CallID] = p
	return done
}

// count — расход хода вызова: у Claude из ответа (один раз на ответ), у codex — следующий token_count.
func (r *Recognizer) count(p *pending, ev transcript.Event) {
	if ev.Reply == nil {
		r.tokensTo = p
		return
	}
	if u := ev.Reply.Usage; u != nil && !p.replies[ev.Reply.ID] {
		p.replies[ev.Reply.ID] = true
		p.w.Tokens += u.Input + u.CacheWrite + u.CacheRead + u.Output
	}
}

var (
	bgStarted = regexp.MustCompile(`running in background with ID: (\w+)`)
	bgMoved   = regexp.MustCompile(`moved to the background \(ID: (\w+)\)`)
	exit124   = regexp.MustCompile(`(?i)exit code:? 124\b`)
	stopped   = regexp.MustCompile(`(?i)interrupted by user|doesn't want to proceed|rejected|\[Request interrupted`)
	// sessionMark — сессия запущенной команды codex в выводе: «Process running with session ID N», JSON ячейки exec
	// ({"session_id":N,…}), строка бойца «идёт, session N». exited — команда кончилась: в выводе её код.
	sessionMark = regexp.MustCompile(`(?i)session[ _]?(?:id)?"?\s*[:= ]\s*(\d+)`)
	exited      = regexp.MustCompile(`"exit_code"\s*:|Process exited with code`)
)

// Running returns the session for a command that is still active; ok is false for other output or an ended command.
func Running(text string) (session string, ok bool) {
	if exited.MatchString(text) {
		return "", false
	}
	if m := sessionMark.FindStringSubmatch(text); m != nil {
		return m[1], true
	}
	return "", false
}

func (r *Recognizer) out(ev transcript.Event) []Wait {
	if cmd, ok := r.shellText[ev.CallID]; ok {
		delete(r.shellText, ev.CallID)
		if sid, ok := Running(ev.Text); ok {
			r.sessionCmd[sid] = cmd
		}
	}
	p := r.byCall[ev.CallID]
	if p == nil {
		return nil
	}
	delete(r.byCall, ev.CallID)
	if p.session != "" {
		p.w.End = ev.Time
		if _, ok := Running(ev.Text); ok {
			return nil // команда ещё идёт
		}
		p.w.Outcome = Done
		if p.w.How != StdinPoll {
			p.w.Outcome = outcome(p, ev)
		}
		delete(r.polls, p.session)
		return []Wait{finish(p)}
	}
	// exec_command ждёт не дольше 30 с: wfor или цикл codex идёт дальше в своей сессии, её опросы - то же ожидание
	if sid, ok := Running(ev.Text); ok && p.codex && r.cur == p {
		p.session = sid
		r.polls[sid] = p
		r.cur = nil
		return nil
	}
	if _, inBG := r.bg[ev.CallID]; inBG { // вывод запуска фоновой задачи: её id — для класса ждущих её вывод
		if m := bgStarted.FindStringSubmatch(ev.Text); m != nil {
			r.tasks[m[1]] = p.w.Command
		}
		return nil
	}
	if m := bgMoved.FindStringSubmatch(ev.Text); m != nil { // Bash не уложился в свой таймаут и ушёл в фон
		r.tasks[m[1]] = p.w.Command
		if r.cur == p {
			r.cur = nil
		}
		r.bg[ev.CallID] = p
		return nil
	}
	if p.w.How == Wakeup {
		p.outSeen = true
		return nil
	}
	p.w.End = ev.Time
	p.w.Outcome = outcome(p, ev)
	return nil
}

// outcome — исход вызова переднего плана по его выводу и длительности.
func outcome(p *pending, ev transcript.Event) string {
	text := ev.Text
	switch {
	case ev.Error && stopped.MatchString(text):
		return Stopped
	case p.w.How == Wfor && strings.Contains(text, "wfor: done in"):
		return Done
	case p.w.How == Wfor && strings.Contains(text, "wfor: not yet after"):
		return Timeout
	case p.w.How == Wfor:
		return None
	case p.w.How == TaskOutput && strings.Contains(text, "<retrieval_status>timeout"):
		return Timeout
	case exit124.MatchString(text), strings.Contains(text, "Command timed out"):
		return Timeout
	case p.limit > 0 && !p.callAt.IsZero() && ev.Time.Sub(p.callAt) >= p.limit-time.Second:
		return Timeout
	}
	return Done
}

var (
	notifyCall   = regexp.MustCompile(`<tool-use-id>([^<]+)</tool-use-id>`)
	notifyStatus = regexp.MustCompile(`<status>([^<]+)</status>`)
	notifyExit   = regexp.MustCompile(`exit code (\d+)`)
)

// notified — уведомление фоновой задачи: её ожидание закончено.
func (r *Recognizer) notified(ev transcript.Event) []Wait {
	m := notifyCall.FindStringSubmatch(ev.Text)
	if m == nil {
		return nil
	}
	p := r.bg[m[1]]
	if p == nil {
		return nil
	}
	delete(r.bg, m[1])
	delete(r.byCall, m[1])
	p.w.End = ev.Time
	status := ""
	if s := notifyStatus.FindStringSubmatch(ev.Text); s != nil {
		status = s[1]
	}
	code := ""
	if c := notifyExit.FindStringSubmatch(ev.Text); c != nil {
		code = c[1]
	}
	switch {
	case status == "killed" || status == "stopped":
		p.w.Outcome = Stopped
	case code == "124", p.w.How == Wfor && code == "1":
		p.w.Outcome = Timeout
	default:
		p.w.Outcome = Done
	}
	return []Wait{finish(p)}
}

// replied — письмо сессии: ожидания ответа от неё закончены.
func (r *Recognizer) replied(ev transcript.Event) []Wait {
	var done []Wait
	r.sends = slices.DeleteFunc(r.sends, func(p *pending) bool {
		to := strings.TrimSpace(p.to)
		name, _, _ := strings.Cut(to, " [")
		if to == "" || to != ev.From && name != ev.Who {
			return false
		}
		p.w.End, p.w.Outcome = ev.Time, Done
		done = append(done, finish(p))
		return true
	})
	return done
}

// wake — реплика, письмо или уведомление после ScheduleWakeup: агент проснулся.
func (r *Recognizer) wake(ev transcript.Event) []Wait {
	p := r.wakeup
	if p == nil || !p.outSeen {
		return nil
	}
	r.wakeup = nil
	p.w.End, p.w.Outcome = ev.Time, Done
	return []Wait{finish(p)}
}

// shells — инструменты, которые запускают команду: Bash у Claude, у codex — exec и старые вызовы.
var shells = map[string]bool{"Bash": true, "exec": true, "exec_command": true, "shell": true, "shell_command": true}

// CodexShell reports whether a tool runs a codex command: only its output can name a running session.
func CodexShell(who string) bool {
	return who != "Bash" && shells[who]
}

var (
	stdinChars = regexp.MustCompile("\\bchars\\s*:\\s*([\"'`])(.)")
)

// kind — способ ожидания вызова и текст для класса; "" — вызов не ожидание.
func kind(ev transcript.Event) (how, text string) {
	switch ev.Who {
	case "Monitor":
		return Monitor, ev.Text
	case "SendMessage":
		return Send, ev.Text
	case "ScheduleWakeup":
		return Wakeup, ev.Text
	case "TaskOutput":
		return TaskOutput, ev.Text
	case "write_stdin":
		if emptyStdin(ev.Raw) {
			return StdinPoll, ""
		}
		return "", ""
	}
	if !shells[ev.Who] {
		return "", ""
	}
	if ev.Who == "exec" && strings.Contains(ev.Text, "write_stdin(") && !strings.Contains(ev.Text, "exec_command(") {
		if m := stdinChars.FindStringSubmatch(ev.Text); m != nil && m[2] != m[1] {
			return "", "" // ввод в запущенную команду, не опрос
		}
		return StdinPoll, ""
	}
	if how := CommandHow(ev.Text); how != "" {
		return how, ev.Text
	}
	return "", ""
}

// CommandHow — способ ожидания по вызовам программ в команде оболочки.
func CommandHow(command string) string {
	calls, err := shellcmd.Calls(command)
	var nested *shellcmd.NestedError
	if err != nil && !errors.As(err, &nested) {
		return ""
	}
	var sleep bool
	for i := range calls {
		call := &calls[i]
		if call.Background {
			continue
		}
		switch {
		case call.Name == "wfor" && wforAction(call.Args):
			return Wfor
		case call.Name == "sleep" && call.Loop:
			return Loop
		case isWaitScript(call):
			return Script
		case call.Name == "sleep" && len(call.Args) > 0:
			d, parseErr := durationArg(call.Args[0])
			if parseErr != nil { //nolint:silent-error-handling // Неподходящий аргумент sleep не задаёт ожидание.
				continue
			}
			if d >= minSleep {
				sleep = true
			}
		}
	}
	if sleep {
		return Sleep
	}
	return ""
}

func wforAction(args []string) bool {
	for _, arg := range args {
		if slices.Contains([]string{"file", "gone", "line", "exit", "time", "ok", "idle"}, arg) || strings.HasPrefix(arg, "exit-") {
			return true
		}
	}
	return false
}

var waitScriptName = regexp.MustCompile(`^wait[-_a-z]*\.sh$`)

func isWaitScript(call *shellcmd.Call) bool {
	if waitScriptName.MatchString(call.Name) || call.Name == "wait-file" || call.Name == "inotifywait" {
		return true
	}
	if call.Name == "worker" && len(call.Args) > 0 && call.Args[0] == "wait" {
		return true
	}
	if shellcmd.IsShell(call.Name) || call.Name == "source" || call.Name == "." {
		for _, arg := range call.Args {
			if arg == "--" {
				continue
			}
			if strings.HasPrefix(arg, "-") {
				continue
			}
			return waitScriptName.MatchString(path.Base(arg)) || path.Base(arg) == "wait-file"
		}
	}
	return false
}

func durationArg(arg string) (time.Duration, error) {
	unit := ""
	if len(arg) > 0 && strings.ContainsRune("smh", rune(arg[len(arg)-1])) {
		unit, arg = arg[len(arg)-1:], arg[:len(arg)-1]
	}
	return duration(arg, unit)
}

var (
	pollSession = regexp.MustCompile(`session_id"?\s*[:=]\s*"?(\d+)`)
)

// SessionOf returns the session polled by write_stdin.
func SessionOf(ev transcript.Event) string {
	for _, s := range []string{ev.Text, string(ev.Raw)} {
		if m := pollSession.FindStringSubmatch(s); m != nil {
			return m[1]
		}
	}
	return ""
}

// edit — вызов правит файлы: Edit, Write, apply_patch, sub, sed -i и подобные.
func edit(ev transcript.Event) bool {
	switch ev.Who {
	case "Edit", "Write", "MultiEdit", "NotebookEdit", "apply_patch":
		return true
	}
	if strings.Contains(ev.Text, "*** Begin Patch") {
		return true
	}
	calls, err := shellcmd.Calls(ev.Text)
	var nested *shellcmd.NestedError
	if err != nil && !errors.As(err, &nested) {
		return false
	}
	for i := range calls {
		if isEditCall(&calls[i]) {
			return true
		}
	}
	return false
}

func isEditCall(call *shellcmd.Call) bool {
	switch call.Name {
	case "sub":
		return true
	case "sed":
		return hasOption(call.Args, "-i")
	case "perl":
		return hasOption(call.Args, "-p") && hasOption(call.Args, "-i") || slices.Contains(call.Args, "-pi") || slices.Contains(call.Args, "-ip")
	case "gofmt":
		return hasOption(call.Args, "-w")
	case "git":
		return len(call.Args) > 0 && slices.Contains([]string{"commit", "apply", "checkout", "restore"}, call.Args[0])
	case "graft":
		return len(call.Args) > 0 && call.Args[0] == "commit"
	}
	return false
}

func hasOption(args []string, option string) bool {
	for _, arg := range args {
		if arg == option || strings.HasPrefix(arg, option) && len(arg) > len(option) && arg[0:len(option)] == option {
			return true
		}
	}
	return false
}

// emptyStdin — у вызова write_stdin (старый вид, function_call) нет ввода: это опрос.
func emptyStdin(raw json.RawMessage) bool {
	var a struct {
		Chars string `json:"chars"`
	}
	return json.Unmarshal(raw, &a) != nil || a.Chars == ""
}

// background — Bash с run_in_background.
func background(raw json.RawMessage) bool {
	var a struct {
		Background bool `json:"run_in_background"`
	}
	return json.Unmarshal(raw, &a) == nil && a.Background
}

// sendTo — адресат SendMessage.
func sendTo(raw json.RawMessage) string {
	var a struct {
		To string `json:"to"`
	}
	if json.Unmarshal(raw, &a) != nil {
		return ""
	}
	return a.To
}

// limitOf — timeout N вокруг команды; 0 — нет.
func limitOf(cmd string) time.Duration {
	calls, err := shellcmd.Calls(cmd)
	var nested *shellcmd.NestedError
	if err != nil && !errors.As(err, &nested) {
		return 0
	}
	for i := range calls {
		call := &calls[i]
		if call.Name != "timeout" {
			continue
		}
		for _, arg := range call.Args {
			if strings.HasPrefix(arg, "-") {
				continue
			}
			if d, err := durationArg(arg); err == nil {
				return d
			}
			break
		}
	}
	return 0
}

// duration — число и единица из команды (sleep 5, timeout 4m); без единицы — секунды.
func duration(n, unit string) (time.Duration, error) {
	if unit == "" {
		unit = "s"
	}
	d, err := time.ParseDuration(n + unit)
	if err != nil {
		return 0, fmt.Errorf("длительность %s%s: %w", n, unit, err)
	}
	return d, nil
}

// classes — что ждут, по тексту команды: первое совпадение.
var classes = []struct {
	name string
	re   *regexp.Regexp
}{
	{"graft", regexp.MustCompile(`\bgraft\b|load\.lock`)},
	// test — и проверки без тестов: vet, линтеры, приёмка бойца (accept*.sh). goFlags — флаги go до подкоманды (go -C dir test).
	{"test", regexp.MustCompile(`(?i)\bgo` + goFlags + `\s(?:test|vet)\b|gotest|pytest|vitest|playwright|\be2e\b|npm (?:run )?test|\btest[-_.\w]*\.log\b|== done|\bFAIL\b|accept[-\w]*\.sh|scene-orders|probes\.sh|\bglint\s+check\b|staticcheck|golangci-lint`)},
	{"build", regexp.MustCompile(`\bgo` + goFlags + `\sbuild\b|docker build|\bmake\b|npm run build|cargo build`)},
	{"deploy", regexp.MustCompile(`(?i)deploy|ansible|kubectl|helm\b|systemctl|rsync`)},
	{"human", regexp.MustCompile(`said\.tsv|user-word|AskUserQuestion`)},
	{"agent", regexp.MustCompile(`subagents/|\bagent-\w+|\bcodex\b|\bworker\b|\bclaude\s+-p\b|\.claude/projects|cc-socks`)},
	{"external", regexp.MustCompile(`\bcurl\b|\bwget\b|https?://|\bgh (?:run|pr|api)\b|\bssh\s|journalctl`)},
}

const goFlags = `(?:\s+-C\s+\S+|\s+-\w+(?:=\S+)?)*`

// taskOutput — файл вывода фоновой задачи Claude: ждут её, класс — по её команде.
var taskOutput = regexp.MustCompile(`/tasks/(\w+)\.output`)

// class — что ждали: письмо — агента, остальное — по команде (или команде фоновой задачи, чей вывод ждут).
func (r *Recognizer) class(how, text string) string {
	if how == Send {
		return "agent"
	}
	if m := taskOutput.FindStringSubmatch(text); m != nil {
		if cmd, ok := r.tasks[m[1]]; ok {
			text = cmd + "\n" + text
		}
	}
	for _, c := range classes {
		if c.re.MatchString(text) {
			return c.name
		}
	}
	return "other"
}
