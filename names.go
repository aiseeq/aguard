package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// subcommands — подкоманды claude (claude --help, 2.1.302): им флаги сессии не нужны.
var subcommands = map[string]bool{
	"agents": true, "attach": true, "auth": true, "auto-mode": true, "doctor": true, "gateway": true,
	"import": true, "install": true, "logs": true, "mcp": true, "plugin": true, "plugins": true, "purge": true,
	"respawn": true, "rm": true, "setup-token": true, "stop": true, "kill": true, "update": true, "upgrade": true,
}

func lookClaude() (string, error) {
	return exec.LookPath("claude")
}

// interactive — запуск сессии, а не подкоманда и не разовый -p.
func interactive(args []string) bool {
	if len(args) > 0 && subcommands[args[0]] {
		return false
	}
	for _, a := range args {
		if a == "-p" || a == "--print" {
			return false
		}
	}
	return true
}

func hasName(args []string) bool {
	for _, a := range args {
		if a == "-n" || a == "--name" || strings.HasPrefix(a, "--name=") {
			return true
		}
	}
	return false
}

// readTable читает «папка<TAB>имя»; пустые строки и # — пропуск. Нет файла — пустая таблица.
func readTable(path, home string) (map[string]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("таблица имён: %w", err)
	}
	defer f.Close()
	table := map[string]string{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		dir, name, ok := strings.Cut(line, "\t")
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("%s:%d: нужно «папка<TAB>имя»", path, n)
		}
		table[expand(strings.TrimSpace(dir), home)] = strings.TrimSpace(name)
	}
	return table, sc.Err()
}

func expand(dir, home string) string {
	if dir == "~" {
		return home
	}
	if rest, ok := strings.CutPrefix(dir, "~/"); ok {
		return filepath.Join(home, rest)
	}
	return filepath.Clean(dir)
}

// nameFor — имя из таблицы, иначе master-<проект> для ~/git/<проект> и ~/work/<проект>.
func nameFor(cwd, home string, table map[string]string) string {
	cwd = filepath.Clean(cwd)
	if name, ok := table[cwd]; ok {
		return name
	}
	parent := filepath.Dir(cwd)
	if parent == filepath.Join(home, "git") || parent == filepath.Join(home, "work") {
		return "master-" + strings.ToLower(filepath.Base(cwd))
	}
	return ""
}

// liveNames — имена живых сессий из реестра Claude Code (<pid>.json с полем name). Файл, исчезнувший между
// списком и чтением (сессия вышла), — не ошибка.
func liveNames(dir string) (map[string]bool, error) {
	names := map[string]bool{}
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, fmt.Errorf("реестр сессий: %w", err)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("реестр сессий: %w", err)
		}
		var s struct {
			PID  int    `json:"pid"`
			Name string `json:"name"`
		}
		if json.Unmarshal(data, &s) != nil || s.Name == "" || !alive(s.PID) {
			continue
		}
		names[s.Name] = true
	}
	return names, nil
}

func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid)))
	return err == nil
}

// free — name, а занято — name-2, name-3…
func free(name string, taken map[string]bool) string {
	if !taken[name] {
		return name
	}
	for i := 2; ; i++ {
		if c := name + "-" + strconv.Itoa(i); !taken[c] {
			return c
		}
	}
}
