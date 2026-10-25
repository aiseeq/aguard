// Package activity — учёт срабатываний наблюдателей aguard и сбоев самого демона для отчёта `aguard report`:
// что наблюдатель увидел и сделал, кому ушла сводка, сколько было проходов, паник и пропусков по конфигу.
// Срабатывания копятся в памяти по ключу (наблюдатель, вид, что, кому) и раз в несколько минут сливаются
// в дневные JSONL: проход раз в минуту не пишет по строке. При падении демона теряется несохранённый кусок.
package activity

import (
	"cmp"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/aiseeq/aguard/internal/daily"
)

// Виды записей. Наблюдатели добавляют свои (would_reap, sent, cleaned…): вид — строка, отчёт её не толкует.
const (
	KindStart         = "start"          // демон запущен
	KindPass          = "pass"           // проход наблюдателя (у restart — ответ на вопрос cl)
	KindPassFailed    = "pass_failed"    // проход вернул ошибку
	KindPanic         = "panic"          // паника в проходе
	KindConfigSkipped = "config_skipped" // конфиг не прочитался, проход пропущен
	KindWarn          = "warn"           // часть данных не прочиталась, проход продолжен
)

// Record — срабатывания одного ключа за период [From, To].
type Record struct {
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
	Watcher string    `json:"watcher"`
	Kind    string    `json:"kind"`
	What    string    `json:"what,omitempty"`
	Target  string    `json:"to_whom,omitempty"` // кому ушла сводка
	Count   int       `json:"count"`
	Max     int64     `json:"max,omitempty"`  // наибольшее значение за период
	Unit    string    `json:"unit,omitempty"` // единица Max: bytes, ms, entries, procs, trees
	Last    time.Time `json:"last"`           // последнее срабатывание
}

type key struct{ watcher, kind, what, target string }

// Recorder копит срабатывания между сливами. Нулевой указатель ничего не делает: команды вне демона
// (tick, dry-run) не пишут учёт.
type Recorder struct {
	mu    sync.Mutex
	from  time.Time
	cur   map[key]*Record
	store daily.Store[Record]
}

// NewStore — дневные файлы учёта в dir.
func NewStore(dir string) daily.Store[Record] {
	return daily.Store[Record]{Dir: dir, What: "учёт срабатываний", At: func(r *Record) time.Time { return r.To }}
}

// New — учёт с началом периода now.
func New(dir string, now time.Time) *Recorder {
	return &Recorder{from: now, cur: map[key]*Record{}, store: NewStore(dir)}
}

// Add — одно срабатывание; value — величина для Max в единицах unit (0 — без неё).
func (r *Recorder) Add(watcher, kind, what, target string, value int64, unit string, at time.Time) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	k := key{watcher, kind, what, target}
	rec := r.cur[k]
	if rec == nil {
		rec = &Record{Watcher: watcher, Kind: kind, What: what, Target: target, Unit: unit}
		r.cur[k] = rec
	}
	rec.Count++
	rec.Max = max(rec.Max, value)
	rec.Last = later(rec.Last, at)
}

// Flush сливает накопленное в файл дня и сжимает прошлые дни старше keep. Не записалось — накопленное
// остаётся и уйдёт следующим сливом.
func (r *Recorder) Flush(now time.Time, keep time.Duration) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	cur, from := r.cur, r.from
	r.cur, r.from = map[key]*Record{}, now
	r.mu.Unlock()
	recs := make([]Record, 0, len(cur))
	for _, k := range slices.SortedFunc(maps.Keys(cur), cmpKey) {
		rec := *cur[k]
		rec.From, rec.To = from, now
		recs = append(recs, rec)
	}
	if len(recs) > 0 {
		if err := r.store.Append(recs); err != nil {
			r.restore(cur, from)
			return err
		}
	}
	return r.store.Rotate(now, keep)
}

// restore возвращает несохранённое: оно сольётся с пришедшим за время записи.
func (r *Recorder) restore(cur map[key]*Record, from time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.from = from
	for k, old := range cur {
		if c := r.cur[k]; c != nil {
			old.Count += c.Count
			old.Max = max(old.Max, c.Max)
			old.Last = later(old.Last, c.Last)
		}
		r.cur[k] = old
	}
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func cmpKey(a, b key) int {
	return cmp.Or(cmp.Compare(a.watcher, b.watcher), cmp.Compare(a.kind, b.kind),
		cmp.Compare(a.what, b.what), cmp.Compare(a.target, b.target))
}
