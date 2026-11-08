package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aiseeq/aguard/internal/cllog"
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
	// dir — папка сессии (cwd cl, его наследует claude): по ней роль выбирает файл настроек (settings).
	dir string
	// console — claude под pty cl (restart.pty_names); nil — claude получает терминал cl напрямую.
	console *console
	// selfExe — ссылка на свой бинарник (/proc/self/exe), exec — syscall.Exec: подхват нового cl (upgrade.go);
	// пусто — не подхватывать. upgraded — состояние от прежнего cl: продолжить с его аргументами.
	selfExe  string
	exec     func(path string, argv, env []string) error
	upgraded *upgrade
	// check — срок между проверками; 0 — restart.check_seconds из конфига (не меньше 10 с).
	check time.Duration
}

// outcome — чем кончился один запуск claude.
type outcome struct {
	err error
	// restart — сессия вышла по сигналу перезапуска от cl; sessionID, name, version — что поднять снова; at — когда cl
	// решил (в {why} строки роли после сброса).
	restart   bool
	sessionID string
	name      string
	version   string
	at        time.Time
	// compact — перед подъёмом сжать беседу (`claude -p --resume <сессия> /compact`).
	compact bool
	// reset — поднять не прежнюю беседу, а новую: строка роли и файл возобновления.
	reset bool
	// typed — не перезапускать, а набрать эту команду в живой сессии под pty (restart.type_mode act).
	typed string
	// lost — таймеры, унаследованные через /clear: --resume их не вернёт, подъём — со строкой роли.
	lost []restart.LostCron
	// stale — перезапуск решён при этом расхождении аргументов с ролью (staleSettings): тот же текст у нового
	// процесса — подъём его не исправил, больше не просить.
	stale string
}

// humanQuiet — сколько человек не должен ничего вводить, чтобы cl набирал в его сессии.
const humanQuiet = time.Minute

// Что случилось с сессией перед запуском claude: от этого зависит {why} строки роли.
const (
	eventStart = "старт"

	eventReset = "сброс"
)

// compactTimeout — срок на `claude -p … /compact`: сжатие большой беседы идёт минуты, дольше — зависло.
const compactTimeout = 10 * time.Minute

// supervise запускает claude и поднимает его снова с той же беседой, если он вышел по сигналу перезапуска от cl.
// Любой другой выход (/exit, Ctrl+C, Ctrl+D, закрытие вкладки, сбой) — выход cl с кодом claude.
func (l launcher) supervise(pre, userArgs []string) int {
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP)
	defer func() { // консоль открывает openPty, и не обязательно на первом подъёме
		r := recover()
		if l.console != nil {
			l.console.restore()
		}
		if r != nil {
			panic(r)
		}
	}()
	keep, argsProblem := restartArgs(userArgs)
	args := append(append([]string{}, pre...), userArgs...)
	first, stale := 0, ""
	switch {
	case l.upgraded != nil:
		args, first = l.upgraded.Args, l.upgraded.N
	case bare(userArgs):
		args = l.withRoleLine(args, l.name, eventStart, outcome{})
	}
	for n := first; ; n++ {
		if n > first {
			l.upgrade(n, args)
		}
		l.openPty()
		var proc *os.Process
		var finish func()
		var err error
		if n == first && l.upgraded != nil && l.upgraded.Pid > 0 {
			if proc, finish, err = l.adopt(*l.upgraded); err != nil {
				l.journalUpgradeFail(err, "выход")
			}
		} else {
			proc, finish, err = l.start(n, args)
		}
		if err != nil {
			fail(err)
		}
		out := l.watch(proc, sigs, argsProblem, n, args, stale)
		finish()
		stale = out.stale
		if !out.restart {
			return exitCode(out.err)
		}
		l.name = out.name
		if out.reset {
			args = l.withRoleLine(append(append(sessionFlags(l.home), "-n", out.name), keep...), out.name, eventReset, out)
			continue
		}
		if out.compact {
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
		// Без реплики: беседа поднята целиком, а --resume возвращает таймеры своей беседы. Реплика — лишний ход на всём
		// контексте и непонятная строка в консоли (пользователь 27.10 09:50 CET: «если консоль работает, не надо её
		// рестартить. А если не работает, то не о чем напоминать»). Таймеры, унаследованные через /clear, --resume не
		// вернёт (проба 30.10 на 2.1.314) — тогда строка роли, как у старта cl руками: роль ставит свои таймеры сама
		// (пользователь 08.11 у лида: «Перезапуск cl сам бы выдал роль»; `claude --resume <id> "<строка>"` берёт строку
		// первой репликой — проба 08.11 21:19 на 2.1.319).
		args = append(args, "--resume", out.sessionID)
		if len(out.lost) > 0 {
			args = l.withRoleLine(args, out.name, eventStart, out)
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
	args := l.withRoleFlags(append(append(sessionFlags(l.home), keep...), "-p", "--resume", out.sessionID, "/compact"), out.name)
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
			stopped := ""
			if stop != 0 {
				stopped = stop.String()
			}
			journal(cfg.Restart.Journal, time.Now(), cllog.CompactResult(out.name, out.sessionID, time.Since(start), err, stopped, output.String()))
			return stop, nil
		case sig := <-sigs:
			if s, ok := sig.(syscall.Signal); ok && (s == syscall.SIGTERM || s == syscall.SIGHUP) {
				stop = s
				if err := signalChild(cmd.Process, syscall.SIGTERM); err != nil {
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

// withRoleFlags добавляет флаги роли при каждом подъёме: старт, перезапуск, сжатие, сброс. Prompt — флагом
// --append-system-prompt: Claude Code записывает системный промпт на первом запросе беседы и при --resume отдаёт
// записанный, пока беседу не сожмут (--system-prompt-snapshot, по умолчанию on, claude --help 2.1.311), и флаг без
// подъёма-другого потерялся бы на ближайшем сжатии. Свой флаг системного промпта в аргументах или роли нет — промпта
// нет. Файл настроек — флагом --settings (settingsFlags).
func (l launcher) withRoleFlags(args []string, name string) []string {
	cfg, err := loadConfig(l.configFile, l.home)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cl: роль:", err)
		return args
	}
	pre := l.settingsFlags(cfg, args, name)
	if text := cfg.systemPrompt(name); text != "" && !slices.ContainsFunc(args, promptFlag) {
		pre = append(pre, "--append-system-prompt", text)
	}
	return append(pre, args...)
}

// settingsFlags — `--settings <файл>` роли сессии name по папке сессии. Файла нет или в нём не объект JSON — сессия
// поднимается без него, ошибка в журнал cl и stderr: без файла роль лишь видит лишний инструмент или скилл, а
// отказ в подъёме оставил бы консоль мёртвой до человека. Свой --settings в аргументах — файл роли не добавляется
// (строка в журнал): Claude Code берёт один --settings на запуск (проба 08.11, README), и второй вытеснил бы первый.
func (l launcher) settingsFlags(cfg config, args []string, name string) []string {
	file := cfg.settingsFile(name, l.dir)
	if file == "" {
		return nil
	}
	if slices.ContainsFunc(args, settingsFlag) {
		journal(cfg.Restart.Journal, time.Now(), fmt.Sprintf("%s: свой --settings в аргументах — файл роли %s не добавляю", name, file))
		return nil
	}
	if err := settingsObject(file); err != nil {
		fmt.Fprintln(os.Stderr, "cl: настройки роли:", err)
		journal(cfg.Restart.Journal, time.Now(), fmt.Sprintf("%s: %v — поднимаю без файла настроек роли", name, err))
		return nil
	}
	return []string{"--settings", file}
}

// staleAsk — расхождение файла настроек для вопроса aguard (staleSettings): ошибка сверки — не судить, то же, ради
// чего прошлый процесс перезапустили, — не просить снова (петля); и то и другое — строкой в журнал один раз
// (lastKey — ключ прошлой строки, возвращается новый).
func (l launcher) staleAsk(cfg config, pid int, args []string, prevStale, lastKey string) (stale, key string) {
	stale, err := l.staleSettings(cfg, pid, args)
	switch {
	case err != nil:
		return "", journalOnce(cfg, lastKey, "error/"+err.Error(), fmt.Sprintf("%s pid=%d: %v — о файле настроек роли не сужу", l.name, pid, err))
	case stale != "" && stale == prevStale:
		return "", journalOnce(cfg, lastKey, "loop/"+stale, fmt.Sprintf("%s pid=%d: %s и после перезапуска ради него — больше не прошу", l.name, pid, stale))
	}
	return stale, lastKey
}

// staleSettings — чем файл настроек живого claude pid (--settings в /proc/<pid>/cmdline) расходится с положенным по
// роли и папке сессии: «файл настроек роли: нет → /x.json». Пусто — совпадает или судить не о чем: у роли нет
// settings, свой --settings в аргументах, положенный файл сейчас не читается или не объект JSON (его могут
// дописывать: перезапуск поднял бы сессию без файла — сценарий противника замысла 08.11). Пути — после Clean.
func (l launcher) staleSettings(cfg config, pid int, args []string) (string, error) {
	r, ok := cfg.roleFor(l.name)
	if !ok || len(r.Settings) == 0 || slices.ContainsFunc(args, settingsFlag) {
		return "", nil
	}
	want := cfg.settingsFile(l.name, l.dir)
	if want != "" && settingsObject(want) != nil {
		return "", nil
	}
	if l.registry.ProcRoot == "" {
		return "", errors.New("корень /proc не задан")
	}
	running, err := cmdlineArgs(l.registry.ProcRoot, pid)
	if err != nil {
		return "", err
	}
	have := settingsValue(running)
	if cleanOrEmpty(have) == cleanOrEmpty(want) {
		return "", nil
	}
	return "файл настроек роли: " + orNone(have) + " → " + orNone(want), nil
}

// settingsValue — значение последнего --settings в аргументах (Claude Code берёт последний); нет — пусто.
func settingsValue(args []string) string {
	value := ""
	for i, a := range args {
		if v, ok := strings.CutPrefix(a, "--settings="); ok {
			value = v
		} else if a == "--settings" && i+1 < len(args) {
			value = args[i+1]
		}
	}
	return value
}

func cleanOrEmpty(p string) string {
	if p == "" {
		return ""
	}
	return filepath.Clean(p)
}

func orNone(p string) string {
	if p == "" {
		return "нет"
	}
	return p
}

// settingsObject — файл читается и в нём объект JSON.
func settingsObject(file string) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("файл настроек роли: %w", err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return fmt.Errorf("файл настроек роли %s: не объект JSON: %w", file, err)
	}
	return nil
}

// settingsFlag — флаг --settings (с значением через = или следующим аргументом).
func settingsFlag(arg string) bool {
	flag, _, _ := strings.Cut(arg, "=")
	return flag == "--settings"
}

// promptFlag — флаг системного промпта Claude Code (с значением через = или следующим аргументом).
func promptFlag(arg string) bool {
	flag, _, _ := strings.Cut(arg, "=")
	switch flag {
	case "--system-prompt", "--system-prompt-file", "--append-system-prompt", "--append-system-prompt-file":
		return true
	}
	return false
}

// withRoleLine добавляет первой репликой строку роли из конфига; роли нет или конфиг не прочитался — аргументы как
// есть (сбой — в stderr: сессия важнее строки роли).
func (l launcher) withRoleLine(args []string, name, event string, out outcome) []string {
	line, err := l.roleLine(name, event, out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cl: строка роли:", err)
	}
	if line == "" {
		return args
	}
	return append(args, line)
}

func (l launcher) roleLine(name, event string, out outcome) (string, error) {
	cfg, err := loadConfig(l.configFile, l.home)
	if err != nil {
		return "", err
	}
	version, err := restart.InstalledVersion(l.claude)
	if err != nil {
		return "", err
	}
	why := whyText(event, version, out.at)
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

// whyText — {why} строки роли. Старт — «старт cl на Claude Code 2.1.310»; at — cl решил перезапустить сессию (подъём
// --resume при потерянных таймерах), это не старт cl. Сброс — что cl сделал и когда: строка без времени и причины
// выглядела сбоем (пользователь 27.10 09:39 CET, задание лида). Время местное, без пояса: строку читает человек в
// своей консоли.
func whyText(event, version string, at time.Time) string {
	switch {
	case event == eventReset:
		return fmt.Sprintf("cl начал новую беседу в %s вместо простаивавшей; Claude Code %s", at.Local().Format("15:04"), version)
	case !at.IsZero():
		return fmt.Sprintf("cl поднял сессию заново в %s на Claude Code %s", at.Local().Format("15:04"), version)
	}
	return eventStart + " cl на Claude Code " + version
}

// start — запуск n claude с аргументами args в своём scope.
func (l launcher) start(n int, args []string) (*os.Process, func(), error) {
	unit := strconv.Itoa(os.Getpid())
	if n > 0 {
		unit += "-r" + strconv.Itoa(n)
	}
	cmd := exec.Command(l.systemdRun, scopeArgv(l.systemdRun, l.claude, l.name, unit, l.withRoleFlags(args, l.name))[1:]...)
	finish, err := l.launch(cmd)
	if err != nil {
		return nil, nil, fmt.Errorf("запуск claude: %w", err)
	}
	return cmd.Process, finish, nil
}

// launch запускает claude: под pty cl или прямо на терминале cl. finish — после выхода claude.
func (l launcher) launch(cmd *exec.Cmd) (finish func(), err error) {
	if l.console != nil {
		return l.console.start(cmd, l.trustDir())
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return func() {}, cmd.Start()
}

// trustDir — папка, окно доверия к которой cl подтверждает сам: домашняя, если claude запущен в ней (Claude Code не
// сохраняет доверие к ней, trust.go); иначе пусто.
func (l launcher) trustDir() string {
	if cwd, err := os.Getwd(); err == nil && l.home != "" && filepath.Clean(cwd) == filepath.Clean(l.home) {
		return l.home
	}
	return ""
}

// wait ждёт выхода claude; код не 0 — *exec.ExitError, как у exec.Cmd.Wait: claude мог запустить прежний cl
// (liveUpgrade), и exec.Cmd у этого нет.
func wait(proc *os.Process) error {
	st, err := proc.Wait()
	if err != nil {
		return fmt.Errorf("дождаться claude: %w", err)
	}
	if !st.Success() {
		return &exec.ExitError{ProcessState: st}
	}
	return nil
}

// signalChild шлёт сигнал claude; вышедший процесс — не ошибка (его выход и так придёт в done).
func signalChild(proc *os.Process, sig os.Signal) error {
	if err := proc.Signal(sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("сигнал %v claude: %w", sig, err)
	}
	return nil
}

// every — срок между проверками: свой check, иначе из конфига.
func (l launcher) every(c restartConfig) time.Duration {
	if l.check > 0 {
		return l.check
	}
	return c.interval()
}

// watch ждёт выхода claude; на каждой проверке спрашивает aguard (решение о перезапуске и сжатии — у него,
// internal/restart), пишет в журнал смену решения, а в режиме act перезапускает: SIGTERM — мягкий выход Claude Code
// (беседа дописана, «Resume this session with: claude --resume <id>»). Пока claude рисует экран, cl в терминал не
// пишет: всё — в журнал.
// Расхождение аргументов с ролью (staleSettings) идёт в вопрос aguard; prevStale — расхождение, ради которого
// прошлый процесс перезапустили: у нового оно же — не просить снова (петля), строка в журнал.
func (l launcher) watch(proc *os.Process, sigs <-chan os.Signal, argsProblem string, n int, args []string, prevStale string) outcome {
	done := make(chan error, 1)
	go func() { done <- wait(proc) }()
	pid := proc.Pid
	cfg, err := loadConfig(l.configFile, l.home)
	if err != nil {
		journal(cfg.Restart.Journal, time.Now(), fmt.Sprintf("%s pid=%d: %v — проверка с умолчаниями, режим log", l.name, pid, err))
	}
	ticker := time.NewTicker(l.every(cfg.Restart))
	defer ticker.Stop()
	var out outcome
	stopping, upgradeTried := false, false
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
				if err := signalChild(proc, sig); err != nil {
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
			upgradeTried = upgradeTried || l.liveUpgrade(cfg, now, pid, n, args)
			var stale string
			stale, last.stale = l.staleAsk(cfg, pid, args, prevStale, last.stale)
			a, askErr := l.ask(pid, argsProblem, stale, l.console != nil && cfg.Restart.TypeMode == modeAct)
			out, last = l.next(cfg, now, pid, a, askErr, last)
			out.stale = stale // нужен только перезапуску: без него supervise выходит
			if out.typed != "" {
				l.typeInto(cfg, now, pid, out)
				out = outcome{}
			}
			if !out.restart {
				continue
			}
			if err := signalChild(proc, syscall.SIGTERM); err != nil {
				out.restart = false
				journal(cfg.Restart.Journal, now, fmt.Sprintf("%s pid=%d: %s", out.name, pid, cllog.Cancelled(err)))
			}
		}
	}
}

// openPty — консоль под pty cl, если имени сессии она положена (restart.pty_names) и ещё не открыта. Перед каждым
// подъёмом claude, не только при старте cl: имя, внесённое в pty_names, включается на ближайшем перезапуске, сжатии
// или сбросе, без перезапуска консоли. Открытая остаётся до выхода cl. Живой claude под pty от прежнего cl — консоль
// нужна при любом конфиге: его вывод идёт только через неё.
func (l *launcher) openPty() {
	if l.console != nil {
		return
	}
	adopted := l.upgraded != nil && l.upgraded.Master > 0
	if cfg, err := loadConfig(l.configFile, l.home); adopted || err == nil && matchAny(cfg.Restart.PtyNames, l.name) {
		l.console = openConsole(os.Stdin, os.Stdout, func(s string) {
			// pid cl — aguard night отличает окно, на котором claude стоит, от закрытой консоли
			journal(cfg.Restart.Journal, time.Now(), fmt.Sprintf("%s pid=%d: %s", l.name, os.Getpid(), s))
		})
	}
}

// ask — ответ aguard о сессии pid; typed — сжатие и сброс cl наберёт в живой сессии. Сокета нет в настройках —
// ответа нет.
func (l launcher) ask(pid int, argsProblem, stale string, typed bool) (restart.Answer, error) {
	if l.aguard == "" {
		return restart.Answer{}, errors.New("aguard не задан")
	}
	req := restart.NewRequest(pid, l.name, argsProblem)
	req.Typed, req.Stale = typed, stale
	req.Clean = l.console != nil && l.console.clean()
	asked := time.Now()
	a, err := askAguard(l.aguard, req)
	if err == nil && a.FieldEmpty && l.console != nil {
		l.console.emptySeen(asked)
	}
	return a, err
}

// journalKeys — ключи последних строк журнала: решения о перезапуске, сжатии и сбросе пишутся один раз на решение.
type journalKeys struct {
	verdict, compact, reset, stale string
}

// next — что делать с сессией по ответу aguard: перезапуск (act) важнее сжатия, сжатие — сброса. aguard не ответил —
// ничего: своего решения у cl нет, а сомнение — не перезапускать.
func (l *launcher) next(cfg config, now time.Time, pid int, a restart.Answer, askErr error, last journalKeys) (outcome, journalKeys) {
	name := firstNonEmpty(a.Name, l.name)
	if askErr != nil {
		last.verdict = journalOnce(cfg, last.verdict, "error/"+askErr.Error(),
			fmt.Sprintf("%s pid=%d %s: aguard не ответил (%v) — не перезапускаю и не сжимаю", name, pid, cfg.Restart.Mode, askErr))
		return outcome{}, last
	}
	last.verdict = journalOnce(cfg, last.verdict, a.Key, journalLine(cfg.Restart.Mode, name, pid, a))
	if a.Restart && cfg.Restart.Mode == modeAct && a.SessionID != "" {
		journal(cfg.Restart.Journal, now, fmt.Sprintf("%s pid=%d: SIGTERM, поднимаю --resume %s на %s", name, pid, a.SessionID, a.Installed))
		for _, job := range a.Lost {
			journal(cfg.Restart.Journal, now, fmt.Sprintf("%s pid=%d: %s", name, pid, cllog.LostCron(job.ID, job.Cron, job.Prompt)))
		}
		return outcome{restart: true, sessionID: a.SessionID, name: name, version: a.Installed, at: now, lost: a.Lost}, last
	}
	// Сброс раньше сжатия: где можно сбросить, aguard и так не сжимает (preferReset), порядок — на случай обоих «да».
	steps := []struct {
		on   bool
		last *string
		want func(config, int, restart.Answer, string) (outcome, string, string)
	}{{a.Reset, &last.reset, l.wantReset}, {a.Compact, &last.compact, l.wantCompact}}
	for _, s := range steps {
		if !s.on {
			continue
		}
		out, line, key := s.want(cfg, pid, a, *s.last)
		out.at = now
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
	// noReset — к строкам о сжатии: почему не сброс (cllog.NoReset).
	noReset string
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
		doing:    cllog.Compact(a.Tokens, a.SessionID),
		out:      outcome{compact: true, version: a.Running},
		typeMode: cfg.Restart.TypeMode, typed: cllog.TypedCompact,
		typing:  cllog.Typing(a.Tokens, cllog.TypedCompact),
		noReset: cllog.NoReset(restart.Texts(a.ResetAgainst)),
	})
}

// wantReset — что делать с вердиктом aguard «сбросить»: остановить сессию и поднять новую беседу со строкой роли;
// под pty — /clear без строки роли: таймеры CronCreate переживают /clear (пилот 26.10 18:06 CET), и строка не нужна
// даже лиду (пользователь 26.10 18:00:18 CET в транскрипте 6eca916e: «даже ему не надо»).
func (l launcher) wantReset(cfg config, pid int, a restart.Answer, last string) (outcome, string, string) {
	return l.wantIdle(pid, a, last, idleAction{
		mode: cfg.Restart.ResetMode, names: "reset_names", pilot: cfg.Restart.ResetNames, key: a.ResetKey,
		would: fmt.Sprintf("сбросил бы%%s: контекст %d тыс., работа закрыта, простаивает", a.Tokens/1000), none: "не сбрасываю",
		doing:    cllog.Reset(a.Tokens, a.SessionID),
		out:      outcome{reset: true, version: a.Running},
		typeMode: cfg.Restart.TypeMode, typed: cllog.TypedClear,
		typing: cllog.Typing(a.Tokens, cllog.TypedClear),
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
		return outcome{}, onceLine(key, last, head+fmt.Sprintf(x.would, why)+x.noReset+note), key
	case l.console != nil && x.typeMode == modeAct:
		return outcome{typed: x.typed, name: name, sessionID: a.SessionID}, head + x.typing + x.noReset, key
	}
	out := x.out
	out.restart, out.sessionID, out.name = true, a.SessionID, name
	return out, head + x.doing + x.noReset + note, key
}

// typeInto набирает out.typed в живой сессии под pty; человек вводил меньше humanQuiet назад — не набирает. Исход —
// строкой в журнал (её читает отчёт сжатия, cllog.Read); не набрал — следующий вопрос aguard решит заново.
func (l launcher) typeInto(cfg config, now time.Time, pid int, out outcome) {
	head := fmt.Sprintf("%s pid=%d: ", out.name, pid)
	if l.console == nil { // typed ставит только wantIdle при открытой консоли
		journal(cfg.Restart.Journal, now, head+"набор без консоли pty — не набираю (ошибка cl)")
		return
	}
	if quiet := l.console.idle(now); quiet < humanQuiet {
		journal(cfg.Restart.Journal, now, head+cllog.HumanTyping(quiet))
		return
	}
	if err := l.console.typeLine(out.typed); err != nil {
		journal(cfg.Restart.Journal, time.Now(), head+cllog.TypeFailed(out.typed, err))
		return
	}
	journal(cfg.Restart.Journal, time.Now(), head+cllog.Typed(out.typed, out.sessionID))
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
	if a.Stale != "" && !a.Fresh {
		head = fmt.Sprintf("%s pid=%d %s→%s, %s %s: ", name, pid, restart.OrDash(a.Running), restart.OrDash(a.Installed), a.Stale, mode)
	}
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
	return head + word + ": " + strings.Join(restart.Texts(a.Against), "; ")
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
	if _, err := io.WriteString(f, cllog.Line(now, line)); err != nil {
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
