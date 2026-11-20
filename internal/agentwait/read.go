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
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/aiseeq/akit/transcript"
	"github.com/aiseeq/akit/transcript/waits"

	"github.com/aiseeq/aguard/internal/statefile"
)

// Source — транскрипт: Claude (сессия или её суб-агент) или боец codex.
type Source struct {
	Path, Key, Runner, Session, Agent, Project string
	size                                       int64
	mtime                                      time.Time
}

type workerCache struct {
	RootMod int64                  `json:"root_mod"`
	Tasks   map[string]*workerTask `json:"tasks,omitempty"`
}

type workerTask struct {
	StatusMod    int64              `json:"status_mod,omitempty"`
	StatusSize   int64              `json:"status_size,omitempty"`
	Done         bool               `json:"done,omitempty"`
	Final        bool               `json:"final,omitempty"`
	StatusRead   bool               `json:"status_read,omitempty"`
	Ended        time.Time          `json:"ended,omitzero"`
	PruneCheckAt time.Time          `json:"prune_check_at,omitzero"`
	Sources      []workerTranscript `json:"sources,omitempty"`
}

type workerTranscript struct {
	Key   string `json:"key"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"`
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
	errs := []error{
		add(projects, "*/*.jsonl", RunnerClaude, claude),
		add(projects, "*/*/subagents/agent-*.jsonl", RunnerClaude, claude),
	}
	if workers != "" {
		errs = append(errs, add(workers, "*/codex/sessions/*/*/*/rollout-*.jsonl", RunnerCodex, codex))
	}
	return out, errors.Join(errs...)
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
	Started      bool             `json:"started"`
	Files        map[string]*File `json:"files"`
	Workers      workerCache      `json:"workers,omitzero"`
	workersDirty bool             `json:"-"`
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
	cacheBytes, err := statefile.Load(workerCachePath(path))
	if err == nil {
		if err := json.Unmarshal(cacheBytes, &st.Workers); err != nil {
			return nil, fmt.Errorf("кеш транскриптов worker: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("кеш транскриптов worker: %w", err)
	}
	return st, nil
}

// Save пишет состояние атомарно.
func (st *State) Save(path string) error {
	if st.workersDirty {
		cache, err := json.Marshal(st.Workers)
		if err != nil {
			return fmt.Errorf("кеш транскриптов worker: %w", err)
		}
		if err := statefile.Save(workerCachePath(path), cache); err != nil {
			return fmt.Errorf("кеш транскриптов worker: %w", err)
		}
		st.workersDirty = false
	}
	b, err := json.Marshal(struct {
		Started bool             `json:"started"`
		Files   map[string]*File `json:"files"`
	}{st.Started, st.Files})
	if err != nil {
		return fmt.Errorf("смещения ожиданий: %w", err)
	}
	if err := statefile.Save(path, b); err != nil {
		return fmt.Errorf("смещения ожиданий: %w", err)
	}
	return nil
}

func workerCachePath(path string) string { return strings.TrimSuffix(path, ".json") + ".workers.json" }

// Pass — проход по транскриптам.
type Pass struct {
	Projects, Workers string
	Backfill          time.Duration
	// Budget — байт за проход. Файл, не влезший в остаток, читается куском до остатка: больше бюджета не читался
	// бы никогда — первым в проходе его не ставят хвосты помельче (llmark 210 МБ 22.10, 27.10 14:47 CET).
	Budget  int64
	MaxOpen time.Duration
	// Name — имя сессии Claude по её папке (names.For); пусто — без имени.
	Name       func(cwd string) string
	openFile   func(string) (io.ReaderAt, io.Closer, error)
	listWorker func(root, task string) ([]Source, error)
	statStatus func(string) (os.FileInfo, error)
	statPruned func(string) (os.FileInfo, error)
}

const (
	workerPruneAfter     = 72 * time.Hour
	workerPruneRecheck   = 6 * time.Hour
	workerPrunedFilename = "pruned"
)

// Run читает дописанное и отдаёт законченные ожидания. Сначала файлы с меньшим непрочитанным: хвосты живых
// сессий не ждут, пока дочитается неделя старых. Ошибки отдельных файлов возвращаются вместе с ожиданиями;
// смещение такого файла не двигается.
func (p *Pass) Run(st *State, now time.Time) ([]Wait, error) {
	files, err := List(p.Projects, "")
	if err != nil {
		return nil, err
	}
	workerFiles, skipWorkers, workerErrs, err := p.workerSources(st, now)
	if err != nil {
		return nil, err
	}
	files = append(files, workerFiles...)
	files = prepareFiles(st, files, now, p.Backfill)
	var out []Wait
	errs := workerErrs
	var used int64
	for _, f := range files {
		if skipWorkers[f.Key] {
			s := st.Files[f.Key]
			if s.Offset >= f.size && !s.PendingBad {
				out = append(out, p.expireWorker(f, s, now)...)
				continue
			}
		}
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

func prepareFiles(st *State, files []Source, now time.Time, backfill time.Duration) []Source {
	seen := map[string]bool{}
	for _, f := range files {
		seen[f.Key] = true
		if st.Files[f.Key] == nil {
			s := &File{}
			if !st.Started && now.Sub(f.mtime) > backfill {
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
	return files
}

func (p *Pass) expireWorker(f Source, s *File, now time.Time) []Wait {
	expired, ok := p.expired(s.Pending, now)
	if !ok {
		return nil
	}
	out := make([]Wait, 0, len(expired))
	for i := range expired {
		out = append(out, f.wait(&expired[i], f.Project))
	}
	s.Pending, s.Seen, s.Line, s.PendingAt, s.OpenAt = nil, nil, 0, 0, nil
	return out
}

// workerSources caches each task's transcript list. A completed task is re-listed only after
// status.json changes; failed task refreshes leave that task's prior cache intact.
func (p *Pass) workerSources(st *State, now time.Time) ([]Source, map[string]bool, []error, error) {
	cache := &st.Workers
	root, err := os.Stat(p.Workers)
	if errors.Is(err, fs.ErrNotExist) {
		if len(cache.Tasks) > 0 || cache.RootMod != 0 {
			cache.Tasks, cache.RootMod = map[string]*workerTask{}, 0
			st.workersDirty = true
		}
		return nil, nil, nil, nil
	}
	if err != nil {
		return nil, nil, nil, fmt.Errorf("каталог worker: %w", err)
	}
	rootMod := root.ModTime().UnixNano()
	rootChanged := rootMod != cache.RootMod
	tasks := cache.Tasks
	if tasks == nil {
		tasks = map[string]*workerTask{}
	}
	if rootChanged {
		entries, err := os.ReadDir(p.Workers)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("каталог worker: %w", err)
		}
		next := make(map[string]*workerTask, len(entries))
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			next[e.Name()] = cache.Tasks[e.Name()]
		}
		tasks = next
	}
	var out []Source
	skip := map[string]bool{}
	var errs []error
	committed := make(map[string]*workerTask, len(tasks))
	untrackedError := false
	for _, name := range slices.Sorted(maps.Keys(tasks)) {
		old := tasks[name]
		known := old != nil
		if !known {
			old = &workerTask{}
		}
		candidate := *old
		candidate.Sources = slices.Clone(old.Sources)
		sources, skipTask, err := p.workerTaskSources(name, &candidate, now)
		if err != nil {
			errs = append(errs, err)
			if known {
				committed[name] = old
				out = append(out, sourcesFromCache(p.Workers, name, old.Sources)...)
			} else {
				untrackedError = true
			}
			continue
		}
		committed[name] = &candidate
		if !reflect.DeepEqual(old, &candidate) {
			st.workersDirty = true
		}
		out = append(out, sources...)
		if skipTask {
			for _, source := range sources {
				skip[source.Key] = true
			}
		}
	}
	if !sameTaskKeys(cache.Tasks, committed) {
		st.workersDirty = true
	}
	cache.Tasks = committed
	if rootChanged && !untrackedError {
		cache.RootMod = rootMod
		st.workersDirty = true
	}
	return out, skip, errs, nil
}

func sameTaskKeys(a, b map[string]*workerTask) bool {
	if len(a) != len(b) {
		return false
	}
	for key := range a {
		if b[key] == nil {
			return false
		}
	}
	return true
}

func (p *Pass) workerTaskSources(name string, task *workerTask, now time.Time) ([]Source, bool, error) {
	if task.Final {
		return sourcesFromCache(p.Workers, name, task.Sources), true, nil
	}
	statusPath := filepath.Join(p.Workers, name, "status.json")
	stat := p.statStatus
	if stat == nil {
		stat = os.Stat
	}
	info, err := stat(statusPath)
	if errors.Is(err, fs.ErrNotExist) {
		task.Done, task.Final = false, false
		task.StatusRead = false
		task.StatusMod, task.StatusSize = 0, 0
		return p.refreshWorkerSources(name, task)
	}
	if err != nil {
		return nil, false, fmt.Errorf("статус worker %s: %w", name, err)
	}
	changed := task.StatusMod != info.ModTime().UnixNano() || task.StatusSize != info.Size()
	// Старый формат кеша не сохранял ended. Перечитываем его один раз, даже если
	// mtime и размер совпали, иначе Done-задачи никогда не получат Final.
	legacyDone := task.Done && !task.StatusRead
	if changed || legacyDone {
		cachedSources := task.Sources
		if err := p.readWorkerStatus(name, statusPath, info, task); err != nil {
			return nil, false, err
		}
		if !changed {
			task.Sources = cachedSources
		}
	}
	if changed || !task.Done {
		sources, skip, err := p.refreshWorkerSources(name, task)
		if err != nil {
			return nil, false, err
		}
		// Сначала обновляем список источников для изменившегося status.json; только затем можно кешировать
		// окончательность по released_at, отсутствию thread или метке prune.
		if _, err := p.finalAfterPrune(name, task, now); err != nil {
			return nil, false, err
		}
		return sources, skip, nil
	}
	if final, err := p.finalAfterPrune(name, task, now); err != nil {
		return nil, false, err
	} else if final {
		return sourcesFromCache(p.Workers, name, task.Sources), true, nil
	}
	return sourcesFromCache(p.Workers, name, task.Sources), true, nil
}

// finalAfterPrune по ended ограничивает проверку метки задачами, которым уже пора в worker prune.
// Одного возраста мало: continue не вызывает autoPrune и может начаться до появления метки.
func (p *Pass) finalAfterPrune(name string, task *workerTask, now time.Time) (bool, error) {
	if !task.Done || task.Ended.IsZero() || now.Sub(task.Ended) <= workerPruneAfter ||
		(!task.PruneCheckAt.IsZero() && now.Sub(task.PruneCheckAt) < workerPruneRecheck) {
		return false, nil
	}
	stat := p.statPruned
	if stat == nil {
		stat = os.Stat
	}
	_, err := stat(filepath.Join(p.Workers, name, workerPrunedFilename))
	task.PruneCheckAt = now
	if err == nil {
		task.Final = true
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("метка worker prune %s: %w", name, err)
}

func (p *Pass) readWorkerStatus(name, path string, info os.FileInfo, task *workerTask) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("статус worker %s: %w", name, err)
	}
	var status struct {
		Phase      string    `json:"phase"`
		Thread     string    `json:"thread"`
		Ended      time.Time `json:"ended"`
		ReleasedAt time.Time `json:"released_at"`
	}
	if json.Unmarshal(b, &status) != nil {
		status.Phase = ""
	}
	task.Done = status.Phase == "готово"
	task.StatusRead = true
	task.Ended = status.Ended
	// Без thread продолжать нечего; ReleasedAt и метка prune worker тоже запрещают continue.
	task.Final = task.Done && (status.Thread == "" || !status.ReleasedAt.IsZero())
	task.StatusMod, task.StatusSize = info.ModTime().UnixNano(), info.Size()
	task.Sources = nil
	return nil
}

func (p *Pass) refreshWorkerSources(name string, task *workerTask) ([]Source, bool, error) {
	list := p.listWorker
	if list == nil {
		list = listWorkerTask
	}
	sources, err := list(p.Workers, name)
	if err != nil {
		return nil, false, fmt.Errorf("список транскриптов worker %s: %w", name, err)
	}
	cache := make([]workerTranscript, 0, len(sources))
	for _, source := range sources {
		cache = append(cache, workerTranscript{Key: source.Key, Size: source.size, Mtime: source.mtime.UnixNano()})
	}
	task.Sources = cache
	return sourcesFromCache(p.Workers, name, cache), false, nil
}

func sourcesFromCache(root, task string, sources []workerTranscript) []Source {
	out := make([]Source, 0, len(sources))
	for _, cached := range sources {
		rel := strings.TrimPrefix(cached.Key, RunnerCodex+"/")
		if rel == cached.Key {
			continue
		}
		base := filepath.Base(rel)
		out = append(out, Source{
			Path: filepath.Join(root, filepath.FromSlash(rel)), Key: cached.Key, Runner: RunnerCodex,
			Project: task, Session: strings.TrimSuffix(strings.TrimPrefix(base, "rollout-"), ".jsonl"),
			size: cached.Size, mtime: time.Unix(0, cached.Mtime),
		})
	}
	return out
}

func listWorkerTask(root, task string) ([]Source, error) {
	base := filepath.Join(root, task)
	paths, err := filepath.Glob(filepath.Join(base, "codex", "sessions", "*", "*", "*", "rollout-*.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("список транскриптов worker %s: %w", task, err)
	}
	out := make([]Source, 0, len(paths))
	for _, path := range paths {
		info, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("транскрипт %s: %w", path, err)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil, fmt.Errorf("путь транскрипта %s: %w", path, err)
		}
		out = append(out, Source{Path: path, Key: RunnerCodex + "/" + rel, Runner: RunnerCodex, Project: task,
			Session: strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "rollout-"), ".jsonl"), size: info.Size(), mtime: info.ModTime()})
	}
	return out, nil
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
