// Package compact — журнал своего сжатия в простое: что aguard сжал бы (или не стал бы и почему) и что потом
// случилось на самом деле — сжало ли встроенное сжатие, вернулся ли человек при живом кэше или уже при мёртвом. Рядом —
// решения «сбросил бы» (/clear) беседу с закрытой работой и их исходы: сбросил ли человек сам, с чем вернулся.
// Записи — ~/.local/state/aguard/compact/<день>.jsonl, решения без исхода — state.json там же.
package compact

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"time"

	"github.com/aiseeq/aguard/internal/daily"
	"github.com/aiseeq/aguard/internal/record"
	"github.com/aiseeq/aguard/internal/restart"
)

// Виды записей.
const (
	KindWould    = "would"     // сжал бы сейчас
	KindWouldNot = "would_not" // в окне, но против: Against
	KindOutcome  = "outcome"   // исход решения Decision
	// KindWouldReset — сбросил бы беседу (/clear), KindWouldNotReset — большой контекст и простой, но против.
	KindWouldReset    = "would_reset"
	KindWouldNotReset = "would_not_reset"
)

// resetKey — ключ решения о сбросе в Tracker рядом с ключом сжатия (id сессии).
const resetKey = "/reset"

// Исходы решения.
const (
	// OutcomeCompacted — беседу сжали после решения (встроенное в простое, автосжатие или /compact человека).
	OutcomeCompacted = "compacted"
	// OutcomeCacheAlive — следующий запрос прочитал контекст из кэша: человек вернулся, пока кэш жил.
	OutcomeCacheAlive = "cache_alive"
	// OutcomeCacheExpired — следующий запрос записал контекст в кэш заново: сжатие сэкономило бы.
	OutcomeCacheExpired = "cache_expired"
	// OutcomeLost — исход не дождались: сессия пропала.
	OutcomeLost = "lost"
	// OutcomeCleared — у процесса сессии новая беседа: человек сбросил (/clear) сам.
	OutcomeCleared = "cleared"
)

// Record — запись журнала сжатия.
type Record struct {
	At        time.Time `json:"at"`
	Kind      string    `json:"kind"`
	SessionID string    `json:"session_id"`
	Name      string    `json:"name"`
	// Request — начало запроса, после которого решали; Tokens — контекст его ответа.
	Request time.Time `json:"request"`
	Tokens  int64     `json:"tokens"`
	// Against — коды доводов против (would_not).
	Against []string `json:"against,omitempty"`
	// Decision — к какому решению исход (would или would_not), Outcome — исход.
	Decision string `json:"decision,omitempty"`
	Outcome  string `json:"outcome,omitempty"`
	// AfterS — секунд от Request до сжатия или следующего запроса.
	AfterS int64 `json:"after_s,omitempty"`
	// CacheRead — сколько прочитал из кэша запрос решения (у исхода — следующий запрос); Trigger — чьё сжатие.
	CacheRead int64  `json:"cache_read,omitempty"`
	Trigger   string `json:"trigger,omitempty"`
	// Model — модель беседы; RewriteMicroUSD — во что обойдётся следующий запрос, если кэш умрёт (контекст пишется
	// заново), ReadMicroUSD — каждый запрос при живом кэше; в миллионных долях доллара. Оценка по справке цен (price.go).
	Model           string `json:"model,omitempty"`
	RewriteMicroUSD int64  `json:"rewrite_micro_usd,omitempty"`
	ReadMicroUSD    int64  `json:"read_micro_usd,omitempty"`
	// Reply — итог последнего хода (у решений о сбросе); Prompt — реплика, с которой беседа продолжилась (у исхода):
	// по ней видно, нужен ли был старый контекст.
	Reply  string `json:"reply,omitempty"`
	Prompt string `json:"prompt,omitempty"`
}

// NewStore — дневные файлы журнала в dir.
func NewStore(dir string) daily.Store[Record] {
	return daily.Store[Record]{Dir: dir, What: "журнал сжатия", At: func(r *Record) time.Time { return r.At }}
}

// pending — решение, исход которого ещё не виден.
type pending struct {
	Decision        string    `json:"decision"`
	SessionID       string    `json:"session_id"`
	Pid             int       `json:"pid"`
	Name            string    `json:"name"`
	Request         time.Time `json:"request"`
	Tokens          int64     `json:"tokens"`
	RewriteMicroUSD int64     `json:"rewrite_micro_usd"`
	At              time.Time `json:"at"`
}

// Tracker помнит решения по сессиям между проходами.
type Tracker struct {
	Pending map[string]pending `json:"pending"`
	// Decided — по сессии: запрос, по которому решение уже записано. Одно решение на запрос.
	Decided map[string]time.Time `json:"decided"`
}

// Load — состояние из файла; нет файла — пустое.
func Load(path string) (*Tracker, error) {
	t := &Tracker{}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("решения сжатия: %w", err)
	default:
		if err := json.Unmarshal(b, t); err != nil {
			return nil, fmt.Errorf("решения сжатия %s: %w", path, err)
		}
	}
	if t.Pending == nil {
		t.Pending = map[string]pending{}
	}
	if t.Decided == nil {
		t.Decided = map[string]time.Time{}
	}
	for key, p := range t.Pending {
		if p.SessionID == "" { // состояние до решений о сбросе: ключ — id сессии
			p.SessionID = key
			t.Pending[key] = p
		}
	}
	return t, nil
}

// Save пишет состояние атомарно.
func (t *Tracker) Save(path string) error {
	b, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("решения сжатия: %w", err)
	}
	if err := record.WriteFileAtomic(path, b, 0o600); err != nil {
		return fmt.Errorf("решения сжатия: %w", err)
	}
	return nil
}

// decision — одно из решений по сессии: сжатие или сброс.
type decision struct {
	key        string // ключ в Tracker
	would, not string // виды записей
	candidate  bool   // решение стоит записать
	yes        bool
	against    []restart.Reason
	reply      bool // писать итог хода
}

// Observe — проверка сессии в проходе: исходы прошлых решений, если они видны (и новая беседа у того же процесса —
// сброс человеком), и новые решения о сжатии и сбросе, если по последнему запросу ещё не решали.
func (t *Tracker) Observe(c restart.CompactCheck, now time.Time) []Record {
	if c.SessionID == "" {
		return nil
	}
	out := t.cleared(c, now)
	for _, d := range []decision{
		{key: c.SessionID, would: KindWould, not: KindWouldNot, candidate: c.InWindow, yes: c.Compact, against: c.Against},
		{key: c.SessionID + resetKey, would: KindWouldReset, not: KindWouldNotReset, candidate: c.Reset.Candidate,
			yes: c.Reset.Reset, against: c.Reset.Against, reply: true},
	} {
		out = append(out, t.observe(d, c, now)...)
	}
	return out
}

// observe — исход прошлого решения d и новое решение по последнему запросу.
func (t *Tracker) observe(d decision, c restart.CompactCheck, now time.Time) []Record {
	var out []Record
	if p, ok := t.Pending[d.key]; ok {
		if r, done := outcome(p, c, now); done {
			out = append(out, r)
			delete(t.Pending, d.key)
		}
	}
	if !d.candidate || t.Decided[d.key].Equal(c.Request) {
		return out
	}
	rewrite, read := cost(c.Model, c.Tokens)
	//nolint:struct-mapping-drops-field // Trigger — чьё сжатие, он нужен только исходу
	r := Record{At: now, Kind: d.would, SessionID: c.SessionID, Name: c.Name, Request: c.Request, Tokens: c.Tokens,
		CacheRead: c.CacheRead, Model: c.Model, RewriteMicroUSD: rewrite, ReadMicroUSD: read}
	if d.reply {
		r.Reply = c.Reply
	}
	if !d.yes {
		r.Kind = d.not
		for _, a := range d.against {
			r.Against = append(r.Against, a.Code)
		}
	}
	t.Decided[d.key] = c.Request
	t.Pending[d.key] = pending{Decision: r.Kind, SessionID: c.SessionID, Pid: c.Pid, Name: c.Name, Request: c.Request,
		Tokens: c.Tokens, RewriteMicroUSD: rewrite, At: now}
	return append(out, r)
}

// cleared — решения о других беседах того же процесса: у процесса новая беседа, старую сбросили.
func (t *Tracker) cleared(c restart.CompactCheck, now time.Time) []Record {
	var out []Record
	for _, key := range slices.Sorted(maps.Keys(t.Pending)) {
		if p := t.Pending[key]; c.Pid != 0 && p.Pid == c.Pid && p.SessionID != c.SessionID {
			r := p.record(now, OutcomeCleared)
			r.AfterS = seconds(now.Sub(p.Request))
			out = append(out, r)
			delete(t.Pending, key)
		}
	}
	return out
}

// record — запись исхода решения p.
func (p pending) record(now time.Time, outcome string) Record {
	return Record{At: now, Kind: KindOutcome, SessionID: p.SessionID, Name: p.Name, Request: p.Request, Tokens: p.Tokens,
		Decision: p.Decision, Outcome: outcome, RewriteMicroUSD: p.RewriteMicroUSD}
}

// outcome — исход решения p по свежей проверке c: сжатие после запроса решения или следующий запрос.
func outcome(p pending, c restart.CompactCheck, now time.Time) (Record, bool) {
	r := p.record(now, "")
	switch {
	case !c.Compacted.IsZero() && c.Compacted.After(p.Request):
		r.Outcome, r.Trigger, r.AfterS = OutcomeCompacted, c.Trigger, seconds(c.Compacted.Sub(p.Request))
	case c.Request.After(p.Request):
		r.Outcome, r.CacheRead, r.AfterS = OutcomeCacheExpired, c.CacheRead, seconds(c.Request.Sub(p.Request))
		if c.PromptAt.After(p.Request) {
			r.Prompt = c.Prompt
		}
		// После смерти кэша из него читается только общий для всех бесед кусок (подсказка и инструменты).
		if c.CacheRead*2 >= p.Tokens {
			r.Outcome = OutcomeCacheAlive
		}
	default:
		return r, false
	}
	return r, true
}

// Expire — решения без исхода дольше lostAfter: сессия пропала. Заодно забывает давние решённые запросы.
func (t *Tracker) Expire(now time.Time, lostAfter time.Duration) []Record {
	var out []Record
	for _, key := range slices.Sorted(maps.Keys(t.Pending)) {
		if p := t.Pending[key]; now.Sub(p.At) > lostAfter {
			out = append(out, p.record(now, OutcomeLost))
			delete(t.Pending, key)
		}
	}
	for id, req := range t.Decided {
		if _, waiting := t.Pending[id]; !waiting && now.Sub(req) > 2*time.Hour {
			delete(t.Decided, id) // запрос давно вне окна: решать по нему снова не придётся
		}
	}
	return out
}

func seconds(d time.Duration) int64 { return int64(d / time.Second) }
