// Слежение за записями в /tmp и папках следов (internal/track): часть демона, обновляется graft install без sudo.
// Дескрипторы, требующие root, даёт служба aguard-track-helper (internal/trackfd).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/aiseeq/aguard/internal/track"
	"github.com/aiseeq/aguard/internal/trackfd"
)

// trackRetry — пауза перед новой парой: помощник ещё не поднялся или экземпляр упал.
const trackRetry = 30 * time.Second

// trackFetch — пара от помощника; подменяется в тестах.
func (a *app) fetchPair() (trackfd.Pair, error) {
	if a.trackFetch != nil {
		return a.trackFetch()
	}
	return trackfd.Fetch(trackfd.Socket, 0)
}

func (a *app) openTrackConnector() (*os.File, error) {
	fd, err := trackfd.FetchConn(trackfd.Socket, 0)
	if err != nil {
		return nil, fmt.Errorf("proc connector: получение дескриптора: %w", err)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("proc connector: неблокирующий режим: %w", err)
	}
	return os.NewFile(uintptr(fd), "proc connector"), nil
}

func (a *app) fetchTrackFan() (int, error) {
	if a.trackFanFetch != nil {
		return a.trackFanFetch()
	}
	return trackfd.FetchFan(trackfd.Socket, 0)
}

func trackV2Unavailable(err error) bool {
	return errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ECONNREFUSED)
}

type trackModeState struct {
	mu   sync.Mutex
	mode string
}

func (a *app) noteTrackMode(mode string) {
	a.trackMode.note(mode, a.log)
}

func (s *trackModeState) note(mode string, log *slog.Logger) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mode == mode {
		return
	}
	s.mode = mode
	if mode == "legacy" {
		log.Warn("aguard слежение: старый helper, используются две подписки; нужен graft install-helper")
	} else {
		log.Info("aguard слежение: общий proc connector доступен")
	}
}

// trackInstances — экземпляры слежения демона: замер каждого уступает занятой машине своим Yielder (замеры экземпляра
// идут по одному, экземпляры — параллельно).
func (a *app) trackInstances() []track.Config {
	insts := track.Instances(a.home)
	cfg, cfgErr := a.config()
	if cfgErr != nil {
		a.warn("track", "конфиг для письма о pressure", "err", cfgErr)
	}
	for i := range insts {
		name := insts[i].Name
		insts[i].Yield = a.pressureYielder("track", func(err error) {
			a.warn("track", "замер машины для паузы замера", "name", name, "err", err)
		}, cfg.Lead, cfg.SendAuth).Wait
	}
	return insts
}

// trackLoop ведёт один экземпляр до отмены ctx. Причина простоя пишется один раз на смену: помощника может не быть часами.
func (a *app) trackLoop(ctx context.Context, cfg *track.Config, retry time.Duration) {
	last := ""
	for ctx.Err() == nil {
		err := a.trackOnce(ctx, cfg)
		if ctx.Err() != nil {
			return
		}
		if text := fmt.Sprint(err); text != last {
			a.log.Error("aguard слежение: простой", "name", cfg.Name, "err", err, "retry", retry.String())
			last = text
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(retry):
		}
	}
}

// trackOnce — один заход на новой паре; паника пишется и не роняет остальных наблюдателей.
func (a *app) trackOnce(ctx context.Context, cfg *track.Config) (err error) {
	defer func() {
		if r := recover(); r != nil {
			a.log.Error("aguard слежение: паника", "name", cfg.Name, "panic", r, "stack", string(debug.Stack()))
			err = fmt.Errorf("паника: %v", r)
		}
	}()
	if a.trackShared != nil {
		fallback, err := a.trackOnceShared(ctx, cfg)
		if !fallback {
			return err
		}
		a.noteTrackMode("legacy")
	}
	return a.trackOncePair(ctx, cfg)
}

func (a *app) trackOnceShared(ctx context.Context, cfg *track.Config) (bool, error) {
	tombs, err := a.trackShared.Acquire(2 * time.Minute)
	if err != nil {
		return trackV2Unavailable(err), err
	}
	defer a.trackShared.Release()
	fd, err := a.fetchTrackFan()
	if err != nil {
		return trackV2Unavailable(err), err
	}
	a.noteTrackMode("shared")
	run := a.trackRunShared
	if run == nil {
		run = func(ctx context.Context, cfg *track.Config, fd int, tombs *track.Tombs) error {
			return track.RunShared(ctx, *cfg, fd, tombs, a.log)
		}
	}
	if err := run(ctx, cfg, fd, tombs); err != nil {
		return false, err
	}
	if ctx.Err() == nil {
		return false, errors.New("сторож остановился без причины")
	}
	return false, nil
}

func (a *app) trackOncePair(ctx context.Context, cfg *track.Config) error {
	pair, err := a.fetchPair()
	if err != nil {
		return err
	}
	run := a.trackRunLegacy
	if run == nil {
		run = func(ctx context.Context, cfg *track.Config, pair trackfd.Pair) error {
			return track.Run(ctx, *cfg, pair, a.log)
		}
	}
	if err := run(ctx, cfg, pair); err != nil {
		return err
	}
	if ctx.Err() == nil {
		return errors.New("сторож остановился без причины")
	}
	return nil
}

// trackCmd — aguard track check: настоящая пара от помощника на настоящем процессе; перед снятием старых служб и при разборе.
func (a *app) trackCmd(args []string) error {
	if len(args) != 1 || args[0] != "check" {
		return errors.New("использование: aguard track check")
	}

	insts := track.Instances(a.home)
	for i := range insts {
		cfg := &insts[i]
		dir := cfg.StateDir
		if len(cfg.Roots) > 0 {
			dir = cfg.Roots[0]
		} else if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("папка %s: %w", dir, err)
		}
		pair, err := a.fetchPair()
		if err != nil {
			return err
		}
		if err := track.Check(pair, dir); err != nil {
			return fmt.Errorf("%s: %w", dir, err)
		}
		if err := checkTrackV2(dir, func() (int, error) { return trackfd.FetchFan(trackfd.Socket, 0) }, func() (int, error) { return trackfd.FetchConn(trackfd.Socket, 0) }, track.Check); err != nil {
			return fmt.Errorf("%s: протокол v2: %w", dir, err)
		}
		fmt.Fprintf(a.out, "слежение %s: создатель виден (%s)\n", cfg.Name, dir)
	}
	return nil
}

func checkTrackV2(dir string, fan, conn func() (int, error), check func(trackfd.Pair, string) error) error {
	f, err := fan()
	if trackV2Unavailable(err) {
		return nil
	}
	if err != nil {
		return err
	}
	c, err := conn()
	if err != nil {
		_ = unix.Close(f)
		return err
	}
	return check(trackfd.Pair{Fan: f, Conn: c}, dir)
}
