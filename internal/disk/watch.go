package disk

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/daily"
	"github.com/aiseeq/aguard/internal/policy"
	"github.com/aiseeq/aguard/internal/record"
)

// Виды записей журнала.
const (
	KindGrew  = "grew"  // запись выросла за сутки на grow и больше
	KindStale = "stale" // запись от stale_min лежит без изменений дольше stale
	KindFast  = "fast"  // свободное место за fast_window убыло на fast и больше: кто вырос с прошлого обхода
	// Следы работы (traces.go).
	KindTrace     = "trace"      // след встречен впервые: учтён
	KindTraceGone = "trace_gone" // след исчез с диска: убран хозяином или кем-то ещё
	KindFinding   = "finding"    // находка о работе по классу следов хозяина, раз в сутки
)

// Повтор того же сигнала о той же записи — не чаще: рост и остаток — раз в сутки, быстрое наполнение — раз в окно.
const (
	repeatDaily = 24 * time.Hour
	// walkGap — обход по тревоге не чаще: сам обход ~20 с по ~/.cache (3,9 млн файлов 26.10).
	walkGap = 2 * time.Minute
	// history — сколько хранить замеры записи: сутки и запас на опоздавший обход.
	history = 25 * time.Hour
)

// Record — запись журнала: сигнал, который получил бы хозяин.
type Record struct {
	At    time.Time `json:"at"`
	Kind  string    `json:"kind"`
	Path  string    `json:"path"`            // пусто у fast, когда рост не нашёлся ни в одной записи
	Bytes int64     `json:"bytes,omitempty"` // размер записи
	// Delta — рост: за сутки (grew), с прошлого обхода (fast); у fast без записи — убыль свободного места.
	Delta int64 `json:"delta,omitempty"`
	// Since — от какого замера рост (grew, fast) или последнее изменение (stale).
	Since time.Time `json:"since"`
	Owner string    `json:"owner"`
	Why   string    `json:"why"` // откуда хозяин: config, session, project, worktree, names, admin, lead
	Free  int64     `json:"free"`
	Admin string    `json:"admin,omitempty"` // через кого сигнал хозяину: админ его пространства, иначе лид
	Trace *Trace    `json:"trace,omitempty"` // trace, trace_gone
	Text  string    `json:"text,omitempty"`  // finding: находка словами
}

// Point — замер.
type Point struct {
	At    time.Time `json:"at"`
	Bytes int64     `json:"bytes"`
}

// State — между проходами: замеры записей за сутки, свободное место за окно, когда сигнал уже записан.
type State struct {
	Sizes    map[string][]Point   `json:"sizes"`
	Free     []Point              `json:"free"`
	LastWalk time.Time            `json:"last_walk"`
	Told     map[string]time.Time `json:"told"`             // вид и путь -> когда записан
	Traces   map[string]Trace     `json:"traces,omitempty"` // учтённые следы, путь -> последний замер
}

// freeRetry — письмо о свободном месте не дошло (адресата нет среди живых сессий) — повтор через столько.
const freeRetry = time.Hour

// FreeTold — сигнал «свободно меньше free_min»: адресат -> когда ему пора следующее письмо. Свой файл (free.json),
// сохраняется сразу после писем: обход записей бывает брошен остановкой демона, а письмо уже ушло.
type FreeTold map[string]time.Time

// LoadFreeTold — отметки сигнала; файла нет — пусто.
func LoadFreeTold(path string) (FreeTold, error) {
	t := FreeTold{}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return t, nil
	}
	if err != nil {
		return nil, fmt.Errorf("сигнал о свободном месте: %w", err)
	}
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("сигнал о свободном месте %s: %w", path, err)
	}
	return t, nil
}

// Save пишет отметки атомарно.
func (t FreeTold) Save(path string) error {
	b, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("сигнал о свободном месте: %w", err)
	}
	if err := record.WriteFileAtomic(path, b, 0o600); err != nil {
		return fmt.Errorf("сигнал о свободном месте: %w", err)
	}
	return nil
}

// Due — кому из to пора письмо: ниже free_min — сразу, дальше раз в сутки; место поднялось — отметки снимаются, следующее
// падение — снова сразу.
func (t FreeTold) Due(c *Config, now time.Time, free int64, to []string) []string {
	if free >= int64(c.FreeMin) {
		clear(t)
		return nil
	}
	var due []string
	for _, n := range to {
		if next, ok := t[n]; (!ok || !now.Before(next)) && !slices.Contains(due, n) {
			due = append(due, n)
		}
	}
	return due
}

// Mark — письмо to ушло (sent) — следующее через сутки; не ушло — повтор через freeRetry.
func (t FreeTold) Mark(to string, now time.Time, sent bool) {
	if sent {
		t[to] = now.Add(repeatDaily)
	} else {
		t[to] = now.Add(freeRetry)
	}
}

// LowFreeMessage — письмо о свободном месте: сколько, порог и самые большие записи по последнему обходу.
func (s *State) LowFreeMessage(c *Config, free int64, home string) string {
	type big struct {
		path  string
		bytes int64
	}
	var top []big
	for path, pts := range s.Sizes {
		if len(pts) > 0 {
			top = append(top, big{path, pts[len(pts)-1].Bytes})
		}
	}
	slices.SortFunc(top, func(a, b big) int { return cmp.Compare(b.bytes, a.bytes) })
	parts := make([]string, 0, 3)
	for _, b := range top[:min(3, len(top))] {
		parts = append(parts, short(b.path, home)+" "+policy.HumanBytes(b.bytes))
	}
	biggest := "обхода записей ещё не было"
	if len(parts) > 0 {
		biggest = "самые большие записи по последнему обходу: " + strings.Join(parts, ", ")
	}
	return fmt.Sprintf("aguard disk: на /home свободно %s — меньше порога %s (free_min); %s. Повтор — раз в сутки, пока "+
		"место не поднимется выше порога.", policy.HumanBytes(free), policy.HumanBytes(int64(c.FreeMin)), biggest)
}

// Load читает состояние; нет файла — пустое.
func Load(path string) (*State, error) {
	s := &State{Sizes: map[string][]Point{}, Told: map[string]time.Time{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("состояние disk: %w", err)
	}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, fmt.Errorf("состояние disk %s: %w", path, err)
	}
	if s.Sizes == nil {
		s.Sizes = map[string][]Point{}
	}
	if s.Told == nil {
		s.Told = map[string]time.Time{}
	}
	return s, nil
}

// Save пишет состояние через временный файл.
func (s *State) Save(path string) error {
	b, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("состояние disk: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("состояние disk: %w", err)
	}
	_, werr := f.Write(b)
	if err := errors.Join(werr, f.Close()); err != nil {
		return fmt.Errorf("состояние disk: %w", errors.Join(err, os.Remove(f.Name())))
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return fmt.Errorf("состояние disk: %w", err)
	}
	return nil
}

// Measure — замер всех записей одним обходом.
type Measure func(entries []string) map[string]Usage

// MeasureAll — замер с диска: жёсткие ссылки считаются один раз на весь обход; долгоживущие кэши — по st_blocks.
// Отменённый ctx обрывает обход (остановка демона): замер неполон, вызывающий его не сохраняет. yield зовётся каждые
// yieldEvery файлов всего обхода, не записи: записей тысячи, и почти все мельче yieldEvery (проверка 30.10); nil — без
// пауз (разовые команды).
func (c *Config) MeasureAll(ctx context.Context, entries []string, yield func()) map[string]Usage {
	seen := map[inode]bool{}
	buf := new(fiemapBuf)
	out := make(map[string]Usage, len(entries))
	var tick func()
	if yield != nil {
		files := 0
		tick = func() {
			if files++; files%yieldEvery == 0 {
				yield()
			}
		}
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			break
		}
		out[e] = measure(ctx, e, seen, buf, !c.IsLasting(e), tick)
	}
	return out
}

// yieldEvery — через сколько файлов обход спрашивает, не занята ли машина: при холодном кэше метаданных 1024 stat и
// FIEMAP — доли секунды чтения диска.
const yieldEvery = 1024

// Pass — один проход: свободное место free; обход записей раз в walk или по тревоге (место тает быстро).
// Unread — записи, где не прочитались папки: их пишет вызывающий.
func (s *State) Pass(c *Config, now time.Time, free int64, entries func() ([]string, error), m Measure,
	owner func(path string) (string, string)) ([]Record, map[string]Usage, error) {
	s.Free = append(slices.DeleteFunc(s.Free, func(p Point) bool { return now.Sub(p.At) > c.FastWindow.D() }),
		Point{At: now, Bytes: free})
	drop := slices.MaxFunc(s.Free, func(a, b Point) int { return cmp.Compare(a.Bytes, b.Bytes) }).Bytes - free
	fast := drop >= int64(c.Fast)
	if since := now.Sub(s.LastWalk); since < c.Walk.D() && (!fast || since < walkGap) {
		return nil, nil, nil
	}
	list, err := entries()
	if err != nil && len(list) == 0 {
		return nil, nil, err
	}
	usage := m(list)
	s.LastWalk = now
	var out []Record
	tell := func(r Record, every time.Duration) {
		key := r.Kind + " " + r.Path
		if t, ok := s.Told[key]; ok && now.Sub(t) < every {
			return
		}
		s.Told[key] = now
		r.At, r.Free = now, free
		r.Owner, r.Why = owner(r.Path)
		out = append(out, r)
	}
	found := false
	for _, path := range list {
		found = s.entry(c, now, path, usage[path], fast, tell) || found
	}
	if fast && !found {
		tell(Record{Kind: KindFast, Delta: drop, Since: s.Free[0].At}, c.FastWindow.D())
	}
	for path := range s.Sizes {
		if _, ok := usage[path]; !ok {
			delete(s.Sizes, path) // записи нет — нет и замеров
		}
	}
	maps.DeleteFunc(s.Told, func(_ string, t time.Time) bool { return now.Sub(t) > repeatDaily })
	return out, usage, err
}

// entry — сигналы одной записи и её замер в историю; true — запись выросла с прошлого обхода на fast/2 при тающем месте.
func (s *State) entry(c *Config, now time.Time, path string, u Usage, fast bool, tell func(Record, time.Duration)) bool {
	pts := s.Sizes[path]
	grewFast := false
	if fast && len(pts) > 0 {
		if prev := pts[len(pts)-1]; u.Bytes-prev.Bytes >= int64(c.Fast)/2 {
			grewFast = true
			tell(Record{Kind: KindFast, Path: path, Bytes: u.Bytes, Delta: u.Bytes - prev.Bytes, Since: prev.At}, c.FastWindow.D())
		}
	}
	pts = append(slices.DeleteFunc(pts, func(p Point) bool { return now.Sub(p.At) > history }), Point{At: now, Bytes: u.Bytes})
	s.Sizes[path] = pts
	if base := baseline(pts, now); u.Bytes-base.Bytes >= int64(c.Grow) {
		tell(Record{Kind: KindGrew, Path: path, Bytes: u.Bytes, Delta: u.Bytes - base.Bytes, Since: base.At}, repeatDaily)
	}
	if u.Bytes >= int64(c.StaleMin) && !u.Changed.IsZero() && now.Sub(u.Changed) >= c.Stale.D() && !c.IsLasting(path) {
		tell(Record{Kind: KindStale, Path: path, Bytes: u.Bytes, Since: u.Changed}, repeatDaily)
	}
	return grewFast
}

// Account — учёт следов: новый след и исчезнувший с диска — в журнал (след учтён до уборки и после неё), находки по
// классу и хозяину — раз в сутки. exists — есть ли путь на диске; admin — через кого сигнал хозяину.
func (s *State) Account(c *Config, now time.Time, traces []Trace, free int64, home string, exists func(string) bool,
	admin func(owner string) string) []Record {
	if s.Traces == nil {
		s.Traces = map[string]Trace{}
	}
	var out []Record
	rec := func(r Record) Record {
		r.At, r.Free, r.Admin = now, free, admin(r.Owner)
		return r
	}
	seen := make(map[string]bool, len(traces))
	for i := range traces {
		t := traces[i]
		seen[t.Path] = true
		if _, ok := s.Traces[t.Path]; !ok {
			out = append(out, rec(Record{Kind: KindTrace, Path: t.Path, Bytes: t.Bytes, Since: t.Born, Owner: t.Owner, Why: t.Why, Trace: &t}))
		}
		s.Traces[t.Path] = t
	}
	for _, path := range slices.Sorted(maps.Keys(s.Traces)) {
		if seen[path] {
			continue
		}
		t := s.Traces[path]
		delete(s.Traces, path)
		if !exists(path) { // путь на месте, но уже не след (сессия ожила, ветка в работе) — не уборка
			out = append(out, rec(Record{Kind: KindTraceGone, Path: path, Bytes: t.Bytes, Since: t.Born, Owner: t.Owner, Why: t.Why, Trace: &t}))
		}
	}
	findings := c.Findings(now, traces)
	for i := range findings {
		g := &findings[i]
		key := KindFinding + " " + g.Class + " " + g.Owner + " " + g.Kind
		if t, ok := s.Told[key]; ok && now.Sub(t) < repeatDaily {
			continue
		}
		s.Told[key] = now
		out = append(out, rec(Record{Kind: KindFinding, Path: g.Class, Bytes: g.Bytes, Owner: g.Owner, Why: g.Why, Text: g.Text(home)}))
	}
	return out
}

// baseline — самый ранний замер за последние сутки: от него считается рост.
func baseline(pts []Point, now time.Time) Point {
	for _, p := range pts {
		if now.Sub(p.At) <= 24*time.Hour {
			return p
		}
	}
	return pts[len(pts)-1]
}

// NewStore — журнал сигналов по дням.
func NewStore(dir string) daily.Store[Record] {
	return daily.Store[Record]{Dir: dir, What: "записи диска", At: func(r *Record) time.Time { return r.At }}
}
