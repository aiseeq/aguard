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
)

// TestMain: с CL_TEST_RUN_MAIN=1 тестовый бинарь — это cl (main с аргументами процесса), так main проверяется
// подпроцессом с подменённым HOME и PATH.
func TestMain(m *testing.M) {
	if os.Getenv("CL_TEST_RUN_MAIN") == "1" {
		main()
		return
	}
	os.Exit(m.Run())
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
	socket := fakeAguard(t, `{"v":1,"restart":true,"key":"true,false","session_id":"s1","name":"x-master",`+
		`"running":"2.1.309","installed":"2.1.310","compact_against":[{"code":"early"}],"reset_against":[{"code":"hint"}]}`)
	l := launcher{claude: w.claude, home: w.dir, configFile: filepath.Join(w.dir, "config.json"), registry: w.registry, aguard: socket}

	out := captureStdout(t, func() {
		if code := runCheck(l); code != 0 {
			t.Errorf("runCheck = %d", code)
		}
	})
	want := "check x-master pid=" + strconv.Itoa(os.Getpid()) + " 2.1.309→2.1.310 log: перезапустил бы: простаивает, ничего не идёт" +
		"; сжатие: нет (early); сброс: нет (hint)"
	if strings.TrimSpace(out) != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
	if lines := readLines(t, journalFile); len(lines) != 1 || !strings.HasSuffix(lines[0], " "+want) {
		t.Errorf("журнал = %q", lines)
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
