package disk

import (
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
)

func traceConfig(t *testing.T, home string) *Config {
	t.Helper()
	c := testConfig(t, home)
	c.Traces = []TraceClass{{Dir: "~/.cache/scene", Kind: "неудача", What: "сверки", Owner: "sc2go-master",
		Cleanup: "удаляет старше 2 суток", Lifetime: policy.Duration(48 * time.Hour)}}
	if err := c.Validate(home); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestFindTraces(t *testing.T) {
	home := "/home/u"
	c := traceConfig(t, home)
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	pads := home + "/.cache/claude-tmp/claude-1000/-home-u-git-x/"
	live, dead, small, copies := pads+"live", pads+"dead", pads+"small", pads+"copies"
	merged, diverged, busy, idle := home+"/.cache/worktrees/x/a", home+"/.cache/worktrees/x/b",
		home+"/.cache/worktrees/x/c", home+"/.cache/worktrees/x/d"
	fresh, old, other := home+"/.cache/scene/s-1", home+"/.cache/scene/s-2", home+"/.cache/elsewhere"
	usage := map[string]Usage{
		live: {Bytes: 5 * gb, Changed: now}, dead: {Bytes: 3 * gb, Changed: now.Add(-9 * 24 * time.Hour)},
		small: {Bytes: gb / 2}, copies: {Bytes: 4 * gb, Copies: 2 * gb, Changed: now},
		merged: {Bytes: gb, Changed: now}, diverged: {Bytes: gb, Changed: now}, busy: {Bytes: gb, Changed: now},
		idle:  {Bytes: gb, Changed: now.Add(-72 * time.Hour)},
		fresh: {Bytes: 85 << 20, Changed: now.Add(-time.Hour)}, old: {Bytes: 85 << 20, Changed: now.Add(-80 * time.Hour)},
		other: {Bytes: 10 * gb},
	}
	f := &Facts{
		Live:    map[string]bool{"live": true, "copies": true},
		Names:   map[string]string{"dead": "x-master"},
		Created: map[string]Creator{fresh: {Session: "боец w/x", Command: "mv /tmp/s-1 /home/u/.cache/scene/", Born: now.Add(-2 * time.Hour)}},
		Verdicts: map[string]Verdict{merged: {"w/a", "would remove", "merged"}, diverged: {"w/b", "check", "conflicts"},
			busy: {"w/c", "in work", "fresh"}, idle: {"", NoVerdict, "no remote"}},
	}
	owner := func(string) (string, string) { return "x-master", "names" }
	list := []string{live, dead, small, copies, merged, diverged, busy, idle, fresh, old, other}
	got := map[string]Trace{}
	for _, tr := range c.FindTraces(now, list, usage, f, owner) {
		got[tr.Path] = tr
	}
	want := map[string]string{dead: TraceLeft, copies: TraceCopy, merged: TraceMerged, diverged: TraceDiverged,
		idle: TraceIdle, fresh: "неудача", old: "неудача"}
	if len(got) != len(want) {
		t.Errorf("следов %d, want %d: %v", len(got), len(want), got)
	}
	for path, kind := range want {
		if got[path].Kind != kind {
			t.Errorf("%s: род %q, want %q", path, got[path].Kind, kind)
		}
	}
	if tr := got[dead]; tr.Producer != "x-master" || !tr.Outlived || tr.Owner != "x-master" {
		t.Errorf("scratchpad умершей сессии: %+v", tr)
	}
	if tr := got[fresh]; tr.Producer != "боец w/x" || tr.Command == "" || tr.Owner != "sc2go-master" || tr.Why != "config" || tr.Outlived {
		t.Errorf("свежая неудача: %+v", tr)
	}
	if !got[old].Outlived {
		t.Error("неудача старше срока не просрочена")
	}
	if tr := got[merged]; tr.Producer != "w/a" || !strings.HasPrefix(tr.Note, "would remove") {
		t.Errorf("worktree влитой ветки: %+v", tr)
	}
}

func TestFindings(t *testing.T) {
	c := traceConfig(t, "/home/u")
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	dir := "/home/u/.cache/scene"
	var traces []Trace
	for i := range 20 { // 20 сверок по 85 МБ за 10 часов: 1.7 ГБ
		traces = append(traces, Trace{Path: dir + "/s", Class: dir, Kind: "неудача", Owner: "sc2go-master", Why: "config",
			Cleanup: "удаляет", Bytes: 85 << 20, Born: now.Add(-time.Duration(i) * 30 * time.Minute), Producer: []string{"w/a", "w/b"}[i%3/2]})
	}
	traces = append(traces, Trace{Path: "/p", Class: ClassScratchpad, Kind: TraceLeft, Owner: "x", Bytes: gb / 4})
	got := c.Findings(now, traces)
	if len(got) != 1 {
		t.Fatalf("находки: %+v", got)
	}
	g := got[0]
	if g.Count != 20 || g.New != 20 || g.Producers[0] != (Count{"w/a", 14}) || g.Producers[1] != (Count{"w/b", 6}) {
		t.Errorf("находка: %+v", g)
	}
	text := g.Text("/home/u")
	for _, s := range []string{"~/.cache/scene (неудача): 20", "за сутки новых 20", "w/a 14", "уборка: удаляет"} {
		if !strings.Contains(text, s) {
			t.Errorf("в %q нет %q", text, s)
		}
	}
	// одна мелкая, но просроченная — находка
	traces[20].Outlived, traces[20].Cleanup = true, scratchCleanup
	if got := c.Findings(now, traces); len(got) != 2 || !strings.Contains(got[1].Text("/home/u"), "не сработала") {
		t.Errorf("просроченная: %+v", got)
	}
}

func TestAccount(t *testing.T) {
	c := traceConfig(t, "/home/u")
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	s := &State{Sizes: map[string][]Point{}, Told: map[string]time.Time{}}
	a := Trace{Path: "/a", Class: "/c", Kind: "неудача", Owner: "o", Bytes: 2 * gb}
	b := Trace{Path: "/b", Class: "/c", Kind: "неудача", Owner: "o", Bytes: gb}
	onDisk := map[string]bool{"/a": true, "/b": true}
	exists := func(p string) bool { return onDisk[p] }
	admin := func(string) string { return "git-admin" }
	kinds := func(recs []Record) string {
		var out []string
		for _, r := range recs {
			out = append(out, r.Kind+" "+r.Path)
		}
		return strings.Join(out, ", ")
	}
	if got := kinds(s.Account(c, now, []Trace{a, b}, 0, "/home/u", exists, admin)); got != "trace /a, trace /b, finding /c" {
		t.Errorf("первый проход: %s", got)
	}
	// через час: следы известны, находка — раз в сутки
	if got := s.Account(c, now.Add(time.Hour), []Trace{a, b}, 0, "/home/u", exists, admin); len(got) != 0 {
		t.Errorf("повтор: %s", kinds(got))
	}
	// /b убран, /a больше не след, но лежит: исчезновение — только /b
	delete(onDisk, "/b")
	got := s.Account(c, now.Add(2*time.Hour), nil, 0, "/home/u", exists, admin)
	if kinds(got) != "trace_gone /b" || got[0].Admin != "git-admin" || got[0].Trace.Bytes != gb {
		t.Errorf("уборка: %s", kinds(got))
	}
}

func TestParseVerdicts(t *testing.T) {
	out := []byte("graft: check     /w/a (w/a): its commits are all in origin/main by subject, but (it) conflicts\n" +
		"graft: would remove /w/b (w/b): merged\nшум\ngraft: in work   /w/c (w/c): used 5m ago\n")
	got := map[string]Verdict{}
	ParseVerdicts(out, got)
	if len(got) != 3 || got["/w/a"].Word != "check" || got["/w/b"] != (Verdict{"w/b", "would remove", "merged"}) ||
		!strings.HasSuffix(got["/w/a"].Reason, "(it) conflicts") {
		t.Errorf("ParseVerdicts = %+v", got)
	}
}

func TestAdminOf(t *testing.T) {
	o := &Owners{
		Admins:   map[string]string{"/home/u/git": "git-admin", "/home/u/work": "work-admin"},
		Lead:     "lead",
		Live:     []Session{{Name: "aguard-master", Cwd: "/home/u/git/aguard"}},
		Projects: map[string]string{ProjectKey("/home/u/work/ledger"): "ledger-master"},
	}
	for owner, want := range map[string]string{"aguard-master": "git-admin", "ledger-master": "work-admin",
		"git-admin": "lead", "lead": "", "chto-to": "lead"} {
		if got := o.AdminOf(owner); got != want {
			t.Errorf("AdminOf(%s) = %q, want %q", owner, got, want)
		}
	}
}
