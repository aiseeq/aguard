package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/restart"
)

// Перезапуск в режиме act целиком: первая проверка (через 10 с — меньше interval() не даёт) получает от aguard
// «перезапустить», cl шлёт SIGTERM и поднимает claude снова с флагами сессии (dafd514: --allowedTools тоже), именем
// из ответа, повторяемыми аргументами пользователя, --resume и строкой роли перезапуска; код выхода второго запуска —
// код cl. systemd-run подменён: первый запуск ждёт сигнала, второй записывает аргументы и выходит с кодом 3.
func TestSuperviseRestart(t *testing.T) {
	w := newWorld(t)
	d := w.dir
	write(t, filepath.Join(d, "config.json"), `{"restart":{"mode":"act","check_seconds":10,`+
		`"journal":"`+filepath.Join(d, "restart.log")+`","resume_dir":"`+filepath.Join(d, "restart")+`"},`+
		`"roles":[{"names":["x-*"],"text":"Роль ({why}).","on_start":"always"}]}`)
	// Нет перезапуска — sleep выходит сам через 25 с, и тест падает на отсутствии второго запуска, а не висит.
	run := fakeTool(t, filepath.Join(d, "bin"), "systemd-run", "D='"+d+"'\n"+
		`if [ -e "$D/run1" ]; then printf '%s\n' "$@" > "$D/run2"; exit 3; fi
printf '%s\n' "$@" > "$D/run1"
exec sleep 25`)
	t.Cleanup(func() { signal.Reset(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP) })
	socket := fakeAguard(t, `{"v":1,"restart":true,"key":"true,false","session_id":"s1","name":"x-master",`+
		`"running":"2.1.309","installed":"2.1.310","crons":true}`) // таймеры есть — реплики всё равно нет: --resume их вернёт
	l := launcher{systemdRun: run, claude: w.claude, home: d, configFile: filepath.Join(d, "config.json"), name: "x-master",
		aguard: socket}

	code := l.supervise(append(sessionFlags(d), "-n", "x-master"), []string{"--model", "opus", "--effort=high"})
	if code != 3 {
		t.Errorf("код выхода cl = %d, want 3 (код второго запуска)", code)
	}
	unit := "--unit=agent-x-master-" + strconv.Itoa(os.Getpid())
	scope := []string{"--user", "--scope", "--quiet", "--collect", "--slice=agents.slice", "--property=IOAccounting=yes"}
	// Свои аргументы — строки роли при старте нет (bare).
	first := append(append(slices.Clone(scope), unit, "--", w.claude),
		"--dangerously-skip-permissions", "--allowedTools=Grep,Glob", "-n", "x-master", "--model", "opus", "--effort=high")
	if got := readLines(t, filepath.Join(d, "run1")); !slices.Equal(got, first) {
		t.Errorf("первый запуск:\n%q\nwant\n%q", got, first)
	}
	second := append(append(slices.Clone(scope), unit+"-r1", "--", w.claude),
		"--dangerously-skip-permissions", "--allowedTools=Grep,Glob", "-n", "x-master", "--model", "opus", "--effort=high",
		"--resume", "s1")
	if _, err := os.Stat(filepath.Join(d, "run2")); err != nil {
		t.Fatalf("второго запуска нет: %v; журнал:\n%s", err, strings.Join(readLines(t, filepath.Join(d, "restart.log")), "\n"))
	}
	if got := anyClock(readLines(t, filepath.Join(d, "run2"))); !slices.Equal(got, second) {
		t.Errorf("перезапуск:\n%q\nwant\n%q", got, second)
	}
	if journalText := strings.Join(readLines(t, filepath.Join(d, "restart.log")), "\n"); !strings.Contains(journalText, "x-master pid=") ||
		!strings.Contains(journalText, "2.1.309→2.1.310 act: перезапускаю") || !strings.Contains(journalText, "SIGTERM, поднимаю --resume s1 на 2.1.310") {
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
	w := newWorld(t)
	d := w.dir
	write(t, filepath.Join(d, "config.json"), `{"restart":{"mode":"act","check_seconds":10,`+
		`"journal":"`+filepath.Join(d, "restart.log")+`","resume_dir":"`+filepath.Join(d, "restart")+`",`+
		`"compact_mode":"act","compact_names":["x-*"]},`+
		`"roles":[{"names":["x-*"],"text":"Роль ({why}).","on_start":"always"}]}`)
	run := fakeTool(t, filepath.Join(d, "bin"), "systemd-run", "D='"+d+"'\n"+
		`if [ -e "$D/run2" ]; then printf '%s\n' "$@" > "$D/run3"; exit 3; fi
if [ -e "$D/run1" ]; then printf '%s\n' "$@" > "$D/run2"; echo 'API Error: overloaded'; exit 1; fi
printf '%s\n' "$@" > "$D/run1"
exec sleep 25`)
	t.Cleanup(func() { signal.Reset(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP) })
	socket := fakeAguard(t, `{"v":1,"restart":false,"fresh":true,"key":"false,true","session_id":"s1","name":"x-master",`+
		`"running":"2.1.310","installed":"2.1.310","compact":true,"compact_key":"true","tokens":150000}`)
	l := launcher{systemdRun: run, claude: w.claude, home: d, configFile: filepath.Join(d, "config.json"),
		aguard: socket, name: "x-master"}

	if code := l.supervise(append(sessionFlags(d), "-n", "x-master"), []string{"--model", "opus"}); code != 3 {
		t.Errorf("код выхода cl = %d, want 3 (код подъёма)", code)
	}
	pid := strconv.Itoa(os.Getpid())
	scope := []string{"--user", "--scope", "--quiet", "--collect", "--slice=agents.slice", "--property=IOAccounting=yes"}
	own := sessionFlags(d)
	compact := append(append(append(slices.Clone(scope), "--unit=agent-x-master-"+pid+"-c1", "--", w.claude), own...),
		"--model", "opus", "-p", "--resume", "s1", "/compact")
	journalText := strings.Join(readLines(t, filepath.Join(d, "restart.log")), "\n")
	if _, err := os.Stat(filepath.Join(d, "run3")); err != nil {
		t.Fatalf("подъёма нет: %v; журнал:\n%s", err, journalText)
	}
	if got := readLines(t, filepath.Join(d, "run2")); !slices.Equal(got, compact) {
		t.Errorf("сжатие:\n%q\nwant\n%q", got, compact)
	}
	resume := append(append(append(slices.Clone(scope), "--unit=agent-x-master-"+pid+"-r1", "--", w.claude), own...),
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

// Сброс: в act cl останавливает claude и поднимает новую беседу — без --resume, с промптом роли и строкой роли (есть
// файл возобновления).
func TestSuperviseReset(t *testing.T) {
	w := newWorld(t)
	d := w.dir
	write(t, filepath.Join(d, "config.json"), `{"restart":{"mode":"act","check_seconds":10,`+
		`"journal":"`+filepath.Join(d, "restart.log")+`","resume_dir":"`+filepath.Join(d, "restart")+`",`+
		`"compact_mode":"act","reset_mode":"act","reset_names":["x-*"]},`+
		`"roles":[{"names":["x-*"],"text":"Роль ({why}).","on_start":"resume_file","prompt":"Ты {name}."}],"resume_text":"Файл: {path}."}`)
	resume := filepath.Join(d, "restart", "2026-10-26", "x-master.md")
	write(t, resume, "сводка")
	run := fakeTool(t, filepath.Join(d, "bin"), "systemd-run", "D='"+d+"'\n"+
		`if [ -e "$D/run1" ]; then printf '%s\n' "$@" > "$D/run2"; exit 4; fi
printf '%s\n' "$@" > "$D/run1"
exec sleep 25`)
	t.Cleanup(func() { signal.Reset(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP) })
	socket := fakeAguard(t, `{"v":1,"restart":false,"fresh":true,"key":"false,true","session_id":"s1","name":"x-master",`+
		`"running":"2.1.310","installed":"2.1.310","compact":false,"compact_key":"false,late","tokens":200000,`+
		`"reset":true,"reset_key":"true"}`)
	// бинарник cl заменён установкой: перед новой беседой cl переходит в новый; exec не вышел — дальше прежний
	exe := filepath.Join(d, "exe")
	symlink(t, "/opt/cl (deleted)", exe)
	var execed, state string
	l := launcher{systemdRun: run, claude: w.claude, home: d, configFile: filepath.Join(d, "config.json"),
		aguard: socket, name: "x-master", selfExe: exe, exec: func(path string, _, env []string) error {
			execed = path
			for _, e := range env {
				if s, ok := strings.CutPrefix(e, upgradeEnv+"="); ok {
					state = s
				}
			}
			return errors.New("нет файла")
		}}

	if code := l.supervise(append(sessionFlags(d), "-n", "x-master"), []string{"--model", "opus"}); code != 4 {
		t.Errorf("код выхода cl = %d, want 4 (код новой беседы)", code)
	}
	pid := strconv.Itoa(os.Getpid())
	scope := []string{"--user", "--scope", "--quiet", "--collect", "--slice=agents.slice", "--property=IOAccounting=yes"}
	fresh := append(append(append(slices.Clone(scope), "--unit=agent-x-master-"+pid+"-r1", "--", w.claude, "--append-system-prompt", "Ты x-master."), sessionFlags(d)...),
		"-n", "x-master", "--model", "opus", "Роль (cl начал новую беседу в ЧЧ:ММ вместо простаивавшей; Claude Code 2.1.310). Файл: "+
			filepath.Join(d, "restart", "2026-10-26", "used", "x-master.md")+".")
	journalText := strings.Join(readLines(t, filepath.Join(d, "restart.log")), "\n")
	if got := anyClock(readLines(t, filepath.Join(d, "run2"))); !slices.Equal(got, fresh) {
		t.Errorf("новая беседа:\n%q\nwant\n%q\nжурнал:\n%s", got, fresh, journalText)
	}
	for _, want := range []string{"act: сбрасываю: контекст 200 тыс.; SIGTERM, поднимаю новую беседу вместо s1",
		"x-master pid=" + pid + ": cl обновлён — перехожу в новый бинарник /opt/cl", "exec /opt/cl: нет файла — дальше прежний cl"} {
		if !strings.Contains(journalText, want) {
			t.Errorf("в журнале нет %q:\n%s", want, journalText)
		}
	}
	var u upgrade
	if err := json.Unmarshal([]byte(state), &u); err != nil || execed != "/opt/cl" || u.Name != "x-master" || u.N != 1 ||
		!slices.Equal(anyClock(u.Args), fresh[slices.Index(fresh, "--append-system-prompt")+2:]) {
		t.Errorf("exec %q, состояние %+v %v", execed, u, err)
	}
	// файл возобновления разовый: отдан новой беседе — убран, следующий подъём его не получит
	if got, err := freshResume(filepath.Join(d, "restart"), "x-master", 24*time.Hour, time.Now()); err != nil || got != "" {
		t.Errorf("отданный файл возобновления снова выдаётся: %q %v", got, err)
	}
}

// wantReset: log — «сбросил бы» один раз на решение; act вне пилота — тоже; act — новая беседа.
func TestWantReset(t *testing.T) {
	cfg := defaultConfig(t.TempDir())
	l := launcher{name: "x-master"}
	a := restart.Answer{SessionID: "s1", Name: "x-master", Reset: true, ResetKey: "true", Tokens: 200_000}
	out, line, key := l.wantReset(cfg, 7, a, "")
	if out.restart || !strings.Contains(line, "log: сбросил бы: контекст 200 тыс.") {
		t.Errorf("log: %+v %q", out, line)
	}
	if _, line, _ := l.wantReset(cfg, 7, a, key); line != "" {
		t.Errorf("повтор того же решения: %q", line)
	}
	cfg.Restart.ResetMode, cfg.Restart.ResetNames = modeAct, []string{"lead"}
	if out, line, _ := l.wantReset(cfg, 7, a, key); out.restart || !strings.Contains(line, "сбросил бы (не в reset_names)") {
		t.Errorf("вне пилота: %+v %q", out, line)
	}
	cfg.Restart.ResetNames = nil
	if out, _, _ := l.wantReset(cfg, 7, a, ""); !out.restart || !out.reset || out.compact || out.sessionID != "s1" {
		t.Errorf("act: %+v", out)
	}
	cfg.Restart.CompactMode = modeAct
	if out, _ := l.next(cfg, time.Now(), 7, restart.Answer{SessionID: "s1", Compact: true, Reset: true, Tokens: 1}, nil, journalKeys{}); out.compact || !out.reset {
		t.Errorf("сброс важнее сжатия: %+v", out)
	}
}

// wantCompact: log и act вне пилота — «сжал бы» один раз на решение; ответ без беседы — не сжимать; таймеры —
// подъём со строкой роли.
func TestWantCompact(t *testing.T) {
	cfg := defaultConfig(t.TempDir())
	l := launcher{name: "x-master"}
	a := restart.Answer{SessionID: "s1", Name: "x-master", Compact: true, CompactKey: "true", Tokens: 120_000,
		ResetAgainst: []restart.Reason{{Code: "tree", Text: "рабочее дерево: 2 изменений"}}}
	out, line, key := l.wantCompact(cfg, 7, a, "")
	if out.restart || !strings.Contains(line, "log: сжал бы: контекст 120 тыс.") || !strings.Contains(line, "; сброс нельзя: рабочее дерево: 2 изменений") {
		t.Errorf("log: %+v %q", out, line)
	}
	if _, line, _ := l.wantCompact(cfg, 7, a, key); line != "" {
		t.Errorf("повтор того же решения: %q", line)
	}
	cfg.Restart.CompactMode, cfg.Restart.CompactNames = modeAct, []string{"lead"}
	if out, line, _ := l.wantCompact(cfg, 7, a, key); out.restart || !strings.Contains(line, "сжал бы (не в compact_names)") {
		t.Errorf("вне пилота: %+v %q", out, line)
	}
	cfg.Restart.CompactNames = nil
	if out, line, _ := l.wantCompact(cfg, 7, restart.Answer{Compact: true}, ""); out.restart || !strings.Contains(line, "не сжимаю") {
		t.Errorf("без беседы: %+v %q", out, line)
	}
	if out, _, _ := l.wantCompact(cfg, 7, a, ""); !out.restart || !out.compact || out.sessionID != "s1" {
		t.Errorf("act: %+v", out)
	}
	cfg.Restart.CompactMode = modeOff
	if out, line, _ := l.wantCompact(cfg, 7, a, ""); out.restart || line != "" {
		t.Errorf("off: %+v %q", out, line)
	}
}

// next: решает aguard. Нет ответа или «нет» — ничего не делать, строка в журнал один раз на решение; «да» — перезапуск
// только в act.
func TestNext(t *testing.T) {
	cfg := defaultConfig(t.TempDir())
	cfg.Restart.Mode = modeAct
	l := launcher{name: "x-master"}
	now := time.Now()
	yes := restart.Answer{Verdict: restart.Verdict{Restart: true}, Key: "true,false", SessionID: "s1", Name: "x-master", Installed: "2.1.311"}
	no := restart.Answer{Verdict: restart.Verdict{Against: []restart.Reason{{Code: "screen", Text: "в поле ввода набранный текст"}}},
		Key: "false,false,screen", SessionID: "s1", Name: "x-master"}
	cases := []struct {
		name string
		a    restart.Answer
		err  error
		want string
	}{
		{"нет ответа", restart.Answer{}, errors.New("dial: нет сокета"), "aguard не ответил (dial: нет сокета)"},
		{"против", no, nil, "act: не перезапускаю: в поле ввода набранный текст"},
	}
	for _, c := range cases {
		out, last := l.next(cfg, now, 7, c.a, c.err, journalKeys{})
		if out.restart {
			t.Errorf("%s: %+v", c.name, out)
		}
		if _, again := l.next(cfg, now, 7, c.a, c.err, last); again != last {
			t.Errorf("%s: ключ сменился на повторе: %+v", c.name, again)
		}
	}
	journalText := strings.Join(readLines(t, cfg.Restart.Journal), "\n")
	for _, c := range cases {
		if strings.Count(journalText, c.want) != 1 {
			t.Errorf("%s: в журнале не одна строка %q:\n%s", c.name, c.want, journalText)
		}
	}
	if out, _ := l.next(cfg, now, 7, yes, nil, journalKeys{}); !out.restart || out.sessionID != "s1" || out.version != "2.1.311" {
		t.Errorf("aguard за — перезапуск: %+v", out)
	}
	if out, _ := l.next(cfg, now, 7, restart.Answer{Verdict: yes.Verdict, Key: yes.Key}, nil, journalKeys{}); out.restart {
		t.Errorf("без беседы — без перезапуска: %+v", out)
	}
	cfg.Restart.Mode = modeLog
	if out, _ := l.next(cfg, now, 7, yes, nil, journalKeys{}); out.restart {
		t.Errorf("log — без перезапуска: %+v", out)
	}
}

// anyClock заменяет время в {why} строки роли (часы теста) на «ЧЧ:ММ».
func anyClock(args []string) []string {
	clock := regexp.MustCompile(` в \d\d:\d\d `)
	for i := range args {
		args[i] = clock.ReplaceAllString(args[i], " в ЧЧ:ММ ")
	}
	return args
}
