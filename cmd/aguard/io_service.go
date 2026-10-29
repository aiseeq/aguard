package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"time"

	"github.com/aiseeq/aguard/internal/activity"
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
	ioCleanupEvery  = time.Minute      // старые ручные снимки удаляются
	ioSaveEvery     = time.Hour        // сводки случаев — копией в файл: на случай падения без остановки
	ioWarnEvery     = time.Minute      // одно и то же предупреждение — не чаще
	ioPassEvery     = time.Minute      // цикл 100 мс — не safePass: проход в учёт отмечает сам, не чаще
	ioNoDeviceEvery = 24 * time.Hour   // нет диска из device — напоминание раз в сутки
)

// errIORestart — изменились параметры замера (mode, step, detail, ring, device): цикл строится заново.
var errIORestart = errors.New("io: параметры замера изменились")

// ioService — наблюдатель io в демоне: сводки случаев фриза в памяти, их копия в файл, письма о
// длинных случаях, ручной снимок буфера. Всё состояние — у горутины ioLoop; конфиг, чистка, запись
// файлов и письма идут в своих горутинах и возвращаются через extra: быстрый цикл не ждёт ни диска,
// ни сокетов.
type ioService struct {
	a         *app
	ctx       context.Context
	dir       string // ручные снимки
	path      string // сводки случаев
	cfg       iowatch.Config
	rec       *iowatch.Recorder
	extra     chan func()
	restart   context.CancelFunc
	restarted bool
	gen       int // номер цикла замера: итог фоновой работы прошлого цикла не применяется
	warnAt    map[string]time.Time
	passAt    time.Time // следующая отметка прохода в учёте срабатываний
	cases     []iowatch.Case
	alerts    []iowatch.Case // случаи от alert_min, ждут письма
	alerting  bool
	alerted   time.Time
}

func newIOService(ctx context.Context, a *app) *ioService {
	return &ioService{a: a, ctx: ctx, dir: filepath.Join(a.dir("io"), "events"), path: filepath.Join(a.dir("io"), ioCasesFile),
		extra: make(chan func(), 16), warnAt: map[string]time.Time{}}
}

// ioCasesFile — копия сводок случаев в папке состояния io.
const ioCasesFile = "cases.jsonl"

// ioLoop — наблюдатель io: mode=off — не замеряет совсем; паника или ошибка цикла — warn и перезапуск.
// usr1 — SIGUSR1, сводки в файл (`aguard io report`), usr2 — SIGUSR2, ручной снимок буфера
// (`aguard io dump`); подписка — в начале run демона, чтобы сигнал до старта наблюдателя не убил
// процесс. При остановке сводки пишутся в файл.
func (a *app) ioLoop(ctx context.Context, usr1, usr2 <-chan os.Signal) {
	s := newIOService(ctx, a)
	s.load()
	go func() {
		for {
			var f func()
			select {
			case <-ctx.Done():
				return
			case <-usr1:
				f = s.save
			case <-usr2:
				f = s.dump
			}
			select {
			case s.extra <- f:
			default: // очередь полна: запрос уже в пути
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
	if err := iowatch.SaveCases(s.path, s.kept(time.Now())); err != nil {
		a.warn("io", "сводки случаев при остановке", "err", err)
	}
}

// load — сводки прошлых запусков; файл не читается — он сохраняется рядом с пометкой, история
// начинается заново (иначе следующая запись затёрла бы его).
func (s *ioService) load() {
	cases, err := iowatch.LoadCases(s.path)
	if err != nil {
		broken := s.path + ".broken-" + time.Now().Format("20060102T150405")
		s.a.warn("io", "сводки случаев не прочитаны, файл отложен", "err", err, "path", broken)
		if err := os.Rename(s.path, broken); err != nil {
			s.a.warn("io", "отложить файл сводок", "err", err)
		}
		return
	}
	s.cases = cases
}

// kept — сводки в сроке cases_keep (по прошлому конфигу, если замер не шёл — по умолчанию).
func (s *ioService) kept(now time.Time) []iowatch.Case {
	keep := s.cfg.CasesKeep.D()
	if keep <= 0 {
		keep = iowatch.Default().CasesKeep.D()
	}
	s.cases = iowatch.MergeCases(s.cases, nil, now.Add(-keep))
	return s.cases
}

// save — копия сводок в файл своей горутиной; запрос `aguard io report` ждёт обновления файла.
func (s *ioService) save() {
	cases := slices.Clone(s.kept(time.Now()))
	go func() {
		s.postWarn(s.ctx, time.Now(), "save", "сводки случаев", iowatch.SaveCases(s.path, cases))
	}()
}

// wait — пауза, в которой исполняются действия extra (запросы сигналов, итог письма).
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

// run — один цикл замера до остановки, перезапуска или паники; идущий случай закрывается.
func (s *ioService) run(cfg iowatch.Config) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("паника в цикле io: %v\n%s", v, debug.Stack())
		}
		if s.rec == nil {
			return
		}
		closed := s.rec.Close()
		for i := range closed {
			s.closed(closed[i])
		}
		s.rec = nil
	}()
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	s.gen++
	s.cfg, s.restart, s.restarted = cfg, cancel, false
	s.rec = iowatch.NewRecorder(cfg)
	go s.housekeep(ctx, s.gen, cfg.Keep.D())
	err = runIOSampling(ctx, cfg, iowatch.SystemRoots(), s, s.extra)
	if s.restarted {
		return errIORestart
	}
	return err
}

func (s *ioService) step(st iowatch.Step) {
	if !st.At.Before(s.passAt) {
		s.a.note("io", activity.KindPass, "", "", 0, "")
		s.passAt = st.At.Add(ioPassEvery)
	}
	closed := s.rec.Step(st)
	for i := range closed {
		s.closed(closed[i])
	}
	s.launchAlert(st.At)
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

// housekeep — своя горутина цикла замера: срок ручных снимков, копия сводок раз в час и конфиг на
// ходу; итог — через extra.
func (s *ioService) housekeep(ctx context.Context, gen int, keep time.Duration) {
	reload := time.NewTicker(ioReloadEvery)
	defer reload.Stop()
	cleanAt, saveAt := time.Time{}, time.Now().Add(ioSaveEvery)
	for {
		now := time.Now()
		if now.After(cleanAt) {
			cleanAt = now.Add(ioCleanupEvery)
			s.postWarn(ctx, now, "cleanup", "удаление старых снимков", iowatch.CleanupSnapshots(s.dir, keep, now))
		}
		if now.After(saveAt) {
			saveAt = now.Add(ioSaveEvery)
			s.post(ctx, s.save)
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

// closed — случай в сводки; от case_min — строка журнала, от alert_min — в очередь письма.
func (s *ioService) closed(c iowatch.Case) {
	s.cases = append(s.cases, c)
	if c.Dur() < s.cfg.CaseMin.D() {
		return
	}
	s.a.log.Info("aguard io: " + ioCaseLine(&c))
	s.a.note("io", "фриз", c.Start.Format(time.RFC3339), "", c.DurMS, "ms")
	if c.Dur() >= s.cfg.AlertMin.D() && !c.Quiet {
		s.alerts = append(s.alerts, c)
	}
}

// dump — ручной снимок буфера по SIGUSR2; файл пишет своя горутина.
func (s *ioService) dump() {
	if s.rec == nil {
		s.a.warn("io", "ручной снимок: замер не идёт (mode = off или перезапуск)")
		return
	}
	snap := s.rec.Dump(s.dir, time.Now())
	go func() {
		if err := snap.Wait(); err != nil {
			s.a.warn("io", "ручной снимок", "err", err)
			return
		}
		s.a.log.Info("aguard io: снимок", "path", snap.Path)
	}()
}

// launchAlert — письмо о накопленных длинных случаях не чаще alert_every и не больше одного сразу:
// первый случай после паузы — сразу, следующие в паузе склеиваются в одно письмо.
func (s *ioService) launchAlert(now time.Time) {
	if s.alerting || len(s.alerts) == 0 || !s.alerted.IsZero() && now.Sub(s.alerted) < s.cfg.AlertEvery.D() {
		return
	}
	cases := s.alerts
	s.alerts, s.alerting, s.alerted = nil, true, now
	cfg, ctx := s.cfg, s.ctx
	go func() {
		var err error
		defer func() {
			if v := recover(); v != nil {
				err = fmt.Errorf("паника письма io: %v\n%s", v, debug.Stack())
			}
			select {
			case s.extra <- func() { s.alertDone(err) }:
			case <-ctx.Done():
			}
		}()
		err = s.a.alertIO(cfg, cases)
	}()
}

func (s *ioService) alertDone(err error) {
	s.alerting = false
	if err != nil {
		s.a.warn("io", "письмо о фризе не ушло", "err", err)
		s.a.note("io", "alert_failed", err.Error(), "", 0, "")
	}
}
