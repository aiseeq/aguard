package restart

import (
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
	// HookTasks — на последнем Stop Claude Code перечислил фоновые задачи (файл <сессия>.tasks chooks).
	HookTasks bool
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

	// Errors — что не прочиталось; любое — сомнение, не перезапускать.
	Errors []string
}

// paths — где cl ищет сведения о сессиях; в тестах — временные папки.
type paths struct {
	sessions   string // реестр Claude Code
	projects   string // транскрипты Claude Code
	hookStates string // состояния chooks (internal/core/state.go: $XDG_RUNTIME_DIR/chooks/sessions)
	proc       string
	claude     string // claude из PATH
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

// gitStatus — строки `git status --porcelain` папки: изменённые и новые файлы; пусто — дерево чистое.
func gitStatus(dir string) ([]string, error) {
	out, err := exec.Command("git", "-C", dir, "status", "--porcelain").Output()
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

// probe собирает сведения о сессии процесса pid; cache — прошлый разбор транскрипта этой сессии. Версия свежая —
// дальше не смотрит: перезапускать незачем.
func probe(p paths, cache *scanCache, pid int, now time.Time) facts {
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
	if !old {
		return f
	}
	state(p, cache, pid, now, &f)
	return f
}

// state — всё о сессии, кроме версий: ход, ввод, дети, транскрипт, экран. f.Info уже прочитан.
func state(p paths, cache *scanCache, pid int, now time.Time, f *facts) {
	fail := func(err error) {
		f.Errors = append(f.Errors, err.Error())
	}
	s := f.Info
	latest := time.UnixMilli(s.StatusUpdatedAt)
	hook, err := readHookState(p, s.SessionID)
	if err != nil {
		fail(err)
	}
	f.HookState, f.HookTasks, latest = hook.state, hook.tasks, later(latest, hook.written)
	transcript, err := transcriptFile(p, s.SessionID)
	if err != nil {
		fail(err)
		return
	}
	info, err := os.Stat(transcript)
	if err != nil {
		fail(fmt.Errorf("транскрипт: %w", err))
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
	if f.Children, err = descendants(p.proc, pid); err != nil {
		fail(err)
	}
	if f.Prompt, err = p.prompt(pid); err != nil {
		f.PromptErr = err.Error()
	}
}

// hookRecord — запись chooks о сессии.
type hookRecord struct {
	state   string    // done, idle, waiting, paused, working, error; пусто — записи нет
	tasks   bool      // есть <сессия>.tasks: на последнем Stop Claude Code перечислил фоновые задачи
	written time.Time // время записи состояния
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
	_, err = os.Stat(file + ".tasks")
	switch {
	case err == nil:
		rec.tasks = true
	case !errors.Is(err, fs.ErrNotExist):
		return rec, fmt.Errorf("задачи chooks: %w", err)
	}
	return rec, nil
}

// transcriptFile — транскрипт сессии в любой папке проектов: имя папки Claude Code выводит из cwd сам.
func transcriptFile(p paths, sessionID string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(p.projects, "*", filepath.Base(sessionID)+".jsonl"))
	if err != nil {
		return "", fmt.Errorf("транскрипт: %w", err)
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("транскрипт сессии %s: нашлось %d файлов", sessionID, len(matches))
	}
	return matches[0], nil
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

// descendants — все потомки pid по /proc/*/stat: «pid имя». Процесс, вышедший между списком и чтением, — пропуск.
func descendants(proc string, pid int) ([]string, error) {
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
