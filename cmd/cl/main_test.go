package main

import (
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
	write(t, filepath.Join(home, ".config", "systemd", "user", agentsSlice), "[Slice]\n")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], args...)
	cmd.Dir = cwd
	cmd.Env = []string{"CL_TEST_RUN_MAIN=1", "HOME=" + home, "PATH=" + bin, "XDG_RUNTIME_DIR=" + home}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cl %q: %v\n%s", args, err, out)
	}
	got := readLines(t, argv)
	i := slices.Index(got, "--")
	if i < 0 || i+1 >= len(got) || got[i+1] != filepath.Join(bin, "claude") || !slices.Contains(got[:i], "--slice="+agentsSlice) {
		t.Fatalf("systemd-run %q", got)
	}
	return got[i+2:]
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
		`"running":"2.1.309","installed":"2.1.310","compact_against":[{"code":"early"}]}`)
	l := launcher{claude: w.claude, home: w.dir, configFile: filepath.Join(w.dir, "config.json"), registry: w.registry, aguard: socket}

	out := captureStdout(t, func() {
		if code := runCheck(l); code != 0 {
			t.Errorf("runCheck = %d", code)
		}
	})
	want := "check x-master pid=" + strconv.Itoa(os.Getpid()) + " 2.1.309→2.1.310 log: перезапустил бы: простаивает, ничего не идёт" +
		"; сжатие: нет (early)"
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
