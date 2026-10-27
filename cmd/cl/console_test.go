package main

import (
	"bytes"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"

	"github.com/aiseeq/aguard/internal/restart"
)

// screen — то, что claude вывел в «терминал cl»: читается с master внешней пары.
type screen struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *screen) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *screen) has(sub string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Contains(s.buf.String(), sub)
}

func (s *screen) wait(t *testing.T, sub string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if s.has(sub) {
			return
		}
	}
	t.Fatalf("на экране нет %q", sub)
}

// outerTerminal — терминал cl в тесте: пара pty, человек пишет в её master, cl читает её slave.
func outerTerminal(t *testing.T) (human, tty *os.File, shown *screen) {
	t.Helper()
	human, tty, err := pty.Open()
	if err != nil {
		t.Fatalf("pty: %v", err)
	}
	t.Cleanup(func() { _ = human.Close(); _ = tty.Close() })
	shown = &screen{}
	go func() { _, _ = shown.ReadFrom(human) }()
	return human, tty, shown
}

func (s *screen) ReadFrom(f *os.File) (int64, error) {
	buf := make([]byte, 4096)
	var n int64
	for {
		k, err := f.Read(buf)
		if k > 0 {
			_, _ = s.Write(buf[:k])
			n += int64(k)
		}
		if err != nil {
			return n, err
		}
	}
}

// Консоль целиком: ввод человека доходит до claude, набор cl — тоже (Enter отдельно), Ctrl+D завершает claude,
// после finish терминал cl — в прежнем режиме, не сыром.
func TestConsole(t *testing.T) {
	human, tty, shown := outerTerminal(t)
	before, err := term.GetState(int(tty.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	var warns []string
	c := openConsole(tty, tty, func(s string) { warns = append(warns, s) })
	if c == nil {
		t.Fatal("терминал не распознан")
	}
	// «claude»: читает строки до конца ввода и отвечает на каждую
	cmd := exec.Command("sh", "-c", `while read -r x; do echo "got:$x"; done; echo bye`)
	finish, err := c.start(cmd, "")
	if err != nil {
		t.Fatal(err)
	}
	if during, err := term.GetState(int(tty.Fd())); err != nil || reflect.DeepEqual(during, before) {
		t.Errorf("пока claude запущен, терминал cl должен быть сырым: %v", err)
	}
	if _, err := human.WriteString("hello\r"); err != nil {
		t.Fatal(err)
	}
	shown.wait(t, "got:hello")
	if c.idle(time.Now()) > time.Second {
		t.Errorf("ввод человека не отмечен: %v", c.idle(time.Now()))
	}
	if err := c.typeLine("/compact"); err != nil {
		t.Fatal(err)
	}
	shown.wait(t, "got:/compact")
	if _, err := human.WriteString("\x04"); err != nil { // Ctrl+D: конец ввода claude
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("claude: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Ctrl+D не завершил claude")
	}
	start := time.Now()
	finish()
	if took := time.Since(start); took > 2*drainTimeout {
		t.Errorf("finish ждал %v: поток ввода не должен его держать", took)
	}
	shown.wait(t, "bye")
	if after, err := term.GetState(int(tty.Fd())); err != nil || !reflect.DeepEqual(after, before) {
		t.Errorf("терминал не вернулся в прежний режим: %v", err)
	}
	if err := c.typeLine("x"); err == nil {
		t.Error("набор без claude — ошибка")
	}
	if len(warns) != 0 {
		t.Errorf("сбои консоли: %q", warns)
	}
}

func TestOpenConsoleNotTerminal(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "in")
	if err != nil {
		t.Fatal(err)
	}
	if openConsole(f, f, nil) != nil {
		t.Error("ввод не терминал — без pty")
	}
}

// Сессия под pty: в type_mode log — прежний путь и «набрал бы» в журнал, в act — набор вместо перезапуска.
func TestWantIdleTyped(t *testing.T) {
	cfg := defaultConfig(t.TempDir())
	cfg.Restart.CompactMode, cfg.Restart.ResetMode = modeAct, modeAct
	l := launcher{name: "x-master", console: &console{}}
	a := restart.Answer{SessionID: "s1", Name: "x-master", Compact: true, Reset: true, Tokens: 120_000}
	out, line, _ := l.wantCompact(cfg, 7, a, "")
	if !out.restart || !out.compact || !strings.HasSuffix(line, "; под pty набрал бы /compact") {
		t.Errorf("log: %+v %q", out, line)
	}
	_, line, _ = l.wantReset(cfg, 7, a, "")
	if !strings.HasSuffix(line, "; под pty набрал бы /clear") {
		t.Errorf("сброс, log: %q", line)
	}
	cfg.Restart.TypeMode = modeAct
	out, line, _ = l.wantReset(cfg, 7, a, "")
	if out.restart || out.typed != "/clear" || !strings.Contains(line, "набираю /clear") {
		t.Errorf("act: %+v %q", out, line)
	}
	cfg.Restart.CompactMode = modeLog // само сжатие в log — и набирать нечего
	if out, line, _ := l.wantCompact(cfg, 7, a, ""); out.restart || out.typed != "" || !strings.Contains(line, "сжал бы") {
		t.Errorf("сжатие в log: %+v %q", out, line)
	}
	l.console = nil
	cfg.Restart.CompactMode = modeAct
	if out, line, _ := l.wantCompact(cfg, 7, a, ""); !out.restart || strings.Contains(line, "pty") {
		t.Errorf("без pty — прежний путь: %+v %q", out, line)
	}
}

// Человек вводил только что — cl не набирает, а пишет почему.
func TestTypeIntoHumanTyping(t *testing.T) {
	cfg := defaultConfig(t.TempDir())
	cfg.Restart.Journal = t.TempDir() + "/restart.log"
	c := &console{}
	now := time.Now()
	c.typed.Store(now.Add(-10 * time.Second).UnixNano())
	launcher{console: c}.typeInto(cfg, now, 7, outcome{name: "x-master", typed: "/compact"})
	data, err := os.ReadFile(cfg.Restart.Journal)
	if err != nil || !strings.Contains(string(data), "человек вводил 10s назад — не набираю") {
		t.Errorf("журнал: %q %v", data, err)
	}
}
