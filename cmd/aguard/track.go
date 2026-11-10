// Слежение за записями в /tmp и папках следов (internal/track): часть демона, обновляется graft install без sudo.
// Дескрипторы, требующие root, даёт служба aguard-track-helper (internal/trackfd).
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"time"

	"github.com/aiseeq/aguard/internal/track"
	"github.com/aiseeq/aguard/internal/trackfd"
	"github.com/aiseeq/aguard/internal/yield"
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

// trackInstances — экземпляры слежения демона: замер каждого уступает занятой машине своим Yielder (замеры экземпляра
// идут по одному, экземпляры — параллельно).
func (a *app) trackInstances() []track.Config {
	insts := track.Instances(a.home)
	for i := range insts {
		name := insts[i].Name
		insts[i].Yield = yield.New(procRoot, func(err error) {
			a.warn("track", "замер машины для паузы замера", "name", name, "err", err)
		}).Wait
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
	pair, err := a.fetchPair()
	if err != nil {
		return err
	}
	if err := track.Run(ctx, *cfg, pair, a.log); err != nil {
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
		fmt.Fprintf(a.out, "слежение %s: создатель виден (%s)\n", cfg.Name, dir)
	}
	return nil
}
