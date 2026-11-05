package said

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/aiseeq/akit/transcript"

	"github.com/aiseeq/aguard/internal/statefile"
	"github.com/aiseeq/aguard/internal/tail"
)

// Виды событий транскрипта.
const (
	evUser  = "user"  // реплика человека
	evAgent = "agent" // текст ответа агента: «в ответ на» для следующей реплики
)

// Event — реплика человека или текст агента из транскрипта.
type Event struct {
	At      time.Time
	Kind    string
	Text    string
	Cwd     string
	Session string
	Project string
	Name    string
	Line    int64
}

// markers — признаки строк, нужных said, до разбора JSON: реплика человека (и из очереди), окно вопроса (вызов —
// чтобы Parser узнал ответ, и сам ответ), текст агента. Остальное — вывод инструментов в мегабайты — не разбирается.
var (
	markers     = [][]byte{[]byte(`"kind":"human"`), []byte(`"AskUserQuestion"`), []byte(`"answers"`)}
	agentMarker = []byte(`"type":"assistant"`)
	textMarker  = []byte(`"type":"text"`)
	namePrefix  = []byte(`{"type":"agent-name","agentName":"`)
)

// Parse — строка транскрипта: реплика человека или его ответ в окне вопроса, текст агента или имя сессии.
// Parser помнит вызовы инструментов: один на проход.
func Parse(p *transcript.Parser, line []byte) (Event, bool, string, error) {
	if rest, ok := bytes.CutPrefix(line, namePrefix); ok {
		name, _, _ := bytes.Cut(rest, []byte(`"`))
		return Event{}, false, string(name), nil
	}
	if !slices.ContainsFunc(markers, func(m []byte) bool { return bytes.Contains(line, m) }) &&
		!(bytes.Contains(line, agentMarker) && bytes.Contains(line, textMarker)) {
		return Event{}, false, "", nil
	}
	evs, err := p.Parse(line)
	if err != nil {
		return Event{}, false, "", fmt.Errorf("строка транскрипта: %w", err)
	}
	var user, agent []string
	var out Event
	for i := range evs {
		e := &evs[i]
		if e.Sidechain || strings.TrimSpace(e.Text) == "" {
			continue
		}
		switch e.Role {
		case transcript.User, transcript.Ask:
			user = append(user, strings.TrimSpace(e.Text))
		case transcript.Agent:
			agent = append(agent, strings.TrimSpace(e.Text))
		default:
			continue
		}
		out.At, out.Cwd = e.Time, e.Cwd
	}
	switch {
	case len(user) > 0:
		out.Kind, out.Text = evUser, strings.Join(user, "\n")
	case len(agent) > 0:
		out.Kind, out.Text = evAgent, strings.Join(agent, "\n")
	default:
		return Event{}, false, "", nil
	}
	return out, true, "", nil
}

// NewTail — проход said: только транскрипты сессий, с номерами строк.
func NewTail(projects string) *tail.Tail[Event] {
	p := transcript.NewParser()
	return &tail.Tail[Event]{Projects: projects, Lines: true,
		Parse: func(line []byte) (Event, bool, string, error) { return Parse(p, line) },
		Stamp: func(ev *Event, src tail.Source) {
			ev.Session, ev.Project, ev.Name, ev.Line = src.Session, src.Project, src.Name, src.Line
		}}
}

// Pending — реплика, которую модель ещё не разметила.
type Pending struct {
	Event    Event  `json:"event"`
	InReply  string `json:"in_reply,omitempty"`
	Attempts int    `json:"attempts"`
}

// State — смещения транскриптов, последний текст агента по сессиям и очередь неразмеченного.
type State struct {
	Tail    *tail.State       `json:"tail"`
	Last    map[string]string `json:"last"` // сессия -> конец последнего текста агента
	Pending []Pending         `json:"pending,omitempty"`
	// Pause — модель не ответила: до этого момента не звать (сбой мощности не лечится частыми повторами).
	Pause time.Time `json:"pause,omitzero"`
}

// LoadState — состояние из файла; нет файла — пустое.
func LoadState(path string) (*State, error) {
	st := &State{Last: map[string]string{}}
	b, err := statefile.Load(path)
	if err != nil {
		return nil, fmt.Errorf("состояние said: %w", err)
	}
	if err := json.Unmarshal(b, st); err != nil {
		return nil, fmt.Errorf("состояние said %s: %w", path, err)
	}
	if st.Tail == nil {
		st.Tail = &tail.State{Files: map[string]*tail.File{}}
	}
	if st.Tail.Files == nil {
		st.Tail.Files = map[string]*tail.File{}
	}
	if st.Last == nil {
		st.Last = map[string]string{}
	}
	return st, nil
}

// Save пишет состояние атомарно.
func (st *State) Save(path string) error {
	b, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("состояние said: %w", err)
	}
	if err := statefile.Save(path, b); err != nil {
		return fmt.Errorf("состояние said: %w", err)
	}
	return nil
}

// Collect — разобрать события прохода: текст агента запоминается, реплики наблюдаемых сессий — в очередь.
func (c *Config) Collect(st *State, evs []Event) {
	for i := range evs {
		ev := evs[i]
		switch ev.Kind {
		case evAgent:
			if !c.Watched(ev.Name) {
				continue
			}
			r := []rune(ev.Text)
			st.Last[ev.Session] = string(r[max(0, len(r)-c.Context):])
		case evUser:
			if c.Watched(ev.Name) {
				st.Pending = append(st.Pending, Pending{Event: ev, InReply: st.Last[ev.Session]})
			}
		}
	}
}
