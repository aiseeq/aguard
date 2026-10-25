package main

import (
	"errors"
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
	socket := fakeAguard(t, `{"v":1,"restart":true,"key":"true,false","session_id":"s1","name":"x-master"}`)
	l := launcher{systemdRun: run, claude: w.p.claude, home: d, configFile: filepath.Join(d, "config.json"), paths: w.p, name: "x-master",
		aguard: socket}

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
		!strings.Contains(journalText, "act: cl за перезапуск") || !strings.Contains(journalText, "SIGTERM, поднимаю --resume s1 на 2.1.310") {
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

// Сжатие в режиме act целиком, со сбоем сжатия: версия свежая (перезапуска нет), aguard отвечает «сжать». cl шлёт
// SIGTERM, запускает `claude -p --resume s1 /compact` — оно падает, — и всё равно поднимает беседу с теми же флагами,
// без строки роли (таймеров не было); в журнале — «сжимаю» и «не сжалось … поднимаю без сжатия».
func TestSuperviseCompact(t *testing.T) {
	w := newSessionWorld(t, worldTimes{now: time.Now(), turnAgo: 10 * time.Minute, ttyAgo: 20 * time.Minute})
	w.setRunning(t, "2.1.310")
	d := w.dir
	write(t, filepath.Join(d, "config.json"), `{"restart":{"mode":"act","idle_minutes":1,"check_seconds":10,`+
		`"journal":"`+filepath.Join(d, "restart.log")+`","resume_dir":"`+filepath.Join(d, "restart")+`",`+
		`"compact_mode":"act","compact_names":["x-*"]},`+
		`"roles":[{"names":["x-*"],"text":"Роль ({why}).","on_start":"always"}]}`)
	run := fakeTool(t, filepath.Join(d, "bin"), "systemd-run", "D='"+d+"'\n"+
		`if [ -e "$D/run2" ]; then printf '%s\n' "$@" > "$D/run3"; exit 3; fi
if [ -e "$D/run1" ]; then printf '%s\n' "$@" > "$D/run2"; echo 'API Error: overloaded'; exit 1; fi
printf '%s\n' "$@" > "$D/run1"
ln -s "$D/proc-tmpl" "$D/proc/$$" && ln -s "$D/session.json" "$D/sessions/$$.json"
exec sleep 25`)
	t.Cleanup(func() { signal.Reset(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP) })
	socket := fakeAguard(t, `{"v":1,"restart":false,"fresh":true,"key":"fresh","session_id":"s1","name":"x-master",`+
		`"compact":true,"compact_key":"true","tokens":150000}`)
	l := launcher{systemdRun: run, claude: w.p.claude, home: d, configFile: filepath.Join(d, "config.json"), paths: w.p,
		aguard: socket, name: "x-master"}

	if code := l.supervise(append(sessionFlags(d), "-n", "x-master"), []string{"--model", "opus"}); code != 3 {
		t.Errorf("код выхода cl = %d, want 3 (код подъёма)", code)
	}
	pid := strconv.Itoa(os.Getpid())
	scope := []string{"--user", "--scope", "--quiet", "--collect", "--slice=agents.slice"}
	own := sessionFlags(d)
	compact := append(append(append(slices.Clone(scope), "--unit=agent-x-master-"+pid+"-c1", "--", w.p.claude), own...),
		"--model", "opus", "-p", "--resume", "s1", "/compact")
	journalText := strings.Join(readLines(t, filepath.Join(d, "restart.log")), "\n")
	if _, err := os.Stat(filepath.Join(d, "run3")); err != nil {
		t.Fatalf("подъёма нет: %v; журнал:\n%s", err, journalText)
	}
	if got := readLines(t, filepath.Join(d, "run2")); !slices.Equal(got, compact) {
		t.Errorf("сжатие:\n%q\nwant\n%q", got, compact)
	}
	resume := append(append(append(slices.Clone(scope), "--unit=agent-x-master-"+pid+"-r1", "--", w.p.claude), own...),
		"-n", "x-master", "--model", "opus", "--resume", "s1")
	if got := readLines(t, filepath.Join(d, "run3")); !slices.Equal(got, resume) {
		t.Errorf("подъём:\n%q\nwant\n%q", got, resume)
	}
	for _, want := range []string{"act: сжимаю: контекст 150 тыс.", "не сжалось (exit status 1: API Error: overloaded) — поднимаю без сжатия"} {
		if !strings.Contains(journalText, want) {
			t.Errorf("в журнале нет %q:\n%s", want, journalText)
		}
	}
}

// wantCompact: log и act вне пилота — «сжал бы» один раз на решение; ответ о другой беседе — не сжимать; таймеры —
// подъём со строкой роли.
func TestWantCompact(t *testing.T) {
	cfg := defaultConfig(t.TempDir())
	l := launcher{name: "x-master"}
	f := facts{session: session{SessionID: "s1", Name: "x-master"}}
	a := aguardAnswer{SessionID: "s1", Compact: true, CompactKey: "true", Tokens: 120_000}
	out, line, key := l.wantCompact(cfg, 7, f, a, "")
	if out.restart || !strings.Contains(line, "log: сжал бы: контекст 120 тыс.") {
		t.Errorf("log: %+v %q", out, line)
	}
	if _, line, _ := l.wantCompact(cfg, 7, f, a, key); line != "" {
		t.Errorf("повтор того же решения: %q", line)
	}
	cfg.Restart.CompactMode, cfg.Restart.CompactNames = modeAct, []string{"lead"}
	if out, line, _ := l.wantCompact(cfg, 7, f, a, key); out.restart || !strings.Contains(line, "сжал бы (не в compact_names)") {
		t.Errorf("вне пилота: %+v %q", out, line)
	}
	cfg.Restart.CompactNames = nil
	if out, line, _ := l.wantCompact(cfg, 7, f, aguardAnswer{SessionID: "s2", Compact: true}, ""); out.restart || !strings.Contains(line, "не сжимаю") {
		t.Errorf("другая беседа: %+v %q", out, line)
	}
	f.Transcript.Crons = []cronJob{{ID: "j1", Prompt: "Сон"}}
	if out, _, _ := l.wantCompact(cfg, 7, f, a, ""); !out.restart || !out.compact || !out.crons || out.sessionID != "s1" {
		t.Errorf("act: %+v", out)
	}
	cfg.Restart.CompactMode = modeOff
	if out, line, _ := l.wantCompact(cfg, 7, f, a, ""); out.restart || line != "" {
		t.Errorf("off: %+v %q", out, line)
	}
}

// Перезапуск в act — только если aguard тоже за и о той же беседе; иначе строка в журнал один раз на решение.
func TestNextNeedsAguard(t *testing.T) {
	cfg := defaultConfig(t.TempDir())
	cfg.Restart.Mode = modeAct
	l := launcher{name: "x-master"}
	f := facts{session: session{SessionID: "s1", Name: "x-master"}, Installed: "2.1.311"}
	v := verdict{Restart: true}
	now := time.Now()
	cases := []struct {
		name string
		a    aguardAnswer
		err  error
		want string
	}{
		{"нет ответа", aguardAnswer{}, errors.New("dial: нет сокета"), "aguard не ответил"},
		{"против", aguardAnswer{SessionID: "s1", Key: "false,false,screen"}, nil, "aguard против: false,false,screen"},
		{"другая беседа", aguardAnswer{SessionID: "s2", Restart: true}, nil, `aguard о беседе "s2"`},
	}
	for _, c := range cases {
		out, last := l.next(cfg, now, 7, f, v, c.a, c.err, nextLines{})
		if out.restart || !strings.Contains(last.veto, c.want) {
			t.Errorf("%s: %+v %q", c.name, out, last.veto)
		}
		if _, again := l.next(cfg, now, 7, f, v, c.a, c.err, last); again != last {
			t.Errorf("%s: ключ сменился на повторе: %q", c.name, again.veto)
		}
	}
	journalText := strings.Join(readLines(t, cfg.Restart.Journal), "\n")
	if n := strings.Count(journalText, "перезапустил бы, но"); n != len(cases) {
		t.Errorf("строк вето %d, ждали %d:\n%s", n, len(cases), journalText)
	}
	out, _ := l.next(cfg, now, 7, f, v, aguardAnswer{SessionID: "s1", Restart: true}, nil, nextLines{})
	if !out.restart || out.sessionID != "s1" || out.version != "2.1.311" {
		t.Errorf("оба за — перезапуск: %+v", out)
	}
	cfg.Restart.Mode = modeLog
	if out, last := l.next(cfg, now, 7, f, v, aguardAnswer{SessionID: "s1", Restart: true}, nil, nextLines{}); out.restart || last.veto != "" {
		t.Errorf("log — без перезапуска: %+v %+v", out, last)
	}
}
