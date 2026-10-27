package agentwait

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/transcript"
)

// fake — распознаватель для проверки обвязки: «sleep …» ждёт до своего вывода, «bg …» — до уведомления.
type fake struct {
	open map[string]Found // id вызова → начатое
	bg   *Found
}

func newFake() Recognizer { return &fake{open: map[string]Found{}} }

func (f *fake) Add(ev transcript.Event) []Found {
	switch {
	case ev.Role == transcript.Cmd && strings.HasPrefix(ev.Text, "sleep"):
		f.open[ev.CallID] = Found{Start: ev.Time, How: "sleep", Class: "other", Command: ev.Text, Calls: 1, Line: ev.Line}
	case ev.Role == transcript.Cmd && strings.HasPrefix(ev.Text, "bg"):
		f.bg = &Found{Start: ev.Time, How: "background", Class: "test", Command: ev.Text, Calls: 1, Line: ev.Line}
	case ev.Role == transcript.Out:
		if w, ok := f.open[ev.CallID]; ok {
			delete(f.open, ev.CallID)
			w.End, w.EndLine, w.Outcome = ev.Time, ev.Line, "done"
			return []Found{w}
		}
	case f.bg != nil && strings.Contains(ev.Text, "task-notification"):
		w := *f.bg
		f.bg = nil
		w.End, w.EndLine, w.Outcome = ev.Time, ev.Line, "done"
		return []Found{w}
	}
	return nil
}

func (f *fake) Open() (int, bool) {
	line := 0
	if f.bg != nil {
		line = f.bg.Line
	}
	for _, w := range f.open {
		if line == 0 || w.Line < line {
			line = w.Line
		}
	}
	return line, line > 0
}

func (f *fake) Flush() []Found {
	var out []Found
	if f.bg != nil {
		w := *f.bg
		w.Outcome = "none"
		out = append(out, w)
		f.bg = nil
	}
	return out
}

func useFake(t *testing.T) {
	t.Helper()
	c, x := NewClaude, NewCodex
	NewClaude, NewCodex = newFake, newFake
	t.Cleanup(func() { NewClaude, NewCodex = c, x })
}

var stamp = time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)

func at(min int) string { return stamp.Add(time.Duration(min) * time.Minute).Format(time.RFC3339) }

func call(min int, id, command string) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"cwd":"/home/alice/git/aguard","message":{"role":"assistant","content":[{"type":"tool_use","id":%q,"name":"Bash","input":{"command":%q}}]}}`, at(min), id, command)
}

func result(min int, id string) string {
	return fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"ok"}]}}`, at(min), id)
}

func notification(min int) string {
	return fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":"<task-notification><task-id>b1</task-id><status>completed</status></task-notification>"}}`, at(min))
}

func appendLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		t.Fatal(err)
	}
}

// Незаконченное ожидание держит смещение: файл перечитывается с его начала, а законченные раньше не повторяются.
func TestRunRereadsOpen(t *testing.T) {
	useFake(t)
	projects := t.TempDir()
	file := filepath.Join(projects, "-home-alice-git-aguard", "s1.jsonl")
	appendLines(t, file, call(0, "t1", "bg go test ./..."), call(1, "t2", "sleep 30"), result(2, "t2"))
	p := &Pass{Projects: projects, Workers: t.TempDir(), Backfill: time.Hour, Budget: 1 << 20, MaxOpen: 6 * time.Hour,
		Name: func(cwd string) string { return filepath.Base(cwd) + "-master" }}
	st := &State{Files: map[string]*File{}}
	now := stamp.Add(5 * time.Minute)
	got, err := p.Run(st, now)
	if err != nil || len(got) != 1 || got[0].How != "sleep" || got[0].Ms != 60_000 || got[0].Name != "aguard-master" || got[0].Session != "s1" {
		t.Fatalf("первый проход: %+v %v", got, err)
	}
	if s := st.Files["claude/-home-alice-git-aguard/s1.jsonl"]; s.Offset != 0 || s.Done == 0 {
		t.Fatalf("смещение держит начало фоновой: %+v", s)
	}
	appendLines(t, file, notification(10))
	got, err = p.Run(st, now.Add(10*time.Minute))
	if err != nil || len(got) != 1 || got[0].How != "background" || got[0].Ms != 600_000 {
		t.Fatalf("второй проход: %+v %v", got, err)
	}
	if got, err = p.Run(st, now.Add(20*time.Minute)); err != nil || len(got) != 0 {
		t.Fatalf("третий проход пуст: %+v %v", got, err)
	}
}

// Открытое дольше MaxOpen закрывается с исходом none, смещение уходит в конец.
func TestRunFlushesStale(t *testing.T) {
	useFake(t)
	projects := t.TempDir()
	file := filepath.Join(projects, "p", "s1", "subagents", "agent-a1.jsonl")
	appendLines(t, file, call(0, "t1", "bg sleep 99999"))
	p := &Pass{Projects: projects, Workers: t.TempDir(), Backfill: time.Hour, Budget: 1 << 20, MaxOpen: 6 * time.Hour}
	st := &State{Started: true, Files: map[string]*File{}}
	got, err := p.Run(st, stamp.Add(7*time.Hour))
	if err != nil || len(got) != 1 || got[0].Outcome != "none" || got[0].Agent != "a1" || got[0].Session != "s1" {
		t.Fatalf("%+v %v", got, err)
	}
	info, _ := os.Stat(file)
	if s := st.Files["claude/p/s1/subagents/agent-a1.jsonl"]; s.Offset != info.Size() {
		t.Errorf("смещение: %+v", s)
	}
}

// До первого прохода старые файлы пропускаются, свежие читаются с начала; codex — по папке задачи.
func TestRunBackfill(t *testing.T) {
	useFake(t)
	projects, workers := t.TempDir(), t.TempDir()
	old := filepath.Join(projects, "p", "old.jsonl")
	appendLines(t, old, call(0, "t1", "sleep 1"), result(1, "t1"))
	ago := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(old, ago, ago); err != nil {
		t.Fatal(err)
	}
	rollout := filepath.Join(workers, "1027-fix", "codex", "sessions", "2026", "10", "27", "rollout-2026-10-27T10-00-00-abc.jsonl")
	appendLines(t, rollout, `{"timestamp":"2026-10-27T10:00:00Z","type":"session_meta","payload":{"id":"abc","cwd":"/w"}}`)
	p := &Pass{Projects: projects, Workers: workers, Backfill: 7 * 24 * time.Hour, Budget: 1 << 20, MaxOpen: 6 * time.Hour}
	st := &State{Files: map[string]*File{}}
	if got, err := p.Run(st, time.Now()); err != nil || len(got) != 0 {
		t.Fatalf("%+v %v", got, err)
	}
	info, _ := os.Stat(old)
	if s := st.Files["claude/p/old.jsonl"]; s.Offset != info.Size() {
		t.Errorf("старый файл не пропущен: %+v", s)
	}
	key := "codex/1027-fix/codex/sessions/2026/10/27/rollout-2026-10-27T10-00-00-abc.jsonl"
	if s := st.Files[key]; s == nil || s.Offset == 0 {
		t.Errorf("rollout не прочитан: %+v", st.Files)
	}
	files, err := List(projects, workers)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Runner == RunnerCodex && (f.Project != "1027-fix" || f.Session != "2026-10-27T10-00-00-abc") {
			t.Errorf("codex: %+v", f)
		}
	}
}

// Сводка: окна, классы по времени ожидания, примеры «прочего», очередь graft только верхнего уровня.
func TestCollect(t *testing.T) {
	now := stamp.Add(24 * time.Hour)
	waits := []Wait{
		{At: now.Add(-time.Hour), Ms: 600_000, Runner: "claude", Session: "s1", How: "loop", Class: "test", Calls: 3, Repeats: 2, Tokens: 300_000, Outcome: "done"},
		{At: now.Add(-2 * time.Hour), Ms: 30_000, Runner: "codex", Session: "x", How: "stdin_poll", Class: "other", Command: "go test", Calls: 5, Tokens: 250_000, Outcome: "done"},
		{At: now.Add(-3 * 24 * time.Hour), Ms: 5_000, Runner: "claude", Session: "s2", How: "sleep", Class: "other", Command: "sleep 5", Calls: 1, Outcome: "done"},
	}
	graft := filepath.Join(t.TempDir(), "stats.jsonl")
	var b bytes.Buffer
	for _, l := range []string{
		`{"time":%q,"kind":"task","top":"exec","wall_s":50,"wait_s":36,"wait_why":{"swap":6,"settle":30}}`,
		`{"time":%q,"kind":"task","top":"commit","in":"gate","wait_s":20,"wait_why":{"queue":20}}`,
		`{"time":%q,"kind":"task","top":"test","wait_s":4}`,
		`{"time":%q,"kind":"step","top":"exec","wall_s":1}`,
	} {
		fmt.Fprintf(&b, l+"\n", now.Add(-time.Hour).Format(time.RFC3339))
	}
	b.WriteString("не json\n")
	if err := os.WriteFile(graft, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Collect(now, func(fn func(*Wait)) (int, error) {
		for i := range waits {
			fn(&waits[i])
		}
		return 0, nil
	}, []string{graft + ".1", graft})
	if err != nil {
		t.Fatal(err)
	}
	day, week := s.Windows["day"], s.Windows["week"]
	if day.Total.Waits != 2 || week.Total.Waits != 3 || day.Total.Calls != 8 || day.Total.Tokens != 550_000 {
		t.Errorf("итог: день %+v, неделя %+v", day.Total, week.Total)
	}
	if day.Classes[0].Class != "test" || day.Classes[0].Label != "тест или гейт" || day.Classes[0].Repeats != 2 {
		t.Errorf("классы: %+v", day.Classes[0])
	}
	if len(week.Other) != 2 || week.Other[0].Runner != "codex" { // при равном числе — дольше ждали выше
		t.Errorf("прочее: %+v", week.Other)
	}
	if g := day.Graft; g.Tasks != 2 || g.Waited != 2 || g.WaitS != 40 || g.Why["settle"] != 30 || g.Why[WhyUnknown] != 4 || g.ByTop["exec"] != 36 {
		t.Errorf("graft: %+v", g)
	}
	if s.Bad != 1 {
		t.Errorf("bad %d", s.Bad)
	}
	var out bytes.Buffer
	if err := s.Print(&out, "week"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"тест или гейт", "всего", "Прочее", "Очередь graft: из 2 задач ждали 2"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("нет %q в\n%s", want, out.String())
		}
	}
}

func TestValidate(t *testing.T) {
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.MaxOpen = 0
	if c.Validate() == nil {
		t.Error("max_open 0 прошёл")
	}
}
