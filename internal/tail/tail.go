// Package tail — чтение дописанного в транскриптах ~/.claude/projects между проходами: смещения по файлам,
// догрузка задним числом, имя сессии по записям agent-name. Что искать в строках, решает наблюдатель (Parse).
package tail

import (
	"bufio"
	"bytes"
	"cmp"
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
	// Lines — целых строк до Offset, если Counted: номер строки события (ссылка «<id>:<строка>»). Считается
	// лениво, при первом событии файла: пересчёт гигабайт транскриптов на первом проходе не нужен.
	Lines   int64 `json:"lines,omitempty"`
	Counted bool  `json:"counted,omitempty"`
}

// Source — откуда событие: файл, сессия, суб-агент, имя сессии и номер строки (с 1; 0 — не считали).
type Source struct {
	Rel, Session, Agent, Project, Name string
	Line                               int64
}

// Parse — разбор строки: событие (ok) и/или новое имя сессии; строка без нужного — пусто без ошибки.
type Parse[E any] func(line []byte) (ev E, ok bool, name string, err error)

// State — смещения по транскриптам. Started — первый проход был: файлы, которых тогда не было, читаются с начала.
type State struct {
	Started bool             `json:"started"`
	Files   map[string]*File `json:"files"` // путь относительно папки проектов
}

// Tail — проход по транскриптам.
type Tail[E any] struct {
	Parse Parse[E]
	// Stamp — ставит событию его источник.
	Stamp func(ev *E, src Source)
	// Lines — считать номера строк событий.
	Lines bool
	// Subagents — читать и транскрипты суб-агентов.
	Subagents bool
	Projects  string // ~/.claude/projects
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
func (t *Tail[E]) Pass(st *State, now time.Time) ([]E, error) {
	files, err := t.list()
	if err != nil {
		return nil, err
	}
	var out []E
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
		evs, err := t.read(f, s, s.Offset, f.size, true, t.parent(st, f))
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
func (t *Tail[E]) backfill(st *State, files []file, now time.Time) ([]E, error) {
	if t.Backfill <= 0 {
		return nil, nil
	}
	budget := t.BackfillBytes
	var out []E
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
		evs, err := t.read(f, s, start, s.From, false, t.parent(st, f))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		budget -= s.From - start
		s.From = start
		out = append(out, evs...)
	}
	return out, errors.Join(errs...)
}

// parent — имя сессии суб-агента: своей записи agent-name у него нет.
func (t *Tail[E]) parent(st *State, f file) string {
	if f.agent == "" {
		return ""
	}
	if p := st.Files[filepath.Join(f.project(), f.session+".jsonl")]; p != nil {
		return p.Name
	}
	return ""
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
func (t *Tail[E]) list() ([]file, error) {
	var out []file
	patterns := []string{"*/*.jsonl"}
	if t.Subagents {
		patterns = append(patterns, "*/*/subagents/agent-*.jsonl")
	}
	for _, pattern := range patterns {
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
func (t *Tail[E]) read(f file, s *File, from, to int64, tail bool, parent string) ([]E, error) {
	if to < from && tail { // файл переписан короче: читать заново с начала
		from, s.From, s.Lines, s.Counted = 0, 0, 0, false
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
	var out []E
	var bad int
	name := s.Name
	pos := from
	// base — строк до from: при чтении нового известен, если файл уже считали; иначе — по первому событию
	ln := lineNo{fh: fh, from: from, known: from == 0}
	if tail && s.Counted {
		ln.base, ln.known = s.Lines, true
	}
	var n int64
	for sc.Scan() {
		line := sc.Bytes()
		pos += int64(len(line)) + 1
		n++
		ev, ok, nm, err := t.Parse(line)
		switch {
		case err != nil:
			bad++
		case nm != "":
			name = nm
		case ok:
			src := Source{Rel: f.rel, Session: f.session, Agent: f.agent, Project: f.project(), Name: cmp.Or(name, parent)}
			if t.Lines || ln.known {
				if src.Line, err = ln.at(n); err != nil {
					return nil, err
				}
			}
			t.Stamp(&ev, src)
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
		s.Lines, s.Counted = ln.base+n, ln.known
	}
	if bad > 0 {
		return out, fmt.Errorf("транскрипт %s: строк не JSON %d", f.rel, bad)
	}
	return out, nil
}

// lineNo — номера строк куска с from: base строк до него известен сразу или считается при первом событии.
type lineNo struct {
	fh         *os.File
	from, base int64
	known      bool
}

// at — номер n-й строки куска.
func (l *lineNo) at(n int64) (int64, error) {
	if !l.known {
		b, err := countLines(l.fh, l.from)
		if err != nil {
			return 0, err
		}
		l.base, l.known = b, true
	}
	return l.base + n, nil
}

// countLines — целых строк в первых to байтах файла.
func countLines(fh *os.File, to int64) (int64, error) {
	r := bufio.NewReaderSize(io.NewSectionReader(fh, 0, to), 1<<20)
	buf := make([]byte, 1<<20)
	var n int64
	for {
		k, err := r.Read(buf)
		n += int64(bytes.Count(buf[:k], []byte{'\n'}))
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return 0, fmt.Errorf("строки транскрипта %s: %w", fh.Name(), err)
		}
	}
}

// scanFullLines — только строки с переводом строки: недописанный хвост оставляем следующему проходу.
func scanFullLines(data []byte, _ bool) (int, []byte, error) {
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[:i], nil
	}
	return 0, nil, nil
}
