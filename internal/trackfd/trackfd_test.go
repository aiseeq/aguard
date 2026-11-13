package trackfd

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// pipes — вместо fanotify и netlink: передача одна и та же, а открыть настоящие можно только под root.
func pipes(t *testing.T, opened *atomic.Int32) func() (Pair, error) {
	return func() (Pair, error) {
		opened.Add(1)
		var a, b [2]int
		if err := unix.Pipe(a[:]); err != nil {
			t.Error(err)
		}
		if err := unix.Pipe(b[:]); err != nil {
			t.Error(err)
		}
		_ = unix.Close(a[1])
		_ = unix.Close(b[1])
		return Pair{Fan: a[0], Conn: b[0]}, nil
	}
}

func serve(t *testing.T, uid int, open func() (Pair, error)) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "x", "fds.sock")
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := Serve(ctx, path, uid, open, slog.New(slog.DiscardHandler)); err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(func() { cancel(); wg.Wait() })
	for end := time.Now().Add(5 * time.Second); ; {
		if fi, err := os.Stat(path); err == nil && fi.Mode().Perm() == 0o666 {
			return path
		}
		if time.Now().After(end) {
			t.Fatal("сокет не появился")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Свой uid получает новую пару на каждое подключение: у двух читателей не общая очередь.
func TestFetchFreshPairEachConnect(t *testing.T) {
	var opened atomic.Int32
	path := serve(t, os.Getuid(), pipes(t, &opened))
	a, err := Fetch(path, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Fetch(path, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if opened.Load() != 2 || a.Fan == b.Fan || a.Fan == a.Conn {
		t.Fatalf("открыто %d, пары %+v %+v", opened.Load(), a, b)
	}
	for _, fd := range []int{a.Fan, a.Conn, b.Fan, b.Conn} {
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		if err != nil || flags&unix.FD_CLOEXEC == 0 {
			t.Fatalf("fd %d без CLOEXEC: %d, %v", fd, flags, err)
		}
	}
}

// Чужой uid дескрипторов не получает, и они для него даже не открываются.
func TestServeRefusesOtherUID(t *testing.T) {
	var opened atomic.Int32
	path := serve(t, os.Getuid()+1, pipes(t, &opened))
	if p, err := Fetch(path, os.Getuid()); err == nil {
		p.Close()
		t.Fatal("чужой uid получил дескрипторы")
	} else if !strings.Contains(err.Error(), "отказал") {
		t.Fatalf("ошибка: %v", err)
	}
	if opened.Load() != 0 {
		t.Fatalf("для чужого открыто %d", opened.Load())
	}
}

// Клиент не верит сокету, который держит не тот uid: подмена дала бы поддельные события.
func TestFetchRefusesWrongServer(t *testing.T) {
	var opened atomic.Int32
	path := serve(t, os.Getuid(), pipes(t, &opened))
	if p, err := Fetch(path, os.Getuid()+1); err == nil {
		p.Close()
		t.Fatal("поверил чужому сокету")
	} else if !strings.Contains(err.Error(), "держит uid") {
		t.Fatalf("ошибка: %v", err)
	}
}

// Помощника нет — ошибка, а не тишина.
// Зависший помощник принял подключение и молчит: срок вместо вечного ожидания демона и установки под sudo.
func TestFetchSilentHelperTimesOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	res := make(chan error, 1)
	go func() {
		_, err := fetch(path, os.Getuid(), 50*time.Millisecond)
		res <- err
	}()
	select {
	case err := <-res:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("молчащий помощник: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("молчащий помощник держит Fetch без срока")
	}
}

// Один дескриптор вместо двух — отказ, а не пара с мусором.
func TestParseRightsRefusesOneFD(t *testing.T) {
	var p [2]int
	if err := unix.Pipe(p[:]); err != nil {
		t.Fatal(err)
	}
	defer unix.Close(p[1])
	pair, err := parseRights(unix.UnixRights(p[0]))
	if err == nil {
		pair.Close()
		t.Fatal("принял один дескриптор как пару")
	}
	if _, err := unix.FcntlInt(uintptr(p[0]), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
		t.Fatalf("лишний дескриптор остался открытым: %v", err)
	}
}

func TestFetchNoHelper(t *testing.T) {
	if _, err := Fetch(filepath.Join(t.TempDir(), "нет.sock"), 0); err == nil {
		t.Fatal("нет ошибки")
	}
}
