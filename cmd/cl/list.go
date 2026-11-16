package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/restart"
	"github.com/aiseeq/akit/session"
)

const (
	listFlag  = "--cl-ls"
	listUsage = "использование: cl --cl-ls [--under <папка>] [--json] [--timers]"
)

type listedTimer struct {
	Schedule string `json:"schedule"`
	Prompt   string `json:"prompt"`
}

type listedSession struct {
	Name        string         `json:"name"`
	Pid         int            `json:"pid"`
	Cwd         string         `json:"cwd"`
	Timers      *[]listedTimer `json:"timers,omitempty"`
	TimersError string         `json:"timers_error,omitempty"`
}

func runList(args []string, registry session.Registry, stdout, stderr io.Writer) int {
	under, asJSON, timers, err := parseListArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "cl: %v\n%s\n", err, listUsage)
		return 2
	}
	if under != "" {
		under, err = filepath.Abs(under)
		if err != nil {
			fmt.Fprintf(stderr, "cl: %v\n%s\n", err, listUsage)
			return 2
		}
	}

	sessions, err := registry.List()
	if err != nil {
		if len(sessions) == 0 {
			if _, dirErr := os.ReadDir(registry.Dir); dirErr != nil {
				fmt.Fprintf(stderr, "cl: %v; каталог реестра: %v\n", err, dirErr)
				return 2
			}
		}
		fmt.Fprintf(stderr, "cl: %v\n", err)
	}

	listed := make([]listedSession, 0, len(sessions))
	for i := range sessions {
		s := &sessions[i]
		if !registry.IsAlive(s.Pid, s.ProcStart) {
			continue
		}
		cwd := s.Cwd
		if under != "" && !pathWithin(cwd, under) {
			continue
		}
		item := listedSession{Name: s.Name, Pid: s.Pid, Cwd: cwd}
		if timers {
			itemTimers, timerErr := listedSessionTimers(registry, *s)
			if timerErr != nil {
				item.TimersError = timerErr.Error()
			} else {
				item.Timers = &itemTimers
			}
		}
		listed = append(listed, item)
	}
	slices.SortFunc(listed, func(a, b listedSession) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return a.Pid - b.Pid
	})

	if asJSON {
		if err := json.NewEncoder(stdout).Encode(listed); err != nil {
			fmt.Fprintf(stderr, "cl: вывод списка сессий: %v\n", err)
			return 2
		}
		return 0
	}
	for _, s := range listed {
		if timers {
			if s.TimersError != "" {
				fmt.Fprintf(stdout, "%s\tошибка: %s\n", s.Name, s.TimersError)
				continue
			}
			for _, timer := range *s.Timers {
				fmt.Fprintf(stdout, "%s\t%s\t%s\n", s.Name, timer.Schedule, timer.Prompt)
			}
			continue
		}
		fmt.Fprintf(stdout, "%s\t%d\t%s\n", s.Name, s.Pid, s.Cwd)
	}
	return 0
}

func listedSessionTimers(registry session.Registry, s session.Info) ([]listedTimer, error) {
	timers := make([]listedTimer, 0)
	if s.SessionID == "" {
		return timers, nil
	}
	crons, err := restart.SessionCrons(filepath.Join(filepath.Dir(registry.Dir), "projects"), s.SessionID,
		time.UnixMilli(s.StartedAt), time.Now())
	if err != nil {
		return nil, err
	}
	for _, cron := range crons {
		timers = append(timers, listedTimer{Schedule: cron.Cron, Prompt: timerPrompt(cron.Prompt)})
	}
	return timers, nil
}

func parseListArgs(args []string) (string, bool, bool, error) {
	var under string
	var underSet, asJSON, timers bool
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--under":
			if underSet || i+1 >= len(args) || args[i+1] == "" || strings.HasPrefix(args[i+1], "--") {
				return "", false, false, fmt.Errorf("--under требует одну папку")
			}
			i++
			under = args[i]
			underSet = true
		case "--json":
			if asJSON {
				return "", false, false, fmt.Errorf("повторный --json")
			}
			asJSON = true
		case "--timers":
			if timers {
				return "", false, false, fmt.Errorf("повторный --timers")
			}
			timers = true
		default:
			return "", false, false, fmt.Errorf("неизвестный аргумент %q", args[i])
		}
	}
	return under, asJSON, timers, nil
}

func timerPrompt(prompt string) string {
	prompt = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "\t", " ").Replace(prompt)
	return restart.ShortPrompt(prompt)
}

func pathWithin(path, dir string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}
