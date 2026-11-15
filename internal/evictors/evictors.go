// Package evictors finds cgroups whose anon growth coincides with swap growth in other sessions.
package evictors

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"text/tabwriter"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
)

const (
	DefaultSwapMin = 1 << 20
	DefaultAnonMin = 1 << 20
	Keep           = 60 * 24 * time.Hour
)

// Group is a cgroup memory snapshot from one existing iowatch detail pass.
type Group struct {
	Path    string
	Anon    int64
	Swap    int64
	Session bool
}

// Change is an in-memory delta. It is consumed immediately and never persisted as a time series.
type Change struct {
	Path      string
	Session   bool
	Anon      int64
	OtherSwap int64
	Sessions  int
	Candidate bool
	Process   string
}

// Interval contains only changes from a tick interval where other sessions passed the swap threshold.
type Interval struct {
	At      time.Time
	Changes []Change
}

// Tracker compares consecutive snapshots; its first sample seeds state without an event.
type Tracker struct {
	previous map[string]Group
	swapMin  int64
	anonMin  int64
}

func NewTracker(swapMin, anonMin int64) Tracker {
	if swapMin <= 0 {
		swapMin = DefaultSwapMin
	}
	if anonMin <= 0 {
		anonMin = DefaultAnonMin
	}
	return Tracker{swapMin: swapMin, anonMin: anonMin}
}

// Sample advances the baseline on every tick, including intervals without swap growth.
func (t *Tracker) Sample(at time.Time, groups []Group) *Interval {
	current := make(map[string]Group, len(groups))
	for _, g := range groups {
		current[g.Path] = g
	}
	if t.previous == nil {
		t.previous = current
		return nil
	}
	out := &Interval{At: at}
	for _, candidate := range groups {
		var otherSwap int64
		otherSessions := 0
		for path, session := range current {
			if !session.Session || path == candidate.Path {
				continue
			}
			old, ok := t.previous[path]
			if !ok || session.Swap <= old.Swap {
				continue
			}
			otherSwap += session.Swap - old.Swap
			otherSessions++
		}
		if otherSwap < t.swapMin {
			continue
		}
		old, ok := t.previous[candidate.Path]
		anonDelta := candidate.Anon
		if ok {
			anonDelta -= old.Anon
		}
		if anonDelta == 0 {
			continue
		}
		out.Changes = append(out.Changes, Change{Path: candidate.Path, Session: candidate.Session, Anon: anonDelta,
			OtherSwap: otherSwap, Sessions: otherSessions, Candidate: anonDelta >= t.anonMin})
	}
	t.previous = current
	if len(out.Changes) == 0 {
		return nil
	}
	slices.SortFunc(out.Changes, func(a, b Change) int { return cmp.Compare(a.Path, b.Path) })
	return out
}

// Row is an hourly aggregate for one cgroup/session; Process is the latest nonempty source name.
type Row struct {
	Cgroup    string `json:"cgroup"`
	Process   string `json:"process,omitempty"`
	Session   bool   `json:"session,omitempty"`
	Anon      int64  `json:"anon"`
	Swap      int64  `json:"swap"`
	Matched   int64  `json:"matched_swap"`
	Intervals int    `json:"intervals"`
	Sessions  int    `json:"sessions"` // count in the last qualifying interval
}

type Hour struct {
	At   time.Time `json:"at"`
	Rows []Row     `json:"rows"`
}

// Aggregate retains compact hour × candidate summaries, not per-tick records.
type Aggregate struct {
	Hours    []Hour `json:"hours"`
	known    map[candidateKey]struct{}
	revision uint64
	saved    atomic.Uint64
}

type candidateKey struct {
	cgroup  string
	session bool
}

func NewAggregate() *Aggregate { return &Aggregate{known: make(map[candidateKey]struct{})} }

func rowKey(a, b Row) int {
	if n := cmp.Compare(a.Cgroup, b.Cgroup); n != 0 {
		return n
	}
	if a.Session != b.Session {
		if a.Session {
			return 1
		}
		return -1
	}
	return 0
}

// Add folds one eligible interval into its hour. Negative anon deltas adjust an existing row's net.
func (a *Aggregate) Add(in *Interval) {
	if in == nil {
		return
	}
	if a.known == nil {
		a.rebuildKnown()
	}
	hour := in.At.Truncate(time.Hour)
	index := slices.IndexFunc(a.Hours, func(h Hour) bool { return h.At.Equal(hour) })
	var h *Hour
	if index >= 0 {
		h = &a.Hours[index]
	}
	changed := false
	for _, change := range in.Changes {
		row := Row{Cgroup: change.Path, Session: change.Session}
		key := candidateKey{cgroup: change.Path, session: change.Session}
		at := -1
		if h != nil {
			at = slices.IndexFunc(h.Rows, func(x Row) bool { return rowKey(x, row) == 0 })
		}
		if at < 0 {
			_, known := a.known[key]
			if !change.Candidate && !known {
				continue
			}
			if h == nil {
				a.Hours = append(a.Hours, Hour{At: hour})
				h = &a.Hours[len(a.Hours)-1]
			}
			h.Rows = append(h.Rows, row)
			at = len(h.Rows) - 1
		}
		if change.Candidate {
			a.known[key] = struct{}{}
		}
		x := &h.Rows[at]
		x.Anon += change.Anon
		if change.Process != "" {
			x.Process = change.Process
		}
		if change.Candidate {
			x.Swap += change.OtherSwap
			x.Matched += min(change.Anon, change.OtherSwap)
			x.Intervals++
			x.Sessions = change.Sessions
		}
		changed = true
	}
	if h != nil {
		slices.SortFunc(h.Rows, rowKey)
	}
	if a.prune(in.At.Add(-Keep)) {
		changed = true
	}
	if changed {
		slices.SortFunc(a.Hours, func(x, y Hour) int { return x.At.Compare(y.At) })
		a.revision++
	}
}

func (a *Aggregate) prune(since time.Time) bool {
	cut := sort.Search(len(a.Hours), func(i int) bool { return !a.Hours[i].At.Before(since) })
	if cut == 0 {
		return false
	}
	a.Hours = slices.Delete(a.Hours, 0, cut)
	a.rebuildKnown()
	return true
}

func (a *Aggregate) rebuildKnown() {
	a.known = make(map[candidateKey]struct{})
	for _, hour := range a.Hours {
		for _, row := range hour.Rows {
			if row.Intervals > 0 {
				a.known[candidateKey{cgroup: row.Cgroup, session: row.Session}] = struct{}{}
			}
		}
	}
}

func (a *Aggregate) Dirty() bool { return a != nil && a.revision > a.saved.Load() }

// Snapshot clones compact state so serialization and disk I/O can run outside the sampling loop.
func (a *Aggregate) Snapshot() (*Aggregate, uint64) {
	if a == nil {
		return nil, 0
	}
	copy := NewAggregate()
	copy.revision = a.revision
	for key := range a.known {
		copy.known[key] = struct{}{}
	}
	copy.Hours = make([]Hour, len(a.Hours))
	for i, hour := range a.Hours {
		copy.Hours[i] = Hour{At: hour.At, Rows: slices.Clone(hour.Rows)}
	}
	return copy, a.revision
}

// MarkSaved acknowledges only the snapshot version that reached disk.
func (a *Aggregate) MarkSaved(revision uint64) {
	for current := a.saved.Load(); revision > current; current = a.saved.Load() {
		if a.saved.CompareAndSwap(current, revision) {
			return
		}
	}
}

func (a *Aggregate) Prune(now time.Time) {
	if a.prune(now.Add(-Keep)) {
		a.revision++
	}
}

// Save atomically replaces the compact aggregate file.
func Save(path string, a *Aggregate) error {
	if a == nil {
		return errors.New("нет сводки вытеснений")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("папка сводки вытеснений: %w", err)
	}
	b, err := json.Marshal(a.Hours)
	if err != nil {
		return fmt.Errorf("сериализовать сводку вытеснений: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("временный файл сводки вытеснений: %w", err)
	}
	tmp := f.Name()
	_, werr := f.Write(append(b, '\n'))
	if werr == nil {
		werr = f.Sync()
	}
	werr = errors.Join(werr, f.Close())
	if werr == nil {
		werr = os.Chmod(tmp, 0o600)
	}
	if werr == nil {
		werr = os.Rename(tmp, path)
	}
	if werr != nil {
		return fmt.Errorf("записать сводку вытеснений: %w", errors.Join(werr, os.Remove(tmp)))
	}
	a.MarkSaved(a.revision)
	return nil
}

// CleanupTemps removes abandoned atomic-save files. It is intended for daemon startup only.
func CleanupTemps(path string) error {
	pattern := filepath.Join(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	tmps, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("найти временные сводки вытеснений: %w", err)
	}
	var failures []error
	for _, tmp := range tmps {
		if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
			failures = append(failures, fmt.Errorf("удалить временную сводку %s: %w", tmp, err))
		}
	}
	return errors.Join(failures...)
}

// Load reads an aggregate copy and drops hours outside the retention window.
func Load(path string, now time.Time) (*Aggregate, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return NewAggregate(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("прочитать сводку вытеснений: %w", err)
	}
	a := NewAggregate()
	if err := json.Unmarshal(b, &a.Hours); err != nil {
		return nil, fmt.Errorf("разобрать сводку вытеснений: %w", err)
	}
	slices.SortFunc(a.Hours, func(x, y Hour) int { return x.At.Compare(y.At) })
	if a.prune(now.Add(-Keep)) {
		a.revision++
	}
	for i := range a.Hours {
		slices.SortFunc(a.Hours[i].Rows, rowKey)
	}
	a.rebuildKnown()
	return a, nil
}

// Report ranks candidates by the part of other-session swap coincident with anon growth.
type Report struct {
	Rows []Row
	Bad  int
	From time.Time
	To   time.Time
}

func (a *Aggregate) Report(since time.Time, top int) Report {
	byKey := map[string]*Row{}
	var from, to time.Time
	for _, h := range a.Hours {
		if h.At.Before(since.Truncate(time.Hour)) {
			continue
		}
		includedHour := false
		for _, x := range h.Rows {
			includedHour = includedHour || x.Intervals > 0
			key := Key(x.Cgroup, x.Session)
			r := byKey[key]
			if r == nil {
				copy := x
				r = &copy
				byKey[key] = r
			} else {
				r.Anon += x.Anon
				r.Swap += x.Swap
				r.Matched += x.Matched
				r.Intervals += x.Intervals
				if x.Intervals > 0 {
					r.Sessions = x.Sessions
				}
				if x.Process != "" {
					r.Process = x.Process
				}
			}
		}
		if includedHour {
			if from.IsZero() || h.At.Before(from) {
				from = h.At
			}
			if to.IsZero() || h.At.After(to) {
				to = h.At
			}
		}
	}
	r := Report{From: from, To: to}
	for _, row := range byKey {
		if row.Intervals > 0 {
			r.Rows = append(r.Rows, *row)
		}
	}
	slices.SortFunc(r.Rows, func(a, b Row) int {
		if n := cmp.Compare(b.Matched, a.Matched); n != 0 {
			return n
		}
		return rowKey(a, b)
	})
	if top >= 0 && len(r.Rows) > top {
		r.Rows = r.Rows[:top]
	}
	return r
}

// Print shows aggregate-hour range and ranks rows by swap coincident with anon growth.
func (r Report) Print(w io.Writer) error {
	if _, err := fmt.Fprintln(w, "Одновременный рост anon — кандидаты, не доказанная причина."); err != nil {
		return fmt.Errorf("заголовок отчёта кандидатов: %w", err)
	}
	if r.From.IsZero() {
		if _, err := fmt.Fprintln(w, "Период агрегатов: нет данных"); err != nil {
			return fmt.Errorf("период отчёта кандидатов: %w", err)
		}
	} else if _, err := fmt.Fprintf(w, "Период агрегатов: %s — %s\n", r.From.Format("2006-01-02 15:04"), r.To.Add(time.Hour).Format("2006-01-02 15:04")); err != nil {
		return fmt.Errorf("период отчёта кандидатов: %w", err)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "кандидат (cgroup)\tпроцесс с наибольшим RssAnon\tчистый прирост anon\tсвоп сессий, совпавший с ростом anon\tприрост swap других сессий\tинтервалов\tсессий в последнем интервале"); err != nil {
		return fmt.Errorf("шапка таблицы кандидатов: %w", err)
	}
	if len(r.Rows) == 0 {
		if _, err := fmt.Fprintln(tw, "кандидатов за период нет\t—\t—\t—\t—\t0\t0"); err != nil {
			return fmt.Errorf("строка пустого отчёта кандидатов: %w", err)
		}
	}
	for _, x := range r.Rows {
		name := x.Cgroup
		if x.Session {
			name += " [сессия]"
		}
		process := x.Process
		if process == "" {
			process = "—"
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%d\n", name, process, policy.HumanBytes(x.Anon), policy.HumanBytes(x.Matched), policy.HumanBytes(x.Swap), x.Intervals, x.Sessions); err != nil {
			return fmt.Errorf("строка кандидата %s: %w", name, err)
		}
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("таблица кандидатов: %w", err)
	}
	if r.Bad > 0 {
		_, err := fmt.Fprintf(w, "битых записей: %d\n", r.Bad)
		if err != nil {
			return fmt.Errorf("счётчик битых записей: %w", err)
		}
	}
	return nil
}

// Key identifies an hourly candidate independently of its changing process-name attribute.
func Key(cgroup string, session bool) string {
	return strings.Join([]string{cgroup, fmt.Sprint(session)}, "\x00")
}
