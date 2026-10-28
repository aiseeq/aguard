package iowatch

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Event — закрытое событие фриза: от первого до последнего шага с превышением.
type Event struct {
	Start, End time.Time
	IdleUS     int64 // простой (Idle) всех шагов от Start до End
	PeakUS     int64 // наибольший простой шага
	Path       string
}

// Recorder — кольцевой буфер и снимки фризов: один для службы и `aguard io record`.
// Принадлежит одной горутине (главный цикл замера).
//
// Шаг с превышением (ShouldTrigger) открывает событие: буфер до него и дальше каждая запись идут в
// файл dir/<начало>.jsonl, пока не пройдёт calm без превышения; новое превышение продлевает. Из
// буфера берутся только записи позже уже отданных прошлым событиям: частые фризы не пишут одни и
// те же пять минут заново. Файл создаёт и пишет своя горутина, главный цикл диска не ждёт ни при
// открытии, ни при закрытии события.
type Recorder struct {
	cfg     Config
	dir     string
	ring    *Ring
	active  *snapshotWriter
	closing []*snapshotWriter // закрытые события, чьи файлы ещё дописываются
	event   Event
	written map[string]time.Time // по виду записи — время последней, отданной в файл события
	// простой и пик спокойных шагов после последнего превышения: войдут в событие, если оно продлится
	pendingIdle, pendingPeak int64
	calmSince                time.Time
}

func NewRecorder(cfg Config, dir string) *Recorder {
	return &Recorder{cfg: cfg, dir: dir, ring: NewRing(cfg), written: map[string]time.Time{}}
}

// SetConfig меняет пороги на ходу; размер буфера — с новым Recorder.
func (r *Recorder) SetConfig(cfg Config) { r.cfg = cfg }

// Active — идёт событие.
func (r *Recorder) Active() bool { return r.active != nil }

// Step кладёт быстрый шаг; закрытое этим шагом событие возвращается. err — сбой записи файлов
// прошлых событий (стало известно сейчас); запись идёт дальше: шаг уже в буфере.
func (r *Recorder) Step(s Step) (*Event, error) {
	err := r.reap()
	rec := Record{Kind: KindStep, Step: s}
	r.ring.Add(rec)
	trigger := ShouldTrigger(s, r.cfg)
	idle := Idle(s)
	if r.active == nil {
		if !trigger {
			return nil, err
		}
		r.active, r.calmSince = startSnapshot(filepath.Join(r.dir, EventName(s.At)), "", r.unwritten()), time.Time{}
		r.event = Event{Start: s.At, End: s.At, IdleUS: idle, PeakUS: idle, Path: r.active.path}
		r.pendingIdle, r.pendingPeak = 0, 0
		return nil, err
	}
	r.toEvent(rec)
	if trigger {
		r.event.End = s.At
		r.event.IdleUS += r.pendingIdle + idle
		r.event.PeakUS = max(r.event.PeakUS, r.pendingPeak, idle)
		r.pendingIdle, r.pendingPeak, r.calmSince = 0, 0, time.Time{}
		return nil, err
	}
	r.pendingIdle += idle
	r.pendingPeak = max(r.pendingPeak, idle)
	if r.calmSince.IsZero() {
		r.calmSince = s.At
	}
	if s.At.Sub(r.calmSince) < r.cfg.Calm.D() {
		return nil, err
	}
	r.active.finish()
	r.closing = append(r.closing, r.active)
	r.active = nil
	ev := r.event
	return &ev, err
}

// Detail кладёт детальную запись в буфер и в открытое событие.
func (r *Recorder) Detail(s Step) {
	rec := Record{Kind: KindDetail, Step: s}
	r.ring.Add(rec)
	if r.active != nil {
		r.toEvent(rec)
	}
}

// toEvent — запись в файл открытого события; отмечается как отданная.
func (r *Recorder) toEvent(rec Record) {
	r.active.add(rec)
	r.written[rec.Kind] = rec.At
}

// unwritten — записи буфера позже уже отданных в файлы событий; отмечаются как отданные.
func (r *Recorder) unwritten() []Record {
	all := r.ring.Records()
	out := all[:0]
	for i := range all {
		if all[i].At.After(r.written[all[i].Kind]) {
			out = append(out, all[i])
		}
	}
	for i := range out {
		r.written[out[i].Kind] = out[i].At
	}
	return out
}

// reap — итоги дописанных файлов закрытых событий, не дожидаясь остальных.
func (r *Recorder) reap() error {
	var errs error
	open := r.closing[:0]
	for _, w := range r.closing {
		if done, err := w.poll(); done {
			errs = errors.Join(errs, err)
		} else {
			open = append(open, w)
		}
	}
	clear(r.closing[len(open):])
	r.closing = open
	return errs
}

// Close при остановке закрывает открытое событие и ждёт дописи всех файлов событий; открытое
// событие возвращается (ok), если было.
func (r *Recorder) Close() (ev Event, ok bool, err error) {
	if r.active != nil {
		r.active.finish()
		r.closing = append(r.closing, r.active)
		r.active, ev, ok = nil, r.event, true
	}
	for _, w := range r.closing {
		err = errors.Join(err, w.wait())
	}
	r.closing = nil
	return ev, ok, err
}

// Dump — ручной снимок буфера в dir/manual-<at>.jsonl. Файл появляется под этим именем, когда
// записан целиком; Wait — дождаться этого.
func (r *Recorder) Dump(at time.Time) *Snapshot {
	path := filepath.Join(r.dir, ManualName(at))
	w := startSnapshot(path+".part", path, r.ring.Records())
	w.finish()
	return &Snapshot{Path: path, w: w}
}

// Snapshot — ручной снимок, который ещё пишется.
type Snapshot struct {
	Path string
	w    *snapshotWriter
}

// Wait — дождаться записи и переименования.
func (s *Snapshot) Wait() error {
	if err := s.w.wait(); err != nil {
		return fmt.Errorf("ручной снимок: %w", err)
	}
	return nil
}

// snapshotQueue — сколько записей события может ждать записи, пока диск занят (80 с шагов по 100 мс).
const snapshotQueue = 1024

// snapshotWriter — файл снимка, который создаёт и пишет своя горутина через bufio. Методы, кроме
// wait у ручного снимка, — только из горутины-владельца Recorder.
type snapshotWriter struct {
	path     string
	queue    chan Record
	done     chan error
	dropped  int // записи, не влезшие в очередь
	finished bool
	result   error
	got      bool
}

// startSnapshot пишет initial и дальнейшие add в новый файл path (создаёт горутина записи: папка,
// O_EXCL). final != "" — после записи переименовать в final.
func startSnapshot(path, final string, initial []Record) *snapshotWriter {
	w := &snapshotWriter{path: path, queue: make(chan Record, snapshotQueue), done: make(chan error, 1)}
	if final != "" {
		w.path = final
	}
	go func() {
		err := writeSnapshot(path, final, initial, w.queue)
		if err != nil {
			err = fmt.Errorf("снимок %s: %w", path, err)
			for range w.queue { // главный цикл не должен упираться в полную очередь
			}
		}
		w.done <- err
	}()
	return w
}

func writeSnapshot(path, final string, initial []Record, queue <-chan Record) (err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("создать папку: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("создать: %w", err)
	}
	buf := bufio.NewWriterSize(f, 64*1024)
	for i := range initial {
		if err == nil {
			err = WriteRecord(buf, initial[i])
		}
	}
	for rec := range queue {
		if err == nil {
			err = WriteRecord(buf, rec)
		}
	}
	if err == nil {
		err = buf.Flush()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && final != "" {
		err = os.Rename(path, final)
	}
	return err
}

// add ставит запись в очередь; очередь полна (диск стоит) — запись теряется и считается, главный
// цикл не ждёт.
func (w *snapshotWriter) add(rec Record) {
	select {
	case w.queue <- rec:
	default:
		w.dropped++
	}
}

// finish — записей больше не будет; файл дописывается без ожидания.
func (w *snapshotWriter) finish() {
	if !w.finished {
		w.finished = true
		close(w.queue)
	}
}

// poll — дописан ли файл, не ожидая; итог — один раз.
func (w *snapshotWriter) poll() (bool, error) {
	select {
	case err := <-w.done:
		return true, w.settle(err)
	default:
		return false, nil
	}
}

// wait — дождаться дописи (после finish).
func (w *snapshotWriter) wait() error {
	if w.got {
		return w.result
	}
	return w.settle(<-w.done)
}

func (w *snapshotWriter) settle(err error) error {
	if w.dropped > 0 {
		err = errors.Join(err, fmt.Errorf("снимок %s: потеряно %d записей — диск не успевал", w.path, w.dropped))
	}
	w.result, w.got = err, true
	return err
}

// CleanupSnapshots удаляет снимки старше keep (по времени изменения).
func CleanupSnapshots(dir string, keep time.Duration, now time.Time) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("прочитать папку снимков %s: %w", dir, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".jsonl") && !strings.HasSuffix(name, ".jsonl.part") {
			continue
		}
		info, err := entry.Info()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("снимок %s: %w", name, err)
		}
		if now.Sub(info.ModTime()) > keep {
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("удалить старый снимок %s: %w", name, err)
			}
		}
	}
	return nil
}
