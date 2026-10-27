package main

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

func TestTakeUpgrade(t *testing.T) {
	t.Setenv(upgradeEnv, `{"v":1,"name":"x-master","n":2,"args":["--resume","s1"]}`)
	u, ok, err := takeUpgrade()
	if err != nil || !ok || u.Name != "x-master" || u.N != 2 || !slices.Equal(u.Args, []string{"--resume", "s1"}) {
		t.Fatalf("%+v %v", u, err)
	}
	if _, ok := os.LookupEnv(upgradeEnv); ok {
		t.Error("переменная осталась бы claude")
	}
	if u, ok, err := takeUpgrade(); u != nil || ok || err != nil {
		t.Errorf("без переменной: %+v %v", u, err)
	}
	t.Setenv(upgradeEnv, `{"v":2,"name":"x-master","args":["a"]}`)
	if u, ok, err := takeUpgrade(); u != nil || !ok || err == nil {
		t.Errorf("чужая версия: %+v %v", u, err)
	}
}

func TestReplaced(t *testing.T) {
	d := t.TempDir()
	gone, live := filepath.Join(d, "gone"), filepath.Join(d, "live")
	symlink(t, "/home/u/bin/cl (deleted)", gone)
	symlink(t, "/home/u/bin/cl", live)
	if path, ok, err := replaced(gone); !ok || err != nil || path != "/home/u/bin/cl" {
		t.Errorf("заменён: %q %v %v", path, ok, err)
	}
	for _, link := range []string{live, ""} {
		if _, ok, err := replaced(link); ok || err != nil {
			t.Errorf("%q — не заменён: %v", link, err)
		}
	}
	if _, ok, err := replaced(filepath.Join(d, "нет")); ok || err == nil {
		t.Error("ссылка не читается — ошибка")
	}
}

// Новый cl после exec продолжает с аргументами и номером запуска прежнего, не спрашивая имени и строки роли.
func TestSuperviseUpgraded(t *testing.T) {
	w := newWorld(t)
	d := w.dir
	run := fakeTool(t, filepath.Join(d, "bin"), "systemd-run", `printf '%s\n' "$@" > '`+filepath.Join(d, "run")+`'`)
	l := launcher{systemdRun: run, claude: w.claude, home: d, configFile: filepath.Join(d, "нет.json"), name: "x-master",
		upgraded: &upgrade{V: upgradeV, Name: "x-master", N: 3, Args: []string{"-n", "x-master", "--resume", "s1"}}}
	if code := l.supervise(nil, nil); code != 0 {
		t.Errorf("код %d", code)
	}
	scope := []string{"--user", "--scope", "--quiet", "--collect", "--slice=agents.slice"}
	want := append(append(scope, "--unit=agent-x-master-"+strconv.Itoa(os.Getpid())+"-r3", "--", w.claude), "-n", "x-master", "--resume", "s1")
	if got := readLines(t, filepath.Join(d, "run")); !slices.Equal(got, want) {
		t.Errorf("\n got %q\nwant %q", got, want)
	}
}
