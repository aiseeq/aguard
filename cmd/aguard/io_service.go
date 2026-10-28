package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	"github.com/aiseeq/aguard/internal/iowatch"
)

// Замер io: свой цикл с тикером без дрейфа (не общий loop «проход + пауза»), детальный проход —
// в своей горутине со своим DetailReader, общих изменяемых данных с быстрым шагом нет.

// ioSink — получатель замеров главного цикла: служба или `aguard io record`. Вызывается только из
// горутины цикла.
type ioSink interface {
	step(s iowatch.Step)
	detail(s iowatch.Step)
	sampleErr(key string, err error) // замер не удался: цикл идёт дальше
}

// runIOSampling — быстрые шаги по тикеру здесь, детальные записи из своей горутины, extra —
// действия других горутин (сигнал, итог разбора), исполняются здесь же. Возвращается по ctx.
func runIOSampling(ctx context.Context, cfg iowatch.Config, roots iowatch.Roots, sink ioSink, extra <-chan func()) error {
	fast := iowatch.NewFastReader(roots.Proc, cfg.Device)
	defer fast.Close()
	if _, err := fast.Read(time.Now()); err != nil { // первое чтение — только точка отсчёта
		return err
	}
	detail, err := iowatch.NewDetailReader(roots, cfg.Device)
	if err != nil {
		return err
	}
	details := make(chan ioDetailResult, 4)
	detailCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer detail.Close()
		runIODetail(detailCtx, cfg.Detail.D(), detail, details)
	}()
	defer func() {
		stop()
		<-done
	}()
	ticker := time.NewTicker(cfg.Step.D())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case now := <-ticker.C:
			s, err := fast.Read(now)
			if err != nil {
				sink.sampleErr("fast", err)
				continue
			}
			sink.step(s)
		case r := <-details:
			if r.err != nil {
				sink.sampleErr("detail", r.err)
				continue
			}
			if r.skipped != nil {
				sink.sampleErr("skipped", r.skipped) // процессы и листья cgroup с ошибкой; остальные записаны
			}
			sink.detail(r.step)
		case f := <-extra:
			f()
		}
	}
}

type ioDetailResult struct {
	step    iowatch.Step
	skipped error // пропущенные процессы: запись годна без них
	err     error
}

// runIODetail — детальный проход раз в interval; паника прохода — ошибкой, цикл идёт дальше.
func runIODetail(ctx context.Context, interval time.Duration, r *iowatch.DetailReader, out chan<- ioDetailResult) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			res := readIODetail(r, now)
			select {
			case out <- res:
			case <-ctx.Done():
				return
			}
		}
	}
}

func readIODetail(r *iowatch.DetailReader, now time.Time) (res ioDetailResult) {
	defer func() {
		if v := recover(); v != nil {
			res.err = fmt.Errorf("паника детального прохода: %v\n%s", v, debug.Stack())
		}
	}()
	res.step, res.skipped, res.err = r.Read(now)
	return res
}

const (
	ioReloadEvery   = 10 * time.Second // конфиг перечитывается на ходу
	ioCleanupEvery  = time.Minute      // старые снимки удаляются
	ioWarnEvery     = time.Minute      // одно и то же предупреждение — не чаще
	ioNoDeviceEvery = 24 * time.Hour   // нет диска из device — напоминание раз в сутки
)

// errIORestart — изменились параметры замера (mode, step, detail, ring, device): цикл строится заново.
var errIORestart = errors.New("io: параметры замера изменились")

// ioService — наблюдатель io в демоне: снимки фриза, строка журнала, разбор агентом и письма.
// Всё состояние — у горутины ioLoop; конфиг и чистка, разбор и запись ручного снимка идут в своих
// горутинах и возвращаются через extra: быстрый цикл не ждёт ни диска, ни команд.
type ioService struct {
	a         *app
	ctx       context.Context
	dir       string
	cfg       iowatch.Config
	rec       *iowatch.Recorder
	extra     chan func()
	restart   context.CancelFunc
	restarted bool
	gen       int // номер цикла замера: итог фоновой работы прошлого цикла не применяется
	warnAt    map[string]time.Time
	pending   []iowatch.Event
	analyzing bool
	analyzed  time.Time
}

func newIOService(ctx context.Context, a *app) *ioService {
	return &ioService{a: a, ctx: ctx, dir: filepath.Join(a.dir("io"), "events"), extra: make(chan func(), 16), warnAt: map[string]time.Time{}}
}

// ioLoop — наблюдатель io: mode=off — не замеряет совсем; паника или ошибка цикла — warn и перезапуск.
// usr1 — SIGUSR1, ручной снимок буфера (`aguard io dump`); подписка — в начале run демона, чтобы
// сигнал до старта наблюдателя не убил процесс.
func (a *app) ioLoop(ctx context.Context, usr1 <-chan os.Signal) {
	s := newIOService(ctx, a)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-usr1:
				select {
				case s.extra <- s.dump:
				default: // очередь полна: снимок уже в пути
				}
			}
		}
	}()
	for ctx.Err() == nil {
		c, err := a.config()
		if err != nil {
			s.warn(time.Now(), "config", "конфиг не прочитан, замер ждёт", err)
			s.wait(ioReloadEvery)
			continue
		}
		if c.IO.Mode == iowatch.ModeOff {
			s.wait(ioReloadEvery)
			continue
		}
		err = s.run(c.IO)
		switch {
		case ctx.Err() != nil || errors.Is(err, errIORestart):
		case errors.Is(err, iowatch.ErrNoDevice):
			s.warnEvery(time.Now(), "device", ioNoDeviceEvery, "замер не идёт: диска из [io] device нет", err)
			s.wait(ioReloadEvery)
		default:
			s.warn(time.Now(), "loop", "цикл замеров перезапускается", err)
			s.wait(time.Second)
		}
	}
}

// wait — пауза, в которой исполняются действия extra (ручной снимок, итог разбора).
func (s *ioService) wait(d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-timer.C:
			return
		case f := <-s.extra:
			f()
		}
	}
}

// run — один цикл замера до остановки, перезапуска или паники; открытое событие закрывается.
func (s *ioService) run(cfg iowatch.Config) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("паника в цикле io: %v\n%s", v, debug.Stack())
		}
		if s.rec == nil {
			return
		}
		ev, open, closeErr := s.rec.Close()
		if closeErr != nil {
			s.a.warn("io", "запись снимка фриза", "err", closeErr)
		}
		if open {
			s.closed(ev)
		}
		s.rec = nil
	}()
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	s.gen++
	s.cfg, s.restart, s.restarted = cfg, cancel, false
	s.rec = iowatch.NewRecorder(cfg, s.dir)
	go s.housekeep(ctx, s.gen, cfg.Keep.D())
	err = runIOSampling(ctx, cfg, iowatch.SystemRoots(), s, s.extra)
	if s.restarted {
		return errIORestart
	}
	return err
}

func (s *ioService) step(st iowatch.Step) {
	ev, err := s.rec.Step(st)
	if err != nil {
		s.warn(st.At, "event", "снимок фриза", err)
	}
	if ev != nil {
		s.closed(*ev)
	}
	s.launchAnalysis(st.At)
}

func (s *ioService) detail(st iowatch.Step) { s.rec.Detail(st) }

func (s *ioService) sampleErr(key string, err error) {
	s.warn(time.Now(), key, "замер не удался", err)
}

// warn — предупреждение не чаще ioWarnEvery на ключ.
func (s *ioService) warn(now time.Time, key, what string, err error) {
	s.warnEvery(now, key, ioWarnEvery, what, err)
}

func (s *ioService) warnEvery(now time.Time, key string, every time.Duration, what string, err error) {
	if now.Before(s.warnAt[key]) {
		return
	}
	s.warnAt[key] = now.Add(every)
	s.a.warn("io", what, "err", err)
}

// housekeep — своя горутина цикла замера: срок снимков и конфиг на ходу; итог — через extra.
func (s *ioService) housekeep(ctx context.Context, gen int, keep time.Duration) {
	reload := time.NewTicker(ioReloadEvery)
	defer reload.Stop()
	cleanAt := time.Time{}
	for {
		now := time.Now()
		if now.After(cleanAt) {
			cleanAt = now.Add(ioCleanupEvery)
			s.postWarn(ctx, now, "cleanup", "удаление старых снимков", iowatch.CleanupSnapshots(s.dir, keep, now))
		}
		select {
		case <-ctx.Done():
			return
		case <-reload.C:
		}
		c, err := s.a.config()
		if err == nil {
			keep = c.IO.Keep.D()
		}
		s.post(ctx, func() { s.reconfigure(gen, c.IO, err) })
	}
}

// postWarn — предупреждение err (nil — нет) из своей горутины в горутину цикла.
func (s *ioService) postWarn(ctx context.Context, now time.Time, key, what string, err error) {
	if err == nil {
		return
	}
	s.post(ctx, func() { s.warn(now, key, what, err) })
}

// post — действие в горутину цикла; цикл остановлен — не нужно.
func (s *ioService) post(ctx context.Context, f func()) {
	select {
	case s.extra <- f:
	case <-ctx.Done():
	}
}

// reconfigure — новый конфиг на ходу: параметры замера — перезапуск цикла, остальное — сразу.
func (s *ioService) reconfigure(gen int, next iowatch.Config, err error) {
	if gen != s.gen || s.rec == nil {
		return // цикл, для которого читали, уже кончился
	}
	if err != nil {
		s.warn(time.Now(), "config", "конфиг не прочитан, работаю по прежнему", err)
		return
	}
	if next.Mode == iowatch.ModeOff || next.Step != s.cfg.Step || next.Detail != s.cfg.Detail || next.Ring != s.cfg.Ring || next.Device != s.cfg.Device {
		s.restarted = true
		s.restart()
		return
	}
	s.cfg = next
	s.rec.SetConfig(next)
}

// closed — строка журнала о событии; простой не меньше analyze_min — в очередь разбора.
func (s *ioService) closed(ev iowatch.Event) {
	line := ioEventLine(ev)
	s.a.log.Info("aguard " + line)
	s.a.note("io", "фриз", ev.Start.Format(time.RFC3339), "", ev.IdleUS/1000, "ms")
	if time.Duration(ev.IdleUS)*time.Microsecond >= s.cfg.AnalyzeMin.D() {
		s.pending = append(s.pending, ev)
	}
}

// dump — ручной снимок буфера по SIGUSR1; файл пишет своя горутина.
func (s *ioService) dump() {
	if s.rec == nil {
		s.a.warn("io", "ручной снимок: замер не идёт (mode = off или перезапуск)")
		return
	}
	snap := s.rec.Dump(time.Now())
	go func() {
		if err := snap.Wait(); err != nil {
			s.a.warn("io", "ручной снимок", "err", err)
			return
		}
		s.a.log.Info("aguard io: снимок", "path", snap.Path)
	}()
}

// launchAnalysis — разбор накопленных событий не чаще analyze_every и не больше одного сразу.
func (s *ioService) launchAnalysis(now time.Time) {
	if s.analyzing || len(s.pending) == 0 || !s.analyzed.IsZero() && now.Sub(s.analyzed) < s.cfg.AnalyzeEvery.D() {
		return
	}
	events := s.pending
	s.pending, s.analyzing, s.analyzed = nil, true, now
	cfg, dir, ctx := s.cfg, s.dir, s.ctx
	go func() {
		var err error
		defer func() {
			if v := recover(); v != nil {
				err = fmt.Errorf("паника разбора io: %v\n%s", v, debug.Stack())
			}
			select {
			case s.extra <- func() { s.analysisDone(err) }:
			case <-ctx.Done():
			}
		}()
		err = s.a.analyzeIO(ctx, cfg, dir, events)
	}()
}

func (s *ioService) analysisDone(err error) {
	s.analyzing = false
	if err != nil {
		s.a.warn("io", "разбор не удался", "err", err)
		s.a.note("io", "analyze_failed", err.Error(), "", 0, "")
	}
}
