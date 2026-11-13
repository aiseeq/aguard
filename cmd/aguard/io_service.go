package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"github.com/aiseeq/aguard/internal/activity"
	"github.com/aiseeq/aguard/internal/ioroute"
	"github.com/aiseeq/aguard/internal/iowatch"
	"github.com/aiseeq/aguard/internal/record"
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
		case <-ticker.C:
			// Время шага — момент чтения: буфер тикера после задержки цикла отдаёт старый тик, а PSI уже за всю задержку.
			s, err := fast.Read(time.Now())
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
		case <-ticker.C:
			// Детальный проход тоже снимает дельты, поэтому At должен быть временем чтения.
			res := readIODetail(r, time.Now())
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
	ioCgroupsEvery  = time.Minute      // кончились ли cgroup профилей (папки нет) — не в цикле 100 мс
	ioWorkerEvery   = 24 * time.Hour   // вид cgroup неполон (класс бойца, вид задачи graft) — напоминание раз в сутки
)

// kindUnknownWhat — предупреждение о неполном виде профиля по источнику (iowatch.KindUnknownError.Source).
var kindUnknownWhat = map[string]string{
	iowatch.KindSourceWorker: "класс бойца не найден, вид " + iowatch.KindWorkerUnknown,
	iowatch.KindSourceGraft:  "вид задачи graft для scope graft-heavy не найден, вид service:<scope>",
}

// errIORestart — изменились параметры замера (mode, step, detail, ring, device): цикл строится заново.
var errIORestart = errors.New("io: параметры замера изменились")

// ioService — наблюдатель io в демоне: сводки случаев фриза в памяти, их копия в файл, письма о
// длинных случаях, ручной снимок буфера. Всё состояние — у горутины ioLoop; конфиг, чистка, запись
// файлов и письма идут в своих горутинах и возвращаются через extra: быстрый цикл не ждёт ни диска,
// ни сокетов.
type ioService struct {
	a            *app
	ctx          context.Context
	dir          string // ручные снимки
	path         string // сводки случаев
	coverPath    string // учёт покрытия замера
	seriesPath   string
	profilesPath string
	cgroupRoot   string              // /sys/fs/cgroup: конец cgroup профиля
	kindSources  iowatch.KindSources // класс бойца из job.json, вид задачи graft по scope
	cfg          iowatch.Config
	// Ряд, накопление профилей и живое состояние — дольше цикла замера: каждый Recorder получает их
	// через Attach. live читают и соединения сокета (под своим мьютексом).
	series    *iowatch.Series
	profiles  *iowatch.Profiles
	live      *iowatch.Live
	ended     []iowatch.Profile // профили кончившихся cgroup в сроке profiles_keep
	rec       *iowatch.Recorder
	extra     chan func()
	restart   context.CancelFunc
	restarted bool
	gen       int // номер цикла замера: итог фоновой работы прошлого цикла не применяется
	warnAt    map[string]time.Time
	passAt    time.Time // следующая отметка прохода в учёте срабатываний
	cases     []iowatch.Case
	cover     *iowatch.Coverer // учёт покрытия замера: шаги продлевают интервал, перезапуск цикла — новый
	offSaveAt time.Time        // mode=off: следующая копия учёта (housekeep не идёт)
	// Маршрут случаев от alert_min (тикет alert-async): письма хозяину или лиду, тикеты — горутиной tickets.
	route   *ioroute.Router
	owners  *ioOwnerFinder
	tickets *ioTicketer
	letters sync.WaitGroup // горутины писем: тест дожидается их, прежде чем читать журнал
	tickAt  time.Time
}

func newIOService(ctx context.Context, a *app) *ioService {
	state := a.dir("io")
	def := iowatch.Default()
	return &ioService{route: ioroute.New(def.Route()), owners: a.ioOwners(&def), a: a, ctx: ctx, dir: filepath.Join(state, "events"), path: filepath.Join(state, ioCasesFile),
		coverPath: filepath.Join(state, ioCoverageFile), cover: iowatch.NewCoverer(iowatch.CoverRecord{}, time.Now()),
		seriesPath: filepath.Join(state, ioSeriesFile), profilesPath: filepath.Join(state, ioProfilesFile),
		cgroupRoot: iowatch.SystemRoots().Cgroup, kindSources: iowatch.KindSources{WorkerState: filepath.Join(a.home, ".local", "state", "worker"), GraftHeavy: iowatch.GraftHeavyDir(record.RuntimeDir())},
		series: iowatch.NewSeries(iowatch.Default().SeriesKeep.D()), profiles: iowatch.NewProfiles(), live: iowatch.NewLive(),
		extra: make(chan func(), 16), warnAt: map[string]time.Time{},
		tickets: &ioTicketer{a: a, queue: make(chan ioTicketJob, 64)}}
}

// Копии в папке состояния io: сводки случаев, посекундный ряд машины, профили cgroup.
const (
	ioCasesFile    = "cases.jsonl"
	ioSeriesFile   = "series.jsonl"
	ioProfilesFile = "profiles.jsonl"
	ioCoverageFile = "coverage.json" // учёт покрытия замера: рядом со сводками, срок — cases_keep
)

// ioLoop — наблюдатель io: mode=off — не замеряет совсем; паника или ошибка цикла — warn и перезапуск.
// usr2 — SIGUSR2, ручной снимок буфера (`aguard io dump`); подписка — в начале run демона,
// чтобы сигнал до старта наблюдателя не убил процесс. При остановке сводки пишутся в файл.
func (a *app) ioLoop(ctx context.Context, usr2 <-chan os.Signal) {
	s := newIOService(ctx, a)
	s.load()
	go s.serve()
	go s.tickets.run(ctx)
	go func() {
		for {
			var f func()
			select {
			case <-ctx.Done():
				return
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
			s.offPass(time.Now())
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
	s.saveStop(time.Now())
}

// offPass — проход цикла в mode=off: учёт пишет off (пробел mode_off, а не not_running); копия учёта раз в
// ioSaveEvery — housekeep в off не идёт.
func (s *ioService) offPass(now time.Time) {
	s.cover.Mark(now, iowatch.ModeOff)
	if now.Before(s.offSaveAt) {
		return
	}
	s.offSaveAt = now.Add(ioSaveEvery)
	rec := s.keptCover(now)
	go func() {
		s.postWarn(s.ctx, now, "save-coverage", "учёт покрытия", iowatch.SaveCoverage(s.coverPath, rec))
	}()
}

// saveStop — копии сводок, профилей, ряда и учёта покрытия при остановке, в горутине цикла.
func (s *ioService) saveStop(now time.Time) {
	if err := iowatch.SaveCases(s.path, s.kept(now)); err != nil {
		s.a.warn("io", "сводки случаев при остановке", "err", err)
	}
	if err := iowatch.SaveCoverage(s.coverPath, s.keptCover(now)); err != nil {
		s.a.warn("io", "учёт покрытия при остановке", "err", err)
	}
	if err := iowatch.SaveProfiles(s.profilesPath, s.keptProfiles(now)); err != nil {
		s.a.warn("io", "профили cgroup при остановке", "err", err)
	}
	if err := iowatch.SaveSeries(s.seriesPath, s.series.Rows(time.Time{}).Seconds()); err != nil {
		s.a.warn("io", "ряд машины при остановке", "err", err)
	}
}

// load — сводки, ряд и профили прошлых запусков в сроке из конфига (не прочитался — по умолчанию).
func (s *ioService) load() {
	cfg := iowatch.Default()
	if c, err := s.a.config(); err == nil {
		cfg = c.IO
	}
	now := time.Now()
	s.cases = loadIOFile(s, s.path, "сводки случаев", iowatch.LoadCases)
	seconds := loadIOFile(s, s.seriesPath, "ряд машины", func(path string) ([]iowatch.MachineSecond, error) {
		return iowatch.LoadSeries(path, now.Add(-cfg.SeriesKeep.D()))
	})
	s.series = iowatch.NewSeries(cfg.SeriesKeep.D())
	for i := range seconds {
		s.series.Add(seconds[i])
	}
	s.loadCover(seconds, now)
	s.ended = loadIOFile(s, s.profilesPath, "профили cgroup", func(path string) ([]iowatch.Profile, error) {
		return iowatch.LoadProfiles(path, now.Add(-cfg.ProfilesKeep.D()))
	})
	s.seedRoute(cfg, now)
}

// loadCover — учёт покрытия прошлых запусков; файла нет — засев из посекундного ряда (до учёта замер шёл там,
// где есть секунды ряда); файл битый — отложен, учёт с now (раньше — no_record, а не пустое покрытие).
func (s *ioService) loadCover(seconds []iowatch.MachineSecond, now time.Time) {
	rec, err := iowatch.LoadCoverage(s.coverPath)
	switch {
	case err != nil:
		broken := s.coverPath + ".broken-" + now.Format("20060102T150405")
		s.a.warn("io", "учёт покрытия не прочитан, файл отложен", "err", err, "path", broken)
		if err := os.Rename(s.coverPath, broken); err != nil {
			s.a.warn("io", "отложить файл: учёт покрытия", "err", err)
		}
		rec = iowatch.CoverRecord{}
	case rec.Start.IsZero():
		rec = iowatch.SeedCoverage(seconds)
	}
	s.cover = iowatch.NewCoverer(rec, now)
}

// loadIOFile — копия прошлого запуска; файл не читается — он сохраняется рядом с пометкой, история
// начинается заново (иначе следующая запись затёрла бы его).
func loadIOFile[T any](s *ioService, path, what string, load func(string) ([]T, error)) []T {
	items, err := load(path)
	if err == nil {
		return items
	}
	broken := path + ".broken-" + time.Now().Format("20060102T150405")
	s.a.warn("io", what+" не прочитаны, файл отложен", "err", err, "path", broken)
	if err := os.Rename(path, broken); err != nil {
		s.a.warn("io", "отложить файл: "+what, "err", err)
	}
	return nil
}

// keepCfg — сроки по текущему циклу; замер не шёл — по умолчанию.
func (s *ioService) keepCfg() iowatch.Config {
	if s.cfg.CasesKeep.D() <= 0 {
		return iowatch.Default()
	}
	return s.cfg
}

// kept — сводки в сроке cases_keep.
func (s *ioService) kept(now time.Time) []iowatch.Case {
	s.cases = iowatch.MergeCases(s.cases, nil, now.Add(-s.keepCfg().CasesKeep.D()))
	return s.cases
}

// keptCover — учёт в сроке cases_keep на now.
func (s *ioService) keptCover(now time.Time) iowatch.CoverRecord {
	s.cover.Trim(now.Add(-s.keepCfg().CasesKeep.D()))
	return s.cover.Record(now)
}

// keptProfiles — профили в сроке profiles_keep.
func (s *ioService) keptProfiles(now time.Time) []iowatch.Profile {
	since := now.Add(-s.keepCfg().ProfilesKeep.D())
	if len(s.ended) > 0 && s.ended[0].End.Before(since) {
		s.ended = iowatch.FilterProfiles(s.ended, "", since)
	}
	return s.ended
}

// save — часовая копия сводок, профилей и учёта покрытия в файлы своей горутиной.
func (s *ioService) save() {
	now := time.Now()
	cases, profiles, cover := slices.Clone(s.kept(now)), slices.Clone(s.keptProfiles(now)), s.keptCover(now)
	go func() {
		s.postWarn(s.ctx, time.Now(), "save", "сводки случаев", iowatch.SaveCases(s.path, cases))
		s.postWarn(s.ctx, time.Now(), "save-coverage", "учёт покрытия", iowatch.SaveCoverage(s.coverPath, cover))
		s.postWarn(s.ctx, time.Now(), "save-profiles", "профили cgroup", iowatch.SaveProfiles(s.profilesPath, profiles))
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

// run — один цикл замера до остановки, перезапуска или паники; идущий случай закрывается, интервал учёта
// покрытия — тоже (следующий цикл начнёт новый).
func (s *ioService) run(cfg iowatch.Config) (err error) {
	defer func() {
		s.cover.Break()
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
	s.setRoute(cfg)
	s.rec = iowatch.NewRecorder(cfg)
	s.resizeSeries(cfg)
	s.rec.Attach(s.series, s.profiles, s.live)
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
	s.cover.Mark(st.At, s.cfg.Mode)
	closed := s.rec.Step(st)
	for i := range closed {
		s.closed(closed[i])
	}
	if !st.At.Before(s.tickAt) {
		s.tickAt = st.At.Add(time.Second)
		s.act(s.route.Tick(st.At))
	}
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
	cleanAt, saveAt, cgroupsAt := time.Time{}, time.Now().Add(ioSaveEvery), time.Now().Add(ioCgroupsEvery)
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
		if now.After(cgroupsAt) {
			cgroupsAt = now.Add(ioCgroupsEvery)
			s.checkCgroups(ctx, now)
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
	s.setRoute(next)
	s.rec.SetConfig(next)
	s.resizeSeries(next)
	s.rec.Attach(s.series, s.profiles, s.live)
}

// resizeSeries — ряд под series_keep из конфига: строки, которые влезают, сохраняются.
func (s *ioService) resizeSeries(cfg iowatch.Config) {
	if s.series.Keep() != cfg.SeriesKeep.D() {
		s.series = s.series.Resize(cfg.SeriesKeep.D())
	}
}

// checkCgroups — из горутины housekeep: какие cgroup профилей кончились (папки нет) и их вид;
// закрывает их горутина цикла (endProfiles). Пути берутся у неё же.
func (s *ioService) checkCgroups(ctx context.Context, now time.Time) {
	paths := make(chan []string, 1)
	s.post(ctx, func() { paths <- s.profiles.Open() })
	var open []string
	select {
	case open = <-paths:
	case <-ctx.Done():
		return
	}
	gone := map[string]string{}
	for _, path := range open {
		ok, err := iowatch.CgroupGone(s.cgroupRoot, path)
		if err != nil {
			s.postWarn(ctx, now, "cgroup-gone", "проверка конца cgroup", err)
			continue
		}
		if !ok {
			continue
		}
		gone[path] = iowatch.ProfileKind(path, s.kindSources, func(e *iowatch.KindUnknownError) { // вид годен, но неполон: предупреждение — раз в сутки на источник
			s.post(ctx, func() { s.warnEvery(now, e.Source, ioWorkerEvery, kindUnknownWhat[e.Source], e.Err) })
		})
	}
	if len(gone) > 0 {
		s.post(ctx, func() { s.endProfiles(gone, now) })
	}
}

// endProfiles — профили кончившихся cgroup в память; свод по видам для now обновляется.
func (s *ioService) endProfiles(gone map[string]string, now time.Time) {
	ended := s.profiles.End(gone, s.keepCfg().Detail.D())
	if len(ended) == 0 {
		return
	}
	s.ended = append(s.ended, ended...)
	iowatch.SortProfiles(s.ended)
	s.keptProfiles(now)
}

// closed — случай в сводки; от case_min — строка журнала, от alert_min — в маршрут (письмо или тикет).
func (s *ioService) closed(c iowatch.Case) {
	s.cases = append(s.cases, c)
	if c.Dur() < s.cfg.CaseMin.D() {
		return
	}
	s.a.log.Info("aguard io: " + ioCaseLine(&c))
	kind := "фриз"
	switch {
	case c.Quiet:
		kind = "тихий"
	case c.Capped:
		kind = "под потолком"
	}
	s.a.note("io", kind, c.Start.Format(time.RFC3339), "", c.DurMS, "ms")
	if alertable(&c, s.cfg) {
		s.routeOne(&s.cases[len(s.cases)-1])
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
