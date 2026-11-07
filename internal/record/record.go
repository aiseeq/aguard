// Package record описывает общие для обеих частей tmpguard форматы:
// журнал владельцев owners.jsonl (пишет слежение, читает aguard tmp)
// и снимок живых записей state.json.
package record

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// RuntimeDir — $XDG_RUNTIME_DIR; без него (запуск не из сеанса) — /run/user/<uid>, как его ставит systemd.
func RuntimeDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return d
	}
	return filepath.Join("/run/user", strconv.Itoa(os.Getuid()))
}

// TrackStateDir — журнал и долговременный снимок экземпляра слежения.
func TrackStateDir(home, name string) string {
	return filepath.Join(home, ".local", "state", "aguard", name)
}

// TrackRunDir — частый снимок экземпляра слежения: tmpfs сеанса.
func TrackRunDir(name string) string { return filepath.Join(RuntimeDir(), "aguard", name) }

// Имена файлов слежения в его папках (StateDir, RunDir).
const (
	OwnersFile   = "owners.jsonl"
	StateFile    = "state.json"
	RotatedOwner = "owners.jsonl.1"
)

// Состояния процесса-создателя на момент разбора события.
const (
	ProcOK      = "ok"      // процесс жив, данные из /proc
	ProcReaped  = "reaped"  // процесс уже умер, данные из proc connector или pidfd
	ProcUnknown = "unknown" // процесс умер и следов не осталось
)

// Виды строк owners.jsonl.
const (
	EvCreate      = "create"      // запись появилась (создана или переименована внутрь)
	EvDelete      = "delete"      // запись удалена или вынесена из отслеживаемой папки
	EvRename      = "rename"      // запись переименована внутри отслеживаемых папок
	EvPreexisting = "preexisting" // запись найдена сверкой, создателя сторож не видел
	EvGone        = "gone"        // запись пропала без события (пропуск или сторож лежал)
)

// Proc — процесс, создавший запись.
type Proc struct {
	Pid    int    `json:"pid"`
	Comm   string `json:"comm,omitempty"`
	Args   string `json:"args,omitempty"`
	Cwd    string `json:"cwd,omitempty"`
	UID    int    `json:"uid"` // -1, если не удалось узнать
	Cgroup string `json:"cgroup,omitempty"`
	State  string `json:"state"`
}

// Link — звено цепочки предков создателя.
type Link struct {
	Pid  int    `json:"pid"`
	Comm string `json:"comm"`
}

// Session — сессия Claude Code, к которой ведёт цепочка предков.
type Session struct {
	Pid       int    `json:"pid"`
	Name      string `json:"name,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
	ID        string `json:"id,omitempty"`
	ProcStart string `json:"proc_start,omitempty"` // поле 22 /proc/<pid>/stat: отличает переиспользованный pid
	Via       string `json:"via"`                  // ppid | cgroup
}

// Owner — всё, что известно о создателе записи в момент создания.
type Owner struct {
	Proc    Proc     `json:"proc"`
	Chain   []Link   `json:"chain,omitempty"` // от родителя создателя вверх до сессии включительно
	Session *Session `json:"session,omitempty"`
}

// Event — одна строка owners.jsonl.
type Event struct {
	T     time.Time `json:"t"`
	Ev    string    `json:"ev"`
	Path  string    `json:"path"`
	Old   string    `json:"old,omitempty"` // rename: прежний путь
	To    string    `json:"to,omitempty"`  // delete при выносе: куда унесли
	Dir   bool      `json:"dir,omitempty"`
	UID   int       `json:"uid"` // владелец записи по файловой системе, -1 если неизвестен
	Owner *Owner    `json:"owner,omitempty"`
	// Поля закрытия записи (delete, gone): итог её жизни.
	Born     time.Time `json:"born,omitzero"`
	LifeS    float64   `json:"life_s,omitempty"`
	MaxBytes int64     `json:"max_bytes,omitempty"`
	MaxFiles int64     `json:"max_files,omitempty"`
	ByteSec  float64   `json:"byte_sec,omitempty"`
	Measured bool      `json:"measured,omitempty"` // размер замерен хотя бы раз
}

// Closed сообщает, закрывает ли строка жизнь записи.
func (e Event) Closed() bool { return e.Ev == EvDelete || e.Ev == EvGone }

// Entry — живая запись верхнего уровня в снимке state.json.
type Entry struct {
	Path       string    `json:"path"`
	Dir        bool      `json:"dir,omitempty"`
	Ino        uint64    `json:"ino"`
	UID        int       `json:"uid"`
	Born       time.Time `json:"born"`            // время создания (наше событие или btime)
	Owner      *Owner    `json:"owner,omitempty"` // nil — создатель неизвестен
	Bytes      int64     `json:"bytes"`           // занято сейчас (блоки tmpfs), по последнему замеру
	Files      int64     `json:"files"`
	MaxBytes   int64     `json:"max_bytes"`
	MaxFiles   int64     `json:"max_files"`
	ByteSec    float64   `json:"byte_sec"` // интеграл размера по времени, байт·с
	MeasuredAt time.Time `json:"measured_at,omitzero"`
}

// Age — возраст записи на момент now.
func (e *Entry) Age(now time.Time) time.Duration { return now.Sub(e.Born) }

// Event — строка журнала о записи. Для закрывающих строк (delete, gone)
// заполняются итоги жизни; остальные поля вызывающий дописывает сам (Old, To).
func (e *Entry) Event(t time.Time, ev string) Event {
	out := Event{T: t, Ev: ev, Path: e.Path, Dir: e.Dir, UID: e.UID, Owner: e.Owner, Born: e.Born}
	if out.Closed() {
		out.LifeS = t.Sub(e.Born).Seconds()
		out.MaxBytes, out.MaxFiles, out.ByteSec = e.MaxBytes, e.MaxFiles, e.ByteSec
		out.Measured = !e.MeasuredAt.IsZero()
	}
	return out
}

// State — снимок живых записей, пишется атомарно после каждого замера.
type State struct {
	Updated    time.Time `json:"updated"`
	Started    time.Time `json:"started"` // запуск сторожа
	Roots      []string  `json:"roots"`
	Containers []string  `json:"containers"`
	Entries    []Entry   `json:"entries"`
	// NoBorn — пути записей без born, исключённых при чтении: возраст неизвестен, меру сводки не посчитать.
	// Вызывающий пишет их в журнал; остальные записи снимка годны.
	NoBorn []string `json:"-"`
}

// ReadState читает снимок; отсутствующий файл — пустой снимок без ошибки.
func ReadState(path string) (*State, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &State{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("снимок: %w", err)
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	// мера сводки — размер × возраст: без born возраст неизвестен, запись исключается на месте
	kept := s.Entries[:0]
	for i := range s.Entries {
		if s.Entries[i].Born.IsZero() {
			s.NoBorn = append(s.NoBorn, s.Entries[i].Path)
			continue
		}
		kept = append(kept, s.Entries[i])
	}
	s.Entries = kept
	return &s, nil
}

// WriteFileAtomic пишет файл через временный и rename, чтобы читатель
// никогда не видел половину снимка.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	return WriteFileAtomicFunc(path, perm, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}

// WriteFileAtomicFunc — то же, но содержимое пишет fill потоком, без
// промежуточного буфера во весь файл (большие снимки).
func WriteFileAtomicFunc(path string, perm os.FileMode, fill func(io.Writer) error) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("запись %s: %w", path, err)
	}
	name := tmp.Name()
	bw := bufio.NewWriterSize(tmp, 256<<10)
	err = fill(bw)
	if err == nil {
		err = bw.Flush()
	}
	err = errors.Join(err, tmp.Close())
	if err == nil {
		err = os.Chmod(name, perm)
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		if rerr := os.Remove(name); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			err = errors.Join(err, rerr)
		}
		return fmt.Errorf("запись %s: %w", path, err)
	}
	return nil
}

// ParseEvents разбирает owners.jsonl. Битые строки (например, оборванная
// последняя при падении) пропускаются и считаются в bad.
func ParseEvents(r io.Reader, fn func(Event)) (bad int, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Event
		if json.Unmarshal(line, &e) != nil || e.Ev == "" {
			bad++
			continue
		}
		fn(e)
	}
	return bad, sc.Err()
}

// ReadEventFiles читает журнал вместе с ротированной частью (сначала старую).
// Нечитаемый файл не мешает прочитать второй: ошибки возвращаются вместе.
func ReadEventFiles(dir string, fn func(Event)) (bad int, err error) {
	var errs []error
	for _, name := range []string{RotatedOwner, OwnersFile} {
		b, ferr := readEventFile(filepath.Join(dir, name), fn)
		bad += b
		if ferr != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, ferr))
		}
	}
	return bad, errors.Join(errs...)
}

func readEventFile(path string, fn func(Event)) (int, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("журнал: %w", err)
	}
	defer f.Close()
	return ParseEvents(f, fn)
}
