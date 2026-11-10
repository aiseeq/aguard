package disk

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
	"github.com/aiseeq/aguard/internal/record"
)

// Маршрут находок (тикет disk-alert-forecast, решение agent-mail-async 08.11): срочное — fast, место тает сейчас —
// письмом хозяину; несрочное — grew, stale, следы от trace_min — тикетом класса в его репо, как у io.

// FastTold — последнее письмо о паре (хозяин, запись): когда, размер записи и рост в письме, свободное место.
type FastTold struct {
	At    time.Time `json:"at"`
	Bytes int64     `json:"bytes,omitempty"`
	Delta int64     `json:"delta,omitempty"`
	Free  int64     `json:"free"`
}

// FastLetter — письмо о быстрой убыли за проход: все записи хозяина одним письмом (сценарий 8 противника 10.11).
type FastLetter struct {
	To, Owner string
	Offline   bool // хозяин не в сети: письмо лиду
	Recs      []Record
	Free      int64
}

// fastDue — пора ли письмо о паре: письма не было, пауза fast_every прошла, рост записи с прошлого письма не меньше
// роста в нём (удвоение) или свободное убыло с него ещё на fast (сценарий 2: +5 ГБ, через 10 мин ещё +100 ГБ).
func (c *Config) fastDue(prev FastTold, ok bool, r *Record, now time.Time) bool {
	if !ok || now.Sub(prev.At) >= c.FastEvery.D() {
		return true
	}
	// У убыли без записи Bytes 0, а Delta письма — убыль: удвоение не срабатывает, только ещё fast свободного.
	if r.Bytes-prev.Bytes >= prev.Delta {
		return true
	}
	return prev.Free-r.Free >= int64(c.Fast)
}

// letterKey — пара письма о fast-находке: хозяин (нет — лид) и запись.
func (c *Config) letterKey(r *Record) string {
	owner := r.Owner
	if owner == "" {
		owner = c.Lead
	}
	return owner + " " + r.Path
}

// fastLetterDue — пора ли письмо о fast-находке r с уже определённым хозяином (Pass: предел журнала не держит её).
func (s *State) fastLetterDue(c *Config, now time.Time, r *Record) bool {
	prev, ok := s.Letters[c.letterKey(r)]
	return c.fastDue(prev, ok, r, now)
}

// FastLetters — письма о fast-находках прохода: одно на хозяина; пара в паузе — только журнал. Хозяин не в сети или
// его нет — письмо лиду. Отметки — сразу: находка прохода повтора не получит.
func (s *State) FastLetters(c *Config, now time.Time, recs []Record, alive func(string) bool) []FastLetter {
	if s.Letters == nil {
		s.Letters = map[string]FastTold{}
	}
	maps.DeleteFunc(s.Letters, func(_ string, t FastTold) bool { return now.Sub(t.At) >= c.FastEvery.D() })
	byOwner := map[string]*FastLetter{}
	var order []string
	for i := range recs {
		r := &recs[i]
		if r.Kind != KindFast {
			continue
		}
		owner := r.Owner
		if owner == "" {
			owner = c.Lead
		}
		key := c.letterKey(r)
		prev, ok := s.Letters[key]
		if !c.fastDue(prev, ok, r, now) {
			continue
		}
		s.Letters[key] = FastTold{At: now, Bytes: r.Bytes, Delta: r.Delta, Free: r.Free}
		l := byOwner[owner]
		if l == nil {
			l = &FastLetter{Owner: owner, To: owner}
			if owner != c.Lead && !alive(owner) {
				l.To, l.Offline = c.Lead, true
			}
			byOwner[owner] = l
			order = append(order, owner)
		}
		l.Recs = append(l.Recs, *r)
		l.Free = r.Free
	}
	out := make([]FastLetter, 0, len(order))
	for _, o := range order {
		out = append(out, *byOwner[o])
	}
	return out
}

// Text — письмо: свободное место сейчас, записи с ростом и сумма; убыль без виновной записи — без выдуманной записи.
func (l *FastLetter) Text(c *Config, home string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "aguard disk: место на /home тает быстро — свободно %s (free_min %s).", policy.HumanBytes(l.Free),
		policy.HumanBytes(int64(c.FreeMin)))
	if l.Offline {
		fmt.Fprintf(&b, " Хозяин %s не в сети — пишу лиду.", l.Owner)
	}
	var sum int64
	var grown []*Record
	for i := range l.Recs {
		r := &l.Recs[i]
		if r.Path == "" {
			fmt.Fprintf(&b, "\nСвободное место убыло на %s с %s, виновника обход не нашёл: ни одна запись так не выросла.",
				policy.HumanBytes(r.Delta), r.Since.Local().Format("15:04"))
			continue
		}
		sum += r.Delta
		grown = append(grown, r)
	}
	if len(grown) > 0 {
		fmt.Fprintf(&b, "\nЗаписи %s выросли с прошлого обхода на %s (%d):", l.Owner, policy.HumanBytes(sum), len(grown))
		for _, r := range grown {
			fmt.Fprintf(&b, "\n- %s: +%s с %s, теперь %s", short(r.Path, home), policy.HumanBytes(r.Delta),
				r.Since.Local().Format("15:04"), policy.HumanBytes(r.Bytes))
		}
	}
	fmt.Fprintf(&b, "\nО той же записи — не чаще %s, раньше — если её рост удвоится или место убудет ещё на %s.",
		policy.HumanAge(c.FastEvery.D()), policy.HumanBytes(int64(c.Fast)))
	return b.String()
}

// Классы тикетов несрочных находок: slug — disk-<вид>, номер — как у io.
const (
	TicketGrew  = "disk-grew"
	TicketStale = "disk-stale"
	TicketTrace = "disk-trace"
)

// TicketClass — класс тикета находки; "" — не в тикет (fast — письмом, след меньше trace_min, прочее — журнал).
func (c *Config) TicketClass(r *Record) string {
	switch r.Kind {
	case KindGrew:
		return TicketGrew
	case KindStale:
		return TicketStale
	case KindTrace:
		if r.Bytes >= int64(c.TraceMin) {
			return TicketTrace
		}
	}
	return ""
}

// ticketRetry — тикет не завёлся (tk упал) — находки ждут и повтор через столько.
const ticketRetry = time.Hour

// TicketState — tickets.json: находки, ждущие тикета, и когда класс хозяина можно снова. Свой файл, сохраняется сразу
// после тикетов: обход бывает брошен остановкой демона, а заметка уже записана.
type TicketState struct {
	Next    map[string]time.Time `json:"next"`              // класс и хозяин -> не раньше
	Pending map[string][]Record  `json:"pending,omitempty"` // класс и хозяин -> находки до тикета
}

// TicketBatch — находки одного класса и хозяина: в один тикет или одну заметку.
type TicketBatch struct {
	Class, Owner string
	Recs         []Record
}

func ticketKey(class, owner string) string { return class + " " + owner }

// LoadTickets — состояние тикетов; файла нет — пусто.
func LoadTickets(path string) (*TicketState, error) {
	t := &TicketState{}
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("тикеты disk: %w", err)
	}
	if err == nil {
		if err := json.Unmarshal(b, t); err != nil {
			return nil, fmt.Errorf("тикеты disk %s: %w", path, err)
		}
	}
	if t.Next == nil {
		t.Next = map[string]time.Time{}
	}
	if t.Pending == nil {
		t.Pending = map[string][]Record{}
	}
	return t, nil
}

// Save пишет состояние атомарно.
func (t *TicketState) Save(path string) error {
	b, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("тикеты disk: %w", err)
	}
	if err := record.WriteFileAtomic(path, b, 0o600); err != nil {
		return fmt.Errorf("тикеты disk: %w", err)
	}
	return nil
}

// Add — находки прохода в ожидание по классу и хозяину; та же находка о том же пути заменяет прежнюю. true — что-то
// добавилось.
func (t *TicketState) Add(c *Config, recs []Record) bool {
	added := false
	for i := range recs {
		r := recs[i]
		class := c.TicketClass(&r)
		if class == "" {
			continue
		}
		owner := r.Owner
		if owner == "" {
			owner = c.Lead
		}
		key := ticketKey(class, owner)
		pending := t.Pending[key][:0]
		for j := range t.Pending[key] {
			if p := &t.Pending[key][j]; p.Kind != r.Kind || p.Path != r.Path {
				pending = append(pending, *p)
			}
		}
		t.Pending[key] = append(pending, r)
		added = true
	}
	return added
}

// Due — классы хозяев, кому пора тикет или заметка: есть ждущие находки и прошли сутки с прошлого (сценарий 4: все
// находки — одной заметкой). Порядок — по ключу.
func (t *TicketState) Due(now time.Time) []TicketBatch {
	var out []TicketBatch
	for _, key := range slices.Sorted(maps.Keys(t.Pending)) {
		recs := t.Pending[key]
		if len(recs) == 0 || now.Before(t.Next[key]) {
			continue
		}
		class, owner, _ := strings.Cut(key, " ")
		out = append(out, TicketBatch{Class: class, Owner: owner, Recs: slices.Clone(recs)})
	}
	return out
}

// Done — тикет класса хозяина заведён (ok): ждущие сняты, следующий — через сутки; не заведён — повтор через ticketRetry.
func (t *TicketState) Done(b *TicketBatch, now time.Time, ok bool) {
	key := ticketKey(b.Class, b.Owner)
	if !ok {
		t.Next[key] = now.Add(ticketRetry)
		return
	}
	delete(t.Pending, key)
	t.Next[key] = now.Add(repeatDaily)
	maps.DeleteFunc(t.Next, func(k string, next time.Time) bool { return now.After(next) && len(t.Pending[k]) == 0 })
}

// ticketWhat — находки класса словами.
var ticketWhat = map[string]string{
	TicketGrew:  "записи на диске выросли за сутки на grow и больше",
	TicketStale: "большие записи лежат без изменений дольше stale — остатки временного?",
	TicketTrace: "следы работы от trace_min: что работа оставила на диске после себя",
}

// Title — заголовок нового тикета.
func (b *TicketBatch) Title() string {
	switch b.Class {
	case TicketGrew:
		return "Рост записей на диске (aguard disk)"
	case TicketStale:
		return "Остатки временного на диске (aguard disk)"
	}
	return "Следы работы на диске (aguard disk)"
}

// Text — текст нового тикета: что за класс, все находки (путь, размер, с какого времени), приёмка.
func (b *TicketBatch) Text(home, prev, redirect string) string {
	var s strings.Builder
	fmt.Fprintf(&s, "aguard disk: %s, хозяин — %s. Несрочное: письма нет (тикет disk-alert-forecast в aguard); новые находки класса — заметками сюда, не чаще раза в сутки.\n",
		ticketWhat[b.Class], b.Owner)
	if redirect != "" {
		fmt.Fprintf(&s, "Папка хозяина не найдена — тикет лиду: %s.\n", redirect)
	}
	if prev != "" {
		fmt.Fprintf(&s, "Прежний тикет класса: %s (закрыт).\n", prev)
	}
	fmt.Fprintf(&s, "\nНаходки (%d):\n", len(b.Recs))
	for i := range b.Recs {
		fmt.Fprintf(&s, "- %s\n", b.Recs[i].What(home))
	}
	s.WriteString("\nПодробнее: aguard disk report -since 168h.\n")
	s.WriteString("\n## Приёмка\nКаждая находка разобрана: убрана, нужна (с доводом в журнале) или уборка хозяина исправлена; `aguard disk report -since 168h` после правки — без новых находок этого класса у хозяина.\n")
	return s.String()
}

// Note — заметка в открытый тикет: все находки прохода одной строкой.
func (b *TicketBatch) Note(home string) string {
	var s strings.Builder
	fmt.Fprintf(&s, "aguard disk: ещё %d:", len(b.Recs))
	for i := range b.Recs {
		s.WriteString(" ")
		s.WriteString(b.Recs[i].What(home))
		s.WriteString(";")
	}
	return s.String()
}
