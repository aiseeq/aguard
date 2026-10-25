package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/restart"
	"github.com/aiseeq/akit/session"
)

// checkFlag — `cl --cl-check`: решение aguard «перезапустил бы / не стал бы» и о сжатии по всем живым интерактивным
// сессиям, в том числе запущенным без присмотра cl; печать и строка в журнал с пометкой check. Ничего не останавливает.
const checkFlag = "--cl-check"

func runCheck(l launcher) int {
	cfg, err := loadConfig(l.configFile, l.home)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cl:", err)
		return 1
	}
	sessions, err := l.registry.List()
	if err != nil { //nolint:fallback-return // List отдаёт прочитанную часть реестра вместе с ошибкой о нечитаемых записях
		fmt.Fprintln(os.Stderr, "cl:", err)
	}
	slices.SortFunc(sessions, func(a, b session.Info) int { return a.Pid - b.Pid })
	now := time.Now()
	for _, s := range sessions {
		if !l.registry.IsAlive(s.Pid, s.ProcStart) {
			continue
		}
		problem := "командная строка процесса не прочиталась"
		if args, err := cmdlineArgs(l.registry.ProcRoot, s.Pid); err == nil {
			_, problem = restartArgs(withoutOwnFlags(args, l.home))
		}
		a, err := askAguard(l.aguard, restart.NewRequest(s.Pid, s.Name, problem))
		if err == nil && notInteractive(a) {
			continue
		}
		line := "check " + checkLine(cfg.Restart.Mode, firstNonEmpty(a.Name, s.Name), s.Pid, a, err)
		fmt.Fprintln(os.Stdout, line)
		journal(cfg.Restart.Journal, now, line)
	}
	return 0
}

// notInteractive — aguard против, потому что сессия не интерактивная (claude -p, фоновая): в проверке её нет.
func notInteractive(a restart.Answer) bool {
	return slices.ContainsFunc(a.Against, func(r restart.Reason) bool { return r.Code == "kind" })
}

// checkLine — строка проверки: решение aguard о перезапуске и о сжатии; нет ответа — почему.
func checkLine(mode, name string, pid int, a restart.Answer, err error) string {
	if err != nil {
		return fmt.Sprintf("%s pid=%d %s: aguard не ответил (%v)", name, pid, mode, err)
	}
	return journalLine(mode, name, pid, a) + compactNote(a, nil)
}

// withoutOwnFlags — командная строка процесса без флагов, которые cl ставит сам (sessionFlags): перезапуск поставит их
// заново, а restartArgs судит только об аргументах пользователя.
func withoutOwnFlags(args []string, home string) []string {
	own := sessionFlags(home)
	var user []string
	for _, a := range args {
		if !slices.Contains(own, a) {
			user = append(user, a)
		}
	}
	return user
}

// cmdlineArgs — аргументы claude живого процесса (без самой программы).
func cmdlineArgs(proc string, pid int) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(proc, strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return nil, fmt.Errorf("командная строка процесса %d: %w", pid, err)
	}
	parts := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
	return parts[1:], nil
}
