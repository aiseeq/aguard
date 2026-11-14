// Package track — слежение за записями в демоне aguard: следит за прямыми потомками /tmp и папок-контейнеров
// (/tmp/claude-tmp) через fanotify, в момент создания выясняет создателя (proc connector, цепочка ppid до сессии Claude
// Code, cgroup) и пишет журнал owners.jsonl и снимок размеров state.json. Дескрипторы, требующие root, даёт
// aguard-track-helper (internal/trackfd). Без root недоступны: cwd root-процессов, размер закрытых папок и исходная папка
// переименования из непомеченной (open_by_handle_at), — такая запись заводится как новая.
package track

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"golang.org/x/sys/unix"

	"github.com/aiseeq/aguard/internal/record"
	"github.com/aiseeq/aguard/internal/trackfd"
)

// Config — настройки одного экземпляра слежения.
type Config struct {
	Name        string        // для журнала
	Roots       []string      // помечаемые папки; их прямые потомки — записи
	Containers  []string      // папки-контейнеры: сами не запись, их потомки — записи
	StateDir    string        // журнал и долговременный снимок (диск)
	RunDir      string        // частый снимок (tmpfs)
	Full        time.Duration // полный замер и сверка с диском
	Young       time.Duration // моложе этого — замер чаще
	YoungEvery  time.Duration // частота замера молодых записей
	Persist     time.Duration // как часто копировать снимок на диск
	RotateBytes int64         // размер owners.jsonl до ротации
	// Yield — уступка занятой машине в замере (internal/yield): зовётся перед каждой записью и папкой-контейнером и
	// каждые yieldEvery записей каталога внутри обхода; true — была пауза, отмена ctx обрывает паузу. nil — без уступки.
	// Зовётся из горутины замера; замеры одного экземпляра идут по одному и при перезапуске цикла (stopMeasure).
	Yield func(context.Context) bool
}

type counters struct {
	events, creates, deletes, renames, overflow int
	procOK, procReaped, procUnknown             int
	sessPpid, sessCgroup, noSession             int
	measurePauses                               int // пауз замера на занятой машине
	measurePaused                               time.Duration
}

// Tracker — состояние сторожа. Все поля меняет только главная горутина.
type Tracker struct {
	cfg        Config
	fd         int      // группа fanotify: fanotify_mark
	fan        *os.File // она же: чтение через опрос среды, Close будит читателя
	done       chan struct{}
	res        *Resolver
	tombs      *Tombs
	owners     *eventLog // owners.jsonl
	log        *slog.Logger
	entries    map[string]*record.Entry
	statMeta   func(string) (meta, error) // подменяется только в тестах восстановления
	watchKey   map[string]string          // ключ handle папки -> путь
	watched    map[string]string          // путь папки -> ключ handle
	containers map[string]bool
	started    time.Time
	lastFull   time.Time
	fullLogged bool
	lastSave   time.Time
	measuring  bool
	dirty      bool // таблица менялась после последнего снимка
	st         counters
}

type measureJob struct {
	start time.Time
	full  bool
	list  bool
	items []usage // путь и inode на входе
	dirs  []string
}

type measureDone struct {
	job     measureJob
	usages  []usage
	listing map[string][]string
	pauses  int           // уступок занятой машине
	paused  time.Duration // их время
	aborted bool          // отменён посреди замера: неполон, не применяется
}

// pollable — дескриптор как файл с опросом: Read просыпается при Close, горутины чтения не остаются.
func pollable(fd int, name string) (*os.File, error) {
	if err := unix.SetNonblock(fd, true); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return os.NewFile(uintptr(fd), name), nil
}

// lockWriter — один писатель снимка и журнала: второй экземпляр затёр бы снимок своей неполной таблицей.
func lockWriter(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, "writer.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("блокировка писателя: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("в %s уже пишет другой экземпляр: %w", dir, err)
	}
	return f, nil
}

// Run работает на паре дескрипторов до отмены ctx и закрывает её. Ошибка — сторож не поднялся или дальше работать не
// может (чтение fanotify, журнал после ротации); вызывающий берёт новую пару.
func Run(ctx context.Context, cfg Config, pair trackfd.Pair, lg *slog.Logger) error {
	fan, err := pollable(pair.Fan, "fanotify")
	if err != nil {
		pair.Close()
		return err
	}
	defer fan.Close()
	conn, err := pollable(pair.Conn, "proc connector")
	if err != nil {
		_ = unix.Close(pair.Conn)
		return err
	}
	defer conn.Close()
	t := &Tracker{
		cfg: cfg, log: lg, entries: map[string]*record.Entry{}, watchKey: map[string]string{},
		watched: map[string]string{}, containers: map[string]bool{}, started: time.Now(),
		fd: pair.Fan, fan: fan, done: make(chan struct{}),
	}
	defer close(t.done)
	for _, c := range cfg.Containers {
		t.containers[c] = true
	}
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return fmt.Errorf("папка состояния: %w", err)
	}
	if err := os.MkdirAll(cfg.RunDir, 0o755); err != nil {
		return fmt.Errorf("папка частого снимка: %w", err)
	}
	lock, err := lockWriter(cfg.RunDir)
	if err != nil {
		return err
	}
	defer lock.Close()
	if t.owners, err = openEventLog(filepath.Join(cfg.StateDir, record.OwnersFile), cfg.RotateBytes, lg); err != nil {
		return err
	}
	defer t.owners.close()
	t.tombs = newTombs(2*time.Minute, lg)
	t.tombs.listen(conn, t.done)
	t.res = newResolver(t.tombs, lg)
	for _, d := range append(append([]string{}, cfg.Roots...), cfg.Containers...) {
		if err := t.mark(d); err != nil && !t.containers[d] {
			return err
		}
	}
	t.restore()
	return t.loop(ctx)
}

// loop — главная горутина: события, замеры, снимки и сводка.
func (t *Tracker) loop(parent context.Context) error {
	evCh := make(chan []fevent, 64)
	readErr := make(chan error, 1)
	go t.readLoop(evCh, readErr)
	doneCh := make(chan measureDone, 1)
	ctx, cancel := context.WithCancel(parent)
	defer t.stopMeasure(cancel, doneCh)
	tick := time.NewTicker(t.cfg.YoungEvery)
	defer tick.Stop()
	hourly := time.NewTicker(time.Hour)
	defer hourly.Stop()
	t.log.Info(fmt.Sprintf("aguard слежение: следим за %v, контейнеры %v, записей при старте %d", t.cfg.Roots, t.cfg.Containers, len(t.entries)))
	t.startInitialMeasure(ctx, doneCh)
	for {
		select {
		case evs := <-evCh:
			for i := range evs {
				t.handle(&evs[i])
			}
		case err := <-readErr:
			return err
		case d := <-doneCh:
			t.finishMeasure(d)
		case <-tick.C:
			t.startMeasure(ctx, doneCh, time.Since(t.lastFull) >= t.cfg.Full)
		case <-hourly.C:
			t.logStats()
		case <-ctx.Done():
			t.log.Info("aguard слежение: остановка, сохраняю снимок", "name", t.cfg.Name)
			t.save(true)
			return nil
		}
		if t.owners.err != nil {
			return t.owners.err
		}
	}
}

// stopMeasure — выход цикла: идущий замер отменяется, и цикл ждёт его горутину. Без этого перезапуск цикла через 30 с
// (trackLoop) шёл бы рядом со старым замером на занятой машине и делил с ним Yield — проверка 10.11: гонка под -race.
func (t *Tracker) stopMeasure(cancel context.CancelFunc, done <-chan measureDone) {
	cancel()
	if t.measuring {
		<-done
		t.measuring = false
	}
}

func (t *Tracker) mark(dir string) error {
	key, err := fanMark(t.fd, dir)
	if err != nil {
		return err
	}
	if old, ok := t.watched[dir]; ok {
		delete(t.watchKey, old)
	}
	t.watched[dir] = key
	t.watchKey[key] = dir
	return nil
}

func (t *Tracker) unmarkGone(dir string) {
	if key, ok := t.watched[dir]; ok {
		delete(t.watchKey, key)
		delete(t.watched, dir)
	}
}

// fanReadBuf — буфер одного read(2) группы fanotify.
const fanReadBuf = 256 << 10

// readLoop читает события fanotify до неустранимой ошибки чтения; её
// отдаёт в errCh, и главный цикл завершает сторож.
func (t *Tracker) readLoop(ch chan<- []fevent, errCh chan<- error) {
	buf := make([]byte, fanReadBuf)
	for {
		evs, err := t.readOnce(buf)
		if err != nil {
			errCh <- err // в канале место на одну ошибку, а отправитель один: запись не ждёт
			return
		}
		select {
		case ch <- evs:
		case <-t.done:
			return
		}
	}
}

// readOnce — один read(2) и разбор буфера; паника в разборе пишется в лог,
// а чтение продолжается.
func (t *Tracker) readOnce(buf []byte) ([]fevent, error) {
	defer recovered(t.log, "разбор событий fanotify")
	n, err := t.fan.Read(buf)
	if err != nil {
		return nil, fmt.Errorf("read fanotify: %w", err)
	}
	return parseEvents(buf[:n]), nil
}

// dirOf — путь помеченной папки события; непомеченная — ошибка: по handle её путь без root не узнать (open_by_handle_at).
func (t *Tracker) dirOf(dn *dirName) (path string, watched bool, err error) {
	if dn == nil {
		return "", false, errors.New("в событии нет папки")
	}
	if p, ok := t.watchKey[dn.key]; ok {
		return p, true, nil
	}
	return "", false, errors.New("папка не помечена")
}

func (t *Tracker) handle(fe *fevent) {
	t.st.events++
	if fe.ovf {
		t.st.overflow++
		t.log.Warn("aguard слежение: переполнение очереди fanotify, полная сверка на следующем тике")
		t.lastFull = time.Time{}
		return
	}
	now := time.Now()
	ownerUsed := false
	owner := func() *record.Owner {
		ownerUsed = true
		o := t.res.Owner(fe.pid, fe.pidfd)
		t.countOwner(o)
		return o
	}
	defer func() {
		if !ownerUsed && fe.pidfd >= 0 {
			unix.Close(fe.pidfd)
		}
	}()
	switch {
	case fe.mask&unix.FAN_RENAME != 0:
		t.handleRename(fe, owner, now)
	case fe.mask&(unix.FAN_CREATE|unix.FAN_DELETE) != 0:
		t.handleCreateDelete(fe, owner, now)
	}
}

// handleRename — переименование: внутри помеченных папок, внутрь, наружу,
// или запись стала контейнером.
func (t *Tracker) handleRename(fe *fevent, owner func() *record.Owner, now time.Time) {
	t.st.renames++
	od, ow, oerr := t.dirOf(fe.old)
	nd, nw, nerr := t.dirOf(fe.new)
	if oerr != nil && nerr != nil {
		t.log.Warn("aguard слежение: переименование без папок", "old", oerr, "new", nerr)
		return
	}
	op, np := "", ""
	if oerr == nil {
		op = filepath.Join(od, fe.old.name)
	}
	if nerr == nil {
		np = filepath.Join(nd, fe.new.name)
	}
	if ow && t.containers[op] {
		t.unmarkGone(op)
	}
	if nw && t.containers[np] {
		if ow {
			t.remove(op, record.EvDelete, np, now) // запись стала контейнером
		}
		t.markLogged(np)
		return
	}
	switch {
	case ow && nw:
		t.rename(op, np, fe.isDir(), owner, now)
	case ow:
		t.remove(op, record.EvDelete, np, now)
	case nw:
		t.create(np, fe.isDir(), owner(), now)
	}
}

// handleCreateDelete — создание и удаление в помеченной папке.
func (t *Tracker) handleCreateDelete(fe *fevent, owner func() *record.Owner, now time.Time) {
	d, w, err := t.dirOf(fe.dn)
	if err != nil {
		t.log.Warn("aguard слежение: событие без папки", "err", err)
		return
	}
	if !w {
		return // событие от папки, с которой метку уже сняли
	}
	p := filepath.Join(d, fe.dn.name)
	if t.containers[p] {
		if fe.mask&unix.FAN_CREATE != 0 && exists(p) {
			t.markLogged(p)
		} else if fe.mask&unix.FAN_DELETE != 0 {
			t.unmarkGone(p)
		}
		return
	}
	if fe.mask&unix.FAN_CREATE != 0 {
		t.st.creates++
		t.create(p, fe.isDir(), owner(), now)
	}
	// create и delete одного имени ядро может слить в одно событие
	if fe.mask&unix.FAN_DELETE != 0 && !exists(p) {
		t.st.deletes++
		t.remove(p, record.EvDelete, "", now)
	}
}

// markLogged помечает появившийся контейнер; неудача — в лог, полная сверка
// попробует снова.
func (t *Tracker) markLogged(dir string) {
	if err := t.mark(dir); err != nil {
		t.log.Warn("aguard слежение: метка контейнера", "err", err)
	}
}

func (t *Tracker) countOwner(o *record.Owner) {
	switch o.Proc.State {
	case record.ProcOK:
		t.st.procOK++
	case record.ProcReaped:
		t.st.procReaped++
	default:
		t.st.procUnknown++
	}
	switch {
	case o.Session == nil:
		t.st.noSession++
	case o.Session.Via == "cgroup":
		t.st.sessCgroup++
	default:
		t.st.sessPpid++
	}
}

func (t *Tracker) create(path string, dir bool, o *record.Owner, now time.Time) {
	if old := t.entries[path]; old != nil {
		t.close(old, record.EvGone, "", now) // пропущенное удаление
	}
	e := &record.Entry{Path: path, Dir: dir, UID: -1, Born: now, Owner: o}
	if m, err := statMeta(path); err != nil { //nolint:silent-error-handling // записи на диске уже нет: uid берём у создателя
		if o != nil {
			e.UID = o.Proc.UID
		}
	} else {
		e.Dir, e.Ino, e.UID = dir || m.dir, m.ino, m.uid
	}
	t.entries[path] = e
	t.dirty = true
	t.owners.write(e.Event(now, record.EvCreate))
}

func (t *Tracker) remove(path, ev, to string, now time.Time) {
	if e := t.entries[path]; e != nil {
		t.close(e, ev, to, now)
	}
}

func (t *Tracker) rename(op, np string, dir bool, owner func() *record.Owner, now time.Time) {
	e := t.entries[op]
	if e == nil {
		t.create(np, dir, owner(), now)
		return
	}
	if tgt := t.entries[np]; tgt != nil && tgt != e {
		t.close(tgt, record.EvDelete, "", now) // перезаписана переименованием
	}
	delete(t.entries, op)
	e.Path = np
	t.entries[np] = e
	t.dirty = true
	ev := e.Event(now, record.EvRename)
	ev.Old = op
	t.owners.write(ev)
}

func (t *Tracker) close(e *record.Entry, ev, to string, now time.Time) {
	line := e.Event(now, ev)
	line.To = to
	t.owners.write(line)
	delete(t.entries, e.Path)
	t.dirty = true
}

// startMeasure запускает du в отдельной горутине, чтобы не задерживать разбор событий.
func (t *Tracker) startMeasure(ctx context.Context, done chan<- measureDone, full bool) {
	t.startMeasureMode(ctx, done, full, full)
}

// startInitialMeasure сверяет список папок после свежего полного замера, не обходя размеры старых записей.
func (t *Tracker) startInitialMeasure(ctx context.Context, done chan<- measureDone) {
	now := time.Now()
	fresh := !t.lastFull.IsZero() && !t.lastFull.After(now) && now.Sub(t.lastFull) < t.cfg.Full
	t.startMeasureMode(ctx, done, !fresh, true)
}

func (t *Tracker) startMeasureMode(ctx context.Context, done chan<- measureDone, full, list bool) {
	if t.measuring {
		return
	}
	now := time.Now()
	job := measureJob{start: now, full: full}
	for _, e := range t.entries {
		// молодые и ещё ни разу не замеренные (найденные сверкой) — каждый тик
		if full || now.Sub(e.Born) < t.cfg.Young || e.MeasuredAt.IsZero() {
			job.items = append(job.items, usage{path: e.Path, ino: e.Ino})
		}
	}
	if list {
		job.list = true
		for d := range t.watched {
			job.dirs = append(job.dirs, d)
		}
		for c := range t.containers {
			if _, ok := t.watched[c]; !ok {
				job.dirs = append(job.dirs, c) // контейнер появился, а событие пропущено
			}
		}
	}
	if len(job.items) == 0 && !full && !list {
		if t.dirty { // замерять нечего, но удаления и создания должны дойти до снимка
			t.save(false)
		}
		return
	}
	t.measuring = true
	go func() { done <- runMeasure(ctx, job, t.cfg.Yield) }()
}

// runMeasure — замер записей и листинг папок с уступкой занятой машине (yield, nil — без неё): полный замер при холодном
// кэше метаданных читал сотни МБ за секунды и давал фризы диска (тикет io-freeze-aguard). Уступают и
// молодые замеры каждого тика: мелкий замер стоит одного сравнения времени на запись и не больше одного замера машины,
// а в нём бывают и большие записи — свежая распаковка, найденная сверкой и ни разу не замеренная. Отмена ctx — итог
// прерван.
func runMeasure(ctx context.Context, job measureJob, yield func(context.Context) bool) measureDone {
	d := measureDone{job: job, listing: map[string][]string{}}
	p := &pacer{ctx: ctx, yield: yield}
	for _, it := range job.items {
		if !p.wait() {
			break
		}
		d.usages = append(d.usages, measure(it.path, it.ino, p))
	}
	for _, dir := range job.dirs {
		if !p.wait() { // отменён — и в первом цикле: дальше не идём
			break
		}
		if names, err := listDir(dir); err == nil {
			d.listing[dir] = names
		}
	}
	d.pauses, d.paused, d.aborted = p.pauses, p.paused, ctx.Err() != nil
	return d
}

func (t *Tracker) finishMeasure(d measureDone) {
	t.measuring = false
	if d.aborted {
		return // остановка демона посреди замера: неполные размеры и листинг не применяем
	}
	now := time.Now()
	t.st.measurePauses += d.pauses
	t.st.measurePaused += d.paused
	// Полный замер — строкой INFO: первый в процессе всегда, следующие — если были паузы. Молодые — каждую минуту:
	// их паузы только в часовой сводке.
	if d.job.full && (!t.fullLogged || d.pauses > 0) {
		var files int64
		for _, u := range d.usages {
			files += u.files
		}
		t.log.Info("aguard слежение: полный замер", "name", t.cfg.Name, "entries", len(d.job.items), "dirs", len(d.job.dirs),
			"files", files, "took", now.Sub(d.job.start).Round(time.Second).String(), "paused", d.paused.String(), "pauses", d.pauses)
		t.fullLogged = true
	}
	for _, u := range d.usages {
		e := t.entries[u.path]
		if e == nil || !u.ok || (e.Ino != 0 && u.ino != e.Ino) {
			continue
		}
		applyUsage(e, u.bytes, u.files, now)
	}
	if d.job.list || d.job.full {
		t.reconcile(d.listing, d.job.start, now)
	}
	if d.job.full {
		t.lastFull = now
	}
	t.save(now.Sub(t.lastSave) >= t.cfg.Persist)
}

// applyUsage обновляет размер и интеграл «размер × время». Между замерами
// размер считаем меняющимся линейно; до первого замера записи, созданной при
// нас, — растущим от нуля, а записи старше сторожа — постоянным.
func applyUsage(e *record.Entry, bytes, files int64, now time.Time) {
	switch {
	case e.MeasuredAt.IsZero() && e.Owner != nil:
		e.ByteSec += float64(bytes) / 2 * now.Sub(e.Born).Seconds()
	case e.MeasuredAt.IsZero():
		e.ByteSec += float64(bytes) * now.Sub(e.Born).Seconds()
	default:
		e.ByteSec += float64(e.Bytes+bytes) / 2 * now.Sub(e.MeasuredAt).Seconds()
	}
	e.Bytes, e.Files, e.MeasuredAt = bytes, files, now
	e.MaxBytes = max(e.MaxBytes, bytes)
	e.MaxFiles = max(e.MaxFiles, files)
}

// reconcile сверяет таблицу с содержимым папок: незнакомое — запись без
// создателя, пропавшее без события — закрыть.
func (t *Tracker) reconcile(listing map[string][]string, scanStart, now time.Time) {
	present := map[string]bool{}
	for dir, names := range listing {
		for _, n := range names {
			p := filepath.Join(dir, n)
			present[p] = true
			t.adopt(p, now)
		}
	}
	for p, e := range t.entries {
		if _, scanned := listing[filepath.Dir(p)]; !scanned || present[p] || !e.Born.Before(scanStart) {
			continue
		}
		if !exists(p) {
			t.close(e, record.EvGone, "", now)
		}
	}
}

// adopt — запись, найденная сверкой: контейнер пометить, незнакомую завести без создателя.
func (t *Tracker) adopt(p string, now time.Time) {
	if t.containers[p] {
		if _, ok := t.watched[p]; ok {
			return
		}
		t.markLogged(p)
		return
	}
	if t.entries[p] != nil {
		return
	}
	m, err := statMeta(p)
	if err != nil { //nolint:silent-error-handling // записи на диске уже нет, хотя листинг её видел: заводить нечего
		return
	}
	e := &record.Entry{Path: p, Dir: m.dir, Ino: m.ino, UID: m.uid, Born: m.born}
	t.entries[p] = e
	t.dirty = true
	t.owners.write(e.Event(now, record.EvPreexisting))
}

// restoreMatches использует inode без btime; с btime запись новая, только если
// рождение на диске позже времени события из снимка больше чем на секунду.
func restoreMatches(e record.Entry, m meta) bool {
	return (e.Ino == 0 || m.ino == e.Ino) && (!m.hasBorn || !m.born.After(e.Born.Add(time.Second)))
}

// restore поднимает таблицу из снимка: свежий из /run, иначе с диска.
// Записи, которых больше нет (или inode/btime другой), закрываются как gone.
func (t *Tracker) restore() {
	var st *record.State
	for _, dir := range []string{t.cfg.RunDir, t.cfg.StateDir} {
		s, err := record.ReadState(filepath.Join(dir, record.StateFile))
		if err != nil {
			t.log.Warn("aguard слежение: снимок не прочитан", "dir", dir, "err", err)
			continue
		}
		for _, p := range s.NoBorn {
			t.log.Warn("aguard слежение: запись снимка без born не восстановлена", "dir", dir, "path", p)
		}
		if st == nil || s.Updated.After(st.Updated) {
			st = s
		}
	}
	if st == nil {
		return
	}
	t.lastFull = st.LastFull
	now := time.Now()
	for i := range st.Entries {
		e := st.Entries[i]
		stat := t.statMeta
		if stat == nil {
			stat = statMeta
		}
		m, err := stat(e.Path)
		if err == nil && restoreMatches(e, m) {
			t.entries[e.Path] = &e
			continue
		}
		t.close(&e, record.EvGone, "", now)
		if err == nil && m.hasBorn && (e.Ino == 0 || m.ino == e.Ino) {
			t.adopt(e.Path, now)
		}
	}
}

func (t *Tracker) save(persist bool) {
	now := time.Now()
	s := record.State{Updated: now, Started: t.started, LastFull: t.lastFull, Roots: t.cfg.Roots, Containers: t.cfg.Containers}
	es := make([]*record.Entry, 0, len(t.entries))
	for _, e := range t.entries {
		es = append(es, e)
	}
	sort.Slice(es, func(i, j int) bool { return es[i].Path < es[j].Path })
	t.dirty = false
	fill := func(w io.Writer) error { return writeState(w, &s, es) }
	if err := record.WriteFileAtomicFunc(filepath.Join(t.cfg.RunDir, record.StateFile), 0o644, fill); err != nil {
		t.log.Error("aguard слежение: снимок", "err", err)
	}
	if persist {
		if err := record.WriteFileAtomicFunc(filepath.Join(t.cfg.StateDir, record.StateFile), 0o644, fill); err != nil {
			t.log.Error("aguard слежение: снимок на диск", "err", err)
		}
		t.lastSave = now
	}
}

func (t *Tracker) logStats() {
	s := t.st
	tombs, open := t.tombs.size()
	t.log.Info(fmt.Sprintf("aguard слежение: за час событий %d (создано %d, удалено %d, переименовано %d, переполнений %d); "+
		"создатель: жив %d, восстановлен %d, неизвестен %d; сессия по ppid %d, по cgroup %d, нет %d; "+
		"потерь proc connector %d; записей %d; надгробий %d (без выхода %d); пауз замера на занятой машине %d (%v)",
		s.events, s.creates, s.deletes, s.renames, s.overflow, s.procOK, s.procReaped, s.procUnknown,
		s.sessPpid, s.sessCgroup, s.noSession, t.tombs.overrun.Load(), len(t.entries), tombs, open,
		s.measurePauses, s.measurePaused.Round(time.Second)))
	t.st = counters{}
}

// writeState пишет снимок в формате record.State (поле Entries — из es)
// потоком по записи: json.Marshal всего снимка выделял ~70 МБ на 30 тыс.
// записей при живой куче 22 МБ и раздувал службу до MemoryMax=256M со
// свопом (тикет tmpguard-track-swap). Ключи — как теги record.State;
// совпадение формата держит TestWriteStateMatchesMarshal.
func writeState(w io.Writer, s *record.State, es []*record.Entry) error {
	enc := json.NewEncoder(w)
	put := func(lit string, v any) error {
		if _, err := io.WriteString(w, lit); err != nil {
			return err
		}
		return enc.Encode(v)
	}
	for _, f := range []struct {
		lit string
		v   any
	}{{`{"updated":`, s.Updated}, {`,"started":`, s.Started}} {
		if err := put(f.lit, f.v); err != nil {
			return err
		}
	}
	if !s.LastFull.IsZero() {
		if err := put(`,"last_full":`, s.LastFull); err != nil {
			return err
		}
	}
	for _, f := range []struct {
		lit string
		v   any
	}{{`,"roots":`, s.Roots}, {`,"containers":`, s.Containers}} {
		if err := put(f.lit, f.v); err != nil {
			return err
		}
	}
	sep := `,"entries":[`
	for _, e := range es {
		if err := put(sep, e); err != nil {
			return err
		}
		sep = ","
	}
	if len(es) == 0 {
		sep += "]" // пустой снимок: `"entries":[]`, как у json.Marshal
	} else {
		sep = "]"
	}
	_, err := io.WriteString(w, sep+"}")
	return err
}
