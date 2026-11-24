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
	Path                                                         string // путь тикета относительно проекта
	CancelAuthor, CancelReason                                   string
	CancelJournalRead                                            bool
	Journal                                                      bool   // журнал, а не задача: вне tk next, кроме наступившего wait
	Transfer                                                     string // перенесён во внешний трекер: ключ и адрес; файла тикета больше нет
	External                                                     bool   // задача внешнего трекера: файла тикета нет
	Started                                                      bool   // внешняя задача уже в работе: письмо мастеру есть, письма админу «не взят» нет
	Created                                                      time.Time
	// MasterNote — последняя запись мастера в журнале тикета (tk ls --json, поле actors); моложе 4 ч — мастер ведёт
	// «открытый» тикет без tk start, письма админу «не взят» нет. Нуль — записей мастера нет.
	MasterNote time.Time
	// ActorNotes — время последней записи каждого автора журнала из tk ls --json: свежая запись живого автора
	// (adminFreshActorNote) держит тикет от предложения админу, пока автор ставит задачу.
	ActorNotes map[string]time.Time
	// BlockedBy — тикеты, которые мастер поставил перед этим (tk set X --blocks Y, tk ls --json blocked_by).
	BlockedBy []string
	Overdue   bool // «ждёт», и срок ожидания прошёл (tk ls --json overdue)
	WaitDue   bool // у «ждёт» есть разбираемый срок (tk ls --json due); без срока overdue всегда false
}

const (
	ModeLog           = "log"
	ModeOn            = "on"
	KindCrit          = "crit"
	KindCritCopy      = "crit_copy"
	KindHigh          = "high"
	KindHighAdmin     = "high_admin"
	KindWake          = "wake"
	KindWakeStuck     = "wake_stuck"
	KindWakeAdmin     = "wake_admin"
	KindBack          = "back"
	KindBackLost      = "back_lost"
	KindCancel        = "cancel"
	KindCancelLost    = "cancel_lost"
	KindCancelUnknown = "cancel_unknown"
	KindUnreachable   = "unreachable"
)

type View struct {
	Now                  time.Time
	Home                 string
	BootAt               time.Time
	BootGrace            time.Duration
	CritCopyAfter        time.Duration
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
	blockers        map[string]bool // папка + "\x00" + slug каждого тикета прохода → движется ли он (blockerMoving); считает Step
}

func (v View) bootGrace() bool {
	age := v.Now.Sub(v.BootAt)
	return !v.BootAt.IsZero() && age >= 0 && age < v.BootGrace
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
	Prio              string    `json:"prio"`
	Seen              time.Time `json:"seen"`
	Closed            time.Time `json:"closed,omitzero"`
	BackSent          bool      `json:"back_sent,omitempty"`
	BackLost          bool      `json:"back_lost,omitempty"`
	Gone              time.Time `json:"gone,omitzero"` // учтённый закрытый тикет пропал из прохода: состояние держится goneKeep, чтобы при возвращении «закрыт» не ушло повторно
	Executor          string    `json:"executor,omitempty"`
	Working           bool      `json:"working,omitempty"`
	CancelAt          time.Time `json:"cancel_at,omitzero"`
	CancelSent        bool      `json:"cancel_sent,omitempty"`
	CancelLost        bool      `json:"cancel_lost,omitempty"`
	CancelJournalRead bool      `json:"cancel_journal_read,omitempty"`
	CancelAuthor      string    `json:"cancel_author,omitempty"`
	CancelReason      string    `json:"cancel_reason,omitempty"`
	Crit              mark      `json:"crit,omitzero"`
	CritTo            string    `json:"crit_to,omitempty"`
	CritTried         time.Time `json:"crit_tried,omitzero"`
	CritTriedTo       string    `json:"crit_tried_to,omitempty"`
	CritCopy          mark      `json:"crit_copy,omitzero"`
	High              mark      `json:"high,omitzero"`
	Admin             mark      `json:"admin,omitzero"`
	Taken             bool      `json:"taken,omitempty"`
	Unreachable       bool      `json:"unreachable,omitempty"`
	Notes             []string  `json:"notes,omitempty"`
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

type delivery struct {
	To                string    `json:"to"`
	At                time.Time `json:"at"`
	CancelAt          time.Time `json:"cancel_at,omitzero"`
	CancelSent        bool      `json:"cancel_sent,omitempty"`
	CancelJournalRead bool      `json:"cancel_journal_read,omitempty"`
	CancelAuthor      string    `json:"cancel_author,omitempty"`
	CancelReason      string    `json:"cancel_reason,omitempty"`
}

type State struct {
	Seeded       bool                      `json:"seeded"`
	Tickets      map[string]*ticketState   `json:"tickets,omitempty"`
	Delivered    map[string]delivery       `json:"delivered,omitempty"`
	Wakes        map[string]Wake           `json:"wakes,omitempty"`
	ProjectWakes map[string]ProjectWake    `json:"project_wakes,omitempty"`
	External     map[string][]ExternalTask `json:"external,omitempty"` // папка проекта → последний ответ трекера
	// Transferred — внешняя задача (проект/ключ трекера) → отметки писем тикета, перенесённого в неё tk transfer,
	// снятые при первом появлении строки переноса; живут transferKeep.
	Transferred map[string]TransferMarks `json:"transferred,omitempty"`
}

func (s *State) init() {
	if s.Tickets == nil {
		s.Tickets = map[string]*ticketState{}
	}
	if s.Delivered == nil {
		s.Delivered = map[string]delivery{}
	}
	if s.Wakes == nil {
		s.Wakes = map[string]Wake{}
	}
	if s.ProjectWakes == nil {
		s.ProjectWakes = map[string]ProjectWake{}
	}
	if s.Transferred == nil {
		s.Transferred = map[string]TransferMarks{}
	}
}
func (s *State) Step(ts []Ticket, v View) []Notice {
	s.init()
	v.restingSessions = restingSessions(ts, v)
	v.blockers = blockers(ts, v)
	s.captureTransfers(ts, v.Now)
	var out []Notice
	wasSeed := s.Seeded
	active := map[string]bool{}
	for i := range ts {
		out = append(out, s.stepDelivered(ts[i], v)...)
		out = append(out, s.stepTicket(ts[i], v, wasSeed, active)...)
	}
	s.cleanupDelivered(ts)
	s.Seeded = true
	out = append(out, s.wakeNotices(ts, v, out)...)
	out = append(out, s.adminWakeNotices(ts, v, out)...)
	s.cleanup(active, v.Now)
	return out
}

func (s *State) stepDelivered(t Ticket, v View) []Notice {
	d, ok := s.Delivered[t.Key]
	if !ok {
		return nil
	}
	if ticketWorking(t) || s.Tickets[t.Key] != nil && s.Tickets[t.Key].Working {
		s.dropDelivery(t.Key)
		return nil
	}
	if t.Status == "открыт" && (!d.CancelAt.IsZero() || d.CancelJournalRead) {
		s.dropDeliveryAndWake(t.Key)
		return nil
	}
	if t.Status == "готово" {
		s.dropDelivery(t.Key)
		return nil
	}
	if !cancelStatus(t.Status) {
		return nil
	}
	return s.cancelDelivered(t, d, v)
}

func (s *State) cancelDelivered(t Ticket, d delivery, v View) []Notice {
	if d.CancelSent {
		return nil
	}
	if t.Status == "отклонён" && !t.CancelJournalRead {
		return nil
	}
	d.CancelJournalRead = t.CancelJournalRead
	d.CancelAuthor, d.CancelReason = t.CancelAuthor, t.CancelReason
	if d.CancelAt.IsZero() {
		d.CancelAt = v.Now
	}
	if t.Status == StatusTransferred && t.CancelAuthor == "" {
		s.dropDeliveryAndWake(t.Key)
		return []Notice{notice(t, v, KindCancelUnknown, "", "перенесён, автор неизвестен — исполнителю не пишу")}
	}
	to := deliveryRecipient(t, d, v)
	if deliverySelfCancelled(t.CancelAuthor, d, to) {
		s.dropDeliveryAndWake(t.Key)
		return nil
	}
	if deliveryRecipientResting(t, to, v) {
		d.CancelAt = v.Rest.End(v.Now)
		s.Delivered[t.Key] = d
		return nil
	}
	if v.Now.Sub(d.CancelAt) >= 7*24*time.Hour {
		s.dropDeliveryAndWake(t.Key)
		return []Notice{notice(t, v, KindCancelLost, "", cancelLine(t))}
	}
	if alive(v, to) {
		s.Delivered[t.Key] = d
		return []Notice{notice(t, v, KindCancel, to, cancelLine(t))}
	}
	s.Delivered[t.Key] = d
	return nil
}

func deliveryRecipient(t Ticket, d delivery, v View) string {
	if alive(v, d.To) {
		return d.To
	}
	return t.Master
}

func deliverySelfCancelled(author string, d delivery, to string) bool {
	return author != "" && (strings.EqualFold(author, d.To) || strings.EqualFold(author, to))
}

func deliveryRecipientResting(t Ticket, to string, v View) bool {
	return v.restingSessions[to] || to != v.Lead && restingDir(t.Dir, v)
}

func (s *State) dropDelivery(key string) {
	delete(s.Delivered, key)
}

func (s *State) dropDeliveryAndWake(key string) {
	s.dropDelivery(key)
	for master, wake := range s.Wakes {
		if wake.Ticket == key {
			delete(s.Wakes, master)
		}
	}
}

func (s *State) cleanupDelivered(ts []Ticket) {
	seen := make(map[string]bool, len(ts))
	for i := range ts {
		seen[ts[i].Key] = true
	}
	for key := range s.Delivered {
		if !seen[key] {
			delete(s.Delivered, key)
		}
	}
}

func (s *State) stepTicket(t Ticket, v View, seeded bool, active map[string]bool) []Notice {
	t.Prio = NormPrio(t.Prio)
	st, eligible := s.prepareTicket(t, v, seeded, active)
	if !eligible {
		return nil
	}
	resetCritAttempt(t, st)
	if closed(t.Status) {
		var out []Notice
		if cancelStatus(t.Status) {
			out = append(out, s.cancelNotices(t, st, v)...)
		}
		return append(out, s.backNotices(t, st, v)...)
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

func cancelStatus(status string) bool {
	return status == "отклонён" || status == StatusTransferred
}

func (s *State) prepareTicket(t Ticket, v View, seeded bool, active map[string]bool) (*ticketState, bool) {
	t.Prio = NormPrio(t.Prio)
	working := ticketWorking(t)
	st := s.Tickets[t.Key]
	resetCancelOnReopen(st, t, working)
	if !s.trackable(t, st, v, working) {
		return nil, false
	}
	active[t.Key] = true
	if st != nil {
		st.Gone = time.Time{}
	}
	if st == nil {
		st = &ticketState{Prio: t.Prio, Seen: v.Now}
		s.Tickets[t.Key] = st
		if !seeded && seedTicket(t, st, v.Now) {
			return st, false
		}
		if m, ok := s.Transferred[t.Key]; t.External && ok {
			m.inherit(st)
		}
	}
	rememberExecutor(st, t)
	return st, true
}

func ticketWorking(t Ticket) bool {
	return (t.Status == "в работе" || t.Status == "ждёт" && t.Executor != "") && !t.Journal
}

// blockers — тикеты прохода как возможные блокеры: ключ есть — тикет найден, значение — движется ли. tk пишет в
// «Заблокирован:» slug; один slug в папке бывает у двух тикетов (перенесённый и вернувшийся) — движение по любому.
func blockers(ts []Ticket, v View) map[string]bool {
	out := make(map[string]bool, len(ts))
	for i := range ts {
		k := ts[i].Dir + "\x00" + ts[i].Slug
		out[k] = out[k] || blockerMoving(ts[i], v)
	}
	return out
}

// blockerMoving — тикет, который держит зависимый не зря: «в работе» у живого исполнителя или «ждёт» внешнего до срока.
// Исполнитель умер, срока нет или он прошёл, тикет открыт, закрыт или журнальный — блокер встал, письмо по зависимому идёт.
func blockerMoving(t Ticket, v View) bool {
	if t.Journal {
		return false
	}
	switch t.Status {
	case "в работе":
		return alive(v, t.Executor)
	case "ждёт":
		return t.WaitDue && !t.Overdue // ожидание без срока держало бы зависимый вечно
	}
	return false
}

// blockerLocation — папка проекта и slug по ссылке «Заблокирован:»: slug своей папки или git|work/<проект>/<slug>
// (разбор tk — akit cmd/tk/blocks.go, referenceLocation).
func blockerLocation(ref string, t Ticket, home string) (string, string, bool) {
	parts := strings.Split(ref, "/")
	if len(parts) == 1 && parts[0] != "" {
		return t.Dir, parts[0], true
	}
	if len(parts) != 3 || (parts[0] != "git" && parts[0] != "work") || parts[1] == "" || parts[2] == "" || home == "" || filepath.Base(parts[1]) != parts[1] || parts[1] == "." || parts[1] == ".." {
		return "", "", false
	}
	return filepath.Join(home, parts[0], parts[1]), parts[2], true
}

// blockedByMoving — тикет ждёт найденный движущийся блокер (tk set X --blocks Y). Иначе — ссылки, которых нет в проходе
// (опечатка, архив, проект вне наблюдения): письмо админу идёт с пометкой о них.
func blockedByMoving(t Ticket, v View) (waiting bool, missing []string) {
	for _, b := range t.BlockedBy {
		dir, id, ok := blockerLocation(b, t, v.Home)
		if !ok {
			missing = append(missing, b)
			continue
		}
		moving, found := v.blockers[dir+"\x00"+id]
		if moving {
			return true, nil
		}
		if !found {
			missing = append(missing, b)
		}
	}
	return false, missing
}

func resetCancelOnReopen(st *ticketState, t Ticket, working bool) {
	if st == nil {
		return
	}
	if t.Status == "открыт" || working {
		st.CancelSent, st.CancelLost = false, false
		st.CancelAt = time.Time{}
		st.CancelJournalRead, st.CancelAuthor, st.CancelReason = false, "", ""
		st.Closed, st.BackSent, st.BackLost = time.Time{}, false, false
	}
	if !closed(t.Status) && !working {
		st.Working = false
	}
}

func (s *State) trackable(t Ticket, st *ticketState, v View, working bool) bool {
	if !closed(t.Status) && !alive(v, t.Master) && frozen(v, t.Dir) {
		s.dropFrozenWakes(t)
		if !working {
			return false
		}
	}
	if closed(t.Status) && t.Back == "" && !cancelStatus(t.Status) || !closed(t.Status) && !working && t.Prio != "CRIT" && t.Prio != "HIGH" {
		return false
	}
	if cancelStatus(t.Status) && t.Back == "" && (st == nil || !st.Working) {
		return false // отклонён без заказчика и не из работы: запись не нужна (и отметка HIGH не держится до переоткрытия)
	}
	return true
}

func rememberExecutor(st *ticketState, t Ticket) {
	if !ticketWorking(t) {
		return
	}
	st.Taken = true
	st.Executor = t.Executor
	if t.Status == "в работе" && st.Executor == "" {
		st.Executor = t.Master
	}
	st.Working = st.Executor != ""
}

// NeedsCancelJournal reports a tracked rejection whose journal has not yet been read.
func (s *State) NeedsCancelJournal(t Ticket) bool {
	st := s.Tickets[t.Key]
	if t.Status != "отклонён" {
		return false
	}
	if st != nil && st.Working && st.Executor != "" {
		return !st.CancelSent && !st.CancelLost && !st.CancelJournalRead
	}
	d, ok := s.Delivered[t.Key]
	return ok && !d.CancelJournalRead
}

// CancelJournal returns the author and reason cached for a rejected ticket.
func (s *State) CancelJournal(key string) (string, string, bool) {
	st := s.Tickets[key]
	if st != nil && st.Working && st.CancelJournalRead {
		return st.CancelAuthor, st.CancelReason, true
	}
	if d, ok := s.Delivered[key]; ok && d.CancelJournalRead {
		return d.CancelAuthor, d.CancelReason, true
	}
	return "", "", false
}

// RememberCancelJournal stores the result, including an empty result after a parse error.
func (s *State) RememberCancelJournal(key, author, reason string) {
	if st := s.Tickets[key]; st != nil && st.Working {
		st.CancelJournalRead, st.CancelAuthor, st.CancelReason = true, author, reason
		return
	}
	if d, ok := s.Delivered[key]; ok {
		d.CancelJournalRead, d.CancelAuthor, d.CancelReason = true, author, reason
		s.Delivered[key] = d
		return
	}
	if st := s.Tickets[key]; st != nil {
		st.CancelJournalRead, st.CancelAuthor, st.CancelReason = true, author, reason
	}
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

// captureTransfers — внешние задачи, в которые перенесены тикеты: строка .transferred.jsonl даёт тикет со статусом
// «перенесён» и Transfer «<ключ трекера> <адрес>». Случай 23.11: HIGH рабочего проекта перенесён в Jira, и его задача
// пришла из tk external под ключом трекера как новая — мастеру ушло повторное первое письмо HIGH.
// Задача приходит из трекера проходом позже (опрос раз в 5 мин), а к тому времени закрытый CRIT теряет отметку
// (resetCritAttempt), а перенос без заказчика — всё состояние (cleanup). Поэтому отметки снимаются в начале прохода,
// где строка переноса появилась впервые, и хранятся до прихода задачи. Состояния не было (тикет MED, демон без
// прошлого) — задача идёт как новая: дубль дешевле потерянного письма.
func (s *State) captureTransfers(ts []Ticket, now time.Time) {
	for i := range ts {
		t := &ts[i]
		if t.Status != StatusTransferred {
			continue
		}
		key, _, _ := strings.Cut(t.Transfer, " ")
		ext := t.Project + "/" + key
		src := s.Tickets[t.Key]
		if key == "" || src == nil || src.Prio == "" {
			// Приоритет у состояния — только от местного тикета, до первого прохода с переносом: у строки переноса его нет,
			// и resetCritAttempt выравнивает Prio. Так снимок берётся один раз, а вечные строки .transferred.jsonl карту не растят.
			continue
		}
		s.Transferred[ext] = TransferMarks{At: now, Seen: src.Seen, Taken: src.Taken, High: src.High, Admin: src.Admin,
			Crit: src.Crit, CritTo: src.CritTo, CritCopy: src.CritCopy}
	}
}

// transferKeep — сколько хранятся отметки перенесённого тикета: задача трекера приходит за минуты, запас — на сбой трекера.
const transferKeep = 7 * 24 * time.Hour

// TransferMarks — отметки писем перенесённого тикета: внешняя задача продолжает ту же работу.
type TransferMarks struct {
	At       time.Time `json:"at"`
	Seen     time.Time `json:"seen,omitzero"`
	Taken    bool      `json:"taken,omitempty"`
	High     mark      `json:"high,omitzero"`
	Admin    mark      `json:"admin,omitzero"`
	Crit     mark      `json:"crit,omitzero"`
	CritTo   string    `json:"crit_to,omitempty"`
	CritCopy mark      `json:"crit_copy,omitzero"`
}

// inherit — новая внешняя задача берёт отметки перенесённого тикета. Чего у тикета не было (письмо ждало свободного
// мастера, тикет был не HIGH), то у задачи пойдёт как у новой.
func (m TransferMarks) inherit(st *ticketState) {
	if !m.Seen.IsZero() {
		st.Seen = m.Seen
	}
	st.Taken, st.High, st.Admin = m.Taken, m.High, m.Admin
	st.Crit, st.CritTo, st.CritCopy = m.Crit, m.CritTo, m.CritCopy
}

// restingSessions — мастера и админы папок в день отдыха: строка о закрытом им ждёт первого рабочего дня. Лиду идёт всегда.
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
	critSentToLead := false
	if !marked(st.Crit, serious, true) {
		out = append(out, notice(t, v, KindCrit, to, line))
		critSentToLead = to == v.Lead
		if st.CritTried.IsZero() || st.CritTriedTo != to || serious && st.Crit.Mode == ModeLog && !st.Crit.At.IsZero() && !st.CritTried.After(st.Crit.At) {
			st.CritTried, st.CritTriedTo = v.Now, to
		}
	}
	copyNotice, sendCopy := critCopyNotice(t, st, v, to, serious, critSentToLead, line)
	if sendCopy {
		out = append(out, copyNotice)
	}
	return out
}

func critCopyNotice(t Ticket, st *ticketState, v View, to string, serious, critToLead bool, line string) (Notice, bool) {
	if v.Lead == t.Master || !alive(v, v.Lead) || marked(st.CritCopy, serious, true) {
		return Notice{}, false
	}
	if critToLead {
		return Notice{}, false
	}
	copyLine, send := critCopyLine(t, st, v, to, serious, line)
	if !send {
		return Notice{}, false
	}
	return notice(t, v, KindCritCopy, v.Lead, copyLine), true
}

func critCopyLine(t Ticket, st *ticketState, v View, to string, serious bool, line string) (string, bool) {
	if (st.CritTriedTo != "" && st.CritTriedTo != t.Master) || (st.CritTo != "" && st.CritTo != t.Master) {
		return line, true
	}
	if st.CritTo == t.Master {
		if st.Crit.At.IsZero() || serious && st.Crit.Mode == ModeLog {
			return failedCritCopyLine(t, st, v, line)
		}
		return deliveredCritCopyLine(t, st, v, line)
	}
	if st.Crit.At.IsZero() && to == t.Master {
		return failedCritCopyLine(t, st, v, line)
	}
	return line, true
}

func failedCritCopyLine(t Ticket, st *ticketState, v View, line string) (string, bool) {
	if st.CritTried.IsZero() || st.CritTriedTo != t.Master || critTaken(t) {
		return "", false
	}
	age := v.Now.Sub(st.CritTried)
	if age < v.CritCopyAfter {
		return "", false
	}
	return line + fmt.Sprintf(". письмо мастеру %s не дошло за %d мин", t.Master, int(age/time.Minute)), true
}

func deliveredCritCopyLine(t Ticket, st *ticketState, v View, line string) (string, bool) {
	if critTaken(t) {
		return "", false
	}
	if !alive(v, t.Master) {
		return line + fmt.Sprintf(". мастер %s недоступен", t.Master), true
	}
	age := v.Now.Sub(st.Crit.At)
	if age < v.CritCopyAfter {
		return "", false
	}
	return line + fmt.Sprintf(". мастер %s не взял за %d мин", t.Master, int(age/time.Minute)), true
}

func resetCritAttempt(t Ticket, st *ticketState) {
	if st.Prio != t.Prio {
		st.Prio = t.Prio
		st.Crit, st.CritTo = mark{}, ""
		st.CritTried, st.CritTriedTo = time.Time{}, ""
	}
	if t.Prio == "CRIT" && t.Status != "открыт" {
		st.Crit, st.CritTo = mark{}, ""
		st.CritTried, st.CritTriedTo = time.Time{}, ""
	}
}

func critTaken(t Ticket) bool {
	return t.Started
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
		if v.bootGrace() {
			return nil
		}
		return s.highAdminNotice(t, st, v, "нет живой сессии "+t.Master)
	}
	var out []Notice
	if !marked(st.High, serious, age < highWait) && v.Free != nil && v.Free(t.Master) {
		line := fmt.Sprintf("aguard: HIGH %s/%s — %s. tk next", t.Project, t.Slug, t.Title)
		out = append(out, notice(t, v, KindHigh, t.Master, line))
	}
	if age >= highWait && !t.Started && !masterNoted(t, v.Now) {
		waiting, missing := blockedByMoving(t, v)
		if !waiting {
			var reason strings.Builder
			reason.WriteString("не взят за 4 ч, мастер " + t.Master)
			for _, ref := range missing {
				reason.WriteString("; связь " + ref + " не найдена")
			}
			out = append(out, s.highAdminNotice(t, st, v, reason.String())...)
		}
	}
	return out
}

// masterNoted — мастер писал в журнал тикета меньше 4 ч назад. Запись из будущего (сдвиг часов, ручная правка) не
// в счёт: tk пишет минуты с отбрасыванием секунд, своя запись позже now не бывает.
func masterNoted(t Ticket, now time.Time) bool {
	return noteWithin(t.MasterNote, now, highWait)
}

// noteWithin — отметка не из будущего и моложе заданного окна.
func noteWithin(at, now time.Time, window time.Duration) bool {
	d := now.Sub(at)
	return d >= 0 && d < window
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

func (s *State) cancelNotices(t Ticket, st *ticketState, v View) []Notice {
	if !cancelPending(t, st) {
		return nil
	}
	if st.CancelAt.IsZero() {
		st.CancelAt = v.Now
	}
	if t.Status == StatusTransferred && t.CancelAuthor == "" {
		st.CancelSent = true
		return []Notice{notice(t, v, KindCancelUnknown, "", "перенесён, автор неизвестен — исполнителю не пишу")}
	}
	to := cancelRecipient(t, st, v)
	if t.CancelAuthor != "" && strings.EqualFold(t.CancelAuthor, to) {
		st.CancelSent = true
		return nil
	}
	line := cancelLine(t)
	if v.restingSessions[to] || to != v.Lead && restingDir(t.Dir, v) {
		// Отдыхающая сессия и проект на дне отдыха по проекту не работают; срок 7 суток переносится на конец отдыха, лиду — без ожидания.
		st.CancelAt = v.Rest.End(v.Now)
		return nil
	}
	if v.Now.Sub(st.CancelAt) >= 7*24*time.Hour {
		st.CancelLost = true
		return []Notice{notice(t, v, KindCancelLost, "", line)}
	}
	// Отмена нужна именно занятому: пока он работает, он может запустить бойца по отменённому тикету.
	// Сообщение сессии доходит и в занятую сессию, поэтому Free здесь не ждём (cancel-letter-not-wait-free).
	if alive(v, to) {
		return []Notice{notice(t, v, KindCancel, to, line)}
	}
	return nil
}

func cancelPending(t Ticket, st *ticketState) bool {
	return st.Working && st.Executor != "" && !st.CancelSent && !st.CancelLost && (t.Status != "отклонён" || t.CancelJournalRead)
}

func cancelRecipient(t Ticket, st *ticketState, v View) string {
	if alive(v, st.Executor) {
		return st.Executor
	}
	return t.Master
}

func cancelLine(t Ticket) string {
	reason := t.CancelReason
	if reason == "" {
		reason = "причина не указана"
	}
	return fmt.Sprintf("aguard: отменён %s/%s — %s; бойцов по нему остановить", t.Project, t.Slug, reason)
}

// goneKeep — сколько держится состояние учтённого закрытого тикета, пропавшего из прохода (папка временно не читается, архив и обратно, сбой tk ls).
const goneKeep = 30 * 24 * time.Hour

func (s *State) cleanup(active map[string]bool, now time.Time) {
	for ext := range s.Transferred {
		if now.Sub(s.Transferred[ext].At) >= transferKeep {
			delete(s.Transferred, ext)
		}
	}
	for key, st := range s.Tickets {
		if active[key] {
			continue
		}
		if !st.BackSent && !st.BackLost {
			delete(s.Tickets, key)
			continue
		}
		if st.Gone.IsZero() {
			st.Gone, st.Notes = now, nil // отложенные строки журнала пропавшему тикету не дописать: «не найден» каждый проход, как прежде — со стёртым состоянием
		} else if now.Sub(st.Gone) >= goneKeep {
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
	s.confirm(n, v, true)
}

// ConfirmUnsent records a log-only notice without treating it as delivered.
func (s *State) ConfirmUnsent(n Notice, v View) {
	s.confirm(n, v, false)
}

func (s *State) confirm(n Notice, v View, sent bool) {
	s.init()
	st := s.Tickets[n.Key]
	mode := ModeLog
	if n.Serious {
		mode = ModeOn
	}
	m := mark{At: v.Now, Mode: mode}
	if st != nil {
		confirmTicketNotice(st, n, v, m)
	}
	if sent && n.Serious {
		if deliveredKind(n.Kind) && n.Key != "" {
			if _, exists := s.Delivered[n.Key]; !exists {
				s.Delivered[n.Key] = delivery{To: n.To, At: v.Now}
			}
		}
	}
	if n.Kind == KindCancel && n.Key != "" {
		if d, ok := s.Delivered[n.Key]; ok {
			if sent {
				s.dropDeliveryAndWake(n.Key)
			} else {
				d.CancelSent = true
				s.Delivered[n.Key] = d
			}
		}
	}
	confirmWakeNotice(s, n, v, mode)
}

func deliveredKind(kind string) bool {
	switch kind {
	case KindWake, KindHigh, KindCrit:
		return true
	}
	return false
}

func confirmTicketNotice(st *ticketState, n Notice, v View, m mark) {
	confirmCancel(st, n.Kind)
	switch n.Kind {
	case KindCrit:
		st.Crit, st.CritTo = m, n.To
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

func confirmWakeNotice(s *State, n Notice, v View, mode string) {
	switch n.Kind {
	case KindWakeAdmin:
		s.ProjectWakes[n.Dir] = ProjectWake{Ticket: n.Key, At: v.Now, Mode: mode}
	case KindWake:
		s.Wakes[n.To] = Wake{Ticket: n.Key, At: v.Now, Mode: mode, Session: n.Session}
	case KindWakeStuck:
		w := s.Wakes[n.Master]
		if w.Ticket == n.Key && w.Session == n.Session {
			w.Stuck = true
			s.Wakes[n.Master] = w
		}
	}
}

func confirmCancel(st *ticketState, kind string) {
	switch kind {
	case KindCancel:
		st.CancelSent = true
	case KindCancelLost:
		st.CancelLost = true
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
