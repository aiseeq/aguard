package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/aiseeq/aguard/internal/restart"
	"github.com/aiseeq/aguard/internal/testutil"
	"github.com/aiseeq/akit/proc"
)

// TestMain: с CL_TEST_RUN_MAIN=1 тестовый бинарь — это cl (main с аргументами процесса), так main проверяется
// подпроцессом с подменённым HOME и PATH.
func TestMain(m *testing.M) {
	if os.Getenv("CL_TEST_RUN_MAIN") == "1" {
		main()
		return
	}
	os.Exit(testutil.RunWithCommandGuard(m, "cmd/cl", "systemd-run"))
}

// runMain — cl с аргументами args в папке cwd под домом home; claude и systemd-run из PATH подменены, systemd-run
// пишет свои аргументы построчно в <home>/argv и выходит. Возвращает аргументы claude (после «--»).
func runMain(t *testing.T, home, cwd string, args ...string) []string {
	t.Helper()
	return runMainEnv(t, home, cwd, nil, args...)
}

// runMainEnv — runMain с добавкой к окружению (CL_UPGRADE и т. п.).
func runMainEnv(t *testing.T, home, cwd string, env []string, args ...string) []string {
	got, _, _ := runMainEnvSlice(t, home, cwd, env, true, args...)
	return got
}

func runMainEnvSlice(t *testing.T, home, cwd string, env []string, installSlice bool, args ...string) ([]string, string, string) {
	t.Helper()
	bin := filepath.Join(home, "bin")
	write(t, filepath.Join(home, "versions", "2.1.310"), "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(home, "versions", "2.1.310"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(bin, "claude")); err != nil {
		symlink(t, filepath.Join(home, "versions", "2.1.310"), filepath.Join(bin, "claude"))
	}
	argv := filepath.Join(home, "argv")
	fakeTool(t, bin, "systemd-run", `printf '%s\n' "$@" > '`+argv+`'`)
	if installSlice {
		write(t, filepath.Join(home, ".config", "systemd", "user", agentsSlice), "[Slice]\n")
	}
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], args...)
	cmd.Dir = cwd
	cmd.Env = append([]string{"CL_TEST_RUN_MAIN=1", "HOME=" + home, "PATH=" + bin, "XDG_RUNTIME_DIR=" + home}, env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		t.Fatalf("cl %q: %v\nstdout:\n%s\nstderr:\n%s", args, err, stdout.String(), stderr.String())
	}
	if slices.Equal(args, []string{checkFlag}) {
		return nil, stdout.String(), stderr.String()
	}
	got := readLines(t, argv)
	i := slices.Index(got, "--")
	if i < 0 || i+1 >= len(got) || got[i+1] != filepath.Join(bin, "claude") || !slices.Contains(got[:i], "--slice="+agentsSlice) {
		t.Fatalf("systemd-run %q", got)
	}
	return got[i+2:], stdout.String(), stderr.String()
}

func TestMainSliceWarning(t *testing.T) {
	for _, installed := range []bool{false, true} {
		name := "missing"
		if installed {
			name = "installed"
		}
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			got, stdout, stderr := runMainEnvSlice(t, home, filepath.Join(home, "project"), nil, installed, "-p", "hello")
			if !slices.Contains(got, "-p") || !slices.Contains(got, "hello") {
				t.Errorf("systemd-run не получил cl -p: %q", got)
			}
			weight := sliceCPUWeight(t)
			warning := "вес CPU/IO будет 100, наравне с приложениями пользователя, вместо " + weight
			install := "cp ~/git/aguard/cmd/cl/agents.slice ~/.config/systemd/user/ && systemctl --user daemon-reload"
			if installed {
				if strings.Contains(stderr, warning) || strings.Contains(stderr, install) {
					t.Errorf("при установленном файле неожиданное предупреждение: %q", stderr)
				}
			} else if strings.Count(stderr, warning) != 1 || !strings.Contains(stderr, install) {
				t.Errorf("в stderr должно быть ровно одно предупреждение с командой установки: %q", stderr)
			}
			if strings.Contains(stdout, warning) || strings.Contains(stdout, install) {
				t.Errorf("предупреждение попало в stdout: %q", stdout)
			}
		})
	}
	home := t.TempDir()
	_, stdout, stderr := runMainEnvSlice(t, home, filepath.Join(home, "project"), nil, false, checkFlag)
	if strings.Count(stderr, "вес CPU/IO будет 100") != 1 || !strings.Contains(stderr, "cp ~/git/aguard/cmd/cl/agents.slice ~/.config/systemd/user/") {
		t.Errorf("--cl-check без файла слайса не предупредил ровно один раз: %q", stderr)
	}
	if strings.Contains(stdout, "вес CPU/IO") {
		t.Errorf("--cl-check напечатал предупреждение в stdout: %q", stdout)
	}

	upgradeEnv := []string{upgradeEnv + `={"v":1,"name":"upgraded","n":2,"args":["-p","hello"]}`}
	home = t.TempDir()
	_, stdout, stderr = runMainEnvSlice(t, home, filepath.Join(home, "project"), upgradeEnv, false, "-p", "hello")
	if strings.Contains(stderr, "вес CPU/IO") || strings.Contains(stdout, "вес CPU/IO") {
		t.Errorf("после self-upgrade предупреждение нарушает живой экран: stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestMainOwnHelp(t *testing.T) {
	cmd := exec.Command(os.Args[0], "--cl-help")
	cmd.Env = []string{"CL_TEST_RUN_MAIN=1", "HOME=" + t.TempDir(), "PATH=" + t.TempDir()}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("cl --cl-help: %v; stderr %q", err, stderr.String())
	}
	want := `cl — запуск Claude Code с настройками сессии.

cl передаёт строку роли (roles[].text, по настройке roles[].on_start — только при запуске без своих аргументов,
кроме -n/--name) и системный промпт из ~/.config/cl/config.json (или файла $CL_CONFIG).
Файл возобновления: restart.resume_dir/<дата>/<имя сессии>.md; cl берёт самый свежий файл не старше
restart.resume_max_age_hours. Если роль получает строку и задан resume_text, cl включает в неё путь к файлу.
Выданный файл cl переносит в used/; если перенос не удался, оставляет прежний путь.

Таймеры прошлой беседы, унаследованные через /clear, при подъёме не переживают; роль ставит их заново сама.
Их список cl пишет в журнал, указанный настройкой restart.journal.
Другим сессиям после старта не отправляй «ты перезапущен» и не пересылай файл возобновления;
выполняй порядок из файла возобновления.

Аргументы Claude Code передаются через cl. Справка Claude Code: claude --help.
cl --cl-ls [--under <папка>] [--json] [--timers] — живые сессии; --timers показывает их таймеры CronCreate.
`
	if stdout.String() != want {
		t.Fatalf("cl --cl-help = %q, want %q", stdout.String(), want)
	}
	if stderr.Len() != 0 {
		t.Errorf("справка напечатала stderr: %q", stderr.String())
	}
}

func TestMainGuideAndHelpHintsStillLaunch(t *testing.T) {
	for _, arg := range []string{"guide", "help"} {
		t.Run(arg, func(t *testing.T) {
			home := t.TempDir()
			got, stdout, stderr := runMainEnvSlice(t, home, filepath.Join(home, "project"), nil, true, arg)
			if !slices.Equal(got, []string{"--dangerously-skip-permissions", "--allowedTools=Grep,Glob", arg}) {
				t.Fatalf("cl %s → claude %q", arg, got)
			}
			if stdout != "" || stderr != "cl: это похоже на подкоманду; справка cl: cl --cl-help\n" {
				t.Fatalf("cl %s output stdout=%q stderr=%q", arg, stdout, stderr)
			}
		})
	}
}

func TestMainOwnHelpWithExtraArgumentIsForwarded(t *testing.T) {
	home := t.TempDir()
	got, stdout, stderr := runMainEnvSlice(t, home, filepath.Join(home, "project"), nil, true, "--cl-help", "extra")
	if !slices.Contains(got, "--cl-help") || !slices.Contains(got, "extra") {
		t.Fatalf("cl --cl-help extra не передал аргументы Claude Code: %q", got)
	}
	if stdout != "" || strings.Contains(stderr, "справка cl") {
		t.Fatalf("лишний аргумент вызвал справку или подсказку: stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestMainOOMPrefixTakesPrecedenceOverHelp(t *testing.T) {
	for _, tc := range []struct {
		name  string
		argv0 string
		args  []string
		want  []string
	}{
		{"symlink help", "cl-oom-prefix", []string{"help"}, []string{"help"}},
		{"symlink help flag", "cl-oom-prefix", []string{"--cl-help"}, []string{"--cl-help"}},
		{"subcommand help", "cl", []string{"oom-prefix", "help"}, []string{"help"}},
		{"subcommand help flag", "cl", []string{"oom-prefix", "--cl-help"}, []string{"--cl-help"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			var got []string
			if !dispatchEarlyArgs(tc.argv0, tc.args, &stdout, &stderr, func(args []string) { got = args }) {
				t.Fatal("обёртка oom-prefix не перехватила аргументы")
			}
			if !slices.Equal(got, tc.want) || stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("args=%q forwarded=%q stdout=%q stderr=%q", tc.args, got, stdout.String(), stderr.String())
			}
		})
	}
}

func TestMainUnknownConfigKeysWarning(t *testing.T) {
	home := t.TempDir()
	configFile := filepath.Join(home, "config.json")
	write(t, configFile, `{"roles":[{"names":["lead"],"restorable_crons":true}],"restart":{"idle_minutes":5}}`)
	args, _, stderr := runMainEnvSlice(t, home, filepath.Join(home, "project"), []string{"CL_CONFIG=" + configFile}, true, "-p", "hello")
	if !slices.Contains(args, "-p") || !slices.Contains(args, "hello") {
		t.Fatalf("systemd-run did not receive cl -p arguments: %q", args)
	}
	warning := "cl: неизвестные ключи конфига " + configFile + ": restart.idle_minutes, roles[lead].restorable_crons\n"
	if stderr != warning {
		t.Fatalf("stderr = %q, want exactly %q", stderr, warning)
	}
	if got := readLines(t, filepath.Join(home, "argv")); len(got) == 0 || !slices.Contains(got, "--") {
		t.Fatalf("systemd-run was not invoked: %q", got)
	}
}

func TestMainUnknownConfigKeysWarnDespiteLoadConfigError(t *testing.T) {
	home := t.TempDir()
	configFile := filepath.Join(home, "config.json")
	write(t, configFile, `{"roles":[{"names":["lead"],"settings":[{"fiel":"x"}]}]}`)
	args, _, stderr := runMainEnvSlice(t, home, filepath.Join(home, "project"), []string{"CL_CONFIG=" + configFile}, true, "-p", "hello")
	warning := "cl: неизвестные ключи конфига " + configFile + ": roles[lead].settings[0].fiel\n"
	if stderr != warning {
		t.Fatalf("stderr = %q, want %q", stderr, warning)
	}
	if !slices.Contains(args, "-p") || !slices.Contains(args, "hello") {
		t.Fatalf("systemd-run was not called with cl -p: %q", args)
	}
}

func TestMainUnknownConfigKeysCheckAndUpgrade(t *testing.T) {
	home := t.TempDir()
	configFile := filepath.Join(home, "config.json")
	write(t, configFile, `{"restart":{"idle_minutes":5}}`)
	env := []string{"CL_CONFIG=" + configFile}
	_, _, stderr := runMainEnvSlice(t, home, filepath.Join(home, "project"), env, true, checkFlag)
	warning := "cl: неизвестные ключи конфига " + configFile + ": restart.idle_minutes\n"
	if stderr != warning {
		t.Fatalf("--cl-check stderr = %q, want %q", stderr, warning)
	}

	upgradeEnv := append(env, upgradeEnv+`={"v":1,"name":"upgraded","n":2,"args":["-p","hello"]}`)
	_, stdout, stderr := runMainEnvSlice(t, home, filepath.Join(home, "project"), upgradeEnv, true, "-p", "hello")
	if strings.Contains(stderr, "неизвестные ключи конфига") || strings.Contains(stdout, "неизвестные ключи конфига") {
		t.Fatalf("self-upgrade printed unknown config warning: stdout=%q stderr=%q", stdout, stderr)
	}
}

func sliceCPUWeight(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("agents.slice")
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if value, ok := strings.CutPrefix(line, "CPUWeight="); ok {
			return strings.TrimSpace(value)
		}
	}
	t.Fatal("в cmd/cl/agents.slice нет CPUWeight=")
	return ""
}

// Флаги сессии (dafd514: --allowedTools=Grep,Glob) получают сессия и cl -p, подкоманда — нет; имя — по папке, занятое
// живой сессией — с -2.
func TestMainArgs(t *testing.T) {
	home := t.TempDir()
	proj := filepath.Join(home, "git", "proj")
	write(t, filepath.Join(home, ".claude", "sessions", "1.json"), `{"pid":`+strconv.Itoa(os.Getpid())+`,"name":"proj-master"}`)
	cases := []struct {
		args []string
		want []string
	}{
		{nil, []string{"--dangerously-skip-permissions", "--allowedTools=Grep,Glob", "-n", "proj-master-2"}},
		{[]string{"-n", "свой"}, []string{"--dangerously-skip-permissions", "--allowedTools=Grep,Glob", "-n", "свой"}},
		{[]string{"-p", "привет"}, []string{"--allowedTools=Grep,Glob", "-p", "привет"}},
		{[]string{"mcp", "list"}, []string{"mcp", "list"}},
	}
	for _, c := range cases {
		if got := runMain(t, home, proj, c.args...); !slices.Equal(got, c.want) {
			t.Errorf("cl %q → claude %q, want %q", c.args, got, c.want)
		}
	}
}

// cl --cl-check: решение aguard по живой сессии реестра — в stdout и журнал с пометкой check; мёртвая пропускается,
// неинтерактивная (aguard против с доводом kind) — тоже.
func TestRunCheck(t *testing.T) {
	w := newWorld(t)
	w.link(t, os.Getpid())
	write(t, filepath.Join(w.registry.Dir, "999999999.json"), `{"pid":999999999,"name":"dead","procStart":"500"}`)
	journalFile := filepath.Join(w.dir, "restart.log")
	write(t, filepath.Join(w.dir, "config.json"), `{"restart":{"mode":"log","journal":"`+journalFile+`"}}`)
	requests := make(chan restart.Request, 1)
	socket := fakeAguardFunc(t, func(req restart.Request) string {
		requests <- req
		return `{"v":1,"restart":true,"fresh":true,"key":"true,true","session_id":"s1","name":"x-master",` +
			`"running":"2.1.310","installed":"2.1.310","compact_against":[` +
			`{"code":"status","text":"Claude Code занят: waiting"},` +
			`{"code":"early","text":"запрос 2 мин назад, окно с 53 мин"}],"reset_against":[` +
			`{"code":"early","text":"запрос 2 мин назад, сброс с 53 мин"},` +
			`{"code":"status","text":"Claude Code занят: waiting"},` +
			`{"code":"tree","text":"в /work/proj незакоммиченное (1): M file.go"},` +
			`{"code":"crons","text":"таймеры пропали бы: c1 «утренний отчёт»"}]}`
	})
	l := launcher{claude: w.claude, home: w.dir, configFile: filepath.Join(w.dir, "config.json"), registry: w.registry, aguard: socket}

	out := captureStdout(t, func() {
		if code := runCheck(l); code != 0 {
			t.Errorf("runCheck = %d", code)
		}
	})
	if req := <-requests; !req.Full {
		t.Errorf("runCheck sent Full=%v, want true", req.Full)
	}
	wantHeader := "check x-master pid=" + strconv.Itoa(os.Getpid()) + " 2.1.310→2.1.310 log: версия свежая"
	want := wantHeader + "; сжатие: нет; сброс: нет" +
		"\n  Claude Code занят: waiting" +
		"\n  запрос 2 мин назад; сжатие с 53 мин, сброс с 53 мин" +
		"\n  сброс: в /work/proj незакоммиченное (1): M file.go" +
		"\n  сброс: таймеры пропали бы: c1 «утренний отчёт»"
	if strings.TrimSpace(out) != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
	journalWant := wantHeader + "; сжатие и сброс: нет — Claude Code занят: waiting" +
		"; сжатие: нет — запрос 2 мин назад, окно с 53 мин; сброс: нет — запрос 2 мин назад, сброс с 53 мин" +
		"; сброс: нет — в /work/proj незакоммиченное (1): M file.go; таймеры пропали бы: c1 «утренний отчёт»"
	if lines := readLines(t, journalFile); len(lines) != 1 || !strings.HasSuffix(lines[0], " "+journalWant) {
		t.Errorf("журнал = %q", lines)
	}
}

func TestCommonCheckReasonPrintedOnce(t *testing.T) {
	common := restart.Reason{Code: "args", Text: "свой флаг claude --append-system-prompt: потерялся бы"}
	entries := []checkEntry{
		{answer: restart.Answer{Verdict: restart.Verdict{Fresh: true}, CompactAgainst: []restart.Reason{common}}},
		{answer: restart.Answer{Verdict: restart.Verdict{Fresh: true}, ResetAgainst: []restart.Reason{common}}},
	}
	omitted, reasons := commonCheckReasons(entries)
	if len(reasons) != 1 || reasons[0] != common || !omitted[reasonKey(common)] {
		t.Fatalf("общие доводы = %+v, omitted=%v", reasons, omitted)
	}
	for i, entry := range entries {
		line := checkLineFiltered("log", "x", i+1, entry.answer, nil, omitted)
		if strings.Contains(line, common.Text) {
			t.Errorf("сессия %d повторила общий довод: %s", i+1, line)
		}
	}
}

func TestRunCheckCommonReasonAfterTwoSessions(t *testing.T) {
	w := newWorld(t)
	w.link(t, os.Getpid())
	other := exec.Command("sleep", "60")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = proc.StopChild(other.Process.Pid, 0, func() error { return other.Process.Kill() })
		_ = other.Wait()
	})
	pid := other.Process.Pid
	symlink(t, filepath.Join(w.dir, "proc-tmpl"), filepath.Join(w.registry.ProcRoot, strconv.Itoa(pid)))
	write(t, filepath.Join(w.registry.Dir, strconv.Itoa(pid)+".json"),
		`{"pid":`+strconv.Itoa(pid)+`,"sessionId":"s2","name":"y-master","kind":"interactive","procStart":"500"}`)
	common := `{"code":"args","text":"свой флаг claude --append-system-prompt: потерялся бы"}`
	socket := fakeAguard(t, `{"v":1,"fresh":true,"name":"x-master","running":"2.1.310","installed":"2.1.310","against":[`+common+`]}`)
	l := launcher{claude: w.claude, home: w.dir, configFile: filepath.Join(w.dir, "config.json"), registry: w.registry, aguard: socket}
	out := captureStdout(t, func() {
		if code := runCheck(l); code != 0 {
			t.Errorf("runCheck = %d", code)
		}
	})
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "check ") || !strings.HasPrefix(lines[1], "check ") {
		t.Fatalf("вывод для двух сессий: %q", out)
	}
	if want := "у всех: свой флаг claude --append-system-prompt: потерялся бы"; lines[2] != want {
		t.Errorf("общая строка = %q, want %q", lines[2], want)
	}
}

func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	r, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = wr
	f()
	os.Stdout = stdout
	if err := wr.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestInteractiveOnly(t *testing.T) {
	kind := restart.Answer{Verdict: restart.Verdict{Against: []restart.Reason{{Code: "kind"}, {Code: "status"}}}}
	if !notInteractive(kind) || notInteractive(restart.Answer{}) {
		t.Error("неинтерактивная — по доводу kind")
	}
}

// Файл настроек роли выбирается по папке, где запущен cl: мастер в ~/work — свой файл, в ~/git и ~/workshop — другой.
func TestMainRoleSettings(t *testing.T) {
	home := t.TempDir()
	work, git := filepath.Join(home, "s", "work.json"), filepath.Join(home, "s", "git.json")
	write(t, work, `{"permissions":{"deny":["Artifact"]}}`)
	write(t, git, `{"permissions":{"deny":["Artifact"]}}`)
	write(t, filepath.Join(home, ".config", "cl", "config.json"), `{"roles":[{"names":["*-master"],`+
		`"settings":[{"dirs":["~/work"],"file":"~/s/work.json"},{"file":"~/s/git.json"}]}]}`)
	for _, c := range []struct{ dir, name, file string }{
		{filepath.Join(home, "work", "shop"), "shop-master", work},
		{filepath.Join(home, "git", "proj"), "proj-master", git},
		{filepath.Join(home, "workshop", "x"), "x-master", git},
	} {
		want := []string{"--settings", c.file, "--dangerously-skip-permissions", "--allowedTools=Grep,Glob", "-n", c.name}
		if got := runMain(t, home, c.dir, "-n", c.name); !slices.Equal(got, want) {
			t.Errorf("cl в %s → claude %q, want %q", c.dir, got, want)
		}
	}
	// Новый cl после живого обновления (CL_UPGRADE, claude не жив — запуск): файл роли — по той же папке.
	upgraded := []string{upgradeEnv + `={"v":1,"name":"shop-master","n":2,"args":["-n","shop-master","--resume","s1"]}`}
	want := []string{"--settings", work, "-n", "shop-master", "--resume", "s1"}
	if got := runMainEnv(t, home, filepath.Join(home, "work", "shop"), upgraded); !slices.Equal(got, want) {
		t.Errorf("cl по %s → claude %q, want %q", upgradeEnv, got, want)
	}
}
