package main

// Claude под своим pty cl (задание git-admin 26.10 по слову пользователя 17:50:26 CET в транскрипте 6eca916e: «Тут
// доработка cl чтобы он мог своим потомкам слать текст выглядит вариантом получше»): ввод человека идёт через cl в
// master pty, вывод claude — в терминал cl, и cl может набрать /compact или /clear в живой сессии без перезапуска
// (фоновые задачи Claude Code при этом сохраняются — пользователь проверил с chooks).

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
)

const (
	// typePause — между текстом и Enter: пачку байт с \r на конце TUI мог бы принять за вставку (проба в tmux
	// 26.10: «/clear», 0,4 с, «\r» — команда).
	typePause = 400 * time.Millisecond
	// drainTimeout — сколько ждать остаток вывода claude после его выхода: последний кадр экрана.
	drainTimeout = time.Second
	pasteStart   = "\x1b[200~"
	pasteEnd     = "\x1b[201~"
)

// errNoSession — claude сейчас не запущен: набирать некуда.
var errNoSession = errors.New("claude не запущен")

// console — терминал cl для claude под своим pty. Один поток читает ввод человека всё время жизни cl и пишет его
// в pty текущего claude (между запусками ввод теряется); сырой режим терминала — только пока claude запущен:
// сообщения cl между запусками идут в обычный терминал.
type console struct {
	in, out *os.File
	mu      sync.Mutex // master и запись в него: набор cl не перемешивается с вводом человека
	master  *os.File
	raw     *term.State
	typed   atomic.Int64 // последний ввод человека, unix nano
	winch   chan os.Signal
	// warn — сбой консоли в журнал cl: claude вышел, а ввод ещё шёл; окно или терминал не вернулись.
	warn func(string)
}

// openConsole — консоль на терминале cl; ввод не терминал (cl из скрипта) — nil: claude получает терминал напрямую.
func openConsole(in, out *os.File, warn func(string)) *console {
	if !term.IsTerminal(int(in.Fd())) || !term.IsTerminal(int(out.Fd())) {
		return nil
	}
	c := &console{in: in, out: out, winch: make(chan os.Signal, 1), warn: warn}
	signal.Notify(c.winch, syscall.SIGWINCH)
	go c.pump()
	go c.resize()
	return c
}

// pump — ввод человека в pty текущего claude. Читает до конца ввода cl; на выходе cl его не ждут.
func (c *console) pump() {
	buf := make([]byte, 4096)
	for {
		n, err := c.in.Read(buf)
		if n > 0 {
			c.typed.Store(time.Now().UnixNano())
			c.mu.Lock()
			if c.master != nil {
				if _, werr := c.master.Write(buf[:n]); werr != nil { // claude вышел — ввод некуда девать
					c.report("ввод человека не дошёл до claude", werr)
				}
			}
			c.mu.Unlock()
		}
		if err != nil {
			c.report("ввод cl кончился", err) // вкладку закрыли: дальше cl выйдет по SIGHUP
			return
		}
	}
}

// report — сбой консоли в журнал; журнала нет (тест) — молча: консоль от этого не ломается.
func (c *console) report(what string, err error) {
	if c.warn != nil {
		c.warn(fmt.Sprintf("pty: %s: %v", what, err))
	}
}

// resize — размер окна cl в pty claude.
func (c *console) resize() {
	for range c.winch {
		c.mu.Lock()
		if c.master != nil {
			if err := pty.InheritSize(c.in, c.master); err != nil { // claude останется в прежнем размере
				c.report("размер окна", err)
			}
		}
		c.mu.Unlock()
	}
}

// start запускает cmd под новым pty и переводит терминал cl в сырой режим. Вернувшаяся функция ждёт остаток вывода,
// закрывает pty и возвращает терминал — звать после выхода claude.
func (c *console) start(cmd *exec.Cmd) (func(), error) {
	size, err := pty.GetsizeFull(c.in)
	if err != nil {
		return nil, fmt.Errorf("размер окна: %w", err)
	}
	master, err := pty.StartWithSize(cmd, size)
	if err != nil {
		return nil, fmt.Errorf("pty: %w", err)
	}
	raw, err := term.MakeRaw(int(c.in.Fd()))
	if err != nil { // без сырого режима ввод шёл бы строками с эхом — claude не запускать
		return nil, errors.Join(fmt.Errorf("сырой режим терминала: %w", err), stopChild(cmd), master.Close())
	}
	c.mu.Lock()
	c.master, c.raw = master, raw
	c.mu.Unlock()
	drained := make(chan struct{})
	go func() {
		// конец — EIO: claude вышел и закрыл свою сторону; иное — сбой вывода
		if _, err := io.Copy(c.out, master); err != nil && !errors.Is(err, syscall.EIO) {
			c.report("вывод claude", err)
		}
		close(drained)
	}()
	return func() {
		select {
		case <-drained:
		case <-time.After(drainTimeout):
		}
		c.mu.Lock()
		c.master = nil
		c.mu.Unlock()
		if err := master.Close(); err != nil {
			c.report("закрыть pty", err)
		}
		c.restore()
	}, nil
}

// stopChild — убить запущенный claude и дождаться его: без Wait он остался бы зомби.
func stopChild(cmd *exec.Cmd) error {
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("остановить claude: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) { // убитый процесс выходит с ошибкой — это и ждали
			return fmt.Errorf("дождаться claude: %w", err)
		}
	}
	return nil
}

// restore возвращает терминал cl в обычный режим; звать и на панике, и на выходе по сигналу.
func (c *console) restore() {
	c.mu.Lock()
	raw := c.raw
	c.raw = nil
	c.mu.Unlock()
	if raw != nil {
		if err := term.Restore(int(c.in.Fd()), raw); err != nil { // вкладку закрыли — терминала нет
			c.report("вернуть режим терминала", err)
		}
	}
}

// idle — сколько человек ничего не вводил; не вводил с запуска cl — с нуля unix-времени (очень долго).
func (c *console) idle(now time.Time) time.Duration {
	return now.Sub(time.Unix(0, c.typed.Load()))
}

// typeLine набирает text в поле ввода claude и отдельно — Enter. paste — вставкой (bracketed paste): переводы строк
// остаются в поле, а не отправляют его по строчке.
func (c *console) typeLine(text string, paste bool) error {
	if paste {
		text = pasteStart + text + pasteEnd
	}
	if err := c.write(text); err != nil {
		return err
	}
	time.Sleep(typePause)
	return c.write("\r")
}

func (c *console) write(s string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.master == nil {
		return errNoSession
	}
	if _, err := io.WriteString(c.master, s); err != nil {
		return fmt.Errorf("ввод в pty: %w", err)
	}
	return nil
}
