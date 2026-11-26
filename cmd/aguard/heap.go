package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"time"

	"github.com/aiseeq/aguard/internal/record"
)

// cpuProfileFor — длина профиля CPU по SIGUSR1; переменная, чтобы тест не ждал минуту.
var cpuProfileFor = time.Minute

// heapLoop — по SIGUSR1 профиль кучи демона в debug/heap-<время>.pprof (тикет tmpguard-track-swap: что держит
// anon службы) и профиль CPU за cpuProfileFor в debug/cpu-<время>.pprof (тикет daemon-cpu-day-x3: профиль
// Go считает и время в системных вызовах, perf без sudo его не видит). Разовые снимки: порта и сокета нет,
// файлы читает `go tool pprof`. Слать только главному процессу: `systemctl --user kill --kill-whom=main -s SIGUSR1 aguard`;
// без --kill-whom=main сигнал получат и дочерние процессы службы (journalctl наблюдателя sigguard завершается).
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
	return a.writeHeapProfileAs("heap-", now)
}

func (a *app) writeHeapProfileAs(prefix string, now time.Time) (string, error) {
	dir := a.dir("debug")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("папка профиля: %w", err)
	}
	path := filepath.Join(dir, prefix+now.Format("20060102-150405")+".pprof")
	runtime.GC()
	if err := record.WriteFileAtomicFunc(path, 0o600, func(w io.Writer) error { return pprof.WriteHeapProfile(w) }); err != nil {
		return "", fmt.Errorf("профиль кучи %s: %w", path, err)
	}
	return path, nil
}

const (
	hwmEvery  = 5 * time.Second
	hwmStepKB = 32 << 10 // рост пика памяти, на котором снимаются стеки и куча
	hwmPrefix = "hwm-"
	hwmKeep   = 7 * 24 * time.Hour // снимки роста пика старше — удаляются при старте hwmLoop
)

// hwmLoop — как набирается пик памяти запуска: «итог запуска» даёт только VmHWM за весь запуск, а пик растёт за
// секунды (полный замер track 25.11 03:08 — +181 МиБ за 30 с). Раз в hwmEvery читается VmHWM; первый замер и каждый
// рост на hwmStepKB — строка журнала, стеки горутин (что шло в этот момент) и профиль кучи (разница alloc_space с
// прошлым снимком — кто выделил) в debug/hwm-* (тикет rss-growth-1118). Снимки только по росту пика, а он за запуск
// монотонный (решение daemon-telemetry-in-memory: «Просто писать файлики - только диск забивать»); старше hwmKeep — удаляются.
func (a *app) hwmLoop(ctx context.Context, statusPath string) {
	a.pruneHWMSnapshots(time.Now())
	t := time.NewTicker(hwmEvery)
	defer t.Stop()
	var last int64
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			hwm, err := a.hwmCheck(now, last, statusPath)
			if err != nil {
				a.log.Warn("aguard: пик памяти не прочитан", "err", err)
				continue
			}
			last = hwm
		}
	}
}

// hwmCheck — замер VmHWM; рост от last на hwmStepKB (первый замер — от 0) — снимки и строка журнала. Возвращает
// пик, от которого считать следующий рост, или сбой чтения.
func (a *app) hwmCheck(now time.Time, last int64, statusPath string) (int64, error) {
	data, err := os.ReadFile(statusPath)
	var hwm int64
	if err == nil {
		hwm, err = parseVmHWM(data)
	}
	if err != nil {
		return 0, fmt.Errorf("VmHWM: %w", err)
	}
	if hwm < last+hwmStepKB {
		return last, nil
	}
	attrs := []any{"vm_hwm_kb", hwm, "prev_kb", last}
	if path, err := a.writeGoroutineProfile(now); err != nil {
		attrs = append(attrs, "goroutine_err", err.Error())
	} else {
		attrs = append(attrs, "goroutines", path)
	}
	if path, err := a.writeHeapProfileAs(hwmPrefix+"heap-", now); err != nil {
		attrs = append(attrs, "heap_err", err.Error())
	} else {
		attrs = append(attrs, "heap", path)
	}
	a.log.Info("aguard: пик памяти вырос", attrs...)
	return hwm, nil
}

// writeGoroutineProfile — стеки всех горутин текстом (debug=1: одинаковые стеки сведены, с числом).
func (a *app) writeGoroutineProfile(now time.Time) (string, error) {
	dir := a.dir("debug")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("папка стеков: %w", err)
	}
	path := filepath.Join(dir, hwmPrefix+"goroutine-"+now.Format("20060102-150405")+".txt")
	if err := record.WriteFileAtomicFunc(path, 0o600, func(w io.Writer) error { return pprof.Lookup("goroutine").WriteTo(w, 1) }); err != nil {
		return "", fmt.Errorf("стеки горутин %s: %w", path, err)
	}
	return path, nil
}

// pruneHWMSnapshots — удалить снимки роста пика старше hwmKeep; сбой — строка журнала, снимки не мешают работе.
func (a *app) pruneHWMSnapshots(now time.Time) {
	entries, err := os.ReadDir(a.dir("debug"))
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		a.log.Warn("aguard: снимки пика памяти не прочитаны", "err", err)
		return
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), hwmPrefix) {
			continue
		}
		info, err := e.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err == nil && now.Sub(info.ModTime()) <= hwmKeep {
			continue
		}
		if err == nil {
			err = os.Remove(filepath.Join(a.dir("debug"), e.Name()))
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			a.log.Warn("aguard: старый снимок пика памяти не удалён", "name", e.Name(), "err", err)
		}
	}
}
