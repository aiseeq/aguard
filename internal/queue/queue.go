// Package queue implements queue gate calculations and ticket notices.
package queue

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
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

type Supply struct {
	Margin  float64
	Burning bool
}

const noBurningRateMargin = 99 // Принятый верхний запас при нулевом расходе.
func Measure(rows []Row, accounts []string, now time.Time) Supply {
	return measure(rows, accounts, now, 72*time.Hour)
}
func MeasureShort(rows []Row, accounts []string, now time.Time) *Supply {
	if len(rows) == 0 {
		return nil
	}
	rows = slices.Clone(rows)
	slices.SortFunc(rows, func(a, b Row) int { return a.At.Compare(b.At) })
	earliest, latest := map[string]time.Time{}, map[string]time.Time{}
	for _, r := range rows {
		if r.At.After(now) {
			continue
		}
		if _, seen := earliest[r.Account]; !seen {
			earliest[r.Account] = r.At
		}
		latest[r.Account] = r.At
	}
	for _, account := range accounts {
		at := latest[account]
		if at.IsZero() || now.Sub(at) > time.Hour || now.Sub(earliest[account]) < 24*time.Hour {
			return nil
		}
	}
	span := min(now.Sub(rows[0].At), 72*time.Hour)
	x := measure(rows, accounts, now, span)
	return &x
}
func measure(rows []Row, accounts []string, now time.Time, span time.Duration) Supply {
	sortRows := slices.Clone(rows)
	slices.SortFunc(sortRows, func(a, b Row) int { return a.At.Compare(b.At) })
	var total, rate float64
	burn := false
	until := time.Duration(0)
	for _, a := range accounts {
		rem, reset := remaining(sortRows, a, now)
		total += rem
		if !reset.IsZero() {
			d := reset.Sub(now)
			if until == 0 || d < until {
				until = d
			}
			if d < 24*time.Hour && rem >= 30 {
				burn = true
			}
		}
		rate += positiveRate(sortRows, a, now, span)
	}
	days := span.Hours() / 24
	daily := rate / days
	var margin float64
	if daily == 0 {
		if total == 0 {
			margin = 0
		} else {
			margin = noBurningRateMargin
		}
	} else {
		margin = total / daily
	}
	margin -= until.Hours() / 24
	return Supply{Margin: margin, Burning: burn}
}

func remaining(rows []Row, account string, now time.Time) (float64, time.Time) {
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		if r.Account == account && !r.At.After(now) {
			if r.ResetsAt.After(now) {
				return 100 - r.Used, r.ResetsAt
			}
			break
		}
	}
	return 100, time.Time{}
}

func positiveRate(rows []Row, account string, now time.Time, span time.Duration) float64 {
	var rate float64
	var before *Row
	for i := range rows {
		r := &rows[i]
		if r.Account != account || r.At.After(now) || r.At.Before(now.Add(-span)) {
			continue
		}
		if before != nil && r.Used > before.Used {
			rate += r.Used - before.Used
		}
		before = r
	}
	return rate
}

type Band int

const (
	BandFull Band = iota
	BandMid
	BandLow
)

func BandOf(v float64) Band {
	if v >= 1 {
		return BandFull
	}
	if v >= 0 {
		return BandMid
	}
	return BandLow
}

type TableRow struct {
	Pair           string
	Full, Mid, Low string
}
type Table []TableRow

var pairs = []string{"MED-DO", "MED-HUNT", "MED-ARCH", "LOW-DO", "LOW-HUNT", "LOW-ARCH"}

func validPair(p string) bool { return slices.Contains(pairs, p) }
func ParseTable(r io.Reader) (Table, error) {
	sc := bufio.NewScanner(r)
	var t Table
	seen := map[string]bool{}
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		c := strings.Split(line, "\t")
		if len(c) != 4 {
			return nil, fmt.Errorf("строка %d: нужно 4 колонки", n)
		}
		if !validPair(c[0]) {
			return nil, fmt.Errorf("строка %d: неизвестная пара %q", n, c[0])
		}
		if seen[c[0]] {
			return nil, fmt.Errorf("строка %d: повтор пары %s", n, c[0])
		}
		seen[c[0]] = true
		for _, v := range c[1:] {
			if v != "да" && v != "нет" && v != "без мастера" {
				return nil, fmt.Errorf("строка %d: значение %q", n, v)
			}
		}
		t = append(t, TableRow{c[0], c[1], c[2], c[3]})
	}
	return t, sc.Err()
}

type Override struct {
	Pair, Value string
	Until       time.Time
	Who, Why    string
}

func ParseOverrides(r io.Reader) ([]Override, error) {
	sc := bufio.NewScanner(r)
	var out []Override
	n := 0
	for sc.Scan() {
		n++
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		c := strings.Split(l, "\t")
		if len(c) != 5 {
			return nil, fmt.Errorf("строка %d: нужно 5 колонок", n)
		}
		for _, x := range c {
			if strings.TrimSpace(x) == "" {
				return nil, fmt.Errorf("строка %d: пустое поле", n)
			}
		}
		if !validPair(c[0]) {
			return nil, fmt.Errorf("строка %d: неизвестная пара %q", n, c[0])
		}
		if c[1] != "да" && c[1] != "нет" && c[1] != "без мастера" {
			return nil, fmt.Errorf("строка %d: значение %q", n, c[1])
		}
		until, e := time.Parse(time.RFC3339, c[2])
		if e != nil {
			if d, de := time.ParseInLocation("2006-01-02", c[2], time.Local); de == nil {
				until = d.AddDate(0, 0, 1).Add(-time.Nanosecond)
			} else {
				return nil, fmt.Errorf("строка %d: срок: %w", n, e)
			}
		}
		out = append(out, Override{c[0], c[1], until, c[3], c[4]})
	}
	return out, sc.Err()
}
func bandValue(r TableRow, b Band) string {
	switch b {
	case BandFull:
		return r.Full
	case BandMid:
		return r.Mid
	default:
		return r.Low
	}
}
func Compute(t Table, ovs []Override, claude Supply, openai *Supply, now time.Time) (open, lane []string) {
	for _, r := range t {
		v := supplierValue(r, claude)
		if openai != nil {
			v = worse(v, supplierValue(r, *openai))
		}
		for _, o := range ovs {
			if o.Pair == r.Pair && !now.After(o.Until) {
				v = o.Value
			}
		}
		if v == "да" {
			open = append(open, r.Pair)
		}
		if strings.HasSuffix(r.Pair, "-DO") && (v == "да" || v == "без мастера") {
			lane = append(lane, r.Pair)
		}
	}
	return
}

// MaxSpaceShift — от LOW до HIGH две ступени: больше сдвигать некуда, такое значение в конфиге — ошибка.
const MaxSpaceShift = 2

// SpaceOpen — планка каждого корня из общих ворот: пара открыта, если открыта пара того же DIFF на shift приоритетов выше.
// Выше MED — HIGH: он открыт всегда. Корень со сдвигом 0 тоже в ответе: tk видит, что планка посчитана.
func SpaceOpen(t Table, open []string, spaces map[string]int) map[string][]string {
	out := make(map[string][]string, len(spaces))
	for root, shift := range spaces {
		pairs := []string{}
		for _, r := range t {
			if shiftedOpen(r.Pair, open, shift) {
				pairs = append(pairs, r.Pair)
			}
		}
		out[root] = pairs
	}
	return out
}

func shiftedOpen(pair string, open []string, shift int) bool {
	prio, diff, ok := strings.Cut(pair, "-")
	if !ok {
		return slices.Contains(open, pair)
	}
	rank := map[string]int{"MED": 0, "LOW": 1}[prio] - shift
	if rank < 0 {
		return true
	}
	return slices.Contains(open, []string{"MED", "LOW"}[rank]+"-"+diff)
}

func supplierValue(row TableRow, supply Supply) string {
	if supply.Burning {
		return "да"
	}
	return bandValue(row, BandOf(supply.Margin))
}

func worse(a, b string) string {
	rank := map[string]int{"нет": 0, "без мастера": 1, "да": 2}
	if rank[b] < rank[a] {
		return b
	}
	return a
}

type Gate struct {
	At     time.Time `json:"at"`
	Live   []string  `json:"live"`
	Claude float64   `json:"claude"`
	OpenAI *float64  `json:"openai"`
	Open   []string  `json:"open"`
	Lane   []string  `json:"lane"`
	// OpenByRoot — планка папок пространства: tk берёт самый длинный корень, куда входит проект, иначе Open.
	OpenByRoot map[string][]string `json:"open_by_root"`
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
	g.Claude = math.Round(g.Claude*100) / 100
	if g.OpenAI != nil {
		x := math.Round(*g.OpenAI*100) / 100
		g.OpenAI = &x
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
	Key, Project, Slug, Title, Prio, Diff, Status, Back, Master, Admin string
	Dir                                                                string
	Executor                                                           string // для «в работе»: tk next показывает его только исполнителю
	WaitDue                                                            bool   // для «ждёт»: срок наступил, tk next его показывает
	Journal                                                            bool   // журнал, а не задача: вне tk next, кроме наступившего wait
	Transfer                                                           string // перенесён во внешний трекер: ключ и адрес; файла тикета больше нет
	External                                                           bool   // задача внешнего трекера: файла тикета нет
	Started                                                            bool   // внешняя задача уже в работе: письмо мастеру есть, письма админу «не взят» нет
	Created                                                            time.Time
}

const (
	ModeLog         = "log"
	ModeOn          = "on"
	KindCrit        = "crit"
	KindCritCopy    = "crit_copy"
	KindHigh        = "high"
	KindHighAdmin   = "high_admin"
	KindMorning     = "morning"
	KindBack        = "back"
	KindBackLost    = "back_lost"
	KindUnreachable = "unreachable"
)

type View struct {
	Now                  time.Time
	Lead                 string
	Open                 []string
	Morning              time.Duration
	Serious, Alive, Free func(string) bool
	WeekendCrit          []string // папки проектов: в субботу и воскресенье из строк идут только CRIT
	OpenByRoot           map[string][]string

	restingSessions map[string]bool // сессии этих папок в такой день, считает Step
}
type Notice struct {
	To, Kind, Key, Line string
	Serious             bool
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
type State struct {
	Seeded   bool                      `json:"seeded"`
	Tickets  map[string]*ticketState   `json:"tickets,omitempty"`
	Mornings map[string]string         `json:"mornings,omitempty"`
	External map[string][]ExternalTask `json:"external,omitempty"` // папка проекта → последний ответ трекера
}

func (s *State) init() {
	if s.Tickets == nil {
		s.Tickets = map[string]*ticketState{}
	}
	if s.Mornings == nil {
		s.Mornings = map[string]string{}
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
	out = append(out, s.morningNotices(ts, v)...)
	s.cleanup(active, v.Now)
	return out
}

func (s *State) stepTicket(t Ticket, v View, seeded bool, active map[string]bool) []Notice {
	t.Prio = NormPrio(t.Prio)
	closedTicket := closed(t.Status)
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
			st.Seen, st.Taken = restEnd(v.Now), false // срок 4 ч пойдёт с понедельника; значение одно на выходные, state.json не переписывается
			return nil
		}
		return s.highNotices(t, st, v)
	}
	return nil
}

// restingDir — суббота или воскресенье, и папка проекта в weekend_crit.
func restingDir(dir string, v View) bool {
	if day := v.Now.Weekday(); day != time.Saturday && day != time.Sunday {
		return false
	}
	return slices.ContainsFunc(v.WeekendCrit, func(root string) bool { return underRoot(dir, root) })
}

// restEnd — понедельник 00:00 после субботы или воскресенья now.
func restEnd(now time.Time) time.Time {
	days := 2
	if now.Weekday() == time.Sunday {
		days = 1
	}
	return time.Date(now.Year(), now.Month(), now.Day()+days, 0, 0, 0, 0, now.Location())
}

func underRoot(dir, root string) bool {
	return dir == root || strings.HasPrefix(dir, root+string(filepath.Separator))
}

// openFor — планка папки: самый длинный корень, куда она входит, иначе общие ворота. Как у tk next.
func openFor(dir string, v View) []string {
	open := v.Open
	for _, root := range slices.Sorted(maps.Keys(v.OpenByRoot)) { // корни папки — цепочка префиксов: длинный идёт последним
		if underRoot(dir, root) {
			open = v.OpenByRoot[root]
		}
	}
	return open
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
	if age >= highWait && !t.Started {
		out = append(out, s.highAdminNotice(t, st, v, "не взят за 4 ч, мастер "+t.Master)...)
	}
	return out
}

func (s *State) highAdminNotice(t Ticket, st *ticketState, v View, reason string) []Notice {
	if !st.Admin.At.IsZero() {
		return nil
	}
	to := liveRecipient(v, t.Admin, v.Lead, t.Master)
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
		st.Closed = restEnd(v.Now) // срок 7 суток не идёт, пока получатель на выходном
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

func (s *State) morningNotices(ts []Ticket, v View) []Notice {
	boundary := time.Date(v.Now.Year(), v.Now.Month(), v.Now.Day(), 0, 0, 0, 0, v.Now.Location()).Add(v.Morning)
	if !v.Now.After(boundary) {
		return nil
	}
	seriousByMaster := map[string]bool{}
	for i := range ts {
		t := &ts[i]
		if !morningTicket(*t, boundary, v) {
			continue
		}
		if !morningOpen(*t, NormPrio(t.Prio), openFor(t.Dir, v)) {
			continue
		}
		seriousByMaster[t.Master] = seriousByMaster[t.Master] || (v.Serious != nil && v.Serious(t.Project))
	}
	masters := slices.Sorted(maps.Keys(seriousByMaster))
	var out []Notice
	day := v.Now.Format("2006-01-02")
	for _, master := range masters {
		key := day + "/" + master
		mode, sent := s.Mornings[key]
		eligible := !sent || seriousByMaster[master] && mode == ModeLog
		if eligible && v.Free != nil && v.Free(master) {
			out = append(out, Notice{To: master, Kind: KindMorning, Key: key, Line: "есть работа: tk next", Serious: seriousByMaster[master]})
		}
	}
	return out
}

func morningTicket(t Ticket, boundary time.Time, v View) bool {
	p := NormPrio(t.Prio)
	return (t.Created.IsZero() || !t.Created.After(boundary)) && t.Master != "" && alive(v, t.Master) &&
		(p == "MED" || p == "LOW") && visibleInNext(t) && !restingDir(t.Dir, v)
}

// visibleInNext — статус тикета, который tk next покажет мастеру: иначе строка «есть работа» придёт при пустом tk next.
func visibleInNext(t Ticket) bool {
	if t.Journal {
		return t.Status == "ждёт" && t.WaitDue
	}
	switch t.Status {
	case "открыт":
		return true
	case "в работе":
		return strings.EqualFold(t.Executor, t.Master)
	case "ждёт":
		return t.WaitDue
	}
	return false
}

// morningOpen — как у tk next: тикет с DIFF смотрит на свою пару, без DIFF виден, только когда открыты все пары приоритета.
func morningOpen(t Ticket, prio string, open []string) bool {
	if t.Diff != "" {
		return slices.Contains(open, prio+"-"+t.Diff)
	}
	for _, pair := range pairs {
		if strings.HasPrefix(pair, prio+"-") && !slices.Contains(open, pair) {
			return false
		}
	}
	return true
}

func (s *State) cleanup(active map[string]bool, now time.Time) {
	for key := range s.Tickets {
		if !active[key] {
			delete(s.Tickets, key)
		}
	}
	cutoff := now.AddDate(0, 0, -2).Format("2006-01-02")
	for key := range s.Mornings {
		date, _, ok := strings.Cut(key, "/")
		if ok && date < cutoff {
			delete(s.Mornings, key)
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
	if n.Kind == KindMorning {
		s.Mornings[v.Now.Format("2006-01-02")+"/"+n.To] = mode
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
