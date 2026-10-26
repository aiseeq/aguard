package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aiseeq/aguard/internal/restart"
	"github.com/aiseeq/akit/session"
)

// launcher — запуск claude под присмотром cl: claude — дочерний процесс в своём scope, cl ждёт его выхода и раз в
// check_seconds спрашивает aguard, перезапустить ли сессию на новую версию Claude Code или сжать беседу.
type launcher struct {
	systemdRun string
	claude     string
	home       string
	configFile string
	// registry — реестр сессий Claude Code и /proc: для --cl-check.
	registry session.Registry
	// aguard — сокет aguard: решение о сессии (ask.go); пусто — не спрашивать, а без ответа — ничего не делать.
	aguard string
	// name — имя, которое дал cl (или пользователь через -n); после перезапуска — имя из реестра Claude Code.
	name string
	// console — claude под pty cl (restart.pty_names); nil — claude получает терминал cl напрямую.
	console *console
}

// outcome — чем кончился один запуск claude.
type outcome struct {
	err error
	// restart — сессия вышла по сигналу перезапуска от cl; sessionID, name, version — что поднять снова.
	restart   bool
	sessionID string
	name      string
	version   string
	// compact — перед подъёмом сжать беседу (`claude -p --resume <сессия> /compact`); crons — у сессии были таймеры:
	// подъём со строкой роли, чтобы роль поставила их заново.
	compact bool
	crons   bool
	// reset — поднять не прежнюю беседу, а новую: строка роли и файл возобновления.
	reset bool
	// typed — не перезапускать, а набрать эту команду в живой сессии под pty (restart.type_mode act).
	typed string
}

// humanQuiet — сколько человек не должен ничего вводить, чтобы cl набирал в его сессии.
const humanQuiet = time.Minute

// Что случилось с сессией перед запуском claude: от этого зависит {why} строки роли.
const (
	eventStart   = "старт"
	eventRestart = "перезапуск"
	eventCompact = "сжатие"
	eventReset   = "сброс"
)

// compactTimeout — срок на `claude -p … /compact`: сжатие большой беседы идёт минуты, дольше — зависло.
const compactTimeout = 10 * time.Minute

// supervise запускает claude и поднимает его снова с той же беседой, если он вышел по сигналу перезапуска от cl.
// Любой другой выход (/exit, Ctrl+C, Ctrl+D, закрытие вкладки, сбой) — выход cl с кодом claude.
func (l launcher) supervise(pre, userArgs []string) int {
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP)
	if cfg, err := loadConfig(l.configFile, l.home); err == nil && matchAny(cfg.Restart.PtyNames, l.name) {
		l.console = openConsole(os.Stdin, os.Stdout, func(s string) {
			journal(cfg.Restart.Journal, time.Now(), l.name+": "+s)
		})
	}
	if l.console != nil {
		defer l.console.restore()
		defer func() {
			if r := recover(); r != nil {
				l.console.restore()
				panic(r)
			}
		}()
	}
	keep, argsProblem := restartArgs(userArgs)
	args := append(append([]string{}, pre...), userArgs...)
	if bare(userArgs) {
		args = l.withRoleLine(args, l.name, eventStart)
	}
	for n := 0; ; n++ {
		unit := strconv.Itoa(os.Getpid())
		if n > 0 {
			unit += "-r" + strconv.Itoa(n)
		}
		cmd := exec.Command(l.systemdRun, scopeArgv(l.systemdRun, l.claude, l.name, unit, args)[1:]...)
		finish, err := l.launch(cmd)
		if err != nil {
			fail(fmt.Errorf("запуск claude: %w", err))
		}
		out := l.watch(cmd, sigs, argsProblem)
		finish()
		if !out.restart {
			return exitCode(out.err)
		}
		l.name = out.name
		if out.reset {
			args = l.withRoleLine(append(append(sessionFlags(l.home), "-n", out.name), keep...), out.name, eventReset)
			continue
		}
		event := eventRestart
		if out.compact {
			event = eventCompact
			sig, err := l.compactSession(sigs, strconv.Itoa(os.Getpid())+"-c"+strconv.Itoa(n+1), keep, out)
			if err != nil {
				cfg, cfgErr := loadConfig(l.configFile, l.home)
				journal(cfg.Restart.Journal, time.Now(), fmt.Sprintf("%s: %v — поднимаю без сжатия", out.name, errors.Join(err, cfgErr)))
			}
			if sig != 0 {
				return 128 + int(sig)
			}
		}
		args = append(append(sessionFlags(l.home), "-n", out.name), keep...)
		args = append(args, "--resume", out.sessionID)
		if !out.compact || out.crons {
			args = l.withRoleLine(args, out.name, event)
		}
	}
}

// compactSession сжимает остановленную беседу: `claude -p --resume <сессия> /compact` с флагами сессии в своём scope,
// без терминала. Любой исход, кроме сигнала выхода, — дальше подъём сессии: не сжалось — поднимется без сжатия,
// строка в журнал. TERM или HUP (закрыли вкладку) во время сжатия — сжатие остановить и выйти: сигнал возвращается
// (0 — подъём). Ошибка — сжатие не запустилось.
func (l launcher) compactSession(sigs <-chan os.Signal, unit string, keep []string, out outcome) (syscall.Signal, error) {
	cfg, err := loadConfig(l.configFile, l.home)
	if err != nil {
		journal(cfg.Restart.Journal, time.Now(), fmt.Sprintf("%s: %v — журнал по умолчанию", out.name, err))
	}
	args := append(append(sessionFlags(l.home), keep...), "-p", "--resume", out.sessionID, "/compact")
	cmd := exec.Command(l.systemdRun, scopeArgv(l.systemdRun, l.claude, out.name, unit, args)[1:]...)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	fmt.Fprintln(os.Stderr, "cl: сжимаю беседу (/compact) и подниму её снова — до", compactTimeout)
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("сжатие не запустилось: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(compactTimeout)
	defer timer.Stop()
	var stop syscall.Signal
	for {
		select {
		case err := <-done:
			journal(cfg.Restart.Journal, time.Now(), compactLine(out, time.Since(start), err, stop, output.String()))
			return stop, nil
		case sig := <-sigs:
			if s, ok := sig.(syscall.Signal); ok && (s == syscall.SIGTERM || s == syscall.SIGHUP) {
				stop = s
				if err := signalChild(cmd, syscall.SIGTERM); err != nil {
					journal(cfg.Restart.Journal, time.Now(), fmt.Sprintf("%s: %v", out.name, err))
				}
			}
		case <-timer.C:
			if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				journal(cfg.Restart.Journal, time.Now(), fmt.Sprintf("%s: сжатие дольше %v, не убил: %v", out.name, compactTimeout, err))
			}
		}
	}
}

// compactLine — строка журнала об исходе сжатия: вывод claude — последние 300 байт одной строкой.
func compactLine(out outcome, took time.Duration, err error, stop syscall.Signal, output string) string {
	head := fmt.Sprintf("%s: /compact --resume %s за %v: ", out.name, out.sessionID, took.Round(time.Second))
	switch {
	case stop != 0:
		return head + fmt.Sprintf("остановлено сигналом %v, выхожу", stop)
	case err == nil:
		return head + "сжал, поднимаю"
	}
	tail := strings.Join(strings.Fields(output), " ")
	if len(tail) > 300 {
		tail = "…" + tail[len(tail)-300:]
	}
	return head + fmt.Sprintf("не сжалось (%v: %s) — поднимаю без сжатия", err, tail)
}

// withRoleLine добавляет первой репликой строку роли из конфига; роли нет или конфиг не прочитался — аргументы как
// есть (сбой — в stderr: сессия важнее строки роли).
func (l launcher) withRoleLine(args []string, name, event string) []string {
	line, err := l.roleLine(name, event)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cl: строка роли:", err)
	}
	if line == "" {
		return args
	}
	return append(args, line)
}

func (l launcher) roleLine(name, event string) (string, error) {
	cfg, err := loadConfig(l.configFile, l.home)
	if err != nil {
		return "", err
	}
	version, err := restart.InstalledVersion(l.claude)
	if err != nil {
		return "", err
	}
	why := event + " cl на Claude Code " + version
	resume, err := freshResume(cfg.Restart.ResumeDir, name, time.Duration(max(cfg.Restart.ResumeHours, 1))*time.Hour, time.Now())
	if err != nil {
		return "", err
	}
	line := cfg.roleLine(name, why, resume, event)
	if resume == "" || !strings.Contains(line, resume) {
		return line, nil
	}
	used, err := takeResume(resume)
	if err != nil {
		return line, err // не убрался — отдаётся по прежнему пути: строка роли важнее разовости файла
	}
	return strings.ReplaceAll(line, resume, used), nil
}

// launch запускает claude: под pty cl или прямо на терминале cl. finish — после выхода claude.
func (l launcher) launch(cmd *exec.Cmd) (finish func(), err error) {
	if l.console != nil {
		return l.console.start(cmd)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return func() {}, cmd.Start()
}

// signalChild шлёт сигнал claude; вышедший процесс — не ошибка (его выход и так придёт в done).
func signalChild(cmd *exec.Cmd, sig os.Signal) error {
	if err := cmd.Process.Signal(sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("сигнал %v claude: %w", sig, err)
	}
	return nil
}

// watch ждёт выхода claude; на каждой проверке спрашивает aguard (решение о перезапуске и сжатии — у него,
// internal/restart), пишет в журнал смену решения, а в режиме act перезапускает: SIGTERM — мягкий выход Claude Code
// (беседа дописана, «Resume this session with: claude --resume <id>»). Пока claude рисует экран, cl в терминал не
// пишет: всё — в журнал.
func (l launcher) watch(cmd *exec.Cmd, sigs <-chan os.Signal, argsProblem string) outcome {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	pid := cmd.Process.Pid
	cfg, err := loadConfig(l.configFile, l.home)
	if err != nil {
		journal(cfg.Restart.Journal, time.Now(), fmt.Sprintf("%s pid=%d: %v — проверка с умолчаниями, режим log", l.name, pid, err))
	}
	ticker := time.NewTicker(cfg.Restart.interval())
	defer ticker.Stop()
	var out outcome
	stopping := false
	var last journalKeys
	for {
		select {
		case err := <-done:
			out.err = err
			out.restart = out.restart && !stopping
			return out
		case sig := <-sigs:
			// Ctrl+C и Ctrl+\ терминал отдаёт всей группе, claude их получил сам; под pty cl они — байты ввода, а
			// сигнал извне claude не получит: передать. TERM и HUP — выход: передать claude.
			exit := sig == syscall.SIGTERM || sig == syscall.SIGHUP
			if exit || l.console != nil {
				stopping = stopping || exit
				if err := signalChild(cmd, sig); err != nil {
					journal(cfg.Restart.Journal, time.Now(), fmt.Sprintf("%s pid=%d: %v", l.name, pid, err))
				}
			}
		case <-ticker.C:
			if out.restart || stopping {
				continue
			}
			var err error
			if cfg, err = loadConfig(l.configFile, l.home); err != nil {
				last.verdict = journalOnce(cfg, last.verdict, "config", fmt.Sprintf("%s pid=%d: %v — не перезапускаю", l.name, pid, err))
				continue
			}
			now := time.Now()
			a, askErr := l.ask(pid, argsProblem)
			out, last = l.next(cfg, now, pid, a, askErr, last)
			if out.typed != "" {
				l.typeInto(cfg, now, pid, out)
				out = outcome{}
			}
			if !out.restart {
				continue
			}
			if err := signalChild(cmd, syscall.SIGTERM); err != nil {
				out.restart = false
				journal(cfg.Restart.Journal, now, fmt.Sprintf("%s pid=%d: %v — перезапуск отменён", out.name, pid, err))
			}
		}
	}
}

// ask — ответ aguard о сессии pid; сокета нет в настройках — ответа нет.
func (l launcher) ask(pid int, argsProblem string) (restart.Answer, error) {
	if l.aguard == "" {
		return restart.Answer{}, errors.New("aguard не задан")
	}
	return askAguard(l.aguard, restart.NewRequest(pid, l.name, argsProblem))
}

// journalKeys — ключи последних строк журнала: решения о перезапуске, сжатии и сбросе пишутся один раз на решение.
type journalKeys struct {
	verdict, compact, reset string
}

// next — что делать с сессией по ответу aguard: перезапуск (act) важнее сжатия, сжатие — сброса. aguard не ответил —
// ничего: своего решения у cl нет, а сомнение — не перезапускать.
func (l launcher) next(cfg config, now time.Time, pid int, a restart.Answer, askErr error, last journalKeys) (outcome, journalKeys) {
	name := firstNonEmpty(a.Name, l.name)
	if askErr != nil {
		last.verdict = journalOnce(cfg, last.verdict, "error/"+askErr.Error(),
			fmt.Sprintf("%s pid=%d %s: aguard не ответил (%v) — не перезапускаю и не сжимаю", name, pid, cfg.Restart.Mode, askErr))
		return outcome{}, last
	}
	last.verdict = journalOnce(cfg, last.verdict, a.Key, journalLine(cfg.Restart.Mode, name, pid, a))
	if a.Restart && cfg.Restart.Mode == modeAct && a.SessionID != "" {
		journal(cfg.Restart.Journal, now, fmt.Sprintf("%s pid=%d: SIGTERM, поднимаю --resume %s на %s", name, pid, a.SessionID, a.Installed))
		return outcome{restart: true, sessionID: a.SessionID, name: name, version: a.Installed}, last
	}
	steps := []struct {
		on   bool
		last *string
		want func(config, int, restart.Answer, string) (outcome, string, string)
	}{{a.Compact, &last.compact, l.wantCompact}, {a.Reset, &last.reset, l.wantReset}}
	for _, s := range steps {
		if !s.on {
			continue
		}
		out, line, key := s.want(cfg, pid, a, *s.last)
		if line != "" {
			journal(cfg.Restart.Journal, now, line)
		}
		*s.last = key
		return out, last
	}
	return outcome{}, last
}

// idleAction — действие над беседой в простое по вердикту aguard: сжатие или сброс.
type idleAction struct {
	mode, names string   // режим и ключ конфига пилота (compact_names, reset_names)
	pilot       []string // имена пилота; пусто — все
	key         string   // решение aguard без чисел
	would, none string   // «сжал бы: …» и «не сжимаю» — для журнала
	doing       string   // строка журнала в act
	out         outcome  // что сделать в act
	// typeMode, typed, typing — то же набором в живой сессии под pty: режим, что набрать, строка журнала в act.
	typeMode string
	typed    string
	typing   string
}

// typedNote — добавка к строке журнала сессии под pty в type_mode log: что cl набрал бы вместо перезапуска.
func (x idleAction) typedNote(pty bool) string {
	if !pty || x.typeMode != modeLog {
		return ""
	}
	return "; под pty набрал бы " + x.typed
}

// wantCompact — что делать с вердиктом aguard «сжать»: остановить сессию, сжать беседу и поднять её снова.
func (l launcher) wantCompact(cfg config, pid int, a restart.Answer, last string) (outcome, string, string) {
	return l.wantIdle(pid, a, last, idleAction{
		mode: cfg.Restart.CompactMode, names: "compact_names", pilot: cfg.Restart.CompactNames, key: a.CompactKey,
		would: fmt.Sprintf("сжал бы%%s: контекст %d тыс., кэш жив, ничего не идёт", a.Tokens/1000), none: "не сжимаю",
		doing:    fmt.Sprintf("сжимаю: контекст %d тыс.; SIGTERM, /compact, поднимаю --resume %s", a.Tokens/1000, a.SessionID),
		out:      outcome{compact: true, crons: a.Crons, version: a.Running},
		typeMode: cfg.Restart.TypeMode, typed: "/compact",
		typing: fmt.Sprintf("сжимаю: контекст %d тыс.; набираю /compact в живой сессии", a.Tokens/1000),
	})
}

// wantReset — что делать с вердиктом aguard «сбросить»: остановить сессию и поднять новую беседу со строкой роли;
// под pty — /clear без строки роли: таймеры CronCreate переживают /clear (пилот 26.10 18:06 CET), и строка не нужна
// даже лиду (пользователь 26.10 18:00:18 CET в транскрипте 6eca916e: «даже ему не надо»).
func (l launcher) wantReset(cfg config, pid int, a restart.Answer, last string) (outcome, string, string) {
	return l.wantIdle(pid, a, last, idleAction{
		mode: cfg.Restart.ResetMode, names: "reset_names", pilot: cfg.Restart.ResetNames, key: a.ResetKey,
		would: fmt.Sprintf("сбросил бы%%s: контекст %d тыс., работа закрыта, простаивает", a.Tokens/1000), none: "не сбрасываю",
		doing:    fmt.Sprintf("сбрасываю: контекст %d тыс.; SIGTERM, поднимаю новую беседу вместо %s", a.Tokens/1000, a.SessionID),
		out:      outcome{reset: true, version: a.Running},
		typeMode: cfg.Restart.TypeMode, typed: "/clear",
		typing: fmt.Sprintf("сбрасываю: контекст %d тыс.; набираю /clear в живой сессии", a.Tokens/1000),
	})
}

// wantIdle — в act (и в пилоте) — остановить сессию для действия x, иначе строка «сжал бы» / «сбросил бы». Строка
// журнала — одна на решение (режим, ключ aguard и контекст: у нового запроса другой).
func (l launcher) wantIdle(pid int, a restart.Answer, last string, x idleAction) (outcome, string, string) {
	name := firstNonEmpty(a.Name, l.name)
	key := fmt.Sprintf("%s/%s/%d", x.mode, x.key, a.Tokens)
	head := fmt.Sprintf("%s pid=%d %s: ", name, pid, x.mode)
	note := x.typedNote(l.console != nil)
	switch {
	case x.mode == modeOff:
		return outcome{}, "", last
	case a.SessionID == "":
		return outcome{}, onceLine(key+"/nosession", last, head+"aguard не назвал беседу — "+x.none), key + "/nosession"
	case !act(x.mode, x.pilot, name):
		why := ""
		if x.mode == modeAct {
			why = " (не в " + x.names + ")"
		}
		return outcome{}, onceLine(key, last, head+fmt.Sprintf(x.would, why)+note), key
	case l.console != nil && x.typeMode == modeAct:
		return outcome{typed: x.typed, name: name, sessionID: a.SessionID}, head + x.typing, key
	}
	out := x.out
	out.restart, out.sessionID, out.name = true, a.SessionID, name
	return out, head + x.doing + note, key
}

// typeInto набирает out.typed в живой сессии под pty; человек вводил меньше humanQuiet назад — не набирает.
func (l launcher) typeInto(cfg config, now time.Time, pid int, out outcome) {
	head := fmt.Sprintf("%s pid=%d: ", out.name, pid)
	if quiet := l.console.idle(now); quiet < humanQuiet {
		journal(cfg.Restart.Journal, now, head+fmt.Sprintf("человек вводил %v назад — не набираю", quiet.Round(time.Second)))
		return
	}
	if err := l.console.typeLine(out.typed); err != nil {
		journal(cfg.Restart.Journal, time.Now(), head+fmt.Sprintf("набор %s: %v", out.typed, err))
		return
	}
	journal(cfg.Restart.Journal, time.Now(), head+fmt.Sprintf("набрал %s в беседе %s", out.typed, out.sessionID))
}

// onceLine — line, если ключ не тот, что в прошлый раз; иначе пусто.
func onceLine(key, last, line string) string {
	if key == last {
		return ""
	}
	return line
}

// journalOnce пишет строку с этим ключом один раз подряд.
func journalOnce(cfg config, lastKey, key, line string) string {
	if key != lastKey {
		journal(cfg.Restart.Journal, time.Now(), line)
	}
	return key
}

// journalLine — строка журнала: сессия, версии, режим cl, решение aguard и доводы против.
func journalLine(mode, name string, pid int, a restart.Answer) string {
	head := fmt.Sprintf("%s pid=%d %s→%s %s: ", name, pid, restart.OrDash(a.Running), restart.OrDash(a.Installed), mode)
	switch {
	case a.Fresh:
		return head + "версия свежая"
	case a.Restart && mode == modeAct:
		return head + "перезапускаю: простаивает, ничего не идёт"
	case a.Restart:
		return head + "перезапустил бы: простаивает, ничего не идёт"
	}
	word := "не стал бы"
	if mode == modeAct {
		word = "не перезапускаю"
	}
	texts := make([]string, len(a.Against))
	for i, r := range a.Against {
		texts[i] = r.Text
	}
	return head + word + ": " + strings.Join(texts, "; ")
}

// journal дописывает строку с местным временем; сбой журнала — в stderr, работе сессии он не мешает.
func journal(file string, now time.Time, line string) {
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "cl: журнал:", err)
		return
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cl: журнал:", err)
		return
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "%s %s\n", now.Format("2006-01-02T15:04:05-07:00"), line); err != nil {
		fmt.Fprintln(os.Stderr, "cl: журнал:", err)
	}
}

// exitCode — код выхода claude; убит сигналом — 128+сигнал, как у оболочки.
func exitCode(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return exit.ExitCode()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "cl:", err)
		return 1
	}
	return 0
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
