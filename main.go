// cl — короткий запуск Claude Code: `claude --dangerously-skip-permissions` с именем сессии по папке запуска.
//
// Имя берётся из таблицы ~/.config/cl/names.tsv (строка «папка<TAB>имя», ~ раскрывается); папки нет в таблице, а
// она прямо в ~/git или ~/work — имя <папка>-master; иначе имя не задаётся (Claude Code выведет своё). Имя уже
// занято живой сессией — к нему добавляется -2, -3… Своё -n/--name, режим -p и подкоманды (mcp, auth, …) cl не
// трогает.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
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
	args := os.Args[1:]
	if interactive(args) {
		cwd, err := os.Getwd()
		if err != nil {
			fail(err)
		}
		table, err := readTable(filepath.Join(home, ".config", "cl", "names.tsv"), home)
		if err != nil {
			fail(err)
		}
		pre := []string{"--dangerously-skip-permissions"}
		if name := nameFor(cwd, home, table); name != "" && !hasName(args) {
			taken, err := liveNames(filepath.Join(home, ".claude", "sessions"))
			if err != nil {
				fail(err)
			}
			pre = append(pre, "-n", free(name, taken))
		}
		args = append(pre, args...)
	}
	argv := append([]string{"claude"}, args...)
	if err := syscall.Exec(claude, argv, os.Environ()); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "cl:", err)
	os.Exit(1)
}
