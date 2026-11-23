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
	// Out — учётки OpenAI, отказавшие по кредитам: до этого момента не звать.
	Out map[string]time.Time `json:"out,omitempty"`
	// DownSince — с какого момента модель не отвечает; DownErr — её последняя ошибка, Skip — когда и почему её не позвали
	// (пересчёт на каждом проходе, исход вызова очищает). Простой из-за замера и из-за сбоя — разные причины: лиду нужны обе.
	DownSince time.Time `json:"down_since,omitzero"`
	DownErr   string    `json:"down_err,omitempty"`
	Skip      string    `json:"skip,omitempty"`
	// Signal — когда лиду ушло письмо о простое.
	Signal time.Time `json:"signal,omitzero"`
	// Pass и Mode — последний проход, дошедший до записи состояния, и его режим; Unread — ошибка чтения части
	// транскриптов в нём. По ним `aguard said status` видит стоящий демон, проход на ошибке и указатель, который не пишется.
	Pass   time.Time `json:"pass,omitzero"`
	Mode   string    `json:"mode,omitempty"`
	Unread string    `json:"unread,omitempty"`
}

// Down — модель позвали, она не ответила: начало простоя и последняя ошибка, причины не звать больше нет.
func (st *State) Down(now time.Time, why string) {
	st.downFrom(now)
	st.DownErr, st.Skip = why, ""
}

// Waiting — модель не зовём, учётки нет: начало простоя и нынешняя причина; ошибка модели остаётся.
func (st *State) Waiting(now time.Time, why string) {
	st.downFrom(now)
	st.Skip = now.Format(time.RFC3339) + ": " + why
}

func (st *State) downFrom(now time.Time) {
	if st.DownSince.IsZero() {
		st.DownSince = now
	}
}

// Why — причины простоя для письма.
func (st *State) Why() string {
	var parts []string
	if st.Skip != "" {
		parts = append(parts, "Проверка учёток "+st.Skip+".")
	}
	if st.DownErr != "" {
		parts = append(parts, "Последняя ошибка модели: "+st.DownErr)
	}
	return strings.Join(parts, " ")
}

// Lag — сколько указатель сказанного отстаёт от транскриптов: очередь неразмеченного и простой модели.
type Lag struct {
	Queue     int           `json:"queue"`
	Oldest    time.Time     `json:"oldest,omitzero"` // время старейшей реплики в очереди
	Behind    time.Duration `json:"behind_ns"`       // сколько она ждёт; 0 — очередь пуста
	Pass      time.Time     `json:"pass"`            // последний проход; нулевой — прохода с записью не было; есть всегда: по нему читатель отличает ответ status от справки старого бинаря
	Since     time.Duration `json:"since_pass_ns"`   // сколько с него прошло
	Mode      string        `json:"mode,omitempty"`
	Unread    string        `json:"unread,omitempty"`
	DownSince time.Time     `json:"down_since,omitzero"`
	Why       string        `json:"why,omitempty"`
}

// Lag — отставание на момент now по состоянию: очередь, последний проход, его режим и ошибки чтения.
func (st *State) Lag(now time.Time) Lag {
	l := Lag{Queue: len(st.Pending), Pass: st.Pass, Mode: st.Mode, Unread: st.Unread, DownSince: st.DownSince, Why: st.Why()}
	for i := range st.Pending {
		at := st.Pending[i].Event.At
		if at.IsZero() {
			continue // время реплики неизвестно — не старейшая
		}
		if l.Oldest.IsZero() || at.Before(l.Oldest) {
			l.Oldest = at
		}
	}
	if !l.Oldest.IsZero() && now.After(l.Oldest) {
		l.Behind = now.Sub(l.Oldest)
	}
	if !l.Pass.IsZero() && now.After(l.Pass) {
		l.Since = now.Sub(l.Pass)
	}
	return l
}

// Stale — почему указатель отстаёт больше max; пусто — не отстаёт или max не задан. Реплики, сказанные после
// последнего прохода, в очереди ещё нет: стоящий проход — отставание само по себе.
func (l Lag) Stale(max time.Duration) []string {
	if max <= 0 {
		return nil
	}
	var why []string
	switch {
	case l.Pass.IsZero():
		why = append(why, "прохода с записью состояния не было")
	case l.Since > max:
		why = append(why, fmt.Sprintf("последний проход %s, %s назад", l.Pass.Local().Format("02.01 15:04"), l.Since.Round(time.Minute)))
	}
	if !l.Pass.IsZero() && l.Mode != ModeWrite {
		why = append(why, fmt.Sprintf("режим %q: said.tsv не пишется", l.Mode))
	}
	if l.Behind > max {
		why = append(why, fmt.Sprintf("реплика %s ждёт разметки %s", l.Oldest.Local().Format("02.01 15:04"), l.Behind.Round(time.Minute)))
	}
	if l.Unread != "" {
		why = append(why, "часть транскриптов не прочиталась: "+l.Unread)
	}
	return why
}

func (l Lag) String() string {
	s := "said: последний проход "
	if l.Pass.IsZero() {
		s += "неизвестен"
	} else {
		s += fmt.Sprintf("%s (%s назад, режим %s)", l.Pass.Local().Format("02.01 15:04:05"), l.Since.Round(time.Second), l.Mode)
	}
	if l.Queue == 0 {
		return s + "; очередь пуста"
	}
	s += fmt.Sprintf("; в очереди %d", l.Queue)
	if !l.Oldest.IsZero() {
		s += fmt.Sprintf(", старейшая реплика %s ждёт %s", l.Oldest.Local().Format("02.01 15:04"), l.Behind.Round(time.Minute))
	}
	if !l.DownSince.IsZero() {
		s += fmt.Sprintf("; модель не отвечает с %s", l.DownSince.Local().Format("02.01 15:04"))
	}
	if l.Why != "" {
		s += ". " + l.Why
	}
	return s
}

// Answered — модель ответила: простоя нет.
func (st *State) Answered() {
	st.DownSince, st.DownErr, st.Skip, st.Signal = time.Time{}, "", "", time.Time{}
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
