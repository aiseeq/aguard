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
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"syscall"
	"time"

	"github.com/aiseeq/aguard/internal/ceiling"
	"github.com/aiseeq/aguard/internal/names"
	"github.com/aiseeq/akit/session"
)

func main() {
	if args, ok := oomPrefixArgs(os.Args[0], os.Args[1:]); ok {
		runOOMPrefix(args)
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fail(err)
	}
	claude, err := lookClaude()
	if err != nil {
		fail(err)
	}
	// CL_CONFIG — другой конфиг: проверка перезапуска на тестовой сессии, не трогая общий.
	configFile := ceiling.ClConfigPath(home)
	l := launcher{claude: claude, home: home, configFile: configFile, registry: session.ForHome(home), aguard: aguardSocket(),
		selfExe: "/proc/self/exe", exec: syscall.Exec}
	if l.upgraded, _, err = takeUpgrade(); err != nil { // не разобралось — старт как обычно: новая беседа, но консоль жива
		fmt.Fprintln(os.Stderr, "cl:", err)
		l.journalUpgradeFail(err, "новая беседа, прежний claude без хозяина pty")
	}
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
		cfg, configErr := l.readConfig()
		if configErr != nil {
			journal(cfg.Restart.Journal, time.Now(), "конфиг cl: "+configErr.Error())
		}
		argv, env := prepareScope(home, l.systemdRun, claude, nameArg(args), fmt.Sprint(os.Getpid()), printArgs(home, args), cfg.Memory, os.Environ(), func(e error) {
			journal(cfg.Restart.Journal, time.Now(), "потолок памяти не поставлен: "+e.Error())
		})
		if err := syscall.Exec(l.systemdRun, argv, env); err != nil {
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
	l.dir = cwd
	if l.upgraded != nil {
		l.name = l.upgraded.Name
	} else if name := names.For(cwd, home, table); name != "" && l.name == "" {
		taken, err := liveNames(filepath.Join(home, ".claude", "sessions"))
		if err != nil {
			fail(err)
		}
		l.name = free(name, taken)
		pre = append(pre, "-n", l.name)
	}
	os.Exit(l.supervise(pre, args))
}

func oomPrefixArgs(argv0 string, args []string) ([]string, bool) {
	if filepath.Base(argv0) == "cl-oom-prefix" {
		return args, true
	}
	if len(args) > 0 && args[0] == "oom-prefix" {
		return args[1:], true
	}
	return nil, false
}

func runOOMPrefix(args []string) {
	writeErr, execErr := executeOOMPrefix(args, writeOOMScoreAdj, syscall.Exec, os.Environ())
	if writeErr != nil {
		fmt.Fprintln(os.Stderr, "cl oom-prefix:", writeErr)
	}
	if execErr != nil {
		fmt.Fprintln(os.Stderr, "cl oom-prefix:", execErr)
		os.Exit(oomExecFailure)
	}
}

const oomExecFailure = 127

func writeOOMScoreAdj(value []byte) error {
	f, err := os.OpenFile("/proc/self/oom_score_adj", os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open oom_score_adj: %w", err)
	}
	n, writeErr := f.Write(value)
	if writeErr == nil && n != len(value) {
		writeErr = io.ErrShortWrite
	}
	return errors.Join(writeErr, f.Close())
}

func executeOOMPrefix(args []string, writeAdj func([]byte) error, execFn func(string, []string, []string) error, env []string) (error, error) {
	writeErr := writeAdj([]byte("1000"))
	var argv []string
	if len(args) == 1 {
		argv = []string{"/bin/bash", "-c", args[0]}
	} else {
		argv = args
	}
	if len(argv) == 0 {
		return writeErr, nil
	}
	return writeErr, execFn(argv[0], argv, env)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "cl:", err)
	os.Exit(1)
}
