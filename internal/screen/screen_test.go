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

// Подсказка сброса — в строках статуса под полем, пустые строки внизу экрана не мешают; в тексте беседы — не она.
func TestReadClearHint(t *testing.T) {
	screen := append(frame("❯ "), "  ⏵⏵ bypass permissions on", "      new task? /clear to save 222.3k tokens", "", "")
	if v := Read(screen); v.Prompt != Empty || !v.ClearHint {
		t.Errorf("подсказка: %+v", v)
	}
	talk := append([]string{"признак: подсказка «/clear to save» на экране"}, frame("❯ ")...)
	if v := Read(talk); v.ClearHint {
		t.Errorf("подсказка в тексте беседы: %+v", v)
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
	// claude под pty cl: его ввод — внутренний pts, вкладка — терминал cl и оболочки
	fakeProc(t, root, 210, 100, "bash", "/dev/pts/4")
	fakeProc(t, root, 310, 210, "cl", "/dev/pts/4")
	fakeProc(t, root, 410, 310, "claude", "/dev/pts/9", "HOME=/h")
	fakeProc(t, root, 510, 1, "cl", "/dev/pts/5", "TMUX=/run/tmux/s,42,0")
	fakeProc(t, root, 520, 510, "claude", "/dev/pts/10", "TMUX=/run/tmux/s,42,0")

	term := &fakeTerm{lines: frame("❯ ")}
	r := &Reader{Proc: root, Term: term}
	if v, err := r.View(400); err != nil || v.Prompt != Empty || term.asked != "konsole 100 /dev/pts/3" {
		t.Errorf("Konsole: %s %v, спросили %q", v.Prompt, err, term.asked)
	}
	if _, err := r.Lines(500); err != nil || term.asked != "tmux /run/tmux/s /dev/pts/7" {
		t.Errorf("tmux: %v, спросили %q", err, term.asked)
	}
	if _, err := r.Lines(410); err != nil || term.asked != "konsole 100 /dev/pts/4" {
		t.Errorf("Konsole, pty cl: %v, спросили %q", err, term.asked)
	}
	if _, err := r.Lines(520); err != nil || term.asked != "tmux /run/tmux/s /dev/pts/5" {
		t.Errorf("tmux, pty cl: %v, спросили %q", err, term.asked)
	}
	if v, err := r.View(600); !errors.Is(err, ErrNoTerminal) || v.Prompt != Unknown {
		t.Errorf("без терминала: %s %v", v.Prompt, err)
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
