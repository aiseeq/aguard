// Package queue implements queue gate calculations and ticket notices.
package queue

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/record"
)

type Row struct {
	At       time.Time `json:"at"`
	ResetsAt time.Time `json:"sevenDayResetsAt"`
	Used     float64   `json:"sevenDayUsedPercentage"`
	Account  string    `json:"account"`
}

func ReadHistory(path string) ([]Row, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	var out []Row
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 4096), 4<<20)
	n := 0
	for s.Scan() {
		n++
		var raw map[string]json.RawMessage
		if e = json.Unmarshal(s.Bytes(), &raw); e != nil {
			return nil, fmt.Errorf("строка %d: %w", n, e)
		}
		if jsonEmpty(raw["account"]) {
			continue // строки без учётки в живом файле есть (account null), к запасу они не относятся
		}
		var r Row
		if e = json.Unmarshal(s.Bytes(), &r); e != nil {
			return nil, fmt.Errorf("строка %d: %w", n, e)
		}
		// Неполная строка — сломанный вход: иначе нулевой срок сброса значит «учётка свободна на 100 %» и запас 99.
		if jsonEmpty(raw["sevenDayUsedPercentage"]) || r.At.IsZero() || r.ResetsAt.IsZero() {
			return nil, fmt.Errorf("строка %d: нужны at, sevenDayResetsAt и sevenDayUsedPercentage", n)
		}
		out = append(out, r)
	}
	if e = s.Err(); e != nil {
		return nil, e
	}
	slices.SortFunc(out, func(a, b Row) int { return a.At.Compare(b.At) })
	return out, nil
}

func jsonEmpty(v json.RawMessage) bool { return len(v) == 0 || string(v) == "null" }

func Accounts(rows []Row, now time.Time, window time.Duration) []string {
	m := map[string]time.Time{}
	for _, r := range rows {
		if window > 0 && (r.At.After(now) || now.Sub(r.At) > window) {
			continue
		}
		if r.Account != "" && r.At.After(m[r.Account]) {
			m[r.Account] = r.At
		}
	}
	return slices.Sorted(maps.Keys(m))
}

// Level — ступень, с которой пары MED и LOW открыты: слово пользователя в queue.level и queue.spaces, без срока и без
// расчёта по запасу (пользователь 10.11: «Мы просто меняем планку по моему слову без всяких таймеров»).
type Level string

const (
	LevelHigh Level = "HIGH" // MED и LOW закрыты: tk даёт только CRIT и HIGH
	LevelMed  Level = "MED"  // открыты MED-*
	LevelLow  Level = "LOW"  // открыты MED-* и LOW-*
)

// pairs — пары ворот в порядке ступеней: первые три открывает MED, все шесть — LOW.
var pairs = []string{"MED-DO", "MED-HUNT", "MED-ARCH", "LOW-DO", "LOW-HUNT", "LOW-ARCH"}

// ParseLevel принимает только HIGH, MED и LOW. Число — прежний сдвиг на ступени от формулы: его больше нет, а молча
// толковать его как ступень значило бы открыть не то, что сказал пользователь.
func ParseLevel(s string) (Level, error) {
	switch l := Level(s); l {
	case LevelHigh, LevelMed, LevelLow:
		return l, nil
	}
	if _, err := strconv.Atoi(s); err == nil {
		return "", fmt.Errorf("ступень HIGH, MED или LOW, а не %s: сдвиг числом от формулы снят 10.11, пишется сама ступень", s)
	}
	return "", fmt.Errorf("ступень HIGH, MED или LOW, а не %q", s)
}

// UnmarshalText — разбор ступени из TOML: число приходит сюда текстом и даёт ошибку ParseLevel с местом в конфиге.
func (l *Level) UnmarshalText(text []byte) error {
	v, err := ParseLevel(string(text))
	if err != nil {
		return err
	}
	*l = v
	return nil
}

// Pairs — открытые на ступени пары; HIGH и неразобранная ступень — пустой список, не nil.
func (l Level) Pairs() []string {
	n := map[Level]int{LevelMed: 3, LevelLow: len(pairs)}[l]
	return slices.Clone(pairs[:n])
}

// UserGate — ворота по слову пользователя: open — ступень вне корней, open_by_root — ступень каждого корня (HIGH тоже:
// tk видит, что планка посчитана), lane пуст всегда — поле читает lane llmark. В день отдыха корни rest и все корни
// spaces внутри них — в crit_only с причиной и без открытых пар: tk берёт самый длинный корень проекта, и вложенный
// корень не должен открыть то, что закрыл внешний.
func UserGate(level Level, spaces map[string]Level, rest Rest, now time.Time) Gate {
	byRoot := make(map[string][]string, len(spaces)+len(rest.Roots))
	for root, l := range spaces {
		byRoot[root] = l.Pairs()
	}
	critOnly := map[string]string{}
	if why := rest.Day(now); why != "" {
		for _, root := range rest.Roots {
			byRoot[root] = []string{}
		}
		for root := range byRoot {
			if slices.ContainsFunc(rest.Roots, func(r string) bool { return underRoot(root, r) }) {
				byRoot[root], critOnly[root] = []string{}, why
			}
		}
	}
	return Gate{Open: level.Pairs(), Lane: []string{}, OpenByRoot: byRoot, CritOnly: critOnly}
}

type Gate struct {
	At   time.Time `json:"at"`
	Live []string  `json:"live"`
	Open []string  `json:"open"`
	// Lane — пары DO для lane llmark: пуст всегда, пока ворота — только слово пользователя.
	Lane []string `json:"lane"`
	// OpenByRoot — планка папок пространства: tk берёт самый длинный корень, куда входит проект, иначе Open.
	OpenByRoot map[string][]string `json:"open_by_root"`
	// CritOnly — корни в день отдыха и причина (суббота, воскресенье, праздник): новая работа там — только CRIT,
	// HIGH тоже закрыт. Корень есть и в OpenByRoot с пустыми парами: tk без этого поля закроет хотя бы MED и LOW.
	CritOnly map[string]string `json:"crit_only"`
}

func ReadGate(path string) (Gate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Gate{}, fmt.Errorf("чтение ворот %s: %w", path, err)
	}
	var g Gate
	if err := json.Unmarshal(b, &g); err != nil {
		return Gate{}, fmt.Errorf("разбор ворот %s: %w", path, err)
	}
	return g, nil
}

func (g Gate) Write(path string) error {
	if g.Live == nil {
		g.Live = []string{}
	}
	if g.Open == nil {
		g.Open = []string{}
	}
	if g.Lane == nil {
		g.Lane = []string{}
	}
	if g.OpenByRoot == nil {
		g.OpenByRoot = map[string][]string{}
	}
	if g.CritOnly == nil {
		g.CritOnly = map[string]string{}
	}
	b, e := json.Marshal(g)
	if e != nil {
		return e
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("создание папки ворот %s: %w", path, err)
	}
	return record.WriteFileAtomic(path, append(b, '\n'), 0o644)
}

func NormPrio(p string) string {
	if p == "MEDIUM" {
		return "MED"
	}
	return p
}

type Ticket struct {
	Key, Project, Slug, Title, Prio, Status, Back, Master, Admin string
	Dir                                                          string
	Executor                                                     string // для «в работе»: tk next показывает его только исполнителю
	Journal                                                      bool   // журнал, а не задача: вне tk next, кроме наступившего wait
	Transfer                                                     string // перенесён во внешний трекер: ключ и адрес; файла тикета больше нет
	External                                                     bool   // задача внешнего трекера: файла тикета нет
	Started                                                      bool   // внешняя задача уже в работе: письмо мастеру есть, письма админу «не взят» нет
	Created                                                      time.Time
	// MasterNote — последняя запись мастера в журнале тикета (tk ls --json, поле actors); моложе 4 ч — мастер ведёт
	// «открытый» тикет без tk start, письма админу «не взят» нет. Нуль — записей мастера нет.
	MasterNote time.Time
}

const (
	ModeLog         = "log"
	ModeOn          = "on"
	KindCrit        = "crit"
	KindCritCopy    = "crit_copy"
	KindHigh        = "high"
	KindHighAdmin   = "high_admin"
	KindWake        = "wake"
	KindWakeStuck   = "wake_stuck"
	KindWakeAdmin   = "wake_admin"
	KindBack        = "back"
	KindBackLost    = "back_lost"
	KindUnreachable = "unreachable"
)

type View struct {
	Now                  time.Time
	Lead                 string
	Serious, Alive, Free func(string) bool
	Next                 func(string, string) (Ticket, bool, error)
	Session              func(string) string
	TurnAfter            func(string, time.Time) bool
	WakeFree             func(string) bool
	Projects             func() []Ticket
	Cwd                  func(string) string
	Frozen               func(string) (bool, error)
	Log                  *slog.Logger // пропуски проекта при ошибке источника: сам источник пишет предупреждение
	AdminNext            func(string, string) (Ticket, bool, error)
	AdminFor             func(string) string
	Rest                 Rest // дни отдыха папок: из строк идут только CRIT

	restingSessions map[string]bool // сессии этих папок в такой день, считает Step
}
type Notice struct {
	To, Kind, Key, Line string
	Serious             bool
	Master, Session     string
	Dir                 string
}

const highWait = 4 * time.Hour

// StatusTransferred — состояние строки .transferred.jsonl: тикет ушёл во внешний трекер, у него только заказчик и ключ.
const StatusTransferred = "перенесён"

type mark struct {
	At   time.Time `json:"at"`
	Mode string    `json:"mode"`
}
type ticketState struct {
	Prio        string    `json:"prio"`
	Seen        time.Time `json:"seen"`
	Closed      time.Time `json:"closed,omitzero"`
	BackSent    bool      `json:"back_sent,omitempty"`
	BackLost    bool      `json:"back_lost,omitempty"`
	Crit        mark      `json:"crit,omitzero"`
	CritCopy    mark      `json:"crit_copy,omitzero"`
	High        mark      `json:"high,omitzero"`
	Admin       mark      `json:"admin,omitzero"`
	Taken       bool      `json:"taken,omitempty"`
	Unreachable bool      `json:"unreachable,omitempty"`
	Notes       []string  `json:"notes,omitempty"`
}
type Wake struct {
	Ticket  string    `json:"ticket"`
	At      time.Time `json:"at"`
	Mode    string    `json:"mode"`
	Session string    `json:"session"`
	Stuck   bool      `json:"stuck,omitempty"`
	NoOne   bool      `json:"no_one,omitempty"` // строка «некому сообщить» уже в журнале; сигнал ждёт живого получателя
}

type ProjectWake struct {
	Ticket string    `json:"ticket"`
	At     time.Time `json:"at"`
	Mode   string    `json:"mode"`
}

type State struct {
	Seeded       bool                      `json:"seeded"`
	Tickets      map[string]*ticketState   `json:"tickets,omitempty"`
	Wakes        map[string]Wake           `json:"wakes,omitempty"`
	ProjectWakes map[string]ProjectWake    `json:"project_wakes,omitempty"`
	External     map[string][]ExternalTask `json:"external,omitempty"` // папка проекта → последний ответ трекера
}

func (s *State) init() {
	if s.Tickets == nil {
		s.Tickets = map[string]*ticketState{}
	}
	if s.Wakes == nil {
		s.Wakes = map[string]Wake{}
	}
	if s.ProjectWakes == nil {
		s.ProjectWakes = map[string]ProjectWake{}
	}
}
func (s *State) Step(ts []Ticket, v View) []Notice {
	s.init()
	v.restingSessions = restingSessions(ts, v)
	var out []Notice
	wasSeed := s.Seeded
	active := map[string]bool{}
	for i := range ts {
		out = append(out, s.stepTicket(ts[i], v, wasSeed, active)...)
	}
	s.Seeded = true
	out = append(out, s.wakeNotices(ts, v, out)...)
	out = append(out, s.adminWakeNotices(ts, v, out)...)
	s.cleanup(active)
	return out
}

func (s *State) stepTicket(t Ticket, v View, seeded bool, active map[string]bool) []Notice {
	t.Prio = NormPrio(t.Prio)
	closedTicket := closed(t.Status)
	if !closedTicket && !alive(v, t.Master) && frozen(v, t.Dir) {
		s.dropFrozenWakes(t)
		return nil
	}
	if closedTicket && t.Back == "" || !closedTicket && t.Prio != "CRIT" && t.Prio != "HIGH" {
		return nil
	}
	active[t.Key] = true
	st := s.Tickets[t.Key]
	if st == nil {
		st = &ticketState{Prio: t.Prio, Seen: v.Now}
		s.Tickets[t.Key] = st
		if !seeded && seedTicket(t, st, v.Now) {
			return nil
		}
	}
	if closedTicket {
		return s.backNotices(t, st, v)
	}
	if t.Journal {
		return nil
	}
	if t.Prio == "CRIT" && t.Status == "открыт" {
		return s.critNotices(t, st, v)
	}
	if t.Prio == "HIGH" {
		if t.Status == "открыт" && restingDir(t.Dir, v) {
			st.Seen, st.Taken = v.Rest.End(v.Now), false // срок 4 ч пойдёт с первого рабочего дня; значение одно на цепочку дней отдыха, state.json не переписывается
			return nil
		}
		return s.highNotices(t, st, v)
	}
	return nil
}

// dropFrozenWakes снимает отметки о письмах по проекту, который заморожен и без живого мастера.
func (s *State) dropFrozenWakes(t Ticket) {
	delete(s.ProjectWakes, t.Dir)
	if w, ok := s.Wakes[t.Master]; ok && strings.HasPrefix(w.Ticket, t.Project+"/") {
		delete(s.Wakes, t.Master)
	}
}

// restingDir — день отдыха, и папка проекта в queue.rest_crit.
func restingDir(dir string, v View) bool { return v.Rest.Covers(dir, v.Now) }

func underRoot(dir, root string) bool {
	return dir == root || strings.HasPrefix(dir, root+string(filepath.Separator))
}

// restingSessions — мастера и админы папок на выходном: строка о закрытом им ждёт понедельника. Лиду идёт всегда.
func restingSessions(ts []Ticket, v View) map[string]bool {
	out := map[string]bool{}
	for i := range ts {
		if restingDir(ts[i].Dir, v) {
			out[ts[i].Master], out[ts[i].Admin] = true, true
		}
	}
	delete(out, v.Lead)
	return out
}

// seedTicket — первая встреча тикета при посеве: true, если он уже учтён и строки не будет.
func seedTicket(t Ticket, st *ticketState, now time.Time) bool {
	switch {
	case closed(t.Status):
		st.Closed, st.BackSent = now, true
		return true
	case t.Prio != "HIGH" || t.Status != "открыт":
		return false
	case !t.Created.IsZero() && now.Sub(t.Created) < highWait:
		st.Seen = t.Created
		return false
	}
	st.High, st.Admin = mark{At: now, Mode: ModeOn}, mark{At: now, Mode: ModeOn}
	return true
}

func notice(t Ticket, v View, kind, to, line string) Notice {
	return Notice{To: to, Kind: kind, Key: t.Key, Line: line, Serious: v.Serious != nil && v.Serious(t.Project)}
}

func marked(m mark, serious, repeatLog bool) bool {
	return !m.At.IsZero() && !(serious && m.Mode == ModeLog && repeatLog)
}

func (s *State) critNotices(t Ticket, st *ticketState, v View) []Notice {
	serious := v.Serious != nil && v.Serious(t.Project)
	show := "tk show " + t.Slug
	if t.External {
		show = "tk next"
	}
	line := fmt.Sprintf("aguard: CRIT %s/%s — %s. Брось текущее, кроме другого CRIT: %s", t.Project, t.Slug, t.Title, show)
	to := firstAlive(v, t.Master, t.Admin, v.Lead)
	var out []Notice
	if to == "" {
		if !st.Unreachable && !marked(st.Crit, serious, true) {
			out = append(out, notice(t, v, KindUnreachable, "", "никто не доступен: "+line))
			st.Unreachable = true
		}
		return out
	}
	if !marked(st.Crit, serious, true) {
		out = append(out, notice(t, v, KindCrit, to, line))
	}
	if v.Lead != t.Master && to != v.Lead && alive(v, v.Lead) && !marked(st.CritCopy, serious, true) {
		out = append(out, notice(t, v, KindCritCopy, v.Lead, line))
	}
	return out
}

func (s *State) highNotices(t Ticket, st *ticketState, v View) []Notice {
	if t.Status != "открыт" {
		st.Taken = true
		return nil
	}
	if t.Started {
		st.Taken = true
	} else if st.Taken {
		st.Seen, st.Taken = v.Now, false
	}
	serious := v.Serious != nil && v.Serious(t.Project)
	age := v.Now.Sub(st.Seen)
	// Вошёл в live моложе 4 ч: журнальная отметка админу не считается, срок идёт как у боевого. Старше — учтён.
	if serious && st.Admin.Mode == ModeLog && age < highWait {
		st.Admin = mark{}
	}
	if !alive(v, t.Master) {
		return s.highAdminNotice(t, st, v, "нет живой сессии "+t.Master)
	}
	var out []Notice
	if !marked(st.High, serious, age < highWait) && v.Free != nil && v.Free(t.Master) {
		line := fmt.Sprintf("aguard: HIGH %s/%s — %s. tk next", t.Project, t.Slug, t.Title)
		out = append(out, notice(t, v, KindHigh, t.Master, line))
	}
	if age >= highWait && !t.Started && !masterNoted(t, v.Now) {
		out = append(out, s.highAdminNotice(t, st, v, "не взят за 4 ч, мастер "+t.Master)...)
	}
	return out
}

// masterNoted — мастер писал в журнал тикета меньше 4 ч назад. Запись из будущего (сдвиг часов, ручная правка) не
// в счёт: tk пишет минуты с отбрасыванием секунд, своя запись позже now не бывает.
func masterNoted(t Ticket, now time.Time) bool {
	d := now.Sub(t.MasterNote)
	return d >= 0 && d < highWait
}

func (s *State) highAdminNotice(t Ticket, st *ticketState, v View, reason string) []Notice {
	if !st.Admin.At.IsZero() {
		return nil
	}
	to := liveRecipient(v, t.Admin, v.Lead, t.Master)
	if to == "" && alive(v, v.Lead) { // живой лид не нашёлся получателем, только когда он сам мастер (папка лида): выше никого — напоминание ему
		to = v.Lead
	}
	if to == "" {
		if !st.Unreachable {
			st.Unreachable = true
			return []Notice{notice(t, v, KindUnreachable, "", "никто не доступен: aguard: HIGH "+t.Project+"/"+t.Slug)}
		}
		return nil
	}
	line := fmt.Sprintf("aguard: HIGH %s/%s — %s", t.Project, t.Slug, reason)
	return []Notice{notice(t, v, KindHighAdmin, to, line)}
}

func (s *State) backNotices(t Ticket, st *ticketState, v View) []Notice {
	if st.Closed.IsZero() {
		st.Closed = v.Now
	}
	if t.Back == "" || st.BackSent || st.BackLost {
		return nil
	}
	if v.restingSessions[t.Back] {
		st.Closed = v.Rest.End(v.Now) // срок 7 суток не идёт, пока получатель на дне отдыха
		return nil
	}
	line := fmt.Sprintf("aguard: закрыт %s/%s (%s) — %s", t.Project, t.Slug, t.Status, t.Title)
	if t.Transfer != "" {
		line = fmt.Sprintf("aguard: перенесён %s/%s → %s", t.Project, t.Slug, t.Transfer)
	}
	if v.Now.Sub(st.Closed) >= 7*24*time.Hour {
		st.BackLost = true
		return []Notice{notice(t, v, KindBackLost, "", line)}
	}
	if alive(v, t.Back) && v.Free != nil && v.Free(t.Back) {
		return []Notice{notice(t, v, KindBack, t.Back, line)}
	}
	return nil
}

func (s *State) cleanup(active map[string]bool) {
	for key := range s.Tickets {
		if !active[key] {
			delete(s.Tickets, key)
		}
	}
}
func closed(s string) bool {
	return s == "готово" || s == "отклонён" || s == StatusTransferred
}
func alive(v View, n string) bool { return n != "" && v.Alive != nil && v.Alive(n) }
func firstAlive(v View, names ...string) string {
	for _, name := range names {
		if alive(v, name) {
			return name
		}
	}
	return ""
}
func liveRecipient(v View, first, second, master string) string {
	for _, n := range []string{first, second} {
		if n != "" && n != master && alive(v, n) {
			return n
		}
	}
	return ""
}
func (s *State) Confirm(n Notice, v View) {
	s.init()
	st := s.Tickets[n.Key]
	mode := ModeLog
	if n.Serious {
		mode = ModeOn
	}
	m := mark{At: v.Now, Mode: mode}
	if st != nil {
		switch n.Kind {
		case KindCrit:
			st.Crit = m
			if n.To == v.Lead {
				st.CritCopy = m
			}
		case KindCritCopy:
			st.CritCopy = m
		case KindHigh:
			st.High = m
		case KindHighAdmin:
			st.Admin = m
		case KindBack:
			st.BackSent = true
		}
		if n.Serious && (n.Kind == KindCrit || n.Kind == KindHigh || n.Kind == KindHighAdmin) && !slices.Contains(st.Notes, n.Line) {
			st.Notes = append(st.Notes, n.Line)
		}
	}
	if n.Kind == KindWakeAdmin {
		s.ProjectWakes[n.Dir] = ProjectWake{Ticket: n.Key, At: v.Now, Mode: mode}
	}
	if n.Kind == KindWake {
		s.Wakes[n.To] = Wake{Ticket: n.Key, At: v.Now, Mode: mode, Session: n.Session}
	}
	if n.Kind == KindWakeStuck {
		w := s.Wakes[n.Master]
		if w.Ticket == n.Key && w.Session == n.Session {
			w.Stuck = true
			s.Wakes[n.Master] = w
		}
	}
}

type PendingNote struct {
	Key  string
	Line string
}

func (s *State) PendingNotes() []PendingNote {
	keys := slices.Sorted(maps.Keys(s.Tickets))
	var out []PendingNote
	for _, key := range keys {
		for _, line := range s.Tickets[key].Notes {
			out = append(out, PendingNote{Key: key, Line: line})
		}
	}
	return out
}

func (s *State) ConfirmNote(key, line string) {
	st := s.Tickets[key]
	if st == nil {
		return
	}
	for i, pending := range st.Notes {
		if pending == line {
			st.Notes = slices.Delete(st.Notes, i, i+1)
			return
		}
	}
}
func (s *State) Save(path string) error {
	b, e := json.MarshalIndent(s, "", "  ")
	if e != nil {
		return e
	}
	return record.WriteFileAtomic(path, append(b, '\n'), 0o600)
}
func LoadState(path string) (State, error) {
	b, e := os.ReadFile(path)
	if errors.Is(e, os.ErrNotExist) {
		return State{}, nil
	}
	if e != nil {
		return State{}, e
	}
	var s State
	if e = json.Unmarshal(b, &s); e != nil {
		return s, e
	}
	return s, nil
}
func Fingerprint(dirs []string) (string, error) {
	dirs = slices.Clone(dirs)
	slices.Sort(dirs)
	h := sha256.New()
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("чтение папки %s: %w", d, err)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return "", fmt.Errorf("метаданные %s/%s: %w", d, entry.Name(), err)
			}
			fmt.Fprintf(h, "%s\x00%s\x00%d\x00%d\n", d, entry.Name(), info.Size(), info.ModTime().UnixNano())
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
