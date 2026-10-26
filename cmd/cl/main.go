// cl — короткий запуск Claude Code: `claude --dangerously-skip-permissions` с именем сессии по папке запуска.
//
// Имя берётся из таблицы ~/.config/cl/names.tsv (строка «папка<TAB>имя», ~ раскрывается); папки нет в таблице, а
// она прямо в ~/git или ~/work — имя <папка>-master; иначе имя не задаётся (Claude Code выведет своё). Имя уже
// занято живой сессией — к нему добавляется -2, -3… Своё -n/--name, режим -p и подкоманды (mcp, auth, …) cl не
// трогает. Сессия запускается в своём scope слайса agents.slice (scope.go): вес процессора и диска ниже, чем у
// приложений пользователя.
//
// Интерактивную сессию cl держит дочерним процессом (supervise.go): простаивающую сессию на старой версии Claude Code
// он перезапускает с той же беседой, когда aguard говорит, что можно (internal/restart, ответ по сокету — ask.go),
// первой репликой — строка роли из ~/.config/cl/config.json. Режим log (по умолчанию) — только журнал решений, act —
// перезапуск.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/aiseeq/aguard/internal/names"
	"github.com/aiseeq/akit/session"
)

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		fail(err)
	}
	claude, err := lookClaude()
	if err != nil {
		fail(err)
	}
	// CL_CONFIG — другой конфиг: проверка перезапуска на тестовой сессии, не трогая общий.
	configFile := os.Getenv("CL_CONFIG")
	if configFile == "" {
		configFile = filepath.Join(home, ".config", "cl", "config.json")
	}
	l := launcher{claude: claude, home: home, configFile: configFile, registry: session.ForHome(home), aguard: aguardSocket()}
	args := os.Args[1:]
	if len(args) == 1 && args[0] == checkFlag {
		os.Exit(runCheck(l))
	}
	if l.systemdRun, err = exec.LookPath("systemd-run"); err != nil {
		fail(fmt.Errorf("без systemd-run сессия не попадёт в %s: %w", agentsSlice, err))
	}
	if err := sliceInstalled(filepath.Join(home, ".config", "systemd", "user", agentsSlice)); err != nil {
		fail(err)
	}
	if !interactive(args) {
		argv := scopeArgv(l.systemdRun, claude, nameArg(args), fmt.Sprint(os.Getpid()), printArgs(home, args))
		if err := syscall.Exec(l.systemdRun, argv, os.Environ()); err != nil {
			fail(err)
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		fail(err)
	}
	table, err := names.ReadTable(filepath.Join(home, names.File), home)
	if err != nil {
		fail(err)
	}
	pre := sessionFlags(home)
	l.name = nameArg(args)
	if name := names.For(cwd, home, table); name != "" && l.name == "" {
		taken, err := liveNames(filepath.Join(home, ".claude", "sessions"))
		if err != nil {
			fail(err)
		}
		l.name = free(name, taken)
		pre = append(pre, "-n", l.name)
	}
	os.Exit(l.supervise(pre, args))
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "cl:", err)
	os.Exit(1)
}
