package main

import (
	"fmt"
	"os"
	"strings"
)

// Слайс агентов: вес процессора и диска ниже, чем у приложений пользователя (100), — машина отдаёт ресурсы
// пользователю, а не агентам. Файл слайса — agents.slice рядом, ставится в ~/.config/systemd/user (deploy/install.sh).
const agentsSlice = "agents.slice"

// scopeArgv — запуск claude в своём scope слайса агентов: systemd-run --scope регистрирует scope и делает exec
// команды в том же процессе, так что терминал, сигналы и код выхода — как у голого claude. Имя scope —
// agent-<имя сессии>-<id> (id — pid cl, у перезапусков с -r<n>), оно видно в /proc/<pid>/cgroup.
func scopeArgv(systemdRun, claude, name, id string, args []string) []string {
	if name == "" {
		name = "claude"
	}
	unit := "agent-" + unitSafe(name) + "-" + unitSafe(id)
	argv := []string{systemdRun, "--user", "--scope", "--quiet", "--collect", "--slice=" + agentsSlice, "--unit=" + unit, "--", claude}
	return append(argv, args...)
}

// unitSafe оставляет в имени юнита только [A-Za-z0-9_.-], прочее — «_».
func unitSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-' {
			return r
		}
		return '_'
	}, s)
}

// sliceInstalled: без файла слайса systemd создаст пустой слайс с весом по умолчанию, и агенты молча получат
// равный с пользователем приоритет — поэтому отказ с подсказкой, а не запуск.
func sliceInstalled(path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("нет %s: агенты получили бы приоритет наравне с пользователем; поставить: "+
			"cp ~/git/aguard/cmd/cl/agents.slice ~/.config/systemd/user/ && systemctl --user daemon-reload", path)
	}
	return nil
}

// nameArg — значение -n/--name из аргументов, если есть.
func nameArg(args []string) string {
	for i, a := range args {
		if v, ok := strings.CutPrefix(a, "--name="); ok {
			return v
		}
		if (a == "-n" || a == "--name") && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
