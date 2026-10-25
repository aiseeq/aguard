package screen

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	rule = "────────────────────────────────────────────────────────────"
	top  = "─────────────────────────────────────────────── compact-probe ─"
)

// frame — экран сессии с полем ввода input (строки внутри рамки) и строками статуса под ним.
func frame(input ...string) []string {
	lines := []string{"● готово", "", "✻ Worked for 45s · done 17:06", top}
	lines = append(lines, input...)
	return append(lines, rule, "   SCRATCHPAD/COMPACT-PROBE  · Opus 5.5·high · ctx ▓▓░░░░░░░░ 21%",
		"  ⏵⏵ auto mode on (shift+tab to cycle)", "", "")
}

func TestParse(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  Prompt
	}{
		{"пустое: неразрывный пробел после ❯", frame("❯ "), Empty},
		{"пустое: без имени сессии", []string{rule, "❯ ", rule, "status"}, Empty},
		{"черновик", frame("❯ черновик во время хода"), Draft},
		{"черновик во второй строке", frame("❯ ", "  вторая строка"), Draft},
		{"образец новой сессии", frame(`❯ Try "edit <filepath> to..."`), Draft},
		{"диалог без рамки", []string{"Do you trust this folder?", "❯ 1. Yes", "  2. No, exit"}, Unknown},
		{"под рамкой длинный список", append(frame("❯ /co"), strings.Split(strings.Repeat("/cmd\n", 12), "\n")...), Unknown},
		{"нет верхней линии", []string{"текст", "❯ ", rule}, Unknown},
		{"между линиями не поле", []string{rule, "просто текст", rule}, Unknown},
		{"пустой экран", nil, Unknown},
		{"короткие линии — не рамка", []string{"──", "❯ ", "──"}, Unknown},
	}
	for _, c := range cases {
		if got := Parse(c.lines); got != c.want {
			t.Errorf("%s: %s, ждали %s", c.name, got, c.want)
		}
	}
}

// fakeTerm — терминал из теста: запоминает, что спросили.
type fakeTerm struct {
	asked string
	lines []string
}

func (f *fakeTerm) Konsole(pid int, tty string) ([]string, error) {
	f.asked = "konsole " + strconv.Itoa(pid) + " " + tty
	return f.lines, nil
}

func (f *fakeTerm) Tmux(socket, tty string) ([]string, error) {
	f.asked = "tmux " + socket + " " + tty
	return f.lines, nil
}

// fakeProc — процесс в папке /proc теста: stat, терминал ввода и окружение.
func fakeProc(t *testing.T, root string, pid, ppid int, comm, tty string, env ...string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(filepath.Join(dir, "fd"), 0o755); err != nil {
		t.Fatal(err)
	}
	stat := strconv.Itoa(pid) + " (" + comm + ") S " + strconv.Itoa(ppid) + strings.Repeat(" 0", 20)
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "environ"), []byte(strings.Join(env, "\x00")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(tty, filepath.Join(dir, "fd", "0")); err != nil {
		t.Fatal(err)
	}
}

func TestLines(t *testing.T) {
	root := t.TempDir()
	fakeProc(t, root, 100, 1, "konsole", "/dev/null")
	fakeProc(t, root, 200, 100, "bash", "/dev/pts/3")
	fakeProc(t, root, 300, 200, "cl", "/dev/pts/3")
	fakeProc(t, root, 400, 300, "claude", "/dev/pts/3", "HOME=/h")
	fakeProc(t, root, 500, 1, "claude", "/dev/pts/7", "TMUX=/run/tmux/s,42,0", "HOME=/h")
	fakeProc(t, root, 600, 1, "claude", "/dev/pts/8")
	fakeProc(t, root, 700, 1, "claude", "/dev/null")

	term := &fakeTerm{lines: frame("❯ ")}
	r := &Reader{Proc: root, Term: term}
	if p, err := r.Prompt(400); err != nil || p != Empty || term.asked != "konsole 100 /dev/pts/3" {
		t.Errorf("Konsole: %s %v, спросили %q", p, err, term.asked)
	}
	if _, err := r.Lines(500); err != nil || term.asked != "tmux /run/tmux/s /dev/pts/7" {
		t.Errorf("tmux: %v, спросили %q", err, term.asked)
	}
	if p, err := r.Prompt(600); !errors.Is(err, ErrNoTerminal) || p != Unknown {
		t.Errorf("без терминала: %s %v", p, err)
	}
	if _, err := r.Lines(700); err == nil {
		t.Error("ввод не терминал — ошибка")
	}
	if _, err := r.Lines(800); err == nil {
		t.Error("нет процесса — ошибка")
	}
}

func TestTmuxPane(t *testing.T) {
	list := []byte("/dev/pts/1 %0\n/dev/pts/16 %3\n")
	if id, ok := tmuxPane(list, "/dev/pts/16"); !ok || id != "%3" {
		t.Errorf("tmuxPane = %q %v", id, ok)
	}
	if _, ok := tmuxPane(list, "/dev/pts/2"); ok {
		t.Error("чужой терминал нашёлся")
	}
}
