// Package bgtask — забытые фоновые задачи Bash сессий Claude Code. Задача — процессы, чей stdout — файл её вывода
// (claude-tmp/<проект>/<беседа>/tasks/<id>.output). Забытой считается обычная команда, которую Claude Code сам увёл
// в фон по таймауту (агент ждал её минуты), если она идёт дольше Age и вывод не меняется дольше Quiet. Задачи,
// запущенные фоном нарочно (run_in_background: ожидания прогонов, Сон, evals, бенчмарк), не трогаются: их вывод
// молчит часами по замыслу. Замер 20–27.10 (задание лида 27.10): из 10 таких уведённых по таймауту задач дольше
// часа все 10 — зависшие или забытые (медленный grep -o по jsonl, ssh с sudo, циклы pgrep -f), из 31 нарочно
// фоновой дольше часа почти все — намеренные ожидания с молчащим выводом.
package bgtask

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
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
	"github.com/aiseeq/aguard/internal/record"
	"github.com/aiseeq/akit/proc"
)

// Режимы: ModeOff — выключено, ModeLog — только журнал «написал бы», ModeAlert — письмо сессии-владельцу.
const (
	ModeOff   = "off"
	ModeLog   = "log"
	ModeAlert = "alert"
)

// Config — раздел [bgtask] конфига aguard.
type Config struct {
	Mode     string          `toml:"mode"`
	Interval policy.Duration `toml:"interval"`
	Age      policy.Duration `toml:"age"`   // задача идёт дольше этого
	Quiet    policy.Duration `toml:"quiet"` // и вывод не меняется дольше этого
	SendAuth bool            `toml:"-"`     // слать peerToken первой строкой (общий send_auth)
}

// Default — письма по задачам, идущим час с молчащим часом выводом; проход раз в 5 минут.
func Default() Config {
	return Config{Mode: ModeAlert, Interval: policy.Duration(5 * time.Minute), Age: policy.Duration(time.Hour),
		Quiet: policy.Duration(time.Hour)}
}

// Validate проверяет раздел.
func (c *Config) Validate() error {
	if c.Mode != ModeOff && c.Mode != ModeLog && c.Mode != ModeAlert {
		return fmt.Errorf("bgtask.mode = %q: ожидается %s, %s или %s", c.Mode, ModeOff, ModeLog, ModeAlert)
	}
	if c.Interval.D() < 10*time.Second {
		return fmt.Errorf("bgtask.interval = %v: не чаще раза в 10 с", c.Interval.D())
	}
	// Обычная команда ждёт до 10 минут (наибольший таймаут Bash), меньший порог задел бы ещё идущие.
	if c.Age.D() < 15*time.Minute || c.Quiet.D() < 15*time.Minute {
		return fmt.Errorf("bgtask: age = %v, quiet = %v: не меньше 15 мин", c.Age.D(), c.Quiet.D())
	}
	return nil
}

// Task — живая фоновая задача.
type Task struct {
	ID     string
	Output string // файл вывода
	// Holder — pid сессии Claude Code (CLAUDE_PID процессов задачи), 0 — не нашлась.
	Holder  int
	Start   time.Time // старт самого раннего процесса задачи
	Written time.Time // последняя запись в вывод
	Pids    []int
}

// Stale — задача идёт дольше age, и вывод не меняется дольше quiet.
func (t *Task) Stale(now time.Time, age, quiet time.Duration) bool {
	return now.Sub(t.Start) >= age && now.Sub(t.Written) >= quiet
}

var outputName = regexp.MustCompile(`/tasks/([A-Za-z0-9_-]+)\.output$`)

// Find — фоновые задачи среди процессов снимка своего uid: stdout процесса — файл вывода задачи.
func Find(snap *proc.Snapshot, procRoot string, uid int) ([]Task, error) {
	byOutput := map[string]*Task{}
	var errs []error
	for _, pid := range slices.Sorted(maps.Keys(snap.Procs)) {
		p := snap.Procs[pid]
		if p.UID != uid || p.Session == 0 {
			continue
		}
		link, err := os.Readlink(filepath.Join(procRoot, fmt.Sprint(p.Pid), "fd", "1"))
		if errors.Is(err, fs.ErrNotExist) {
			continue // процесс ушёл или stdout закрыт: не задача
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("stdout процесса %d: %w", p.Pid, err))
			continue
		}
		m := outputName.FindStringSubmatch(link)
		if m == nil {
			continue
		}
		t := byOutput[link]
		if t == nil {
			info, err := os.Stat(link)
			if err != nil {
				errs = append(errs, fmt.Errorf("вывод задачи %s: %w", m[1], err))
				continue
			}
			t = &Task{ID: m[1], Output: link, Start: p.StartAt, Written: info.ModTime()}
			if h := snap.Holder(p); h != nil {
				t.Holder = h.Pid
			}
			byOutput[link] = t
		}
		t.Pids = append(t.Pids, p.Pid)
		if p.StartAt.Before(t.Start) {
			t.Start = p.StartAt
		}
	}
	out := make([]Task, 0, len(byOutput))
	for _, t := range byOutput {
		slices.Sort(t.Pids)
		out = append(out, *t)
	}
	slices.SortFunc(out, func(a, b Task) int { return strings.Compare(a.ID, b.ID) })
	return out, errors.Join(errs...)
}

// Origin — как задача попала в фон, по транскрипту беседы.
type Origin struct {
	Found bool // результат вызова с этим id нашёлся
	// Auto — обычная команда, которую Claude Code увёл в фон по таймауту (timedOutAfterMs); иначе запущена фоном.
	Auto        bool
	Description string
	Command     string
	Agent       string // id суб-агента, если задачу запустил он; пусто — сама сессия
}

// FindOrigin ищет вызов задачи в транскриптах беседы, которой принадлежит вывод: <projects>/<проект>/<беседа>.jsonl
// и транскрипты её суб-агентов.
func FindOrigin(projects string, t *Task) (Origin, error) {
	sessionDir := filepath.Dir(filepath.Dir(t.Output))
	project, session := filepath.Base(filepath.Dir(sessionDir)), filepath.Base(sessionDir)
	files := []string{filepath.Join(projects, project, session+".jsonl")}
	subs, err := filepath.Glob(filepath.Join(projects, project, session, "subagents", "*.jsonl"))
	if err != nil {
		return Origin{}, fmt.Errorf("транскрипты суб-агентов: %w", err)
	}
	files = append(files, subs...)
	for _, f := range files {
		o, err := originIn(f, t.ID)
		if err != nil {
			return Origin{}, err
		}
		if o.Found {
			if f != files[0] {
				o.Agent = strings.TrimPrefix(strings.TrimSuffix(filepath.Base(f), ".jsonl"), "agent-")
			}
			return o, nil
		}
	}
	return Origin{}, nil
}

// event — нужное из строки транскрипта: блоки вызова и результата, результат вызова.
type event struct {
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	ToolUseResult json.RawMessage `json:"toolUseResult"`
}

type block struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	ToolUseID string `json:"tool_use_id"`
	Input     struct {
		Command     string `json:"command"`
		Description string `json:"description"`
	} `json:"input"`
}

// originIn — вызов задачи id в файле: сначала результат (в нём id задачи), затем сам вызов по его id.
func originIn(file, id string) (Origin, error) {
	var o Origin
	var useID string
	err := eachLine(file, []byte(`"backgroundTaskId":"`+id+`"`), func(line []byte) bool {
		var ev event
		if json.Unmarshal(line, &ev) != nil {
			return true
		}
		var res struct {
			ID       string `json:"backgroundTaskId"`
			TimedOut int64  `json:"timedOutAfterMs"`
		}
		if json.Unmarshal(ev.ToolUseResult, &res) != nil || res.ID != id {
			return true
		}
		var blocks []block
		if json.Unmarshal(ev.Message.Content, &blocks) != nil {
			return true
		}
		for _, b := range blocks {
			if b.Type == "tool_result" {
				o.Found, o.Auto, useID = true, res.TimedOut > 0, b.ToolUseID
				return false
			}
		}
		return true
	})
	if err != nil || !o.Found {
		return o, err
	}
	err = eachLine(file, []byte(`"id":"`+useID+`"`), func(line []byte) bool {
		var ev event
		if json.Unmarshal(line, &ev) != nil {
			return true
		}
		var blocks []block
		if json.Unmarshal(ev.Message.Content, &blocks) != nil {
			return true
		}
		for _, b := range blocks {
			if b.Type == "tool_use" && b.ID == useID {
				o.Description, o.Command = b.Input.Description, b.Input.Command
				return false
			}
		}
		return true
	})
	return o, err
}

// eachLine отдаёт fn строки файла, где есть want, пока fn не вернёт false. Файла нет — строк нет.
func eachLine(file string, want []byte, fn func([]byte) bool) error {
	f, err := os.Open(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("транскрипт: %w", err)
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if bytes.Contains(line, want) && !fn(line) {
			return nil
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("транскрипт %s: %w", file, err)
		}
	}
}

// Message — письмо сессии-владельцу, одной строкой.
func Message(t *Task, o *Origin, now time.Time) string {
	what := "фоновая задача " + t.ID
	label := o.Description
	if label == "" {
		label = short(o.Command)
	}
	if label != "" {
		what += " «" + label + "»"
	}
	if o.Agent != "" {
		what += " суб-агента " + o.Agent
	}
	return fmt.Sprintf("aguard: %s идёт %s, вывод не меняется %s; это обычная команда, которую Claude Code увёл в фон "+
		"по таймауту. Нужна ли? Если нет — TaskStop %s.", what, Span(now.Sub(t.Start)), Span(now.Sub(t.Written)), t.ID)
}

// Span — длительность словами: «1 ч 05 мин», «42 мин».
func Span(d time.Duration) string {
	m := int(d.Minutes())
	if m < 60 {
		return fmt.Sprintf("%d мин", m)
	}
	return fmt.Sprintf("%d ч %02d мин", m/60, m%60)
}

func short(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 60 {
		return string(r[:60]) + "…"
	}
	return s
}

// Seen — решение по задаче: по разу на задачу, пока она жива.
type Seen struct {
	At   time.Time `json:"at"`
	Auto bool      `json:"auto"` // уведена в фон по таймауту: письмо (или «написал бы» в log)
}

// State — решённые задачи по id; хранится в state.json папки наблюдателя.
type State map[string]Seen

// LoadState читает состояние; файла нет — пустое.
func LoadState(file string) (State, error) {
	b, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("состояние bgtask: %w", err)
	}
	s := State{}
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("состояние bgtask %s: %w", file, err)
	}
	return s, nil
}

// Save пишет состояние без задач, которых больше нет среди alive.
func (s State) Save(file string, alive []Task) error {
	keep := State{}
	for _, t := range alive {
		if seen, ok := s[t.ID]; ok {
			keep[t.ID] = seen
		}
	}
	b, err := json.Marshal(keep)
	if err != nil {
		return fmt.Errorf("состояние bgtask: %w", err)
	}
	if err := record.WriteFileAtomic(file, b, 0o600); err != nil {
		return fmt.Errorf("состояние bgtask: %w", err)
	}
	return nil
}
