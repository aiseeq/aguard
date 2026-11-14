package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sync"
	"time"

	"github.com/aiseeq/aguard/internal/record"
)

// cpuProfileFor — длина профиля CPU по SIGUSR1; переменная, чтобы тест не ждал минуту.
var cpuProfileFor = time.Minute

// heapLoop — по SIGUSR1 профиль кучи демона в debug/heap-<время>.pprof (тикет tmpguard-track-swap: что держит
// anon службы) и профиль CPU за cpuProfileFor в debug/cpu-<время>.pprof (тикет daemon-cpu-day-x3: профиль
// Go считает и время в системных вызовах, perf без sudo его не видит). Разовые снимки: порта и сокета нет,
// файлы читает `go tool pprof`.
func (a *app) heapLoop(ctx context.Context, sig <-chan os.Signal) {
	var cpu sync.WaitGroup
	defer cpu.Wait() // профиль CPU пишет журнал до выхода цикла
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
			cpu.Go(func() { a.cpuProfile(ctx, time.Now(), cpuProfileFor) })
		}
	}
}

// cpuProfile пишет профиль CPU за d (или до остановки демона). Второй сигнал, пока идёт первый профиль,
// даёт ошибку StartCPUProfile в журнал, а не второй профиль.
func (a *app) cpuProfile(ctx context.Context, now time.Time, d time.Duration) {
	var buf bytes.Buffer
	if err := pprof.StartCPUProfile(&buf); err != nil {
		a.log.Error("aguard cpu: профиль не начат", "err", err)
		return
	}
	t := time.NewTimer(d)
	select {
	case <-ctx.Done():
	case <-t.C:
	}
	t.Stop()
	pprof.StopCPUProfile()
	dir := a.dir("debug")
	path := filepath.Join(dir, "cpu-"+now.Format("20060102-150405")+".pprof")
	err := os.MkdirAll(dir, 0o700)
	if err == nil {
		err = record.WriteFileAtomic(path, buf.Bytes(), 0o600)
	}
	if err != nil {
		a.log.Error("aguard cpu: профиль не записан", "path", path, "err", err)
		return
	}
	a.log.Info("aguard cpu: профиль CPU", "path", path, "seconds", d.Seconds())
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
