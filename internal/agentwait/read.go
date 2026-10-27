package agentwait

import (
	"bufio"
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

	"github.com/aiseeq/akit/transcript"
	"github.com/aiseeq/akit/transcript/waits"

	"github.com/aiseeq/aguard/internal/record"
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
	// Offset — откуда читать: конец прочитанного или начало строки незаконченного ожидания.
	Offset int64 `json:"offset"`
	// Seen — записанные ожидания, начатые не раньше Offset: id первого вызова → начало. Перечитывание с Offset отдаёт
	// их снова, по Seen они не повторяются.
	Seen map[string]int64 `json:"seen,omitempty"`
}

// State — смещения по транскриптам. Started — первый проход был: новые файлы читаются с начала.
type State struct {
	Started bool             `json:"started"`
	Files   map[string]*File `json:"files"`
}

// LoadState — состояние из файла; нет файла — пустое (первый проход).
func LoadState(path string) (*State, error) {
	st := &State{Files: map[string]*File{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
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
	if err := record.WriteFileAtomic(path, b, 0o600); err != nil {
		return fmt.Errorf("смещения ожиданий: %w", err)
	}
	return nil
}

// Pass — проход по транскриптам.
type Pass struct {
	Projects, Workers string
	Backfill          time.Duration
	Budget            int64 // байт за проход; первый файл прохода читается целиком
	MaxOpen           time.Duration
	// Name — имя сессии Claude по её папке (names.For); пусто — без имени.
	Name func(cwd string) string
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
	pending := func(f Source) int64 { return f.size - st.Files[f.Key].Offset }
	slices.SortStableFunc(files, func(a, b Source) int { return cmp.Compare(pending(a), pending(b)) })
	var out []Wait
	var errs []error
	var used int64
	for _, f := range files {
		n := pending(f)
		if n <= 0 {
			continue
		}
		if used > 0 && used+n > p.Budget {
			break
		}
		used += n
		ws, err := p.read(f, st.Files[f.Key], now)
		out = append(out, ws...)
		errs = append(errs, err)
	}
	return out, errors.Join(errs...)
}

// chunk — кусок одного файла за проход: распознаватель, начало и время каждой строки, законченные ожидания.
type chunk struct {
	src    Source
	s      *File
	rec    *waits.Recognizer
	seen   map[string]int64 // ожидания этого прохода и прошлых, начатые после смещения: id первого вызова → начало
	parser *transcript.Parser
	nameOf func(cwd string) string
	starts []int64
	times  []time.Time
	out    []Wait
	name   string
	last   time.Time
	bad    int
	badErr error // первая битая строка
}

// line — начало и время строки n (с 1) куска.
func (c *chunk) line(n int) (int64, time.Time) { return c.starts[n-1], c.times[n-1] }

// keep — законченные ожидания, кроме записанных в прошлый проход (перечитанный кусок отдаёт их снова).
func (c *chunk) keep(found []waits.Wait) {
	for i := range found {
		w := &found[i]
		key := waitKey(w)
		if _, dup := c.seen[key]; dup {
			continue
		}
		c.seen[key], _ = c.line(w.Line)
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
	evs, err := c.parser.Parse(raw)
	if err != nil { // строка дописывается прямо сейчас или испорчена: считаем, первую — в ошибку прохода
		c.bad++
		if c.badErr == nil {
			c.badErr = fmt.Errorf("строка %d куска: %w", len(c.starts)+1, err)
		}
	}
	for i := range evs {
		c.last = cmp.Or(evs[i].Time, c.last)
		if c.name == "" && evs[i].Cwd != "" && c.nameOf != nil {
			c.name = c.nameOf(evs[i].Cwd)
		}
	}
	c.starts, c.times = append(c.starts, pos), append(c.times, c.last)
	for i := range evs {
		evs[i].Line = len(c.starts)
		c.keep(c.rec.Add(evs[i]))
	}
}

// read — законченные ожидания из [s.Offset, конец последней целой строки) файла f.
func (p *Pass) read(f Source, s *File, now time.Time) ([]Wait, error) {
	fh, err := os.Open(f.Path)
	if err != nil {
		return nil, fmt.Errorf("транскрипт: %w", err)
	}
	defer fh.Close()
	if f.size < s.Offset { // файл переписан короче: заново с начала
		s.Offset, s.Seen = 0, nil
	}
	c := &chunk{src: f, s: s, rec: waits.New(), parser: transcript.NewParser(), nameOf: p.Name, seen: map[string]int64{}}
	maps.Copy(c.seen, s.Seen)
	r := bufio.NewReaderSize(io.NewSectionReader(fh, s.Offset, f.size-s.Offset), 1<<20)
	pos := s.Offset
	for {
		raw, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			raw, err = readLong(r, raw)
		}
		if errors.Is(err, io.EOF) { // недописанный хвост — следующему проходу
			break
		}
		if err != nil {
			return c.out, fmt.Errorf("транскрипт %s: %w", f.Key, err)
		}
		c.add(raw[:len(raw)-1], pos)
		pos += int64(len(raw))
	}
	next := pos
	if n, ok := c.rec.Open(); ok {
		if start, at := c.line(n); at.IsZero() || now.Sub(at) < p.MaxOpen {
			next = start
		} else {
			c.keep(c.rec.Close())
		}
	}
	s.Offset, s.Seen = next, nil
	for k, start := range c.seen { // перечитываться будет только с next: раньше начатые не вернутся
		if start >= next {
			if s.Seen == nil {
				s.Seen = map[string]int64{}
			}
			s.Seen[k] = start
		}
	}
	if c.bad > 0 {
		return c.out, fmt.Errorf("транскрипт %s: строк не JSON %d (%w)", f.Key, c.bad, c.badErr)
	}
	return c.out, nil
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
