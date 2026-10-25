package main

import (
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Перезапуск в режиме act целиком: первая проверка (через 10 с — меньше interval() не даёт) видит простаивающую
// сессию на старой версии, cl шлёт SIGTERM и поднимает claude снова с флагами сессии (dafd514: --allowedTools тоже),
// именем из реестра, повторяемыми аргументами пользователя, --resume и строкой роли перезапуска; код выхода второго
// запуска — код cl. systemd-run подменён: первый запуск становится сессией подменённого мира и ждёт сигнала, второй
// записывает аргументы и выходит с кодом 3.
func TestSuperviseRestart(t *testing.T) {
	w := newSessionWorld(t, worldTimes{now: time.Now(), turnAgo: 10 * time.Minute, ttyAgo: 20 * time.Minute})
	d := w.dir
	write(t, filepath.Join(d, "config.json"), `{"restart":{"mode":"act","idle_minutes":1,"check_seconds":10,`+
		`"journal":"`+filepath.Join(d, "restart.log")+`","resume_dir":"`+filepath.Join(d, "restart")+`"},`+
		`"roles":[{"names":["x-*"],"text":"Роль ({why}).","on_start":"always"}],"restart_note":"Служебная."}`)
	// Нет перезапуска — sleep выходит сам через 25 с, и тест падает на отсутствии второго запуска, а не висит.
	run := fakeTool(t, filepath.Join(d, "bin"), "systemd-run", "D='"+d+"'\n"+
		`if [ -e "$D/run1" ]; then printf '%s\n' "$@" > "$D/run2"; exit 3; fi
printf '%s\n' "$@" > "$D/run1"
ln -s "$D/proc-tmpl" "$D/proc/$$" && ln -s "$D/session.json" "$D/sessions/$$.json"
exec sleep 25`)
	t.Cleanup(func() { signal.Reset(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP) })
	l := launcher{systemdRun: run, claude: w.p.claude, home: d, configFile: filepath.Join(d, "config.json"), paths: w.p, name: "x-master"}

	code := l.supervise(append(sessionFlags(d), "-n", "x-master"), []string{"--model", "opus", "--effort=high"})
	if code != 3 {
		t.Errorf("код выхода cl = %d, want 3 (код второго запуска)", code)
	}
	unit := "--unit=agent-x-master-" + strconv.Itoa(os.Getpid())
	scope := []string{"--user", "--scope", "--quiet", "--collect", "--slice=agents.slice"}
	// Свои аргументы — строки роли при старте нет (bare).
	first := append(append(slices.Clone(scope), unit, "--", w.p.claude),
		"--dangerously-skip-permissions", "--allowedTools=Grep,Glob", "-n", "x-master", "--model", "opus", "--effort=high")
	if got := readLines(t, filepath.Join(d, "run1")); !slices.Equal(got, first) {
		t.Errorf("первый запуск:\n%q\nwant\n%q", got, first)
	}
	second := append(append(slices.Clone(scope), unit+"-r1", "--", w.p.claude),
		"--dangerously-skip-permissions", "--allowedTools=Grep,Glob", "-n", "x-master", "--model", "opus", "--effort=high",
		"--resume", "s1", "Роль (перезапуск cl на Claude Code 2.1.310). Служебная.")
	if _, err := os.Stat(filepath.Join(d, "run2")); err != nil {
		t.Fatalf("второго запуска нет: %v; журнал:\n%s", err, strings.Join(readLines(t, filepath.Join(d, "restart.log")), "\n"))
	}
	if got := readLines(t, filepath.Join(d, "run2")); !slices.Equal(got, second) {
		t.Errorf("перезапуск:\n%q\nwant\n%q", got, second)
	}
	if journalText := strings.Join(readLines(t, filepath.Join(d, "restart.log")), "\n"); !strings.Contains(journalText, "x-master pid=") ||
		!strings.Contains(journalText, "act: перезапускаю") || !strings.Contains(journalText, "SIGTERM, поднимаю --resume s1 на 2.1.310") {
		t.Errorf("журнал:\n%s", journalText)
	}
}

// exitCode — код claude как есть, убит сигналом — 128+сигнал, как у оболочки.
func TestExitCode(t *testing.T) {
	if got := exitCode(nil); got != 0 {
		t.Errorf("успех: %d", got)
	}
	if got := exitCode(exec.Command("/bin/sh", "-c", "exit 7").Run()); got != 7 {
		t.Errorf("exit 7: %d", got)
	}
	if got := exitCode(exec.Command("/bin/sh", "-c", "kill -TERM $$").Run()); got != 128+int(syscall.SIGTERM) {
		t.Errorf("SIGTERM: %d", got)
	}
}
