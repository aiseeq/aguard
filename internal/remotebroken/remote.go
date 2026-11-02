// Package remotebroken — события graft, когда путь на PC сломан и задача выполнена на ноуте.
package remotebroken

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/bgtask"
	"github.com/aiseeq/aguard/internal/policy"
	"github.com/aiseeq/aguard/internal/record"
)

const (
	ModeOff   = bgtask.ModeOff
	ModeLog   = bgtask.ModeLog
	ModeAlert = bgtask.ModeAlert
)

// Config — раздел [remote] конфига aguard.
type Config struct {
	Mode     string          `toml:"mode"`
	Interval policy.Duration `toml:"interval"`
	Repeat   policy.Duration `toml:"repeat"`
	File     string          `toml:"file"`
	Lead     string          `toml:"-"`
	SendAuth bool            `toml:"-"`
}

// Default — события graft проверяются раз в минуту; письмо повторяется не чаще часа.
func Default(home string) Config {
	return Config{Mode: ModeAlert, Interval: policy.Duration(time.Minute), Repeat: policy.Duration(time.Hour),
		File: filepath.Join(home, ".cache", "graft", "remote-broken.jsonl")}
}

// Validate проверяет и раскрывает путь журнала.
func (c *Config) Validate(home string) error {
	if c.Mode != ModeOff && c.Mode != ModeLog && c.Mode != ModeAlert {
		return fmt.Errorf("remote.mode = %q: off, log или alert", c.Mode)
	}
	if c.Interval.D() < 10*time.Second || c.Repeat.D() < c.Interval.D() {
		return errors.New("remote.interval >= 10s, remote.repeat >= remote.interval")
	}
	c.File = policy.ExpandHome(c.File, home)
	if c.File == "" {
		return errors.New("remote.file пуст")
	}
	return nil
}

// Event — одна строка remote-broken.jsonl от graft.
type Event struct {
	At        time.Time `json:"at"`
	Project   string    `json:"project"`
	Task      string    `json:"task"`
	Host      string    `json:"host"`
	Reason    string    `json:"reason"`
	PID       int       `json:"pid"`
	PPID      int       `json:"ppid"`
	CWD       string    `json:"cwd"`
	ClaudePID string    `json:"claude_pid"`
	Worker    string    `json:"worker"`
}

// Parse разбирает запись журнала.
func Parse(line []byte) (Event, error) {
	var e Event
	if err := json.Unmarshal(line, &e); err != nil {
		return e, fmt.Errorf("JSON события: %w", err)
	}
	if e.At.IsZero() || e.Project == "" || e.Task == "" || e.Host == "" {
		return e, errors.New("обязательные поля at, project, task, host не заданы")
	}
	return e, nil
}

// Notice — письмо владельцу или сводка лиду.
type Notice struct {
	To, Key, Message string
	Count            int
	Summary          bool
}
type sent struct {
	At        time.Time `json:"at"`
	PendingAt time.Time `json:"pending_at,omitzero"`
	Pending   int       `json:"pending,omitempty"`
	Event     Event     `json:"event"`
}
type summary struct {
	At time.Time `json:"at"` // когда ушла сводка этого окна; ноль — ещё не уходила
	// Last — последнее событие по машине, переживает смену окна: тишина от него не короче repeat — новая поломка,
	// о ней лиду письмо first.
	Last   time.Time       `json:"last,omitzero"`
	Owners map[string]bool `json:"owners"`
	First  time.Time       `json:"first"`
	Reason string          `json:"reason"`
}

// State — смещение журнала и отметки писем.
type State struct {
	Offset    int64              `json:"offset"`
	Seeded    bool               `json:"seeded"`
	Sent      map[string]sent    `json:"sent,omitempty"`
	Summaries map[string]summary `json:"summaries,omitempty"`
}

func LoadState(path string) (State, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("состояние remote: %w", err)
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("состояние remote %s: %w", path, err)
	}
	return s, nil
}

func (s *State) Save(path string) error {
	b, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("сериализация состояния remote: %w", err)
	}
	if err := record.WriteFileAtomic(path, b, 0o600); err != nil {
		return fmt.Errorf("состояние remote: %w", err)
	}
	return nil
}

// ReadNew читает только новые байты. Состояние без засечки сначала засевается концом файла.
func ReadNew(path string, s *State, warn func(error)) ([]Event, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		if !s.Seeded {
			s.Seeded = true
			s.Offset = 0
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("открытие журнала graft: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("размер журнала graft: %w", err)
	}
	if !s.Seeded {
		s.Offset, s.Seeded = st.Size(), true
		return nil, nil
	}
	if st.Size() < s.Offset {
		s.Offset = 0
	}
	if _, err := f.Seek(s.Offset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("смещение журнала graft: %w", err)
	}
	r := bufio.NewReader(f)
	var out []Event
	for {
		line, e := r.ReadBytes('\n')
		if e == io.EOF && len(line) > 0 { // graft дописывает строку прямо сейчас — дочитать в следующий проход
			break
		}
		if len(line) > 0 {
			s.Offset += int64(len(line))
			if v, pe := Parse(line); pe != nil {
				if warn != nil {
					warn(fmt.Errorf("строка remote-broken: %w", pe))
				}
			} else {
				out = append(out, v)
			}
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			return out, e
		}
	}
	return out, nil
}

// Owner возвращает мастера по worker, затем по сессии Claude Code.
func Owner(e Event, worker func(string) (string, error), session func(int) (string, error), lead string) (string, error) {
	if e.Worker != "" && worker != nil {
		name, err := worker(e.Worker)
		if err != nil {
			return lead, err
		}
		if name != "" {
			return name, nil
		}
	}
	if e.ClaudePID != "" && session != nil {
		pid, err := strconv.Atoi(e.ClaudePID)
		if err != nil {
			return lead, fmt.Errorf("claude_pid %q: %w", e.ClaudePID, err)
		}
		name, err := session(pid)
		if err != nil {
			return lead, err
		}
		if name != "" {
			return name, nil
		}
	}
	return lead, nil
}

// queueReasons — причины перехода на ноут, при которых PC исправен: graft или worker не дождались места в очереди
// тяжёлых задач PC и штатно ушли на ноут (graft-master, тикет pc-busy-fallback). Поломка — связь, запуск, отправка
// дерева, уборка, карантин, сбой самой задачи (лид 02.11 03:3x, тикет remote-queue-not-broken).
// Сверка — с начала причины: подстрока в середине (текст файла карантина, вывод задачи) поломку не прячет.
var queueReasons = []string{"run exited 125: pc: не впущен очередью graft ", "run exited 125: worker: очередь PC дольше "}

// Queued — событие о занятой очереди PC, а не о сломанном пути.
func Queued(e Event) bool {
	for _, r := range queueReasons {
		if strings.HasPrefix(e.Reason, r) {
			return true
		}
	}
	return false
}

// SplitQueued делит события на поломки и переходы по очереди, порядок сохраняется.
func SplitQueued(events []Event) (broken, queued []Event) {
	for _, e := range events {
		if Queued(e) {
			queued = append(queued, e)
		} else {
			broken = append(broken, e)
		}
	}
	return broken, queued
}

// Message — письмо об одном событии в одну строку, время местное.
func Message(e Event) string {
	return fmt.Sprintf("graft-задача %s в %s на %s не дошла (%s): %s; выполнена на ноуте.", e.Task, e.Project, e.Host,
		e.At.Local().Format("15:04"), e.Reason)
}

// Apply учитывает события, отдаёт due письма владельцам и сводки лиду.
func (s *State) Apply(events []Event, owners []string, lead string, now time.Time, repeat time.Duration) []Notice {
	if s.Sent == nil {
		s.Sent = map[string]sent{}
	}
	if s.Summaries == nil {
		s.Summaries = map[string]summary{}
	}
	var out []Notice
	byHost := map[string]bool{}
	newOwner := map[string]bool{}
	for i := range events {
		e := events[i]
		owner := owners[i]
		out = append(out, s.applyEvent(e, owner, lead, now, repeat, newOwner)...)
		byHost[e.Host] = true
	}
	hosts := make([]string, 0, len(byHost))
	for host := range byHost {
		hosts = append(hosts, host)
	}
	slices.Sort(hosts)
	for _, host := range hosts {
		if n, ok := s.applySummary(host, newOwner[host], lead, now); ok {
			out = append(out, n)
		}
	}
	return out
}

func (s *State) applyEvent(e Event, owner, lead string, now time.Time, repeat time.Duration, newOwner map[string]bool) []Notice {
	var out []Notice
	key := e.Project + "\x00" + e.Host
	v := s.Sent[key]
	if !v.At.IsZero() && now.Sub(v.At) < repeat {
		if v.Pending == 0 {
			v.PendingAt = e.At
		}
		v.Pending++
		s.Sent[key] = v
	} else {
		msg := Message(e)
		if v.Pending > 0 {
			msg += fmt.Sprintf(" До этого ещё %d раз с %s.", v.Pending, v.PendingAt.Local().Format("15:04"))
		}
		out = append(out, Notice{To: owner, Key: key, Message: msg, Count: v.Pending + 1})
		s.Sent[key] = sent{At: now, Event: e}
	}

	// Окно сводки начинается с первого события и после сводки — с её времени.
	x := s.Summaries[e.Host]
	last := x.Last
	if x.Owners == nil || (x.At.IsZero() && e.At.Sub(x.First) >= repeat) || (!x.At.IsZero() && now.Sub(x.At) >= repeat) {
		x = summary{Owners: map[string]bool{}, First: e.At, Reason: e.Reason}
	}
	if !x.Owners[owner] {
		newOwner[e.Host] = true
	}
	x.Owners[owner] = true
	if (last.IsZero() || e.At.Sub(last) >= repeat) && owner != lead {
		out = append(out, Notice{To: lead, Key: "first:" + e.Host, Message: Message(e), Count: 1})
	}
	x.Last = last
	if e.At.After(last) {
		x.Last = e.At
	}
	s.Summaries[e.Host] = x
	return out
}

func (s *State) applySummary(host string, newOwner bool, lead string, now time.Time) (Notice, bool) {
	x := s.Summaries[host]
	if len(x.Owners) < 2 || !newOwner || !x.At.IsZero() { // один мастер, повтор владельца или сводка этого окна уже ушла
		return Notice{}, false
	}
	list := make([]string, 0, len(x.Owners))
	for n := range x.Owners {
		list = append(list, n)
	}
	sort.Strings(list)
	msg := fmt.Sprintf("%s: путь сломан у %d мастеров (%s), первая в %s, причина %s", strings.ToUpper(host), len(x.Owners), joinNames(list), x.First.Local().Format("15:04"), x.Reason)
	x.At = now
	s.Summaries[host] = x
	return Notice{To: lead, Key: "summary:" + host, Message: msg, Count: len(x.Owners), Summary: true}, true
}

func joinNames(names []string) string {
	if len(names) < 2 {
		if len(names) == 1 {
			return names[0]
		}
		return ""
	}
	var r strings.Builder
	r.WriteString(names[0])
	for _, n := range names[1:] {
		r.WriteString(", " + n)
	}
	return r.String()
}
