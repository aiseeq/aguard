package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
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
	scope := []string{"--user", "--scope", "--quiet", "--collect", "--slice=agents.slice", "--property=IOAccounting=yes"}
	want := append(append(scope, "--unit=agent-x-master-"+strconv.Itoa(os.Getpid())+"-r3", "--", w.claude), "-n", "x-master", "--resume", "s1")
	if got := readLines(t, filepath.Join(d, "run")); !slices.Equal(got, want) {
		t.Errorf("\n got %q\nwant %q", got, want)
	}
}

// claudeUnderPty — «claude» под своим pty в консоли c: отвечает на каждую строку ввода.
func claudeUnderPty(t *testing.T, c *console) (*exec.Cmd, func()) {
	t.Helper()
	cmd := exec.Command("sh", "-c", `while read -r x; do echo "got:$x"; done`)
	finish, err := c.start(cmd, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd, finish
}

func fdFlags(t *testing.T, f *os.File) int {
	t.Helper()
	var flags int
	var ferr error
	conn, err := f.SyscallConn()
	if err == nil {
		err = conn.Control(func(fd uintptr) { flags, ferr = unix.FcntlInt(fd, unix.F_GETFD, 0) })
	}
	if err = errors.Join(err, ferr); err != nil {
		t.Fatal(err)
	}
	return flags
}

// Бинарник заменён, человек молчит: exec нового cl с pid живого claude и master его pty без close-on-exec, терминал
// в обычном режиме. exec не вышел — всё как было: ввод снова доходит до claude, терминал сырой, строка в журнал.
func TestLiveUpgrade(t *testing.T) {
	human, tty, shown := outerTerminal(t)
	before, err := term.GetState(int(tty.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	c := openConsole(tty, tty, nil)
	cmd, _ := claudeUnderPty(t, c)
	d := t.TempDir()
	gone := filepath.Join(d, "exe")
	symlink(t, "/home/u/bin/cl (deleted)", gone)
	cfg := defaultConfig(d)
	cfg.Restart.Journal = filepath.Join(d, "restart.log")
	now := time.Now()
	c.typed.Store(now.Add(-2 * humanQuiet).UnixNano())
	var got upgrade
	execs := 0
	l := launcher{name: "x-master", console: c, selfExe: gone, exec: func(path string, _, env []string) error {
		execs++
		for _, kv := range env {
			if s, ok := strings.CutPrefix(kv, upgradeEnv+"="); ok {
				if err := json.Unmarshal([]byte(s), &got); err != nil {
					t.Error(err)
				}
			}
		}
		if fdFlags(t, c.master)&unix.FD_CLOEXEC != 0 {
			t.Error("master закрылся бы на exec")
		}
		if st, err := term.GetState(int(tty.Fd())); err != nil || !reflect.DeepEqual(st, before) {
			t.Errorf("новый cl получил бы терминал сырым: %v", err)
		}
		return errors.New("exec не вышел")
	}}
	if l.liveUpgrade(cfg, now, cmd.Process.Pid, 3, []string{"--resume", "s1"}) {
		t.Error("имени нет в live_upgrade_names — не пробовать")
	}
	cfg.Restart.LiveUpgradeNames = []string{"x-*"}
	if l.liveUpgrade(cfg, now.Add(-humanQuiet-time.Minute), cmd.Process.Pid, 3, nil) || execs != 0 {
		t.Error("человек вводил меньше минуты назад — не пробовать")
	}
	if !l.liveUpgrade(cfg, now, cmd.Process.Pid, 3, []string{"--resume", "s1"}) || execs != 1 {
		t.Fatalf("exec: %d", execs)
	}
	// Clean: claude поднят под этой консолью, человек ничего не набирал — новый cl знает, что поле чисто (c794bfa).
	if got.Pid != cmd.Process.Pid || got.Master == 0 || got.N != 3 || got.Typed != c.typed.Load() || got.Name != "x-master" || !got.Clean {
		t.Errorf("состояние: %+v", got)
	}
	if fdFlags(t, c.master)&unix.FD_CLOEXEC == 0 {
		t.Error("exec не вышел — close-on-exec вернуть")
	}
	if st, err := term.GetState(int(tty.Fd())); err != nil || reflect.DeepEqual(st, before) {
		t.Errorf("exec не вышел — терминал снова сырой: %v", err)
	}
	if _, err := human.WriteString("after\r"); err != nil {
		t.Fatal(err)
	}
	shown.wait(t, "got:after")
	if data, err := os.ReadFile(cfg.Restart.Journal); err != nil || !strings.Contains(string(data), "exec не вышел — дальше прежний cl") {
		t.Errorf("журнал: %q %v", data, err)
	}
}

// Новый cl принимает живой claude от прежнего: ввод человека — в его pty, выход claude — как у своего, на finish
// терминал в прежнем режиме.
func TestAdopt(t *testing.T) {
	human, tty, shown := outerTerminal(t)
	before, err := term.GetState(int(tty.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", `while read -r x; do echo "got:$x"; done; echo bye`)
	master, err := pty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	var fd int
	conn, err := master.SyscallConn()
	if err == nil {
		err = conn.Control(func(f uintptr) { fd, err = unix.Dup(int(f)) })
	}
	if err = errors.Join(err, master.Close()); err != nil { // у нового cl — только переданный дескриптор
		t.Fatal(err)
	}
	l := launcher{console: openConsole(tty, tty, nil)}
	if l.console.clean() {
		t.Error("консоль без claude ничего не знает о поле — может быть текст")
	}
	typed := time.Now().Add(-time.Hour).UnixNano()
	proc, finish, err := l.adopt(upgrade{Pid: cmd.Process.Pid, Master: fd, Typed: typed, Clean: true})
	if err != nil {
		t.Fatal(err)
	}
	if l.console.typed.Load() != typed || !l.console.clean() {
		t.Error("тишина человека и чистое поле — от прежнего cl")
	}
	if _, err := human.WriteString("hello\r"); err != nil {
		t.Fatal(err)
	}
	shown.wait(t, "got:hello")
	if _, err := human.WriteString("\x04"); err != nil {
		t.Fatal(err)
	}
	if err := wait(proc); err != nil {
		t.Fatalf("claude: %v", err)
	}
	finish()
	shown.wait(t, "bye")
	if after, err := term.GetState(int(tty.Fd())); err != nil || !reflect.DeepEqual(after, before) {
		t.Errorf("терминал не вернулся в прежний режим: %v", err)
	}
}

// Сбой подхвата в новом бинаре виден в журнале перезапусков, а не только в stderr консоли.
func TestUpgradeFailJournaled(t *testing.T) {
	d := t.TempDir()
	l := launcher{name: "x-master", home: d, configFile: filepath.Join(d, "нет.json")}
	l.journalUpgradeFail(errors.New("состояние прежнего cl: версия 0 (нужна 1)"), "новая беседа")
	data, err := os.ReadFile(defaultConfig(d).Restart.Journal)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "x-master pid=") || !strings.Contains(string(data), "подхват не удался: состояние прежнего cl: версия 0 (нужна 1) — новая беседа") {
		t.Fatalf("журнал: %q", data)
	}
}
