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
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

const (
	// typePause — между текстом и Enter: пачку байт с \r на конце TUI мог бы принять за вставку (проба в tmux
	// 26.10: «/clear», 0,4 с, «\r» — команда).
	typePause = 400 * time.Millisecond
	// drainTimeout — сколько ждать остаток вывода claude после его выхода: последний кадр экрана.
	drainTimeout = time.Second
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

	winch chan os.Signal
	// note — строка в журнал cl: сбой консоли (claude вышел, а ввод ещё шёл; окно или терминал не вернулись) и
	// сделанное за человека (окно доверия, trust.go).
	note func(string)
}

// openConsole — консоль на терминале cl; ввод не терминал (cl из скрипта) — nil: claude получает терминал напрямую.
func openConsole(in, out *os.File, note func(string)) *console {
	if !term.IsTerminal(int(in.Fd())) || !term.IsTerminal(int(out.Fd())) {
		return nil
	}
	c := &console{in: in, out: out, winch: make(chan os.Signal, 1), note: note}
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
			if !gone(err) { // вкладку закрыли — терминала нет, дальше cl выйдет по SIGHUP
				c.report("ввод cl кончился", err)
			}
			return
		}
	}
}

// report — сбой консоли в журнал.
func (c *console) report(what string, err error) {
	c.say(fmt.Sprintf("pty: %s: %v", what, err))
}

// say — строка в журнал; журнала нет (тест) — молча: консоль от этого не ломается.
func (c *console) say(s string) {
	if c.note != nil {
		c.note(s)
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
// закрывает pty и возвращает терминал — звать после выхода claude. trustDir не пусто — окно доверия к этой папке
// cl подтверждает сам (trust.go).
func (c *console) start(cmd *exec.Cmd, trustDir string) (func(), error) {
	size, err := pty.GetsizeFull(c.in)
	if err != nil {
		return nil, fmt.Errorf("размер окна: %w", err)
	}
	master, err := pty.StartWithSize(cmd, size)
	if err != nil {
		return nil, fmt.Errorf("pty: %w", err)
	}
	var watch io.Writer
	if trustDir != "" {
		watch = newTrustWatch(trustDir, func() { c.confirmTrust(trustDir) })
	}
	finish, err := c.attach(master, watch)
	if err != nil { // без сырого режима ввод шёл бы строками с эхом — claude не запускать
		return nil, errors.Join(err, stopChild(cmd), master.Close())
	}
	return finish, nil
}

// attach — терминал cl в сыром режиме, ввод человека — в master, вывод claude из него — в терминал cl: для только что
// запущенного claude и для живого, которого передал прежний cl (upgrade.go). watch не nil — ему копия вывода.
// finish — как у start.
func (c *console) attach(master *os.File, watch io.Writer) (func(), error) {
	raw, err := term.MakeRaw(int(c.in.Fd()))
	if err != nil {
		return nil, fmt.Errorf("сырой режим терминала: %w", err)
	}
	c.mu.Lock()
	c.master, c.raw = master, raw
	c.mu.Unlock()
	drained := make(chan struct{})
	go func() {
		// конец — EIO: claude вышел и закрыл свою сторону; иное — сбой вывода
		out := io.Writer(c.out)
		if watch != nil {
			out = io.MultiWriter(c.out, watch)
		}
		if _, err := io.Copy(out, master); err != nil && !gone(err) {
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

// handOver готовит master живого claude к exec нового cl: снимает close-on-exec и возвращает терминал в обычный режим
// (новый cl сам переведёт его в сырой и запомнит обычный — иначе на выходе вернул бы сырой). Держит mu до exec: набор
// cl и запись ввода не начнутся; ввод, который поток уже прочёл, но не записал, пропадёт — поэтому cl зовёт exec, когда
// человек молчит humanQuiet. typed — последний ввод человека, новый cl считает тишину от него. back — exec не вышел:
// вернуть как было.
func (c *console) handOver() (fd int, typed int64, back func(), err error) {
	c.mu.Lock()
	if c.master == nil {
		c.mu.Unlock()
		return 0, 0, nil, errNoSession
	}
	raw := c.raw
	cloexec := func(flag int) error {
		conn, err := c.master.SyscallConn()
		if err != nil {
			return err
		}
		var ferr error
		if err := conn.Control(func(f uintptr) { fd = int(f); _, ferr = unix.FcntlInt(f, unix.F_SETFD, flag) }); err != nil {
			return err
		}
		return ferr
	}
	if err := cloexec(0); err != nil {
		c.mu.Unlock()
		return 0, 0, nil, fmt.Errorf("pty claude для нового cl: %w", err)
	}
	if raw != nil {
		if err := term.Restore(int(c.in.Fd()), raw); err != nil {
			c.mu.Unlock()
			return 0, 0, nil, errors.Join(fmt.Errorf("вернуть режим терминала: %w", err), cloexec(unix.FD_CLOEXEC))
		}
	}
	return fd, c.typed.Load(), func() {
		defer c.mu.Unlock()
		if err := cloexec(unix.FD_CLOEXEC); err != nil {
			c.report("pty claude: close-on-exec", err)
		}
		if raw != nil {
			if _, err := term.MakeRaw(int(c.in.Fd())); err != nil {
				c.report("сырой режим терминала", err)
			}
		}
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
		if err := term.Restore(int(c.in.Fd()), raw); err != nil && !gone(err) { // вкладку закрыли — возвращать некуда
			c.report("вернуть режим терминала", err)
		}
	}
}

// gone — терминала больше нет: вкладку закрыли (EIO на pty без master) или ввод кончился. Обычный конец, не сбой
// (пилот 26.10: закрытие панели tmux давало обе строки в журнал).
func gone(err error) bool {
	return errors.Is(err, syscall.EIO) || errors.Is(err, io.EOF)
}

// idle — сколько человек ничего не вводил; не вводил с запуска cl — с нуля unix-времени (очень долго).
func (c *console) idle(now time.Time) time.Duration {
	return now.Sub(time.Unix(0, c.typed.Load()))
}

// typeLine набирает text в поле ввода claude и отдельно — Enter.
func (c *console) typeLine(text string) error {
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
