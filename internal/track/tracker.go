// Package track — root-часть tmpguard: следит за прямыми потомками /tmp и
// папок-контейнеров (/tmp/claude-tmp) через fanotify, в момент создания
// выясняет создателя (proc connector, цепочка ppid до сессии Claude Code,
// cgroup) и пишет журнал owners.jsonl и снимок размеров state.json.
package track

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/aiseeq/aguard/internal/record"
)

// Config — настройки сторожа (флаги system-сервиса).
type Config struct {
	Roots       []string      // помечаемые папки; их прямые потомки — записи
	Containers  []string      // папки-контейнеры: сами не запись, их потомки — записи
	Mount       string        // папка файловой системы для open_by_handle_at; пусто — первая из Roots
	StateDir    string        // журнал и долговременный снимок (диск)
	RunDir      string        // частый снимок (tmpfs /run)
	Full        time.Duration // полный замер и сверка с диском
	Young       time.Duration // моложе этого — замер чаще
	YoungEvery  time.Duration // частота замера молодых записей
	Persist     time.Duration // как часто копировать снимок на диск
	RotateBytes int64         // размер owners.jsonl до ротации
}

type counters struct {
	events, creates, deletes, renames, overflow int
	procOK, procReaped, procUnknown             int
	sessPpid, sessCgroup, noSession             int
}

// Tracker — состояние сторожа. Все поля меняет только главная горутина.
type Tracker struct {
	cfg        Config
	fd         int
	mountFD    int
	res        *Resolver
	tombs      *Tombs
	owners     *eventLog // owners.jsonl
	log        *slog.Logger
	entries    map[string]*record.Entry
	watchKey   map[string]string // ключ handle папки -> путь
	watched    map[string]string // путь папки -> ключ handle
	containers map[string]bool
	started    time.Time
	lastFull   time.Time
	lastSave   time.Time
	measuring  bool
	dirty      bool // таблица менялась после последнего снимка
	st         counters
}

type measureJob struct {
	start time.Time
	full  bool
	items []usage // путь и inode на входе
	dirs  []string
}

type measureDone struct {
	job     measureJob
	usages  []usage
	listing map[string][]string
}

// Run запускает сторож и работает до SIGTERM/SIGINT. Ошибка — сторож не
// поднялся или дальше работать не может (чтение fanotify, журнал после ротации).
func Run(cfg Config, lg *slog.Logger) error {
	t := &Tracker{
		cfg: cfg, log: lg, entries: map[string]*record.Entry{}, watchKey: map[string]string{},
		watched: map[string]string{}, containers: map[string]bool{}, started: time.Now(),
	}
	for _, c := range cfg.Containers {
		t.containers[c] = true
	}
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return fmt.Errorf("папка состояния: %w", err)
	}
	if err := os.MkdirAll(cfg.RunDir, 0o755); err != nil {
		return fmt.Errorf("папка частого снимка: %w", err)
	}
	var err error
	if t.owners, err = openEventLog(filepath.Join(cfg.StateDir, record.OwnersFile), cfg.RotateBytes, lg); err != nil {
		return err
	}
	defer t.owners.close()
	t.tombs = newTombs(2*time.Minute, lg)
	if err := t.tombs.listen(); err != nil {
		return err
	}
	t.res = newResolver(t.tombs, lg)
	if t.fd, err = fanInit(); err != nil {
		return fmt.Errorf("fanotify_init: %w", err)
	}
	mount := cfg.Mount
	if mount == "" {
		mount = cfg.Roots[0]
	}
	if t.mountFD, err = unix.Open(mount, unix.O_RDONLY|unix.O_DIRECTORY, 0); err != nil {
		return fmt.Errorf("open %s: %w", mount, err)
	}
	for _, d := range append(append([]string{}, cfg.Roots...), cfg.Containers...) {
		if err := t.mark(d); err != nil && !t.containers[d] {
			return err
		}
	}
	t.restore()
	return t.loop()
}

// loop — главная горутина: события, замеры, снимки и сводка.
func (t *Tracker) loop() error {
	evCh := make(chan []fevent, 64)
	readErr := make(chan error, 1)
	go t.readLoop(evCh, readErr)
	doneCh := make(chan measureDone, 1)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	tick := time.NewTicker(t.cfg.YoungEvery)
	defer tick.Stop()
	hourly := time.NewTicker(time.Hour)
	defer hourly.Stop()
	t.log.Info(fmt.Sprintf("tmpguard-track: следим за %v, контейнеры %v, записей при старте %d", t.cfg.Roots, t.cfg.Containers, len(t.entries)))
	t.startMeasure(doneCh, true)
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
			t.startMeasure(doneCh, time.Since(t.lastFull) >= t.cfg.Full)
		case <-hourly.C:
			t.logStats()
		case s := <-sig:
			t.log.Info(fmt.Sprintf("tmpguard-track: %v, сохраняю снимок", s))
			t.save(true)
			return nil
		}
		if t.owners.err != nil {
			return t.owners.err
		}
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
			errCh <- err
			return
		}
		ch <- evs
	}
}

// readOnce — один read(2) и разбор буфера; паника в разборе пишется в лог,
// а чтение продолжается.
func (t *Tracker) readOnce(buf []byte) ([]fevent, error) {
	defer recovered(t.log, "разбор событий fanotify")
	n, err := unix.Read(t.fd, buf)
	switch {
	case err == nil:
		return parseEvents(buf[:n]), nil
	case errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN):
		return nil, nil
	default:
		return nil, fmt.Errorf("read fanotify: %w", err)
	}
}

// dirOf — путь папки события: помеченная папка или (для переименований
// из чужих папок) разрешённый handle. watched=false для непомеченной.
func (t *Tracker) dirOf(dn *dirName) (path string, watched bool, err error) {
	if dn == nil {
		return "", false, errors.New("в событии нет папки")
	}
	if p, ok := t.watchKey[dn.key]; ok {
		return p, true, nil
	}
	p, err := resolveHandle(t.mountFD, dn.handle)
	if err != nil {
		return "", false, fmt.Errorf("папка по handle: %w", err)
	}
	return p, false, nil
}

func (t *Tracker) handle(fe *fevent) {
	t.st.events++
	if fe.ovf {
		t.st.overflow++
		t.log.Warn("tmpguard-track: переполнение очереди fanotify, полная сверка на следующем тике")
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
		t.log.Warn("tmpguard-track: переименование без папок", "old", oerr, "new", nerr)
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
		t.log.Warn("tmpguard-track: событие без папки", "err", err)
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
		t.log.Warn("tmpguard-track: метка контейнера", "err", err)
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
func (t *Tracker) startMeasure(done chan<- measureDone, full bool) {
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
	if full {
		for d := range t.watched {
			job.dirs = append(job.dirs, d)
		}
		for c := range t.containers {
			if _, ok := t.watched[c]; !ok {
				job.dirs = append(job.dirs, c) // контейнер появился, а событие пропущено
			}
		}
	}
	if len(job.items) == 0 && !full {
		if t.dirty { // замерять нечего, но удаления и создания должны дойти до снимка
			t.save(false)
		}
		return
	}
	t.measuring = true
	go func() {
		d := measureDone{job: job, listing: map[string][]string{}}
		for _, it := range job.items {
			d.usages = append(d.usages, measure(it.path, it.ino))
		}
		for _, dir := range job.dirs {
			if names, err := listDir(dir); err == nil {
				d.listing[dir] = names
			}
		}
		done <- d
	}()
}

func (t *Tracker) finishMeasure(d measureDone) {
	t.measuring = false
	now := time.Now()
	for _, u := range d.usages {
		e := t.entries[u.path]
		if e == nil || !u.ok || (e.Ino != 0 && u.ino != e.Ino) {
			continue
		}
		applyUsage(e, u.bytes, u.files, now)
	}
	if d.job.full {
		t.reconcile(d.listing, d.job.start, now)
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

// restore поднимает таблицу из снимка: свежий из /run, иначе с диска.
// Записи, которых больше нет (или inode другой), закрываются как gone.
func (t *Tracker) restore() {
	var st *record.State
	for _, dir := range []string{t.cfg.RunDir, t.cfg.StateDir} {
		s, err := record.ReadState(filepath.Join(dir, record.StateFile))
		if err != nil {
			t.log.Warn("tmpguard-track: снимок не прочитан", "dir", dir, "err", err)
			continue
		}
		if st == nil || s.Updated.After(st.Updated) {
			st = s
		}
	}
	if st == nil {
		return
	}
	now := time.Now()
	for i := range st.Entries {
		e := st.Entries[i]
		m, err := statMeta(e.Path)
		if err == nil && (e.Ino == 0 || m.ino == e.Ino) {
			t.entries[e.Path] = &e
			continue
		}
		t.close(&e, record.EvGone, "", now)
	}
}

func (t *Tracker) save(persist bool) {
	now := time.Now()
	s := record.State{Updated: now, Started: t.started, Roots: t.cfg.Roots, Containers: t.cfg.Containers}
	s.Entries = make([]record.Entry, 0, len(t.entries))
	for _, e := range t.entries {
		s.Entries = append(s.Entries, *e)
	}
	sort.Slice(s.Entries, func(i, j int) bool { return s.Entries[i].Path < s.Entries[j].Path })
	b, err := json.Marshal(s)
	t.dirty = false
	if err != nil {
		t.log.Error("tmpguard-track: снимок не собран", "err", err)
		return
	}
	if err := record.WriteFileAtomic(filepath.Join(t.cfg.RunDir, record.StateFile), b, 0o644); err != nil {
		t.log.Error("tmpguard-track: снимок", "err", err)
	}
	if persist {
		if err := record.WriteFileAtomic(filepath.Join(t.cfg.StateDir, record.StateFile), b, 0o644); err != nil {
			t.log.Error("tmpguard-track: снимок на диск", "err", err)
		}
		t.lastSave = now
	}
}

func (t *Tracker) logStats() {
	s := t.st
	t.log.Info(fmt.Sprintf("tmpguard-track: за час событий %d (создано %d, удалено %d, переименовано %d, переполнений %d); "+
		"создатель: жив %d, восстановлен %d, неизвестен %d; сессия по ppid %d, по cgroup %d, нет %d; "+
		"потерь proc connector %d; записей %d",
		s.events, s.creates, s.deletes, s.renames, s.overflow, s.procOK, s.procReaped, s.procUnknown,
		s.sessPpid, s.sessCgroup, s.noSession, t.tombs.overrun.Load(), len(t.entries)))
	t.st = counters{}
}
