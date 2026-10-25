package waits

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/record"
)

// File — что известно о транскрипте между проходами.
type File struct {
	Offset int64  `json:"offset"` // до куда прочитано (конец последней целой строки)
	From   int64  `json:"from"`   // начало прочитанного: старше — догрузка задним числом; 0 — прочитано с начала
	Name   string `json:"name,omitempty"`
}

// State — смещения по транскриптам. Started — первый проход был: файлы, которых тогда не было, читаются с начала.
type State struct {
	Started bool             `json:"started"`
	Files   map[string]*File `json:"files"` // путь относительно папки проектов
}

// Tail — проход по транскриптам.
type Tail struct {
	Projects string // ~/.claude/projects
	// Backfill — насколько назад догружать транскрипты, которые были до первого прохода; 0 — не догружать.
	Backfill time.Duration
	// BackfillBytes — сколько байт догрузки читать за проход: старые транскрипты — гигабайты, машину не грузим.
	BackfillBytes int64
}

// LoadState — состояние из файла; нет файла — пустое (первый проход).
func LoadState(path string) (*State, error) {
	st := &State{Files: map[string]*File{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, fmt.Errorf("смещения транскриптов: %w", err)
	}
	if err := json.Unmarshal(b, st); err != nil {
		return nil, fmt.Errorf("смещения транскриптов %s: %w", path, err)
	}
	if st.Files == nil {
		st.Files = map[string]*File{}
	}
	return st, nil
}

// Save пишет состояние атомарно.
func (st *State) Save(path string) error {
	b, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("смещения транскриптов: %w", err)
	}
	if err := record.WriteFileAtomic(path, b, 0o600); err != nil {
		return fmt.Errorf("смещения транскриптов: %w", err)
	}
	return nil
}

// Pass читает дописанное с прошлого прохода и кусок догрузки. Ошибки отдельных файлов возвращаются вместе с
// событиями; смещение такого файла не двигается.
func (t *Tail) Pass(st *State, now time.Time) ([]Event, error) {
	files, err := t.list()
	if err != nil {
		return nil, err
	}
	var out []Event
	var errs []error
	seen := map[string]bool{}
	for _, f := range files {
		seen[f.rel] = true
		s := st.Files[f.rel]
		if s == nil {
			s = &File{}
			if !st.Started { // был до первого прохода: новое — с конца, старое — догрузкой
				s.Offset, s.From = f.size, f.size
			}
			st.Files[f.rel] = s
		}
		evs, err := t.read(f, s, s.Offset, f.size, true)
		t.inherit(st, f, evs)
		out = append(out, evs...)
		errs = append(errs, err)
	}
	st.Started = true
	for _, rel := range slices.Sorted(maps.Keys(st.Files)) {
		if !seen[rel] {
			delete(st.Files, rel)
		}
	}
	evs, err := t.backfill(st, files, now)
	return append(out, evs...), errors.Join(append(errs, err)...)
}

// backfill догружает начало транскриптов, бывших до первого прохода и изменённых не раньше now−Backfill.
// За проход — не больше BackfillBytes; файл, недочитанный до конца догрузки, продолжится в следующий проход.
func (t *Tail) backfill(st *State, files []file, now time.Time) ([]Event, error) {
	if t.Backfill <= 0 {
		return nil, nil
	}
	budget := t.BackfillBytes
	var out []Event
	var errs []error
	for _, f := range files {
		s := st.Files[f.rel]
		if s.From == 0 || now.Sub(f.mtime) > t.Backfill {
			continue
		}
		if budget <= 0 {
			break
		}
		start := max(0, s.From-budget)
		if start > 0 { // начать с целой строки: кусок до неё дочитает следующий проход
			var err error
			if start, err = lineStart(f.path, start); err != nil {
				errs = append(errs, err)
				continue
			}
			if start >= s.From { // одна строка длиннее куска: дочитать начало целиком
				start = 0
			}
		}
		evs, err := t.read(f, s, start, s.From, false)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		budget -= s.From - start
		s.From = start
		t.inherit(st, f, evs)
		out = append(out, evs...)
	}
	return out, errors.Join(errs...)
}

// inherit — имя для событий без него: у суб-агента — имя его сессии (своей записи agent-name у него нет).
func (t *Tail) inherit(st *State, f file, evs []Event) {
	if f.agent == "" {
		return
	}
	parent := st.Files[filepath.Join(f.project(), f.session+".jsonl")]
	if parent == nil || parent.Name == "" {
		return
	}
	for i := range evs {
		if evs[i].Name == "" {
			evs[i].Name = parent.Name
		}
	}
}

// lineStart — начало первой целой строки не раньше off.
func lineStart(path string, off int64) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("транскрипт: %w", err)
	}
	defer f.Close()
	if _, err := f.Seek(off-1, io.SeekStart); err != nil {
		return 0, fmt.Errorf("транскрипт %s: %w", path, err)
	}
	r := bufio.NewReader(f)
	skipped, err := r.ReadBytes('\n')
	if err != nil {
		return 0, fmt.Errorf("транскрипт %s: %w", path, err)
	}
	return off - 1 + int64(len(skipped)), nil
}

type file struct {
	path, rel string
	size      int64
	mtime     time.Time
	session   string
	agent     string
}

// list — транскрипты сессий (<проект>/<сессия>.jsonl) и их суб-агентов (<проект>/<сессия>/subagents/agent-<id>.jsonl).
func (t *Tail) list() ([]file, error) {
	var out []file
	for _, pattern := range []string{"*/*.jsonl", "*/*/subagents/agent-*.jsonl"} {
		paths, err := filepath.Glob(filepath.Join(t.Projects, pattern))
		if err != nil {
			return nil, fmt.Errorf("транскрипты: %w", err)
		}
		for _, p := range paths {
			info, err := os.Stat(p)
			if errors.Is(err, fs.ErrNotExist) {
				continue // удалили между списком и stat
			}
			if err != nil {
				return nil, fmt.Errorf("транскрипт: %w", err)
			}
			rel, err := filepath.Rel(t.Projects, p)
			if err != nil {
				return nil, fmt.Errorf("транскрипт: %w", err)
			}
			out = append(out, describe(p, rel, info))
		}
	}
	return out, nil
}

func describe(path, rel string, info fs.FileInfo) file {
	f := file{path: path, rel: rel, size: info.Size(), mtime: info.ModTime()}
	parts := strings.Split(rel, string(filepath.Separator))
	f.session = strings.TrimSuffix(parts[1], ".jsonl")
	if len(parts) == 4 {
		f.agent = strings.TrimSuffix(strings.TrimPrefix(parts[3], "agent-"), ".jsonl")
	}
	return f
}

func (f file) project() string { return strings.SplitN(f.rel, string(filepath.Separator), 2)[0] }

// read — события из [from, to) файла. tail — чтение нового: смещение двигается до конца последней целой строки,
// имя сессии обновляется. Догрузка смещение не трогает; имя ставит, только если его ещё нет: догрузка идёт от
// конца к началу, первая найденная ею запись agent-name — самая поздняя. События куска до первой записи имени
// в нём получают уже известное (более позднее) имя: имя сессии меняется редко.
func (t *Tail) read(f file, s *File, from, to int64, tail bool) ([]Event, error) {
	if to < from && tail { // файл переписан короче: читать заново с начала
		from, s.From = 0, 0
	}
	if to <= from {
		return nil, nil
	}
	fh, err := os.Open(f.path)
	if err != nil {
		return nil, fmt.Errorf("транскрипт: %w", err)
	}
	defer fh.Close()
	sc := bufio.NewScanner(io.NewSectionReader(fh, from, to-from))
	sc.Buffer(make([]byte, 0, 1<<16), 64<<20) // строки с результатами инструментов бывают в десятки мегабайт
	sc.Split(scanFullLines)
	var out []Event
	var bad int
	name := s.Name
	pos := from
	for sc.Scan() {
		line := sc.Bytes()
		pos += int64(len(line)) + 1
		ev, ok, n, err := parse(line)
		switch {
		case err != nil:
			bad++
		case n != "":
			name = n
		case ok:
			ev.Session, ev.Agent, ev.Project, ev.Name = f.session, f.agent, f.project(), name
			out = append(out, ev)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("транскрипт %s: %w", f.rel, err)
	}
	if tail || s.Name == "" {
		s.Name = name
	}
	if tail {
		s.Offset = pos
	}
	if bad > 0 {
		return out, fmt.Errorf("транскрипт %s: строк не JSON %d", f.rel, bad)
	}
	return out, nil
}

// scanFullLines — только строки с переводом строки: недописанный хвост оставляем следующему проходу.
func scanFullLines(data []byte, _ bool) (int, []byte, error) {
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[:i], nil
	}
	return 0, nil, nil
}
