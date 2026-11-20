package main

import (
	"context"
	"errors"
	"fmt"
	"io"
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

	"github.com/aiseeq/aguard/internal/track"
	"github.com/aiseeq/aguard/internal/trackfd"
)

func TestTrackOnceSharedReleasesOnRestart(t *testing.T) {
	var writer *os.File
	var opens atomic.Int32
	s := track.NewSharedTombs(2*time.Minute, slog.New(slog.DiscardHandler), func() (*os.File, error) {
		opens.Add(1)
		r, w, err := os.Pipe()
		writer = w
		return r, err
	})
	_, err := s.Acquire(2 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	want := errors.New("перезапуск")
	a := &app{log: slog.New(slog.DiscardHandler), trackShared: s, trackMode: &trackModeState{}}
	a.trackFanFetch = func() (int, error) {
		var f [2]int
		if err := unix.Pipe(f[:]); err != nil {
			return -1, err
		}
		_ = unix.Close(f[1])
		return f[0], nil
	}
	a.trackRunShared = func(_ context.Context, _ *track.Config, fd int, _ *track.Tombs) error {
		_ = unix.Close(fd)
		return want
	}
	for range 2 {
		if err := a.trackOnce(t.Context(), &track.Config{}); !errors.Is(err, want) {
			t.Fatalf("trackOnce: %v", err)
		}
	}
	if opens.Load() != 1 {
		t.Fatalf("открытий connector %d", opens.Load())
	}
	s.Release()
	if _, err := writer.Write([]byte{1}); !errors.Is(err, unix.EPIPE) {
		t.Fatalf("последний потребитель не закрыл connector: %v", err)
	}
}

func TestTrackOnceOldHelperFallbackAndUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fds.sock")
	legacy, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	var legacyMu sync.Mutex
	var legacyWrites []int
	legacyDone := make(chan struct{})
	defer func() {
		_ = legacy.Close()
		<-legacyDone
		legacyMu.Lock()
		defer legacyMu.Unlock()
		for _, fd := range legacyWrites {
			_ = unix.Close(fd)
		}
	}()
	go func() {
		defer close(legacyDone)
		for {
			c, err := legacy.AcceptUnix()
			if err != nil {
				return
			}
			var a, b [2]int
			if unix.Pipe(a[:]) != nil || unix.Pipe(b[:]) != nil {
				_ = c.Close()
				return
			}
			_, _, _ = c.WriteMsgUnix([]byte{1}, unix.UnixRights(a[0], b[0]), nil)
			legacyMu.Lock()
			legacyWrites = append(legacyWrites, a[1], b[1])
			legacyMu.Unlock()
			for _, fd := range []int{a[0], b[0]} {
				_ = unix.Close(fd)
			}
			_ = c.Close()
		}
	}()
	logs := &syncBuilder{}
	want := errors.New("остановка пробы")
	a := &app{log: slog.New(slog.NewTextHandler(logs, nil)), trackMode: &trackModeState{}}
	a.trackFetch = func() (trackfd.Pair, error) { return trackfd.Fetch(path, os.Getuid()) }
	a.trackShared = track.NewSharedTombs(time.Minute, a.log, func() (*os.File, error) {
		fd, err := trackfd.FetchConn(path, os.Getuid())
		if err != nil {
			return nil, err
		}
		if err := unix.SetNonblock(fd, true); err != nil {
			_ = unix.Close(fd)
			return nil, err
		}
		return os.NewFile(uintptr(fd), "connector"), nil
	})
	a.trackFanFetch = func() (int, error) { return trackfd.FetchFan(path, os.Getuid()) }
	var oldRuns, newRuns int
	a.trackRunLegacy = func(ctx context.Context, _ *track.Config, pair trackfd.Pair) error {
		oldRuns++
		dir := t.TempDir()
		cfg := track.Config{Name: "test", StateDir: filepath.Join(dir, "state"), RunDir: filepath.Join(dir, "run"), YoungEvery: time.Minute, Full: time.Hour, Young: time.Minute, Persist: time.Hour, RotateBytes: 1 << 20}
		runCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		if err := track.Run(runCtx, cfg, pair, a.log); err != nil {
			t.Errorf("legacy tracker: %v", err)
		}
		return want
	}
	a.trackRunShared = func(_ context.Context, _ *track.Config, fd int, _ *track.Tombs) error {
		_ = unix.Close(fd)
		newRuns++
		return want
	}
	if err := a.trackOnce(t.Context(), &track.Config{}); !errors.Is(err, want) {
		t.Fatalf("fallback ENOENT: %v", err)
	}
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path + ".v2", Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.trackOnce(t.Context(), &track.Config{}); !errors.Is(err, want) {
		t.Fatalf("fallback ECONNREFUSED: %v", err)
	}
	if err := os.Remove(path + ".v2"); err != nil {
		t.Fatal(err)
	}
	if oldRuns != 2 || strings.Count(logs.String(), "graft install-helper") != 1 {
		t.Fatalf("runs=%d log=%s", oldRuns, logs.String())
	}
	if !strings.Contains(logs.String(), "следим за") {
		t.Fatal("старый трекер не запустился")
	}
	v2, err := net.ListenUnix("unix", &net.UnixAddr{Name: path + ".v2", Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	var writers []*os.File
	var writerMu sync.Mutex
	v2Done := make(chan struct{})
	defer func() {
		_ = v2.Close()
		<-v2Done
		writerMu.Lock()
		defer writerMu.Unlock()
		for _, w := range writers {
			_ = w.Close()
		}
	}()
	go func() {
		defer close(v2Done)
		for {
			c, err := v2.AcceptUnix()
			if err != nil {
				return
			}
			var req [1]byte
			if _, err := io.ReadFull(c, req[:]); err != nil {
				_ = c.Close()
				continue
			}
			r, w, err := os.Pipe()
			if err != nil {
				_ = c.Close()
				return
			}
			writerMu.Lock()
			writers = append(writers, w)
			writerMu.Unlock()
			_, _, _ = c.WriteMsgUnix([]byte{1}, unix.UnixRights(int(r.Fd())), nil)
			_ = r.Close()
			_ = c.Close()
		}
	}()
	if err := a.trackOnce(t.Context(), &track.Config{}); !errors.Is(err, want) {
		t.Fatalf("v2: %v", err)
	}
	if newRuns != 1 || oldRuns != 2 || !strings.Contains(logs.String(), "общий proc connector доступен") {
		t.Fatalf("old=%d new=%d log=%s", oldRuns, newRuns, logs.String())
	}
}

func TestCheckTrackV2(t *testing.T) {
	called := 0
	check := func(p trackfd.Pair, dir string) error {
		called++
		p.Close()
		if dir != "probe" {
			t.Errorf("dir=%q", dir)
		}
		return nil
	}
	fd := func() (int, error) {
		var p [2]int
		if err := unix.Pipe(p[:]); err != nil {
			return -1, err
		}
		_ = unix.Close(p[1])
		return p[0], nil
	}
	if err := checkTrackV2("probe", func() (int, error) { return -1, unix.ENOENT }, fd, check); err != nil || called != 0 {
		t.Fatalf("старый helper: %v called=%d", err, called)
	}
	if err := checkTrackV2("probe", fd, fd, check); err != nil || called != 1 {
		t.Fatalf("новый helper: %v called=%d", err, called)
	}
	if err := checkTrackV2("probe", func() (int, error) { return -1, unix.EACCES }, fd, check); !errors.Is(err, unix.EACCES) {
		t.Fatalf("скрыта ошибка: %v", err)
	}
}

func TestTrackFallbackOnlyForUnavailableV2(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{{unix.ENOENT, true}, {unix.ECONNREFUSED, true}, {unix.EACCES, false}, {io.EOF, false}} {
		if got := trackV2Unavailable(fmt.Errorf("v2: %w", tc.err)); got != tc.want {
			t.Errorf("%v: fallback=%v", tc.err, got)
		}
	}
}

// Помощника нет часами: одна строка журнала на причину, а не по строке раз в 30 с; отмена выходит сразу.
func TestTrackLoopLogsOncePerCause(t *testing.T) {
	logs := &syncBuilder{}
	a := &app{home: t.TempDir(), log: slog.New(slog.NewTextHandler(logs, nil))}
	var calls atomic.Int32
	a.trackFetch = func() (trackfd.Pair, error) {
		if calls.Add(1) <= 3 {
			return trackfd.Pair{}, errors.New("помощника нет")
		}
		return trackfd.Pair{}, errors.New("другая причина")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Go(func() { a.trackLoop(ctx, &track.Config{Name: "track"}, time.Millisecond) })
	for end := time.Now().Add(5 * time.Second); calls.Load() < 6; {
		if time.Now().After(end) {
			t.Fatalf("заходов %d, ждали 6", calls.Load())
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("отмена не завершила цикл")
	}
	for _, cause := range []string{"помощника нет", "другая причина"} {
		if n := strings.Count(logs.String(), cause); n != 1 {
			t.Fatalf("%q: строк %d:\n%s", cause, n, logs.String())
		}
	}
}

// Паника экземпляра не роняет демон: она становится причиной простоя.
func TestTrackOnceRecoversPanic(t *testing.T) {
	logs := &syncBuilder{}
	a := &app{home: t.TempDir(), log: slog.New(slog.NewTextHandler(logs, nil))}
	a.trackFetch = func() (trackfd.Pair, error) { panic("сбой") }
	err := a.trackOnce(context.Background(), &track.Config{Name: "track"})
	if err == nil || !strings.Contains(err.Error(), "паника") {
		t.Fatalf("%v", err)
	}
}

type syncBuilder struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuilder) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuilder) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// Замер каждого экземпляра слежения демона уступает занятой машине (тикет io-freeze-aguard).
func TestTrackInstancesYield(t *testing.T) {
	a := &app{home: t.TempDir()}
	insts := a.trackInstances()
	if len(insts) == 0 {
		t.Fatal("нет экземпляров")
	}
	for _, c := range insts {
		if c.Yield == nil {
			t.Fatalf("экземпляр %s без уступки", c.Name)
		}
	}
}
