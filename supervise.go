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
)

// launcher — запуск claude под присмотром cl: claude — дочерний процесс в своём scope, cl ждёт его выхода и раз в
// check_seconds решает, можно ли перезапустить сессию на новую версию Claude Code.
type launcher struct {
	systemdRun string
	claude     string
	home       string
	configFile string
	paths      paths
	// aguard — сокет aguard: на время перехода cl сверяет с ним своё решение (ask.go); пусто — не спрашивать.
	aguard string
	// name — имя, которое дал cl (или пользователь через -n); после перезапуска — имя из реестра Claude Code.
	name string
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
}

// Что случилось с сессией перед запуском claude: от этого зависит {why} строки роли.
const (
	eventStart   = "старт"
	eventRestart = "перезапуск"
	eventCompact = "сжатие"
)

// compactTimeout — срок на `claude -p … /compact`: сжатие большой беседы идёт минуты, дольше — зависло.
const compactTimeout = 10 * time.Minute

// supervise запускает claude и поднимает его снова с той же беседой, если он вышел по сигналу перезапуска от cl.
// Любой другой выход (/exit, Ctrl+C, Ctrl+D, закрытие вкладки, сбой) — выход cl с кодом claude.
func (l launcher) supervise(pre, userArgs []string) int {
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP)
	keep, argsProblem := restartArgs(userArgs)
	args := append(append([]string{}, pre...), userArgs...)
	if bare(userArgs) {
		args = l.withRoleLine(args, l.name, eventStart)
	}
	cache := &scanCache{}
	for n := 0; ; n++ {
		unit := strconv.Itoa(os.Getpid())
		if n > 0 {
			unit += "-r" + strconv.Itoa(n)
		}
		cmd := exec.Command(l.systemdRun, scopeArgv(l.systemdRun, l.claude, l.name, unit, args)[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Start(); err != nil {
			fail(fmt.Errorf("запуск claude: %w", err))
		}
		out := l.watch(cmd, sigs, cache, argsProblem)
		if !out.restart {
			return exitCode(out.err)
		}
		l.name = out.name
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
		return args
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
	version, err := installedVersion(l.claude)
	if err != nil {
		return "", err
	}
	why := event + " cl на Claude Code " + version
	restart := event != eventStart
	resume, err := freshResume(cfg.Restart.ResumeDir, name, time.Duration(max(cfg.Restart.ResumeHours, 1))*time.Hour, time.Now())
	if err != nil {
		return "", err
	}
	return cfg.roleLine(name, why, resume, restart), nil
}

// signalChild шлёт сигнал claude; вышедший процесс — не ошибка (его выход и так придёт в done).
func signalChild(cmd *exec.Cmd, sig os.Signal) error {
	if err := cmd.Process.Signal(sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("сигнал %v claude: %w", sig, err)
	}
	return nil
}

// watch ждёт выхода claude; на каждой проверке пишет в журнал смену решения, а в режиме act перезапускает: SIGTERM —
// мягкий выход Claude Code (беседа дописана, «Resume this session with: claude --resume <id>»). Пока claude рисует
// экран, cl в терминал не пишет: всё — в журнал.
func (l launcher) watch(cmd *exec.Cmd, sigs <-chan os.Signal, cache *scanCache, argsProblem string) outcome {
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
	stopping, lastKey, lastCompact := false, "", ""
	for {
		select {
		case err := <-done:
			out.err = err
			out.restart = out.restart && !stopping
			return out
		case sig := <-sigs:
			// Ctrl+C и Ctrl+\ терминал отдаёт всей группе, claude их получил сам. TERM и HUP — выход: передать claude.
			if sig == syscall.SIGTERM || sig == syscall.SIGHUP {
				stopping = true
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
				lastKey = l.journalOnce(cfg, lastKey, "config", fmt.Sprintf("%s pid=%d: %v — не перезапускаю", l.name, pid, err))
				continue
			}
			now := time.Now()
			f := probe(l.paths, cache, pid, now)
			v := l.verdict(cfg, f, argsProblem)
			a, askErr := l.compare(cfg, now, pid, f, v, argsProblem)
			if v.key() != lastKey {
				journal(cfg.Restart.Journal, now, journalLine(cfg.Restart.Mode, l.name, pid, f, v))
				lastKey = v.key()
			}
			if out, lastCompact = l.next(cfg, now, pid, f, v, a, askErr, lastCompact); !out.restart {
				continue
			}
			if err := signalChild(cmd, syscall.SIGTERM); err != nil {
				out.restart = false
				journal(cfg.Restart.Journal, now, fmt.Sprintf("%s pid=%d: %v — перезапуск отменён", f.Name, pid, err))
			}
		}
	}
}

// next — что делать с сессией по своему вердикту и ответу aguard: перезапуск (act) важнее сжатия.
func (l launcher) next(cfg config, now time.Time, pid int, f facts, v verdict, a aguardAnswer, askErr error, lastCompact string) (outcome, string) {
	switch {
	case v.Restart && cfg.Restart.Mode == modeAct:
		journal(cfg.Restart.Journal, now, fmt.Sprintf("%s pid=%d: SIGTERM, поднимаю --resume %s на %s", f.Name, pid, f.SessionID, f.Installed))
		return outcome{restart: true, sessionID: f.SessionID, name: firstNonEmpty(f.Name, l.name), version: f.Installed}, lastCompact
	case askErr == nil && a.Compact:
		out, line, key := l.wantCompact(cfg, pid, f, a, lastCompact)
		if line != "" {
			journal(cfg.Restart.Journal, now, line)
		}
		return out, key
	}
	return outcome{}, lastCompact
}

// wantCompact — что делать с вердиктом aguard «сжать»: в act (и в пилоте) — остановить сессию для сжатия, иначе
// строка «сжал бы». Строка журнала — одна на решение (режим, ключ aguard и контекст: у нового запроса другой).
// aguard ответил о другой беседе, чем видит cl, — не сжимать.
func (l launcher) wantCompact(cfg config, pid int, f facts, a aguardAnswer, last string) (outcome, string, string) {
	name := firstNonEmpty(f.Name, l.name)
	key := fmt.Sprintf("%s/%s/%d", cfg.Restart.CompactMode, a.CompactKey, a.Tokens)
	head := fmt.Sprintf("%s pid=%d %s: ", name, pid, cfg.Restart.CompactMode)
	switch {
	case cfg.Restart.CompactMode == modeOff:
		return outcome{}, "", last
	case f.SessionID == "" || a.SessionID != f.SessionID:
		return outcome{}, onceLine(key+"/other", last, head+fmt.Sprintf("aguard о беседе %q, у cl %q — не сжимаю", a.SessionID, f.SessionID)), key + "/other"
	case !cfg.Restart.compactAct(name):
		why := ""
		if cfg.Restart.CompactMode == modeAct {
			why = " (не в compact_names)"
		}
		return outcome{}, onceLine(key, last, head+fmt.Sprintf("сжал бы%s: контекст %d тыс., кэш жив, ничего не идёт", why, a.Tokens/1000)), key
	}
	line := head + fmt.Sprintf("сжимаю: контекст %d тыс.; SIGTERM, /compact, поднимаю --resume %s", a.Tokens/1000, f.SessionID)
	return outcome{restart: true, compact: true, crons: len(f.Transcript.Crons) > 0, sessionID: f.SessionID, name: name, version: f.Running}, line, key
}

// onceLine — line, если ключ не тот, что в прошлый раз; иначе пусто.
func onceLine(key, last, line string) string {
	if key == last {
		return ""
	}
	return line
}

func (l launcher) verdict(cfg config, f facts, argsProblem string) verdict {
	r, _ := cfg.roleFor(firstNonEmpty(f.Name, l.name))
	return decide(f, policy{idle: cfg.Restart.idle(), role: r, argsProblem: argsProblem})
}

// journalOnce пишет строку с этим ключом один раз подряд.
func (l launcher) journalOnce(cfg config, lastKey, key, line string) string {
	if key != lastKey {
		journal(cfg.Restart.Journal, time.Now(), line)
	}
	return key
}

// journalLine — строка журнала: сессия, версии, режим, решение и доводы против.
func journalLine(mode, name string, pid int, f facts, v verdict) string {
	head := fmt.Sprintf("%s pid=%d %s→%s %s: ", firstNonEmpty(f.Name, name), pid, orDash(f.Running), orDash(f.Installed), mode)
	switch {
	case v.Fresh:
		return head + "версия свежая"
	case v.Restart && mode == modeAct:
		return head + "перезапускаю: простаивает, ничего не идёт"
	case v.Restart:
		return head + "перезапустил бы: простаивает, ничего не идёт"
	}
	word := "не стал бы"
	if mode == modeAct {
		word = "не перезапускаю"
	}
	texts := make([]string, len(v.Against))
	for i, r := range v.Against {
		texts[i] = r.text
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

// compare — вердикт aguard по той же сессии. На время перехода перезапуск решает сам cl, а вердикт aguard о
// перезапуске идёт рядом со своим строкой в журнал сверки (restart.log и aguard-compare.log в одной папке). Сжатие
// решает только aguard. Сокета нет в настройках — ответа нет.
func (l launcher) compare(cfg config, now time.Time, pid int, f facts, v verdict, argsProblem string) (aguardAnswer, error) {
	if l.aguard == "" {
		return aguardAnswer{}, errors.New("aguard не задан")
	}
	name := firstNonEmpty(f.Name, l.name)
	a, err := askAguard(l.aguard, aguardRequest{PID: pid, Name: name, ArgsProblem: argsProblem})
	journal(filepath.Join(filepath.Dir(cfg.Restart.Journal), "aguard-compare.log"), now, compareLine(name, pid, v, a, err))
	return a, err
}
