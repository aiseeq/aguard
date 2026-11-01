package disk

import (
	"cmp"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
)

// Следы работы агентов (задание лида 26.10 по реплике пользователя 13:42:17 CET в транскрипте лида: «вот всё такое
// aguard и должен отслеживать… сам факт таких следов уже что-то говорит о работе агентов, что можно было бы
// исправить, и что было бы утеряно если бы мы просто сносили всё не глядя»). След — то, что работа оставила на диске
// после себя: материалы неудачной сверки, scratchpad умершей сессии, копия транскриптов, worktree влитой ветки.
// У каждого — кто оставил, какого рода, есть ли у хозяина уборка и её срок. Каждый след записывается в журнал при
// первой встрече и при исчезновении — учтён до уборки; хозяину — находка о работе по классу следов раз в сутки.

// Роды следов, что aguard определяет сам; род класса [[disk.traces]] — из конфига («неудача», «прогон», «копия»).
const (
	TraceCopy     = "копия"
	TraceLeft     = "scratchpad умершей сессии"
	TraceMerged   = "worktree влитой ветки"
	TraceDiverged = "worktree влит по темам, расходится с trunk"
	TraceIdle     = "worktree без движения"
)

// Классы следов, которые aguard знает сам.
const (
	ClassScratchpad = "scratchpad"
	ClassWorktree   = "worktree"
)

// TraceClass — папка, где каждый ребёнок — один след (раздел [[disk.traces]]).
type TraceClass struct {
	Dir      string          `toml:"dir"`
	Kind     string          `toml:"kind"`     // род следа
	What     string          `toml:"what"`     // что это, для находки
	Owner    string          `toml:"owner"`    // сессия-хозяин
	Cleanup  string          `toml:"cleanup"`  // уборка хозяина: что и когда; пусто — нет
	Lifetime policy.Duration `toml:"lifetime"` // срок по уборке хозяина; 0 — срока нет
}

// Trace — след.
type Trace struct {
	Path     string `json:"path"`
	Class    string `json:"class"` // scratchpad, worktree или dir класса каталога
	Kind     string `json:"kind"`
	Producer string `json:"producer,omitempty"` // сессия, ветка или боец
	Command  string `json:"command,omitempty"`  // команда создателя по tmpguard-track
	Note     string `json:"note,omitempty"`     // подробность: вердикт graft и т. п.
	Owner    string `json:"owner"`
	Why      string `json:"why"`
	Cleanup  string `json:"cleanup,omitempty"`
	// Lifetime — числом наносекунд, как time.Duration: у policy.Duration нет MarshalText, JSON писал число и не читал
	// его обратно (проход падал с 26.10 15:00 CET).
	Lifetime time.Duration `json:"lifetime,omitempty"`
	Bytes    int64         `json:"bytes"`
	Copies   int64         `json:"copies,omitempty"` // из них копии транскриптов
	Born     time.Time     `json:"born,omitzero"`
	Changed  time.Time     `json:"changed,omitzero"`
	Outlived bool          `json:"outlived,omitempty"` // старше срока уборки хозяина: уборка не сработала
}

// Creator — создатель записи по tmpguard-track.
type Creator struct {
	Session, Command string
	Born             time.Time
}

// Verdict — вердикт graft worktree clean о worktree.
type Verdict struct {
	Branch, Word, Reason string // Word: would remove, check, in work, unmerged
}

// Facts — что известно о записях помимо замера.
type Facts struct {
	Live     map[string]bool    // id живых сессий
	Names    map[string]string  // id сессии -> имя по транскрипту
	Created  map[string]Creator // путь -> создатель
	Verdicts map[string]Verdict // путь worktree -> вердикт graft
	Born     func(path string) time.Time
}

// scratchCleanup — уборка scratchpad: user tmpfiles (~/.config/user-tmpfiles.d, «D ~/.cache/claude-tmp 0700 alice
// alice 7d»), таймер systemd-tmpfiles-clean раз в сутки.
const (
	scratchCleanup = "systemd-tmpfiles-clean: D ~/.cache/claude-tmp 7d"
	scratchLife    = 7 * 24 * time.Hour
	worktreeClean  = "graft worktree clean --apply (вручную)"
	// slack — запас к сроку уборки: таймер раз в сутки.
	slack = 24 * time.Hour
)

// copyName — имя файла транскрипта Claude Code: сессия (uuid) или суб-агент.
var copyName = regexp.MustCompile(`^([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|agent-[0-9a-f]+)\.jsonl$`)

// isScratchpad — путь — scratchpad сессии: …/claude-tmp/claude-<uid>/<папка проекта>/<сессия>.
func isScratchpad(path string) bool {
	parent := filepath.Dir(path)
	return strings.HasPrefix(filepath.Base(filepath.Dir(parent)), "claude-") &&
		filepath.Base(filepath.Dir(filepath.Dir(parent))) == "claude-tmp"
}

// FindTraces — следы среди замеренных записей. Запись в работе (живая сессия без копий, worktree в работе и свежий)
// следом не считается.
func (c *Config) FindTraces(now time.Time, list []string, usage map[string]Usage, f *Facts,
	owner func(path string) (string, string)) []Trace {
	var out []Trace
	for _, path := range list {
		u := usage[path]
		t := Trace{Path: path, Bytes: u.Bytes, Copies: u.Copies, Changed: u.Changed}
		if cr, ok := f.Created[path]; ok {
			t.Producer, t.Command, t.Born = cr.Session, cr.Command, cr.Born
		}
		if t.Born.IsZero() && f.Born != nil {
			t.Born = f.Born(path)
		}
		ok := false
		switch {
		case isScratchpad(path):
			ok = c.scratchTrace(&t, now, f)
		case f.Verdicts[path] != Verdict{}:
			ok = c.worktreeTrace(&t, now, f.Verdicts[path])
		default:
			ok = c.classTrace(&t, now)
		}
		if !ok {
			continue
		}
		if t.Owner == "" {
			t.Owner, t.Why = owner(path)
		}
		out = append(out, t)
	}
	return out
}

func (c *Config) scratchTrace(t *Trace, now time.Time, f *Facts) bool {
	id := filepath.Base(t.Path)
	t.Class, t.Cleanup, t.Lifetime = ClassScratchpad, scratchCleanup, scratchLife
	if name := f.Names[id]; name != "" {
		t.Producer = name
	} else if t.Producer == "" {
		t.Producer = "сессия " + id[:min(8, len(id))]
	}
	switch {
	case t.Copies >= int64(c.TraceMin):
		t.Kind = TraceCopy
	case f.Live[id] || t.Bytes < int64(c.TraceMin):
		return false
	default:
		t.Kind = TraceLeft
	}
	t.Outlived = !f.Live[id] && !t.Changed.IsZero() && now.Sub(t.Changed) > scratchLife+slack
	return true
}

func (c *Config) worktreeTrace(t *Trace, now time.Time, v Verdict) bool {
	t.Class, t.Cleanup, t.Note = ClassWorktree, worktreeClean, v.Word+": "+v.Reason
	if t.Producer == "" {
		t.Producer = v.Branch
	}
	idle := !t.Changed.IsZero() && now.Sub(t.Changed) >= c.Stale.D()
	switch v.Word {
	case "would remove":
		t.Kind = TraceMerged
	case "check":
		t.Kind = TraceDiverged
	default: // in work, unmerged, без вердикта: след, только если давно не менялся
		if !idle {
			return false
		}
		t.Kind = TraceIdle
	}
	return true
}

func (c *Config) classTrace(t *Trace, now time.Time) bool {
	dir := filepath.Dir(t.Path)
	i := slices.IndexFunc(c.Traces, func(tc TraceClass) bool { return tc.Dir == dir })
	if i < 0 {
		return false
	}
	tc := &c.Traces[i]
	t.Class, t.Kind, t.Note, t.Owner, t.Why = tc.Dir, tc.Kind, tc.What, tc.Owner, "config"
	t.Cleanup, t.Lifetime = tc.Cleanup, tc.Lifetime.D()
	if t.Owner == "" {
		t.Why = ""
	}
	// срок уборки хозяев — от конца работы (farm prune — 72 ч после конца, scene-orders — по mtime): от последнего
	// изменения, а создание — если изменений не видно
	end := cmp.Or(t.Changed, t.Born)
	t.Outlived = t.Lifetime > 0 && !end.IsZero() && now.Sub(end) > t.Lifetime+slack
	return true
}

// Finding — находка о работе по классу следов одного хозяина.
type Finding struct {
	Class, Owner, Why, Kind string
	Count, Outlived         int
	Bytes, OutlivedBytes    int64
	Copies                  int64
	New                     int   // следов, появившихся за сутки
	NewBytes                int64 // их место
	NewSpan                 time.Duration
	Producers               []Count // кто оставил, больше всего — первым
	Cleanup                 string
	Lifetime                time.Duration
}

// Count — имя и число.
type Count struct {
	Name string `json:"name"`
	N    int    `json:"n"`
}

// Findings — находки по следам: класс и хозяин вместе, от trace_min места или с просроченными следами.
func (c *Config) Findings(now time.Time, traces []Trace) []Finding {
	type key struct{ class, owner, kind string }
	groups := map[key]*Finding{}
	producers := map[key]map[string]int{}
	var order []key
	for i := range traces {
		t := &traces[i]
		k := key{t.Class, t.Owner, t.Kind}
		g := groups[k]
		if g == nil {
			g = &Finding{Class: t.Class, Owner: t.Owner, Why: t.Why, Kind: t.Kind, Cleanup: t.Cleanup, Lifetime: t.Lifetime}
			groups[k], producers[k] = g, map[string]int{}
			order = append(order, k)
		}
		g.Count++
		g.Bytes += t.Bytes
		g.Copies += t.Copies
		if t.Outlived {
			g.Outlived++
			g.OutlivedBytes += t.Bytes
		}
		if !t.Born.IsZero() && now.Sub(t.Born) <= 24*time.Hour {
			g.New++
			g.NewBytes += t.Bytes
			g.NewSpan = max(g.NewSpan, now.Sub(t.Born))
		}
		producers[k][cmp.Or(t.Producer, "неизвестно")]++
	}
	var out []Finding
	for _, k := range order {
		g := groups[k]
		if g.Bytes < int64(c.TraceMin) && g.Outlived == 0 {
			continue
		}
		for name, n := range producers[k] {
			g.Producers = append(g.Producers, Count{name, n})
		}
		slices.SortFunc(g.Producers, func(a, b Count) int { return cmp.Or(cmp.Compare(b.N, a.N), cmp.Compare(a.Name, b.Name)) })
		out = append(out, *g)
	}
	return out
}

// Text — находка словами: что накопилось, кто оставил, работает ли уборка хозяина.
func (g *Finding) Text(home string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s): %d, %s", short(g.Class, home), g.Kind, g.Count, policy.HumanBytes(g.Bytes))
	if g.Copies > 0 {
		fmt.Fprintf(&b, ", из них копии транскриптов %s", policy.HumanBytes(g.Copies))
	}
	if g.New > 0 && g.NewSpan >= time.Hour {
		fmt.Fprintf(&b, "; за сутки новых %d, %s, %.1f ГБ/ч", g.New, policy.HumanBytes(g.NewBytes),
			float64(g.NewBytes)/float64(1<<30)/g.NewSpan.Hours())
	}
	b.WriteString("; кто оставил: ")
	for i, p := range g.Producers[:min(3, len(g.Producers))] {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s %d", p.Name, p.N)
	}
	if len(g.Producers) > 3 {
		fmt.Fprintf(&b, " и ещё %d", len(g.Producers)-3)
	}
	switch {
	case g.Cleanup == "":
		b.WriteString("; своей уборки нет")
	case g.Outlived > 0:
		fmt.Fprintf(&b, "; уборка «%s» не сработала: %d старше срока, %s", g.Cleanup, g.Outlived, policy.HumanBytes(g.OutlivedBytes))
	default:
		fmt.Fprintf(&b, "; уборка: %s", g.Cleanup)
	}
	return b.String()
}

func short(path, home string) string {
	if rest, ok := strings.CutPrefix(path, home+"/"); ok {
		return "~/" + rest
	}
	return path
}
