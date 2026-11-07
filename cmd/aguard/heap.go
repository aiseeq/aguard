package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"time"

	"github.com/aiseeq/aguard/internal/record"
)

// heapLoop — по SIGUSR1 профиль кучи демона в debug/heap-<время>.pprof (тикет tmpguard-track-swap: что держит
// anon службы). Разовый снимок: порта и сокета нет, файл читает `go tool pprof`.
func (a *app) heapLoop(ctx context.Context, sig <-chan os.Signal) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-sig:
			path, err := a.writeHeapProfile(time.Now())
			if err != nil {
				a.log.Error("aguard heap: профиль не записан", "err", err)
				continue
			}
			a.log.Info("aguard heap: профиль кучи", "path", path)
		}
	}
}

// writeHeapProfile — профиль кучи после сборки мусора: inuse — живые объекты на этот момент.
func (a *app) writeHeapProfile(now time.Time) (string, error) {
	dir := a.dir("debug")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("папка профиля: %w", err)
	}
	path := filepath.Join(dir, "heap-"+now.Format("20060102-150405")+".pprof")
	runtime.GC()
	if err := record.WriteFileAtomicFunc(path, 0o600, func(w io.Writer) error { return pprof.WriteHeapProfile(w) }); err != nil {
		return "", fmt.Errorf("профиль кучи %s: %w", path, err)
	}
	return path, nil
}
