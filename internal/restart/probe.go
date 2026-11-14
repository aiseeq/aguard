package restart

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aiseeq/aguard/internal/screen"
	"github.com/aiseeq/akit/session"
)

// facts — всё, что известно о живой сессии к решению о перезапуске.
type facts struct {
	session.Info        // запись реестра Claude Code (~/.claude/sessions/<pid>.json)
	Running      string // версия бинаря процесса
	Installed    string // версия, которую запустит claude из PATH
	// HookState — состояние от chooks: done и idle (ход закончен, idle — человек открыл вкладку), waiting (вопрос
	// человеку), paused (ждёт фоновые задачи или таймеры), working, error (ход оборвала ошибка API); пусто — записи
	// нет.
	HookState string
	// HookTasks — на последнем Stop Claude Code перечислил фоновые задачи (файл <сессия>.tasks chooks), кроме монитора
	// артефакта: его Claude Code ставит сам при публикации Artifact, срока у него нет, он только ловит комментарии на
	// странице, и после подъёма его можно поставить заново (лид 27.10: держал довод против перезапуска лида с 05:01).
	HookTasks bool
	// HookWaits — paused объяснён без задач: таймеры и расписания по chooks или монитор артефакта.
	HookWaits bool
	// HookAgents — среди задач HookTasks суб-агенты или workflow (типы agentTypes в chooks internal/core/background.go).
	HookAgents bool
	// Quiet — сколько времени не менялись транскрипт, состояние chooks и статус в реестре.
	Quiet time.Duration
	// TTYQuiet — сколько времени в терминал сессии не приходил ввод (atime pts: клавиатура, фокус окна).
	TTYQuiet time.Duration
	// TurnQuiet — сколько времени не менялся транскрипт: последняя запись хода.
	TurnQuiet time.Duration
	// Children — потомки процесса claude: фоновые команды Bash и их дети, хуки, MCP-серверы.
	Children   []string
	Transcript transcriptFacts
	// Prompt — поле ввода по экрану терминала; PromptErr — почему экран не прочитался.
	Prompt    screen.Prompt
	PromptErr string
	// Suggestion — текст в поле не набран, а это серая подсказка Claude Code (prompt suggestion): по тексту экрана
	// её не отличить, но после отправки последней реплики typed поле пусто, а ввода в терминал с тех пор не было
	// (atime pts раньше реплики). Реплика queued не в счёт: её Enter раньше записи, набранное между ними не видно.
	Suggestion bool
	// Clean — cl видел весь ввод человека, текста после последней отправки нет (Request.Clean): atime pts, который
	// двигают и фокус окна, и ответы терминала, тогда не нужен.
	Clean bool

	// Errors — что не прочиталось; любое — сомнение, не перезапускать.
	Errors []string
}

// paths — где cl ищет сведения о сессиях; в тестах — временные папки.
type paths struct {
	sessions     string // реестр Claude Code
	projects     string // транскрипты Claude Code
	hookStates   string // состояния chooks (internal/core/state.go: $XDG_RUNTIME_DIR/chooks/sessions)
	proc         string
	scanChildren func(string, int) ([]string, error)
	claude       string // claude из PATH
	// prompt — поле ввода по экрану терминала процесса (screen.Reader.Prompt).
	prompt func(pid int) (screen.Prompt, error)
	// tree — незакоммиченное в git-дереве папки (gitStatus); спрашивается только для решения о сбросе.
	tree func(dir string) ([]string, error)
}

// pathsFor — пути живой машины: реестр и транскрипты в home, остальное — из раздела [restart].
func pathsFor(home string, cfg *Config) paths {
	return paths{
		sessions:   filepath.Join(home, ".claude", "sessions"),
		projects:   filepath.Join(home, ".claude", "projects"),
		hookStates: cfg.HookStates,
		proc:       "/proc",
		claude:     cfg.Claude,
		prompt:     screen.NewReader().Prompt,
		tree:       gitStatus,
	}
}

// gitStatus — строки `git status --porcelain` папки: изменённые и новые файлы; пусто — дерево чистое. Папка не в
// репозитории — тоже пусто: незакоммиченного нет (папки админов ~/git, ~/work и лида ~ — не репозитории; до 27.10
// ошибка git держала сброс git-admin: шесть отказов «tree» за день, вместо сброса cl сжал беседу в 12:27 CET).
func gitStatus(dir string) ([]string, error) {
	cmd := exec.Command("git", "-C", dir, "status", "--porcelain")
	cmd.Env = append(os.Environ(), "LC_ALL=C") // notRepo читает сообщение git по-английски
	out, err := cmd.Output()
	if ee, ok := errors.AsType[*exec.ExitError](err); ok && notRepo(ee.Stderr) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("git status %s: %w", dir, err)
	}
	var dirty []string
	for l := range strings.Lines(string(out)) {
		if l = strings.TrimSpace(l); l != "" {
			dirty = append(dirty, l)
		}
	}
	return dirty, nil
}

// notRepo — git сказал, что папка не в репозитории.
func notRepo(stderr []byte) bool {
	return bytes.Contains(stderr, []byte("not a git repository"))
}

// readSession — запись реестра Claude Code в папке dir о процессе pid; записи нет — ошибка.
func readSession(dir string, pid int) (session.Info, error) {
	in, err := session.Registry{Dir: dir}.Get(pid)
	switch {
	case err != nil:
		return session.Info{}, fmt.Errorf("реестр сессий: %w", err)
	case in == nil:
		return session.Info{}, fmt.Errorf("реестр сессий: нет записи о процессе %d", pid)
	}
	return *in, nil
}

// probe собирает сведения о сессии процесса pid; cache — прошлый разбор транскрипта этой сессии. Версия свежая и не
// stale — дальше не смотрит: перезапускать незачем. stale — у процесса устарели аргументы (Request.Stale): состояние
// нужно и при свежей версии, иначе decide видел бы нулевую тишину и пустой экран и всегда был бы против.
func probe(p paths, cache *scanCache, pid int, now time.Time, stale bool) facts {
	return probeWithChildren(p, cache, pid, now, stale, &descendantsCache{})
}

func probeWithChildren(p paths, cache *scanCache, pid int, now time.Time, stale bool, children *descendantsCache) facts {
	var f facts
	fail := func(err error) {
		f.Errors = append(f.Errors, err.Error())
	}
	s, err := readSession(p.sessions, pid)
	if err != nil {
		fail(err)
		return f
	}
	f.Info = s
	if f.Running, err = runningVersion(p.proc, pid); err != nil {
		fail(err)
	}
	if f.Installed, err = InstalledVersion(p.claude); err != nil {
		fail(err)
	}
	if len(f.Errors) > 0 {
		return f
	}
	old, err := older(f.Running, f.Installed)
	if err != nil {
		fail(err)
		return f
	}
	if !old && !stale {
		return f
	}
	state(p, cache, pid, now, &f, children)
	return f
}

// state — всё о сессии, кроме версий: ход, ввод, дети, транскрипт, экран. f.Info уже прочитан.
func state(p paths, cache *scanCache, pid int, now time.Time, f *facts, children *descendantsCache) {
	fail := func(err error) {
		f.Errors = append(f.Errors, err.Error())
	}
	s := f.Info
	latest := time.UnixMilli(s.StatusUpdatedAt)
	hook, err := readHookState(p, s.SessionID)
	if err != nil {
		fail(err)
	}
	f.HookState, f.HookTasks, f.HookWaits, f.HookAgents = hook.state, hook.tasks, hook.waits, hook.agents
	latest = later(latest, hook.written)
	transcript, info, err := transcriptFile(p, s.SessionID)
	if err != nil {
		fail(err)
		return
	}
	latest = later(latest, info.ModTime())
	f.TurnQuiet = now.Sub(info.ModTime())
	if f.Transcript, err = cache.read(transcript, info, time.UnixMilli(s.StartedAt), now); err != nil {
		fail(err)
	}
	f.Quiet = now.Sub(latest)
	if f.TTYQuiet, err = ttyQuiet(p, pid, now); err != nil {
		fail(err)
	}
	if f.Children, err = children.get(p.proc, pid, p.scanChildren); err != nil {
		fail(err)
	}
	if f.Prompt, err = p.prompt(pid); err != nil {
		f.PromptErr = err.Error()
	}
	c := f.Transcript.Context
	f.Suggestion = f.Prompt == screen.Draft && c.Human == "typed" && f.TTYQuiet > now.Sub(c.HumanAt)
}

// cleanField — cl видел, что человек текста не набирал (Request.Clean): текст в поле по экрану — подсказка.
func (f *facts) cleanField(clean bool) {
	f.Clean = clean
	if clean && f.Prompt == screen.Draft {
		f.Suggestion = true
	}
}

// hookRecord — запись chooks о сессии.
type hookRecord struct {
	state   string    // done, idle, waiting, paused, working, error; пусто — записи нет
	tasks   bool      // в <сессия>.tasks есть фоновые задачи, кроме мониторов артефактов
	agents  bool      // среди них суб-агенты или workflow
	waits   bool      // в <сессия>.tasks таймеры, расписания или мониторы артефактов
	written time.Time // время записи состояния
}

// hookBackground — <сессия>.tasks chooks (chooks internal/core/background.go, Background).
type hookBackground struct {
	Tasks []struct {
		Type        string `json:"type"`
		Description string `json:"description"`
	} `json:"tasks"`
	Crons     int `json:"crons"`
	Schedules int `json:"schedules"`
}

// artifactMonitor — монитор комментариев Artifact, который Claude Code ставит сам при публикации.
func artifactMonitor(typ, description string) bool {
	return typ == "monitor" && strings.HasPrefix(description, "live updates for artifact ") &&
		strings.HasSuffix(description, "(auto-armed on publish)")
}

// readHookState читает состояние chooks; записи нет — пустое состояние без ошибки (решение его не примет).
func readHookState(p paths, sessionID string) (hookRecord, error) {
	file := filepath.Join(p.hookStates, filepath.Base(sessionID))
	info, err := os.Stat(file)
	if errors.Is(err, fs.ErrNotExist) {
		return hookRecord{}, nil
	}
	if err != nil {
		return hookRecord{}, fmt.Errorf("состояние chooks: %w", err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return hookRecord{}, fmt.Errorf("состояние chooks: %w", err)
	}
	rec := hookRecord{state: strings.TrimSpace(string(data)), written: info.ModTime()}
	data, err = os.ReadFile(file + ".tasks")
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return rec, nil
	case err != nil:
		return rec, fmt.Errorf("задачи chooks: %w", err)
	}
	var bg hookBackground
	if err := json.Unmarshal(data, &bg); err != nil {
		rec.tasks = true // не прочиталось — сомнение в пользу задач
		return rec, fmt.Errorf("задачи chooks %s: %w", file+".tasks", err)
	}
	rec.waits = bg.Crons+bg.Schedules > 0
	for _, t := range bg.Tasks {
		if artifactMonitor(t.Type, t.Description) {
			rec.waits = true
			continue
		}
		rec.tasks = true
		rec.agents = rec.agents || t.Type == "subagent" || t.Type == "workflow"
	}
	return rec, nil
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// ttyQuiet — сколько не было ввода в терминал процесса: ядро ставит atime pts при чтении ввода.
func ttyQuiet(p paths, pid int, now time.Time) (time.Duration, error) {
	tty, err := os.Readlink(filepath.Join(p.proc, strconv.Itoa(pid), "fd", "0"))
	if err != nil {
		return 0, fmt.Errorf("терминал процесса: %w", err)
	}
	if !strings.HasPrefix(tty, "/dev/pts/") && !strings.HasPrefix(tty, "/dev/tty") {
		return 0, fmt.Errorf("ввод процесса не терминал: %s", tty)
	}
	info, err := os.Stat(tty)
	if err != nil {
		return 0, fmt.Errorf("терминал процесса: %w", err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("терминал процесса: нет atime у %s", tty)
	}
	return now.Sub(time.Unix(st.Atim.Sec, st.Atim.Nsec)), nil
}

type descendantsCache struct {
	loaded bool
	pid    int
	proc   string
	items  []string
	err    error
}

func (c *descendantsCache) get(proc string, pid int, scan func(string, int) ([]string, error)) ([]string, error) {
	if c.loaded && c.pid == pid && c.proc == proc {
		return slices.Clone(c.items), c.err
	}
	c.loaded, c.pid, c.proc = true, pid, proc
	if scan == nil {
		scan = descendants
	}
	c.items, c.err = scan(proc, pid)
	return slices.Clone(c.items), c.err
}

// descendants — потомки pid по /proc/<pid>/task/<tid>/children: «pid имя». Ядро без CONFIG_PROC_CHILDREN — прежний
// полный обход /proc без строки в журнал: у пакета нет журнала, а вердикт от способа обхода не зависит.
func descendants(proc string, pid int) ([]string, error) {
	items, err := descendantsSubtree(proc, pid)
	if errors.Is(err, errNoProcChildren) {
		return descendantsFull(proc, pid)
	}
	return items, err
}

var errNoProcChildren = errors.New("нет /proc task children")

// descendantsSubtree читает списки детей только у потоков процессов поддерева pid.
func descendantsSubtree(proc string, pid int) ([]string, error) {
	out := make([]string, 0)
	seen := map[int]bool{pid: true}
	queue := []int{pid}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		kids, err := processChildren(proc, parent)
		if err != nil {
			return nil, err
		}
		for _, child := range kids {
			if seen[child] {
				continue
			}
			name, ok, err := childName(proc, child, parent)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			seen[child] = true
			queue = append(queue, child)
			out = append(out, strconv.Itoa(child)+" "+name)
		}
	}
	slices.Sort(out)
	return out, nil
}

// goneErr — процесса или потока уже нет: ENOENT или ESRCH.
func goneErr(err error) bool { return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) }

// missingOrNoChildren — файла нет: вышел ли path (пропуск) или ядро не даёт children (errNoProcChildren).
func missingOrNoChildren(path, what string) (gone bool, err error) {
	if _, statErr := os.Stat(path); goneErr(statErr) {
		return true, nil
	} else if statErr != nil {
		return false, fmt.Errorf("%s: %w", what, statErr)
	}
	return false, errNoProcChildren
}

// processChildren — дети процесса parent по children всех его потоков; вышедший процесс — пусто.
func processChildren(proc string, parent int) ([]int, error) {
	taskDir := filepath.Join(proc, strconv.Itoa(parent), "task")
	tasks, err := os.ReadDir(taskDir)
	if goneErr(err) {
		_, err := missingOrNoChildren(filepath.Join(proc, strconv.Itoa(parent)), fmt.Sprintf("процесс %d", parent))
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("потоки процесса %d: %w", parent, err)
	}
	var kids []int
	for _, task := range tasks {
		if !isPID(task.Name()) {
			continue
		}
		got, err := threadChildren(filepath.Join(taskDir, task.Name()), task.Name())
		if err != nil {
			return nil, err
		}
		kids = append(kids, got...)
	}
	return kids, nil
}

// threadChildren — дети одного потока из task/<tid>/children; вышедший поток — пусто.
func threadChildren(tid, name string) ([]int, error) {
	data, err := os.ReadFile(filepath.Join(tid, "children"))
	if errors.Is(err, fs.ErrNotExist) {
		_, err := missingOrNoChildren(tid, "поток "+name)
		return nil, err
	}
	if errors.Is(err, syscall.ESRCH) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("потомки потока %s: %w", name, err)
	}
	var kids []int
	for field := range strings.FieldsSeq(string(data)) {
		child, err := strconv.Atoi(field)
		if err != nil {
			return nil, fmt.Errorf("потомок потока %s: %w", name, err)
		}
		kids = append(kids, child)
	}
	return kids, nil
}

// childName — имя ребёнка из /proc/<child>/stat; ok=false — вышел или pid переиспользован (ppid не parent).
func childName(proc string, child, parent int) (string, bool, error) {
	stat, err := os.ReadFile(filepath.Join(proc, strconv.Itoa(child), "stat"))
	if goneErr(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("процесс %d: %w", child, err)
	}
	name, ppid, err := parseStat(string(stat))
	if err != nil {
		return "", false, fmt.Errorf("процесс %d: %w", child, err)
	}
	if ppid != parent {
		return "", false, nil // PID переиспользован после чтения children
	}
	return name, true, nil
}

// descendantsFull — прежний полный обход /proc, нужен ядрам без CONFIG_PROC_CHILDREN.
func descendantsFull(proc string, pid int) ([]string, error) {
	entries, err := os.ReadDir(proc)
	if err != nil {
		return nil, fmt.Errorf("процессы: %w", err)
	}
	children := map[int][]int{}
	comm := map[int]string{}
	for _, e := range entries {
		if !isPID(e.Name()) {
			continue // не процесс: /proc/self, /proc/sys и прочее
		}
		child, err := strconv.Atoi(e.Name())
		if err != nil {
			return nil, fmt.Errorf("процесс %s: %w", e.Name(), err)
		}
		data, err := os.ReadFile(filepath.Join(proc, e.Name(), "stat"))
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("процесс %d: %w", child, err)
		}
		name, parent, err := parseStat(string(data))
		if err != nil {
			return nil, fmt.Errorf("процесс %d: %w", child, err)
		}
		children[parent] = append(children[parent], child)
		comm[child] = name
	}
	var out []string
	queue := slices.Clone(children[pid])
	for len(queue) > 0 {
		c := queue[0]
		queue = append(queue[1:], children[c]...)
		out = append(out, strconv.Itoa(c)+" "+comm[c])
	}
	slices.Sort(out)
	return out, nil
}

// parseStat — имя и родитель из /proc/<pid>/stat: «pid (имя) состояние ppid …»; имя может содержать скобки.
func parseStat(stat string) (string, int, error) {
	open, end := strings.IndexByte(stat, '('), strings.LastIndexByte(stat, ')')
	if open < 0 || end < open {
		return "", 0, fmt.Errorf("stat без имени: %q", stat)
	}
	fields := strings.Fields(stat[end+1:])
	if len(fields) < 2 {
		return "", 0, fmt.Errorf("stat без родителя: %q", stat)
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return "", 0, fmt.Errorf("stat: родитель %q: %w", fields[1], err)
	}
	return stat[open+1 : end], ppid, nil
}

// isPID — имя папки /proc из одних цифр.
func isPID(name string) bool {
	return name != "" && strings.Trim(name, "0123456789") == ""
}
