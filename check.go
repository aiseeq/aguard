package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// checkFlag — `cl --cl-check`: решение «перезапустил бы / не стал бы» по всем живым интерактивным сессиям, в том
// числе запущенным без присмотра cl; печать и строка в журнал с пометкой check. Ничего не останавливает.
const checkFlag = "--cl-check"

func runCheck(l launcher) int {
	cfg, err := loadConfig(l.configFile, l.home)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cl:", err)
		return 1
	}
	files, err := filepath.Glob(filepath.Join(l.paths.sessions, "*.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "cl:", err)
		return 1
	}
	slices.Sort(files)
	now := time.Now()
	for _, file := range files {
		s, err := readSession(file)
		if err != nil {
			fmt.Fprintln(os.Stderr, "cl:", err)
			continue
		}
		if !alive(s.PID) || s.Kind != "interactive" {
			continue
		}
		f := probe(l.paths, &scanCache{}, s.PID, now)
		problem := "командная строка процесса не прочиталась"
		if args, err := cmdlineArgs(l.paths.proc, s.PID); err == nil {
			_, problem = restartArgs(withoutOwnFlags(args, l.home))
		}
		v := l.verdict(cfg, f, problem)
		line := "check " + journalLine(cfg.Restart.Mode, s.Name, s.PID, f, v)
		fmt.Fprintln(os.Stdout, line)
		journal(cfg.Restart.Journal, now, line)
	}
	return 0
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
