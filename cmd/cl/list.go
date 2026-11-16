package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aiseeq/akit/session"
)

const (
	listFlag  = "--cl-ls"
	listUsage = "использование: cl --cl-ls [--under <папка>] [--json]"
)

type listedSession struct {
	Name string `json:"name"`
	Pid  int    `json:"pid"`
	Cwd  string `json:"cwd"`
}

func runList(args []string, registry session.Registry, stdout, stderr io.Writer) int {
	under, asJSON, err := parseListArgs(args)
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
		listed = append(listed, listedSession{Name: s.Name, Pid: s.Pid, Cwd: cwd})
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
		fmt.Fprintf(stdout, "%s\t%d\t%s\n", s.Name, s.Pid, s.Cwd)
	}
	return 0
}

func parseListArgs(args []string) (string, bool, error) {
	var under string
	var underSet, asJSON bool
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--under":
			if underSet || i+1 >= len(args) || args[i+1] == "" || strings.HasPrefix(args[i+1], "--") {
				return "", false, fmt.Errorf("--under требует одну папку")
			}
			i++
			under = args[i]
			underSet = true
		case "--json":
			if asJSON {
				return "", false, fmt.Errorf("повторный --json")
			}
			asJSON = true
		default:
			return "", false, fmt.Errorf("неизвестный аргумент %q", args[i])
		}
	}
	return under, asJSON, nil
}

func pathWithin(path, dir string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}
