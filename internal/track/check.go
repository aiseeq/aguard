package track

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/aiseeq/aguard/internal/record"
	"github.com/aiseeq/aguard/internal/trackfd"
)

// checkWait — сколько ждать события от настоящего ядра.
const checkWait = 5 * time.Second

// Check проверяет пару от помощника на настоящем процессе в папке dir: пометка ставится без root, событие называет pid
// создателя и живой pidfd, proc connector видит его запуск. Идёт перед снятием прежних служб: иначе первый же отказ ядра
// оставил бы машину без слежения.
func Check(pair trackfd.Pair, dir string) (err error) {
	fan, err := pollable(pair.Fan, "fanotify")
	if err != nil {
		pair.Close()
		return err
	}
	defer fan.Close()
	conn, err := pollable(pair.Conn, "proc connector")
	if err != nil {
		_ = unix.Close(pair.Conn)
		return err
	}
	defer conn.Close()
	sub, err := os.MkdirTemp(dir, "aguard-track-check-")
	if err != nil {
		return fmt.Errorf("папка пробы: %w", err)
	}
	defer os.RemoveAll(sub)
	if _, err := fanMark(pair.Fan, sub); err != nil {
		return err
	}
	child := exec.Command("sh", "-c", `: >"$0/x"; exec sleep 30`, sub)
	if err := child.Start(); err != nil {
		return fmt.Errorf("процесс пробы: %w", err)
	}
	defer func() { err = errors.Join(err, stopChild(child)) }()
	if err := checkFan(fan, child.Process.Pid); err != nil {
		return err
	}
	return checkConn(conn, child.Process.Pid)
}

// stopChild снимает процесс пробы; его выход по сигналу и есть успех.
func stopChild(child *exec.Cmd) error {
	if err := child.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("процесс пробы: %w", err)
	}
	var exit *exec.ExitError
	if err := child.Wait(); err != nil && !errors.As(err, &exit) {
		return fmt.Errorf("процесс пробы: %w", err)
	}
	return nil
}

// checkFan — событие создания называет pid пробы и живой pidfd.
func checkFan(fan *os.File, pid int) error {
	if err := fan.SetReadDeadline(time.Now().Add(checkWait)); err != nil {
		return fmt.Errorf("fanotify: срок чтения: %w", err)
	}
	buf := make([]byte, fanReadBuf)
	n, err := fan.Read(buf)
	if err != nil {
		return fmt.Errorf("fanotify: событие пробы не пришло: %w", err)
	}
	evs := parseEvents(buf[:n])
	created := false
	var bad error
	for i := range evs {
		e := &evs[i]
		if e.mask&unix.FAN_CREATE != 0 {
			created = true
			bad = errors.Join(bad, checkCreator(e, pid))
		}
		if e.pidfd >= 0 {
			if err := unix.Close(e.pidfd); err != nil {
				bad = errors.Join(bad, fmt.Errorf("fanotify: pidfd: %w", err))
			}
		}
	}
	if !created {
		return fmt.Errorf("fanotify: среди %d событий нет создания", len(evs))
	}
	return bad
}

// checkCreator — событие называет процесс пробы и живой pidfd: ради них нужен root.
func checkCreator(e *fevent, pid int) error {
	if e.pid != pid {
		return fmt.Errorf("fanotify: создатель %d, ждали %d: дескриптор потерял права", e.pid, pid)
	}
	if e.pidfd < 0 {
		return fmt.Errorf("fanotify: нет pidfd создателя: %d", e.pidfd)
	}
	if err := unix.PidfdSendSignal(e.pidfd, 0, nil, 0); err != nil {
		return fmt.Errorf("fanotify: pidfd создателя: %w", err)
	}
	return nil
}

// checkConn — в сокете есть запуск пробы: подписка помощника работает, читать может пользователь.
func checkConn(conn *os.File, pid int) error {
	if err := conn.SetReadDeadline(time.Now().Add(checkWait)); err != nil {
		return fmt.Errorf("proc connector: срок чтения: %w", err)
	}
	tombs := newTombs(time.Minute, slog.New(slog.DiscardHandler))
	buf := make([]byte, 64<<10)
	for {
		n, err := conn.Read(buf)
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return fmt.Errorf("proc connector: запуск процесса %d не виден за %v", pid, checkWait)
		}
		if err != nil && !errors.Is(err, unix.ENOBUFS) {
			return fmt.Errorf("proc connector: чтение: %w", err)
		}
		if err == nil {
			tombs.messages(buf[:n])
		}
		if _, ok := tombs.lookup(pid); ok {
			return nil
		}
	}
}

// messages разбирает одну датаграмму netlink.
func (t *Tombs) messages(b []byte) {
	for len(b) >= 16 {
		l := int(binary.LittleEndian.Uint32(b))
		if l < 16 || l > len(b) {
			break
		}
		t.handle(b[16:l])
		b = b[min((l+3)&^3, len(b)):]
	}
}

// Instances — что и где слежение ведёт: /tmp и папки классов следов в ~/.cache и ~/.local/state.
// Папки следов — не ~/.cache целиком: иначе полный замер обходил бы go-build и другие кэши.
func Instances(home string) []Config {
	base := func(name string) Config {
		return Config{Name: name, StateDir: record.TrackStateDir(home, name), RunDir: record.TrackRunDir(name), Full: 5 * time.Minute, Young: 15 * time.Minute,
			YoungEvery: time.Minute, Persist: 30 * time.Minute, RotateBytes: 64 << 20}
	}
	tmp := base("track")
	tmp.Roots, tmp.Containers = []string{"/tmp"}, []string{"/tmp/claude-tmp"}
	tmp.Persist = 24 * time.Hour // /tmp — tmpfs: после сбоя питания записей нет, снимок с диска не нужен; при остановке пишется всегда
	home2 := base("track-home")
	home2.Full = 6 * time.Hour
	for _, d := range []string{".cache/sc2go-scene-orders", ".cache/farm-hard", ".local/state/worker", ".cache/moment-frame"} {
		home2.Containers = append(home2.Containers, filepath.Join(home, d))
	}
	return []Config{tmp, home2}
}
