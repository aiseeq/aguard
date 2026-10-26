package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

// searchToolsFlag возвращает в Bash настоящие grep и find вместо встроенных ugrep и bfs, а сессии — инструменты Grep и
// Glob, не урезая остальной набор (tools-reference, «Glob tool behavior»; отдельного выключателя подмены нет: #69736).
// Форма с «=»: без неё вариадический флаг съел бы позиционный промпт (строку роли).
const searchToolsFlag = "--allowedTools=Grep,Glob"

// sharedDirs — общие папки вне корня сессии: worktree (graft worktree add) и временные файлы Claude Code
// (CLAUDE_CODE_TMPDIR, scratchpad). Без --add-dir Claude Code возвращает cwd Bash в корень после каждого cd в них.
var sharedDirs = []string{filepath.Join(".cache", "worktrees"), filepath.Join(".cache", "claude-tmp")}

// addDirFlags — --add-dir на каждую общую папку, которая есть. Форма с «=», как у searchToolsFlag: вариадический
// флаг съел бы позиционный промпт. Несуществующую папку не передаём, чтобы её отсутствие не мешало запуску; другой
// сбой stat — в stderr и тоже без флага: сессия важнее.
func addDirFlags(home string) []string {
	var flags []string
	for _, rel := range sharedDirs {
		dir := filepath.Join(home, rel)
		info, err := os.Stat(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			fmt.Fprintln(os.Stderr, "cl: --add-dir:", err)
		case info.IsDir():
			flags = append(flags, "--add-dir="+dir)
		}
	}
	return flags
}

// sessionFlags — флаги каждого запуска сессии, первого и после перезапуска на новую версию.
func sessionFlags(home string) []string {
	return append([]string{"--dangerously-skip-permissions", searchToolsFlag}, addDirFlags(home)...)
}

// printArgs — аргументы разового запуска: -p получает те же grep и find и общие папки, что и сессия, подкоманды —
// как есть.
func printArgs(home string, args []string) []string {
	if len(args) > 0 && subcommands[args[0]] {
		return args
	}
	return append(append([]string{searchToolsFlag}, addDirFlags(home)...), args...)
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
