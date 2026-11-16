package agentwait

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

	"github.com/aiseeq/aguard/internal/agentwait/waits"
	"github.com/aiseeq/akit/transcript"

	"github.com/aiseeq/aguard/internal/statefile"
)

// Source — транскрипт: Claude (сессия или её суб-агент) или боец codex.
type Source struct {
	Path, Key, Runner, Session, Agent, Project string
	size                                       int64
	mtime                                      time.Time
}

// List — транскрипты Claude в projects (<проект>/<сессия>.jsonl, <проект>/<сессия>/subagents/agent-<id>.jsonl) и
// бойцов codex в workers (<задача>/codex/sessions/ГГГГ/ММ/ДД/rollout-*.jsonl). Key — ключ в состоянии.
func List(projects, workers string) ([]Source, error) {
	var out []Source
	add := func(root, pattern, runner string, fill func(*Source, []string)) error {
		paths, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil {
			return fmt.Errorf("транскрипты: %w", err)
		}
		for _, p := range paths {
			info, err := os.Stat(p)
			if errors.Is(err, fs.ErrNotExist) {
				continue // удалили между списком и stat
			}
			if err != nil {
				return fmt.Errorf("транскрипт: %w", err)
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return fmt.Errorf("транскрипт: %w", err)
			}
			s := Source{Path: p, Key: runner + "/" + rel, Runner: runner, size: info.Size(), mtime: info.ModTime()}
			fill(&s, strings.Split(rel, string(filepath.Separator)))
			out = append(out, s)
		}
		return nil
	}
	claude := func(s *Source, parts []string) {
		s.Project, s.Session = parts[0], strings.TrimSuffix(parts[1], ".jsonl")
		if len(parts) == 4 {
			s.Agent = strings.TrimSuffix(strings.TrimPrefix(parts[3], "agent-"), ".jsonl")
		}
	}
	codex := func(s *Source, parts []string) {
		s.Project, s.Session = parts[0], strings.TrimSuffix(strings.TrimPrefix(parts[len(parts)-1], "rollout-"), ".jsonl")
	}
	return out, errors.Join(
		add(projects, "*/*.jsonl", RunnerClaude, claude),
		add(projects, "*/*/subagents/agent-*.jsonl", RunnerClaude, claude),
		add(workers, "*/codex/sessions/*/*/*/rollout-*.jsonl", RunnerCodex, codex))
}

// File — что известно о транскрипте между проходами.
type File struct {
	// Offset — конец прочитанного.
	Offset int64 `json:"offset"`
	// Seen — ожидания для подавления дублей при чтении старого состояния с начала открытого фрагмента.
	Seen map[string]int64 `json:"seen,omitempty"`
	// Pending — снимок распознавателя для продолжения открытых ожиданий.
	Pending *waits.Recognizer `json:"pending,omitempty"`
	// Line — номер последней строки, назначенный событиям Pending.
	Line int `json:"line,omitempty"`
	// PendingAt — начало фрагмента для восстановления при битом снимке.
	PendingAt    int64         `json:"pending_at,omitempty"`
	OpenAt       map[int]int64 `json:"open_at,omitempty"`
	Cwd          string        `json:"cwd,omitempty"`
	PendingBad   bool          `json:"pending_bad,omitempty"`
	PendingError string        `json:"pending_error,omitempty"`
}

func (f *File) UnmarshalJSON(data []byte) error {
	var raw struct {
		Offset       int64            `json:"offset"`
		Seen         map[string]int64 `json:"seen,omitempty"`
		Pending      json.RawMessage  `json:"pending,omitempty"`
		Line         int              `json:"line,omitempty"`
		PendingAt    int64            `json:"pending_at,omitempty"`
		OpenAt       map[int]int64    `json:"open_at,omitempty"`
		Cwd          string           `json:"cwd,omitempty"`
		PendingBad   bool             `json:"pending_bad,omitempty"`
		PendingError string           `json:"pending_error,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*f = File{Offset: raw.Offset, Seen: raw.Seen, Line: raw.Line, PendingAt: raw.PendingAt, OpenAt: raw.OpenAt, Cwd: raw.Cwd, PendingBad: raw.PendingBad, PendingError: raw.PendingError}
	if len(raw.Pending) == 0 || bytes.Equal(bytes.TrimSpace(raw.Pending), []byte("null")) {
		return nil
	}
	r := waits.New()
	if err := json.Unmarshal(raw.Pending, r); err != nil {
		f.PendingBad = true
		f.PendingError = err.Error()
		return nil
	}
	if len(r.OpenStates()) == 0 {
		f.PendingBad = true
		f.PendingError = "снимок не содержит открытых ожиданий"
		return nil
	}
	f.Pending = r
	return nil
}

// State — смещения по транскриптам. Started — первый проход был: новые файлы читаются с начала.
type State struct {
	Started bool             `json:"started"`
	Files   map[string]*File `json:"files"`
}

// LoadState — состояние из файла; нет файла — пустое (первый проход).
func LoadState(path string) (*State, error) {
	st := &State{Files: map[string]*File{}}
	b, err := statefile.Load(path)
	if err != nil {
		return nil, fmt.Errorf("смещения ожиданий: %w", err)
	}
	if err := json.Unmarshal(b, st); err != nil {
		return nil, fmt.Errorf("смещения ожиданий %s: %w", path, err)
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
		return fmt.Errorf("смещения ожиданий: %w", err)
	}
	if err := statefile.Save(path, b); err != nil {
		return fmt.Errorf("смещения ожиданий: %w", err)
	}
	return nil
}

// Pass — проход по транскриптам.
type Pass struct {
	Projects, Workers string
	Backfill          time.Duration
	// Budget — байт за проход. Файл, не влезший в остаток, читается куском до остатка: больше бюджета не читался
	// бы никогда — первым в проходе его не ставят хвосты помельче (llmark 210 МБ 22.10, 27.10 14:47 CET).
	Budget  int64
	MaxOpen time.Duration
	// Name — имя сессии Claude по её папке (names.For); пусто — без имени.
	Name     func(cwd string) string
	openFile func(string) (io.ReaderAt, io.Closer, error)
}

// Run читает дописанное и отдаёт законченные ожидания. Сначала файлы с меньшим непрочитанным: хвосты живых
// сессий не ждут, пока дочитается неделя старых. Ошибки отдельных файлов возвращаются вместе с ожиданиями;
// смещение такого файла не двигается.
func (p *Pass) Run(st *State, now time.Time) ([]Wait, error) {
	files, err := List(p.Projects, p.Workers)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, f := range files {
		seen[f.Key] = true
		if st.Files[f.Key] == nil {
			s := &File{}
			if !st.Started && now.Sub(f.mtime) > p.Backfill {
				s.Offset = f.size
			}
			st.Files[f.Key] = s
		}
	}
	st.Started = true
	for k := range st.Files {
		if !seen[k] {
			delete(st.Files, k)
		}
	}
	unread := func(f Source) int64 { return max(f.size-st.Files[f.Key].Offset, 0) }
	slices.SortStableFunc(files, func(a, b Source) int { return cmp.Compare(unread(a), unread(b)) })
	var out []Wait
	var errs []error
	var used int64
	for _, f := range files {
		s := st.Files[f.Key]
		badSnapshot := s.PendingBad
		if badSnapshot {
			errs = append(errs, recoverSnapshot(f, s))
		}
		if f.size < s.Offset && !badSnapshot {
			s.Offset = f.size
			continue
		}
		if f.size == s.Offset && !badSnapshot {
			if expired, ok := p.expired(s.Pending, now); ok {
				name := ""
				if p.Name != nil && s.Cwd != "" {
					name = p.Name(s.Cwd)
				}
				for i := range expired {
					out = append(out, f.wait(&expired[i], name))
				}
				s.Pending, s.Seen, s.Line, s.PendingAt, s.OpenAt = nil, nil, 0, 0, nil
			}
			continue
		}
		if used >= p.Budget {
			break
		}
		ws, n, err := p.read(f, s, now, p.Budget-used)
		used += n
		out = append(out, ws...)
		errs = append(errs, err)
	}
	return out, errors.Join(errs...)
}

func recoverSnapshot(f Source, s *File) error {
	// PendingAt is the recovery point used by the former rewind behavior.
	s.Offset = s.PendingAt
	if s.Offset < 0 {
		s.Offset = 0
	} else if s.Offset >= f.size {
		s.Offset = f.size
	}
	reason := s.PendingError
	if reason == "" {
		reason = "снимок распознавателя повреждён"
	}
	s.Pending, s.PendingBad, s.PendingError, s.Line, s.PendingAt, s.OpenAt = nil, false, "", 0, 0, nil
	return fmt.Errorf("снимок распознавателя %s повреждён (%s): перечитаю от начала открытого ожидания", f.Key, reason)
}

func (p *Pass) expired(r *waits.Recognizer, now time.Time) ([]waits.Wait, bool) {
	if r == nil {
		return nil, false
	}
	open := r.OpenStates()
	if len(open) == 0 || open[0].Start.IsZero() || now.Sub(open[0].Start) < p.MaxOpen {
		return nil, false
	}
	return r.Close(), true
}

// chunk — кусок одного файла за проход: распознаватель, начало и время каждой строки, законченные ожидания.
type chunk struct {
	src          Source
	rec          *waits.Recognizer
	seen         map[string]int64 // ожидания этого прохода и прошлых, начатые после смещения: id первого вызова → начало
	parser       *transcript.Parser
	nameOf       func(cwd string) string
	lineCount    int
	lineBase     int
	linePos      map[int]int64
	openAt       map[int]int64
	cwd          string
	out          []Wait
	name         string
	pendingAt    int64
	hasPendingAt bool
	bad          int
	badErr       error // первая битая строка
}

// open — есть незаконченное ожидание: кусок на нём не обрывается.
func (c *chunk) open() bool {
	_, ok := c.rec.Open()
	return ok
}

// keep — законченные ожидания, кроме записанных в прошлый проход (перечитанный кусок отдаёт их снова).
func (c *chunk) keep(found []waits.Wait) {
	for i := range found {
		w := &found[i]
		key := waitKey(w)
		if _, dup := c.seen[key]; dup {
			continue
		}
		c.seen[key] = 0
		c.out = append(c.out, c.src.wait(w, c.name))
	}
}

// waitKey — ожидание в файле: id первого вызова, без вызова — начало и способ.
func waitKey(w *waits.Wait) string {
	if len(w.CallIDs) > 0 {
		return w.CallIDs[0]
	}
	return w.How + " " + w.Start.Format(time.RFC3339Nano)
}

// add — строка raw (без перевода строки) с началом в pos.
func (c *chunk) add(raw []byte, pos int64) {
	c.lineCount++
	line := c.lineBase + c.lineCount
	evs, err := c.parser.Parse(raw)
	if err != nil { // строка дописывается прямо сейчас или испорчена: считаем, первую — в ошибку прохода
		c.bad++
		if c.badErr == nil {
			c.badErr = fmt.Errorf("строка %d куска: %w", c.lineCount, err)
		}
	}
	for i := range evs {
		if c.cwd == "" && evs[i].Cwd != "" {
			c.cwd = evs[i].Cwd
		}
		if c.name == "" && c.cwd != "" && c.nameOf != nil {
			c.name = c.nameOf(c.cwd)
		}
	}
	for i := range evs {
		evs[i].Line = line
		c.keep(c.rec.Add(evs[i]))
	}
	c.linePos[line] = pos
	c.updateOpen()
}

func (c *chunk) updateOpen() {
	states := c.rec.OpenStates()
	active := make(map[int]int64, len(states))
	for _, state := range states {
		if pos, ok := c.openAt[state.Line]; ok {
			active[state.Line] = pos
		} else if pos, ok := c.linePos[state.Line]; ok {
			active[state.Line] = pos
		} else if c.hasPendingAt && len(active) == 0 {
			active[state.Line] = c.pendingAt // Older snapshots have only the first open start.
		} else {
			active[state.Line] = 0
		}
	}
	c.openAt = active
	if len(states) == 0 {
		c.pendingAt, c.hasPendingAt = 0, false
	} else {
		c.pendingAt, c.hasPendingAt = active[states[0].Line], true
	}
}

// read — законченные ожидания из [s.Offset, конец последней целой строки) файла f и сколько байт прочитано. Дальше
// limit байт — только пока ожидание открыто: кусок кончается на строке без открытых, и следующий кусок
// продолжает с неё же.
func (p *Pass) read(f Source, s *File, now time.Time, limit int64) ([]Wait, int64, error) {
	openFn := p.openFile
	if openFn == nil {
		openFn = func(path string) (io.ReaderAt, io.Closer, error) {
			fh, err := os.Open(path)
			return fh, fh, err
		}
	}
	rd, closer, err := openFn(f.Path)
	if err != nil {
		return nil, 0, fmt.Errorf("транскрипт: %w", err)
	}
	defer closer.Close()
	from, lineBase, pendingAt := s.Offset, s.Line, s.PendingAt
	oldPending, oldSeen, cwd := s.Pending, s.Seen, s.Cwd
	openAt := maps.Clone(s.OpenAt)
	if f.size < from { // файл переписан короче: сдвиг к концу без повторной выдачи
		s.Offset = f.size
		return nil, 0, nil
	}
	rec := waits.New()
	if oldPending != nil {
		b, err := json.Marshal(oldPending)
		if err != nil {
			return nil, 0, fmt.Errorf("снимок распознавателя %s: %w", f.Key, err)
		}
		if err := json.Unmarshal(b, rec); err != nil || len(rec.OpenStates()) == 0 {
			return nil, 0, fmt.Errorf("снимок распознавателя %s повреждён", f.Key)
		}
	}
	if openAt == nil {
		openAt = map[int]int64{}
		if oldPending != nil {
			states := rec.OpenStates()
			if len(states) > 0 {
				openAt[states[0].Line] = pendingAt
			}
		}
	}
	c := &chunk{src: f, rec: rec, parser: transcript.NewParser(), nameOf: p.Name,
		seen: map[string]int64{}, lineBase: lineBase, linePos: map[int]int64{}, openAt: openAt, cwd: cwd,
		pendingAt: pendingAt, hasPendingAt: oldPending != nil}
	if c.cwd != "" && c.nameOf != nil {
		c.name = c.nameOf(c.cwd)
	}
	maps.Copy(c.seen, oldSeen)
	pos, err := scan(c, rd, from, f.size, limit)
	if err != nil {
		return nil, pos - from, fmt.Errorf("транскрипт %s: %w", f.Key, err)
	}
	next := pos
	open := c.rec.OpenStates()
	if len(open) > 0 && !open[0].Start.IsZero() && now.Sub(open[0].Start) >= p.MaxOpen {
		c.keep(c.rec.Close())
		open = nil
	}
	s.Offset, s.Cwd = next, c.cwd
	if len(open) == 0 {
		s.Pending, s.Seen, s.Line, s.PendingAt, s.OpenAt = nil, nil, 0, 0, nil
	} else {
		s.Pending, s.Seen, s.Line, s.PendingAt, s.OpenAt = c.rec, c.seen, c.lineBase+c.lineCount, c.pendingAt, c.openAt
	}
	if c.bad > 0 {
		return c.out, pos - from, fmt.Errorf("транскрипт %s: строк не JSON %d (%w)", f.Key, c.bad, c.badErr)
	}
	return c.out, pos - from, nil
}

func scan(c *chunk, rd io.ReaderAt, from, size, limit int64) (int64, error) {
	r := bufio.NewReaderSize(io.NewSectionReader(rd, from, max(size-from, 0)), 1<<20)
	pos := from
	for pos-from < limit || c.open() {
		raw, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			raw, err = readLong(r, raw)
		}
		if errors.Is(err, io.EOF) {
			return pos, nil // недописанный хвост — следующему проходу
		}
		if err != nil {
			return pos, fmt.Errorf("чтение строки транскрипта: %w", err)
		}
		c.add(raw[:len(raw)-1], pos)
		pos += int64(len(raw))
	}
	return pos, nil
}

// readLong дочитывает строку длиннее буфера: записи с выводом инструментов бывают в десятки мегабайт.
func readLong(r *bufio.Reader, head []byte) ([]byte, error) {
	line := slices.Clone(head)
	for {
		more, err := r.ReadSlice('\n')
		line = append(line, more...)
		if !errors.Is(err, bufio.ErrBufferFull) {
			return line, err
		}
	}
}

// wait — найденное с источником.
func (f Source) wait(w *waits.Wait, name string) Wait {
	if f.Runner == RunnerCodex {
		name = f.Project
	}
	out := Wait{
		At: w.Start, End: w.End, Runner: f.Runner, Session: f.Session, Agent: f.Agent, Project: f.Project, Name: name,
		How: w.How, Class: w.Class, Command: w.Command, Calls: w.Calls, Repeats: w.Repeats, Tokens: w.Tokens,
		Outcome: w.Outcome,
	}
	if !w.End.IsZero() {
		out.Ms = w.End.Sub(w.Start).Milliseconds()
	}
	return out
}
