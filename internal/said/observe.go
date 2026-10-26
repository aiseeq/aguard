package said

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/record"
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

var (
	humanMarker = []byte(`"origin":{"kind":"human"`)
	agentMarker = []byte(`"type":"assistant"`)
	textMarker  = []byte(`"type":"text"`)
	nameMarker  = []byte(`"agent-name"`)
	// reminder — напоминание обвязки внутри реплики: его никто не набирал.
	reminder = regexp.MustCompile(`(?s)<system-reminder[^>]*>.*?</system-reminder[^>]*>`)
)

type rawLine struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	Cwd       string    `json:"cwd"`
	AgentName string    `json:"agentName"`
	Sidechain bool      `json:"isSidechain"`
	Origin    *struct {
		Kind string `json:"kind"`
	} `json:"origin"`
	Message *struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// text — слова содержимого сообщения: строка или части type=text.
func text(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type, Text string
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" && p.Text != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// Parse — строка транскрипта: реплика человека, текст агента или имя сессии.
func Parse(line []byte) (Event, bool, string, error) {
	human := bytes.Contains(line, humanMarker)
	agent := !human && bytes.Contains(line, agentMarker) && bytes.Contains(line, textMarker)
	if !human && !agent && !bytes.Contains(line, nameMarker) {
		return Event{}, false, "", nil
	}
	var r rawLine
	if err := json.Unmarshal(line, &r); err != nil {
		return Event{}, false, "", fmt.Errorf("строка транскрипта: %w", err)
	}
	switch {
	case r.Type == "agent-name":
		return Event{}, false, r.AgentName, nil
	case r.Sidechain || r.Message == nil:
		return Event{}, false, "", nil
	case r.Type == "user" && r.Origin != nil && r.Origin.Kind == "human":
		t := strings.TrimSpace(reminder.ReplaceAllString(text(r.Message.Content), ""))
		if t == "" {
			return Event{}, false, "", nil
		}
		return Event{At: r.Timestamp, Kind: evUser, Text: t, Cwd: r.Cwd}, true, "", nil
	case r.Type == "assistant":
		if t := strings.TrimSpace(text(r.Message.Content)); t != "" {
			return Event{At: r.Timestamp, Kind: evAgent, Text: t, Cwd: r.Cwd}, true, "", nil
		}
	}
	return Event{}, false, "", nil
}

// NewTail — проход said: только транскрипты сессий, с номерами строк.
func NewTail(projects string) *tail.Tail[Event] {
	return &tail.Tail[Event]{Projects: projects, Parse: Parse, Lines: true,
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
}

// LoadState — состояние из файла; нет файла — пустое.
func LoadState(path string) (*State, error) {
	st := &State{Last: map[string]string{}}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("состояние said: %w", err)
	default:
		if err := json.Unmarshal(b, st); err != nil {
			return nil, fmt.Errorf("состояние said %s: %w", path, err)
		}
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
	if err := record.WriteFileAtomic(path, b, 0o600); err != nil {
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
