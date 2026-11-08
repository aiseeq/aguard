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
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/restart"
)

// testCheck — срок проверки в тестах act вместо 10 с конфига: запас на старт подменного systemd-run, иначе SIGTERM
// придёт раньше записи его аргументов.
const testCheck = time.Second

// Перезапуск в режиме act целиком: первая проверка (через testCheck) получает от aguard
// «перезапустить», cl шлёт SIGTERM и поднимает claude снова с флагами сессии (dafd514: --allowedTools тоже), именем
// из ответа, повторяемыми аргументами пользователя, --resume и строкой роли перезапуска; код выхода второго запуска —
// код cl. Файл настроек роли (settings по папке сессии) — `--settings` в обоих запусках. systemd-run подменён: первый
// запуск ждёт сигнала, второй записывает аргументы и выходит с кодом 3.
func TestSuperviseRestart(t *testing.T) {
	w := newWorld(t)
	d := w.dir
	settings := filepath.Join(d, "role.json")
	write(t, settings, `{"permissions":{"deny":["Artifact"]}}`)
	write(t, filepath.Join(d, "config.json"), `{"restart":{"mode":"act","check_seconds":10,`+
		`"journal":"`+filepath.Join(d, "restart.log")+`","resume_dir":"`+filepath.Join(d, "restart")+`"},`+
		`"roles":[{"names":["x-*"],"text":"Роль ({why}).","on_start":"always","settings":[{"dirs":["`+d+`"],"file":"`+settings+`"}]}]}`)
	// Нет перезапуска — sleep выходит сам через 25 с, и тест падает на отсутствии второго запуска, а не висит.
	run := fakeTool(t, filepath.Join(d, "bin"), "systemd-run", "D='"+d+"'\n"+
		`if [ -e "$D/run1" ]; then printf '%s\n' "$@" > "$D/run2"; exit 3; fi
printf '%s\n' "$@" > "$D/run1"
exec sleep 25`)
	t.Cleanup(func() { signal.Reset(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP) })
	socket := fakeAguard(t, `{"v":1,"restart":true,"key":"true,false","session_id":"s1","name":"x-master",`+
		`"running":"2.1.309","installed":"2.1.310","crons":true}`) // таймеры есть — реплики всё равно нет: --resume их вернёт
	l := launcher{systemdRun: run, claude: w.claude, home: d, configFile: filepath.Join(d, "config.json"), name: "x-master",
		dir: filepath.Join(d, "proj"), aguard: socket, check: testCheck}

	code := l.supervise(append(sessionFlags(d), "-n", "x-master"), []string{"--model", "opus", "--effort=high"})
	if code != 3 {
		t.Errorf("код выхода cl = %d, want 3 (код второго запуска)", code)
	}
	unit := "--unit=agent-x-master-" + strconv.Itoa(os.Getpid())
	scope := []string{"--user", "--scope", "--quiet", "--collect", "--slice=agents.slice", "--property=IOAccounting=yes"}
	// Свои аргументы — строки роли при старте нет (bare).
	first := append(append(slices.Clone(scope), unit, "--", w.claude), "--settings", settings,
		"--dangerously-skip-permissions", "--allowedTools=Grep,Glob", "-n", "x-master", "--model", "opus", "--effort=high")
	if got := readLines(t, filepath.Join(d, "run1")); !slices.Equal(got, first) {
		t.Errorf("первый запуск:\n%q\nwant\n%q", got, first)
	}
	second := append(append(slices.Clone(scope), unit+"-r1", "--", w.claude), "--settings", settings,
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
// без строки роли (таймеров не было); в журнале — «сжимаю» и «не сжалось … поднимаю без сжатия». Флаги роли (файл
// настроек и промпт) — и у сжатия, и у подъёма.
func TestSuperviseCompact(t *testing.T) {
	w := newWorld(t)
	d := w.dir
	settings := filepath.Join(d, "role.json")
	write(t, settings, `{"permissions":{"deny":["Artifact"]}}`)
	write(t, filepath.Join(d, "config.json"), `{"restart":{"mode":"act","check_seconds":10,`+
		`"journal":"`+filepath.Join(d, "restart.log")+`","resume_dir":"`+filepath.Join(d, "restart")+`",`+
		`"compact_mode":"act","compact_names":["x-*"]},`+
		`"roles":[{"names":["x-*"],"text":"Роль ({why}).","on_start":"always","prompt":"Ты {name}.",`+
		`"settings":[{"dirs":["`+d+`"],"file":"`+settings+`"}]}]}`)
	run := fakeTool(t, filepath.Join(d, "bin"), "systemd-run", "D='"+d+"'\n"+
		`if [ -e "$D/run2" ]; then printf '%s\n' "$@" > "$D/run3"; exit 3; fi
if [ -e "$D/run1" ]; then printf '%s\n' "$@" > "$D/run2"; echo 'API Error: overloaded'; exit 1; fi
printf '%s\n' "$@" > "$D/run1"
exec sleep 25`)
	t.Cleanup(func() { signal.Reset(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP) })
	socket := fakeAguard(t, `{"v":1,"restart":false,"fresh":true,"key":"false,true","session_id":"s1","name":"x-master",`+
		`"running":"2.1.310","installed":"2.1.310","compact":true,"compact_key":"true","tokens":150000}`)
	l := launcher{systemdRun: run, claude: w.claude, home: d, configFile: filepath.Join(d, "config.json"),
		aguard: socket, name: "x-master", dir: filepath.Join(d, "proj"), check: testCheck}

	if code := l.supervise(append(sessionFlags(d), "-n", "x-master"), []string{"--model", "opus"}); code != 3 {
		t.Errorf("код выхода cl = %d, want 3 (код подъёма)", code)
	}
	pid := strconv.Itoa(os.Getpid())
	scope := []string{"--user", "--scope", "--quiet", "--collect", "--slice=agents.slice", "--property=IOAccounting=yes"}
	own := append([]string{"--settings", settings, "--append-system-prompt", "Ты x-master."}, sessionFlags(d)...)
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
		aguard: socket, name: "x-master", check: testCheck, selfExe: exe, exec: func(path string, _, env []string) error {
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

func TestNextTypesRecreate(t *testing.T) {
	cfg := defaultConfig(t.TempDir())
	cfg.Restart.Mode = modeAct
	l := launcher{name: "x-master", console: &console{}}
	text := `cl: перед перезапуском ради версии Claude Code поставь заново таймеры прошлой беседы — --resume их не вернёт: CronCreate cron "47 1 * * *" prompt "Сон" recurring true; CronCreate …. Потом удали старые CronDelete j1 (не выйдет — оставь). Больше ничего не делай.`
	a := restart.Answer{Verdict: restart.Verdict{Against: []restart.Reason{{Code: "crons", Text: "таймеры прошлой беседы --resume не вернёт"}}, Recreate: text},
		Key: "false,false,crons", SessionID: "s1", Name: "x-master"}
	now := time.Now()
	if out, _ := l.next(cfg, now, 7, a, nil, journalKeys{}); out.typed != text || !out.recreate || out.restart {
		t.Fatalf("команда восстановления не набрана: %+v", out)
	}
	if out, _ := l.next(cfg, now.Add(time.Minute), 7, a, nil, journalKeys{}); out.typed != "" || out.restart {
		t.Fatalf("повтор в 30 минут не подавлен: %+v", out)
	}
	if journal := strings.Join(readLines(t, cfg.Restart.Journal), "\n"); strings.Contains(journal, "набор таймеров не помог") {
		t.Errorf("через минуту сессия ещё может ставить таймеры, а журнал уже пишет «не помог»: %s", journal)
	}
	if out, _ := l.next(cfg, now.Add(recreateCheck), 7, a, nil, journalKeys{}); out.typed != "" || out.restart {
		t.Fatalf("повтор в 30 минут не подавлен: %+v", out)
	}
	if journal := strings.Join(readLines(t, cfg.Restart.Journal), "\n"); !strings.Contains(journal, "набор таймеров не помог") {
		t.Errorf("в журнале нет результата повтора: %s", journal)
	}
	if out, _ := l.next(cfg, now.Add(recreateRetry), 7, a, nil, journalKeys{}); out.typed != text {
		t.Fatalf("через 30 минут набор не повторён: %+v", out)
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

// Набор таймеров не удался — повтор не ждёт recreateRetry, следующий вопрос набирает снова; удался — в журнале
// строка набора таймеров, не обычного набора (dce2d00).
func TestTypeOutcomeRecreate(t *testing.T) {
	cfg := defaultConfig(t.TempDir())
	cfg.Restart.Mode = modeAct
	c := &console{}
	l := launcher{name: "x-master", console: c}
	a := restart.Answer{Verdict: restart.Verdict{Against: []restart.Reason{{Code: "crons"}}, Recreate: "cl: поставь таймеры"},
		Key: "false,crons", SessionID: "s1", Name: "x-master"}
	now := time.Now()
	out, _ := l.next(cfg, now, 7, a, nil, journalKeys{})
	if !out.recreate {
		t.Fatalf("набор таймеров: %+v", out)
	}
	l.typeOutcome(cfg, now, 7, out) // pty claude нет — набор не удался
	if out, _ = l.next(cfg, now.Add(time.Minute), 7, a, nil, journalKeys{}); out.typed != a.Recreate {
		t.Fatalf("набор не удался — следующий вопрос набирает снова: %+v", out)
	}
	pty := filepath.Join(t.TempDir(), "pty")
	master, err := os.Create(pty)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = master.Close() })
	c.master = master
	l.typeOutcome(cfg, now.Add(time.Minute), 7, out)
	if data, err := os.ReadFile(pty); err != nil || string(data) != a.Recreate+"\r" {
		t.Errorf("в pty: %q %v", data, err)
	}
	if journal := strings.Join(readLines(t, cfg.Restart.Journal), "\n"); !strings.Contains(journal, "набрал заново таймеры перед перезапуском ради версии") {
		t.Errorf("журнал: %s", journal)
	}
}

// Флаги роли при подъёме: файл настроек роли — `--settings <файл>` впереди аргументов, вместе с промптом; роль без
// settings — аргументы прежние; нет файла, в нём не объект JSON или свой --settings в аргументах — без файла роли,
// строка в журнал cl.
func TestWithRoleFlags(t *testing.T) {
	d := t.TempDir()
	good, notObject := filepath.Join(d, "role.json"), filepath.Join(d, "list.json")
	write(t, good, `{"permissions":{"deny":["Artifact"]}}`)
	write(t, notObject, `["Artifact"]`)
	journalFile := filepath.Join(d, "restart.log")
	config := func(file string) {
		write(t, filepath.Join(d, "config.json"), `{"restart":{"journal":"`+journalFile+`"},"roles":[`+
			`{"names":["x-master"],"prompt":"Ты {name}.","settings":[{"dirs":["`+d+`"],"file":"`+file+`"}]},`+
			`{"names":["y-master"],"prompt":"Ты {name}."}]}`)
	}
	l := launcher{home: d, configFile: filepath.Join(d, "config.json"), dir: filepath.Join(d, "proj")}
	args := []string{"-n", "x-master", "--resume", "s1"}

	config(good)
	want := append([]string{"--settings", good, "--append-system-prompt", "Ты x-master."}, args...)
	if got := l.withRoleFlags(slices.Clone(args), "x-master"); !slices.Equal(got, want) {
		t.Errorf("роль с settings:\n%q\nwant\n%q", got, want)
	}
	if got := l.withRoleFlags(slices.Clone(args), "y-master"); !slices.Equal(got, append([]string{"--append-system-prompt", "Ты y-master."}, args...)) {
		t.Errorf("роль без settings — только промпт: %q", got)
	}
	other := launcher{home: d, configFile: l.configFile, dir: "/elsewhere"}
	if got := other.withRoleFlags(slices.Clone(args), "x-master"); slices.Contains(got, "--settings") {
		t.Errorf("папка не подходит — без файла: %q", got)
	}
	if _, err := os.Stat(journalFile); err == nil {
		t.Errorf("без сбоев журнал пуст: %q", readLines(t, journalFile))
	}

	own := append(slices.Clone(args), "--settings=/own.json")
	if got := l.withRoleFlags(slices.Clone(own), "x-master"); !slices.Equal(got, append([]string{"--append-system-prompt", "Ты x-master."}, own...)) {
		t.Errorf("свой --settings — файл роли не добавляется: %q", got)
	}
	for _, file := range []string{filepath.Join(d, "missing.json"), notObject} {
		config(file)
		if got := l.withRoleFlags(slices.Clone(args), "x-master"); !slices.Equal(got, append([]string{"--append-system-prompt", "Ты x-master."}, args...)) {
			t.Errorf("%s — подъём без файла роли: %q", file, got)
		}
	}
	text := strings.Join(readLines(t, journalFile), "\n")
	for _, s := range []string{"свой --settings в аргументах — файл роли " + good + " не добавляю",
		"missing.json: no such file or directory — поднимаю без файла настроек роли", "list.json: не объект JSON"} {
		if !strings.Contains(text, s) {
			t.Errorf("в журнале нет %q:\n%s", s, text)
		}
	}
}

// Расхождение файла настроек живого claude с положенным по роли: --settings из /proc/<pid>/cmdline (обе формы, пути
// после Clean) против файла роли по папке. Судить не о чем — роль без settings, свой --settings, положенный файл не
// читается; cmdline не прочиталась — ошибка.
func TestStaleSettings(t *testing.T) {
	d := t.TempDir()
	good, broken := filepath.Join(d, "role.json"), filepath.Join(d, "broken.json")
	write(t, good, `{"permissions":{"deny":["Artifact"]}}`)
	write(t, broken, `{"permissions":`)
	proc := filepath.Join(d, "proc")
	cmdline := func(args ...string) {
		write(t, filepath.Join(proc, "7", "cmdline"), strings.Join(append([]string{"claude"}, args...), "\x00")+"\x00")
	}
	cfg := config{Roles: []role{{Names: []string{"x-master"}, Settings: []roleSettings{{Dirs: []string{d}, File: good}}},
		{Names: []string{"y-master"}}}}
	l := launcher{name: "x-master", dir: filepath.Join(d, "proj")}
	l.registry.ProcRoot = proc
	for _, c := range []struct {
		running []string
		want    string
	}{
		{nil, "файл настроек роли: нет → " + good},
		{[]string{"--settings", "/other.json", "-n", "x"}, "файл настроек роли: /other.json → " + good},
		{[]string{"--settings", good}, ""},
		{[]string{"--settings=" + filepath.Join(d, ".", "role.json")}, ""},
		{[]string{"--settings", "/other.json", "--settings", good}, ""}, // Claude Code берёт последний
	} {
		cmdline(c.running...)
		if got, err := l.staleSettings(cfg, 7, nil); err != nil || got != c.want {
			t.Errorf("claude %q: %q, %v; want %q", c.running, got, err, c.want)
		}
	}
	cmdline()
	if got, err := l.staleSettings(cfg, 7, []string{"--settings=/own.json"}); err != nil || got != "" {
		t.Errorf("свой --settings — не судить: %q, %v", got, err)
	}
	if got, err := (launcher{name: "y-master", registry: l.registry}).staleSettings(cfg, 7, nil); err != nil || got != "" {
		t.Errorf("роль без settings — не судить: %q, %v", got, err)
	}
	if got, err := (launcher{name: "x-master", dir: "/elsewhere", registry: l.registry}).staleSettings(cfg, 7, nil); err != nil || got != "" {
		t.Errorf("папке файл не положен, у claude его нет — совпадает: %q, %v", got, err)
	}
	cmdline("--settings", good)
	if got, err := (launcher{name: "x-master", dir: "/elsewhere", registry: l.registry}).staleSettings(cfg, 7, nil); err != nil ||
		got != "файл настроек роли: "+good+" → нет" {
		t.Errorf("файл есть, а папке не положен: %q, %v", got, err)
	}
	cfg.Roles[0].Settings[0].File = broken
	cmdline()
	if got, err := l.staleSettings(cfg, 7, nil); err != nil || got != "" {
		t.Errorf("положенный файл битый (могут дописывать) — не судить: %q, %v", got, err)
	}
	cfg.Roles[0].Settings[0].File = good
	if _, err := l.staleSettings(cfg, 8, nil); err == nil {
		t.Error("cmdline не прочиталась — ошибка")
	}
}

// Перезапуск ради файла настроек роли целиком: у живого claude --settings нет, роль его кладёт — cl спрашивает aguard
// с stale, по «да» поднимает --resume уже с --settings. Новый процесс расходится так же (подменный systemd-run claude
// не запускает) — cl больше не просит, строка в журнал.
func TestSuperviseStaleSettings(t *testing.T) {
	w := newWorld(t)
	d := w.dir
	settings := filepath.Join(d, "role.json")
	write(t, settings, `{"permissions":{"deny":["Artifact"]}}`)
	journalFile := filepath.Join(d, "restart.log")
	write(t, filepath.Join(d, "config.json"), `{"restart":{"mode":"act","check_seconds":10,`+
		`"journal":"`+journalFile+`","resume_dir":"`+filepath.Join(d, "restart")+`"},`+
		`"roles":[{"names":["x-*"],"settings":[{"dirs":["`+d+`"],"file":"`+settings+`"}]}]}`)
	run := fakeTool(t, filepath.Join(d, "bin"), "systemd-run", "D='"+d+"'\n"+
		`if [ -e "$D/run1" ]; then printf '%s\n' "$@" > "$D/run2"; exec sleep 4; fi
printf '%s\n' "$@" > "$D/run1"
exec sleep 25`)
	t.Cleanup(func() { signal.Reset(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP) })
	var mu sync.Mutex
	stalePids := map[int]string{}
	asked := map[int]bool{}
	socket := fakeAguardFunc(t, func(req restart.Request) string {
		mu.Lock()
		defer mu.Unlock()
		asked[req.Pid] = true
		if req.Stale == "" {
			return `{"v":1,"fresh":true,"key":"false,true","session_id":"s1","name":"x-master","running":"2.1.319","installed":"2.1.319"}`
		}
		stalePids[req.Pid] = req.Stale
		return `{"v":1,"restart":true,"args_stale":true,"key":"true,false,stale","session_id":"s1","name":"x-master",` +
			`"running":"2.1.319","installed":"2.1.319","stale":` + strconv.Quote(req.Stale) + `}`
	})
	l := launcher{systemdRun: run, claude: w.claude, home: d, configFile: filepath.Join(d, "config.json"), name: "x-master",
		dir: filepath.Join(d, "proj"), aguard: socket, check: testCheck}
	l.registry.ProcRoot = "/proc"

	if code := l.supervise(append(sessionFlags(d), "-n", "x-master"), nil); code != 0 {
		t.Errorf("код выхода cl = %d, want 0 (второй запуск вышел сам)", code)
	}
	journalText := strings.Join(readLines(t, journalFile), "\n")
	if _, err := os.Stat(filepath.Join(d, "run2")); err != nil {
		t.Fatalf("подъёма нет: %v; журнал:\n%s", err, journalText)
	}
	if got := readLines(t, filepath.Join(d, "run2")); !slices.Contains(got, settings) || !slices.Contains(got, "--resume") {
		t.Fatalf("подъём без --settings или без --resume: %q; журнал:\n%s", got, journalText)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(stalePids) != 1 || len(asked) != 2 {
		t.Errorf("stale — только у первого процесса из двух: stale %v, спрошены %v", stalePids, asked)
	}
	for _, s := range stalePids {
		if s != "файл настроек роли: нет → "+settings {
			t.Errorf("stale %q", s)
		}
	}
	for _, want := range []string{"2.1.319→2.1.319, файл настроек роли: нет → " + settings + " act: перезапускаю",
		"файл настроек роли: нет → " + settings + " и после перезапуска ради него — больше не прошу"} {
		if !strings.Contains(journalText, want) {
			t.Errorf("в журнале нет %q:\n%s", want, journalText)
		}
	}
}
