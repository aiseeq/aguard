// Package compact — журнал своего сжатия в простое: что aguard сжал бы (или не стал бы и почему) и что потом
// случилось на самом деле — сжало ли встроенное сжатие, вернулся ли человек при живом кэше или уже при мёртвом.
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
)

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
}

// NewStore — дневные файлы журнала в dir.
func NewStore(dir string) daily.Store[Record] {
	return daily.Store[Record]{Dir: dir, What: "журнал сжатия", At: func(r *Record) time.Time { return r.At }}
}

// pending — решение, исход которого ещё не виден.
type pending struct {
	Decision string    `json:"decision"`
	Name     string    `json:"name"`
	Request  time.Time `json:"request"`
	Tokens   int64     `json:"tokens"`
	At       time.Time `json:"at"`
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

// Observe — проверка сессии в проходе: исход прошлого решения, если он виден, и новое решение, если последний
// запрос в окне и по нему ещё не решали.
func (t *Tracker) Observe(c restart.CompactCheck, now time.Time) []Record {
	if c.SessionID == "" {
		return nil
	}
	var out []Record
	if p, ok := t.Pending[c.SessionID]; ok {
		if r, done := outcome(p, c, now); done {
			r.SessionID = c.SessionID
			out = append(out, r)
			delete(t.Pending, c.SessionID)
		}
	}
	if !c.InWindow || t.Decided[c.SessionID].Equal(c.Request) {
		return out
	}
	//nolint:struct-mapping-drops-field // Trigger — чьё сжатие, он нужен только исходу
	r := Record{At: now, Kind: KindWould, SessionID: c.SessionID, Name: c.Name, Request: c.Request, Tokens: c.Tokens, CacheRead: c.CacheRead}
	if !c.Compact {
		r.Kind = KindWouldNot
		for _, a := range c.Against {
			r.Against = append(r.Against, a.Code)
		}
	}
	t.Decided[c.SessionID] = c.Request
	t.Pending[c.SessionID] = pending{Decision: r.Kind, Name: c.Name, Request: c.Request, Tokens: c.Tokens, At: now}
	return append(out, r)
}

// outcome — исход решения p по свежей проверке c: сжатие после запроса решения или следующий запрос.
func outcome(p pending, c restart.CompactCheck, now time.Time) (Record, bool) {
	r := Record{At: now, Kind: KindOutcome, Name: p.Name, Request: p.Request, Tokens: p.Tokens, Decision: p.Decision}
	switch {
	case !c.Compacted.IsZero() && c.Compacted.After(p.Request):
		r.Outcome, r.Trigger, r.AfterS = OutcomeCompacted, c.Trigger, seconds(c.Compacted.Sub(p.Request))
	case c.Request.After(p.Request):
		r.Outcome, r.CacheRead, r.AfterS = OutcomeCacheExpired, c.CacheRead, seconds(c.Request.Sub(p.Request))
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
	for _, id := range slices.Sorted(maps.Keys(t.Pending)) {
		if p := t.Pending[id]; now.Sub(p.At) > lostAfter {
			out = append(out, Record{At: now, Kind: KindOutcome, SessionID: id, Name: p.Name, Request: p.Request,
				Tokens: p.Tokens, Decision: p.Decision, Outcome: OutcomeLost})
			delete(t.Pending, id)
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
