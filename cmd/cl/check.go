package main

import (
	"fmt"
	"maps"
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
	cfg, err := l.readConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cl:", err)
		return 1
	}
	sessions, err := l.registry.List()
	if err != nil { //nolint:fallback-return // List отдаёт прочитанную часть реестра вместе с ошибкой о нечитаемых записях
		fmt.Fprintln(os.Stderr, "cl:", err)
	}
	slices.SortFunc(sessions, func(a, b session.Info) int { //nolint:large-value-copied-per-element // десяток записей раз за ручной запуск
		return a.Pid - b.Pid
	})
	now := time.Now()
	var entries []checkEntry
	for i := range sessions {
		s := &sessions[i]
		if !l.registry.IsAlive(s.Pid, s.ProcStart) {
			continue
		}
		problem := "командная строка процесса не прочиталась"
		if args, err := cmdlineArgs(l.registry.ProcRoot, s.Pid); err == nil {
			_, problem = restartArgs(withoutOwnFlags(args, l.home))
		}
		req := restart.NewRequest(s.Pid, s.Name, problem)
		req.Full = true
		a, err := askAguard(l.aguard, req)
		if err == nil && notInteractive(a) {
			continue
		}
		name := firstNonEmpty(a.Name, s.Name)
		line := "check " + checkLine(cfg.Restart.Mode, name, s.Pid, a, err)
		var journalEntry string
		if err == nil {
			journalEntry = fmt.Sprintf("check %s", journalLine(cfg.Restart.Mode, name, s.Pid, a)+compactNote(a))
		} else {
			journalEntry = line
		}
		journal(cfg.Restart.Journal, now, journalEntry)
		entries = append(entries, checkEntry{name: name, pid: s.Pid, answer: a, err: err})
	}
	common, commonReasons := commonCheckReasons(entries)
	for i := range entries {
		entry := &entries[i]
		fmt.Fprintln(os.Stdout, "check "+checkLineFiltered(cfg.Restart.Mode, entry.name, entry.pid, entry.answer, entry.err, common))
	}
	if len(commonReasons) > 0 {
		texts := make([]string, len(commonReasons))
		for i, reason := range commonReasons {
			texts[i] = restart.Text(reason)
		}
		fmt.Fprintln(os.Stdout, "у всех: "+strings.Join(texts, "; "))
	}
	return 0
}

type checkEntry struct {
	name   string
	pid    int
	answer restart.Answer
	err    error
}

func reasonKey(r restart.Reason) string { return r.Code + "\x00" + r.Text }

// commonCheckReasons — доводы, которые повторяются у каждой отвеченной сессии.
func commonCheckReasons(entries []checkEntry) (map[string]bool, []restart.Reason) {
	counts := map[string]int{}
	reasons := map[string]restart.Reason{}
	valid := 0
	for i := range entries {
		entry := &entries[i]
		if entry.err != nil {
			continue
		}
		valid++
		seen := map[string]bool{}
		all := append(append(append([]restart.Reason{}, entry.answer.Against...), entry.answer.CompactAgainst...), entry.answer.ResetAgainst...)
		for _, reason := range all {
			key := reasonKey(reason)
			if seen[key] {
				continue
			}
			seen[key] = true
			counts[key]++
			reasons[key] = reason
		}
	}
	common := map[string]bool{}
	var list []restart.Reason
	if valid < 2 {
		return common, nil
	}
	for key, count := range counts {
		if count == valid {
			common[key] = true
			list = append(list, reasons[key])
		}
	}
	slices.SortFunc(list, func(a, b restart.Reason) int {
		if a.Code == b.Code {
			return strings.Compare(a.Text, b.Text)
		}
		return strings.Compare(a.Code, b.Code)
	})
	return common, list
}

// notInteractive — aguard против, потому что сессия не интерактивная (claude -p, фоновая): в проверке её нет.
func notInteractive(a restart.Answer) bool {
	return slices.ContainsFunc(a.Against, func(r restart.Reason) bool { return r.Code == "kind" })
}

// checkLine — строка проверки: решение aguard о перезапуске и о сжатии; нет ответа — почему.
func checkLine(mode, name string, pid int, a restart.Answer, err error) string {
	return checkLineFiltered(mode, name, pid, a, err, nil)
}

func checkLineFiltered(mode, name string, pid int, a restart.Answer, err error, omitted map[string]bool) string {
	if err != nil {
		return fmt.Sprintf("%s pid=%d %s: aguard не ответил (%v)", name, pid, mode, err)
	}
	filteredOmissions := make(map[string]bool, len(omitted)+len(a.Against))
	maps.Copy(filteredOmissions, omitted)
	filter := func(reasons []restart.Reason) []restart.Reason {
		filtered := make([]restart.Reason, 0, len(reasons))
		for _, reason := range reasons {
			if !filteredOmissions[reasonKey(reason)] {
				filtered = append(filtered, reason)
			}
		}
		return filtered
	}
	a.Against = filter(a.Against)
	a.CompactAgainst = filter(a.CompactAgainst)
	a.ResetAgainst = filter(a.ResetAgainst)
	var line strings.Builder
	line.WriteString(checkHeader(mode, name, pid, a) + compactStatus(a))
	for _, reason := range a.Against {
		line.WriteString("\n  " + restart.Text(reason))
		filteredOmissions[reasonKey(reason)] = true
	}
	for _, detail := range checkDetailsFiltered(a, filteredOmissions) {
		line.WriteString("\n  " + detail)
	}
	return line.String()
}

func checkHeader(mode, name string, pid int, a restart.Answer) string {
	head := restart.Header(mode, name, pid, a)
	switch {
	case a.Fresh:
		return head + "версия свежая"
	case a.Restart && mode == modeAct:
		return head + "перезапускаю: простаивает, ничего не идёт"
	case a.Restart:
		return head + "перезапустил бы: простаивает, ничего не идёт"
	case mode == modeAct:
		return head + "не перезапускаю: есть доводы против"
	default:
		return head + "не стал бы: есть доводы против"
	}
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
