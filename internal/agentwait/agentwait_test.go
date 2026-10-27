package agentwait

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var stamp = time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)

func at(min int) string { return stamp.Add(time.Duration(min) * time.Minute).Format(time.RFC3339) }

func call(min int, id, command string, bg bool) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"cwd":"/home/alice/git/aguard","message":{"role":"assistant","content":[{"type":"tool_use","id":%q,"name":"Bash","input":{"command":%q,"run_in_background":%v}}]}}`, at(min), id, command, bg)
}

func result(min int, id, text string) string {
	return fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":%q}]}}`, at(min), id, text)
}

func notification(min int, id string) string {
	return fmt.Sprintf(`{"type":"user","origin":{"kind":"task-notification"},"timestamp":%q,"message":{"role":"user","content":"<task-notification><task-id>b1</task-id><tool-use-id>%s</tool-use-id><status>completed</status><summary>Background command completed (exit code 0)</summary></task-notification>"}}`, at(min), id)
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
	projects := t.TempDir()
	file := filepath.Join(projects, "-home-alice-git-aguard", "s1.jsonl")
	appendLines(t, file, call(0, "t1", "go test ./...", true), result(0, "t1", "Command running in background with ID: b1."),
		call(1, "t2", "sleep 30", false), result(2, "t2", ""), call(3, "t3", "ls", false), result(3, "t3", "a"))
	p := &Pass{Projects: projects, Workers: t.TempDir(), Backfill: time.Hour, Budget: 1 << 20, MaxOpen: 6 * time.Hour,
		Name: func(cwd string) string { return filepath.Base(cwd) + "-master" }}
	st := &State{Files: map[string]*File{}}
	now := stamp.Add(5 * time.Minute)
	got, err := p.Run(st, now)
	if err != nil || len(got) != 1 || got[0].How != "sleep" || got[0].Ms != 60_000 || got[0].Name != "aguard-master" || got[0].Session != "s1" {
		t.Fatalf("первый проход: %+v %v", got, err)
	}
	if s := st.Files["claude/-home-alice-git-aguard/s1.jsonl"]; s.Offset != 0 || len(s.Seen) != 1 {
		t.Fatalf("смещение держит начало фоновой, sleep — в записанных: %+v", s)
	}
	appendLines(t, file, notification(10, "t1"))
	got, err = p.Run(st, now.Add(10*time.Minute))
	if err != nil || len(got) != 1 || got[0].How != "background" || got[0].Class != "test" || got[0].Ms != 600_000 {
		t.Fatalf("второй проход: %+v %v", got, err)
	}
	if got, err = p.Run(st, now.Add(20*time.Minute)); err != nil || len(got) != 0 {
		t.Fatalf("третий проход пуст: %+v %v", got, err)
	}
	if s := st.Files["claude/-home-alice-git-aguard/s1.jsonl"]; s.Seen != nil {
		t.Errorf("открытых нет — записанные не хранятся: %+v", s)
	}
}

// Файл больше бюджета читается кусками по остатку бюджета, хотя хвост помельче идёт первым каждый проход (открытое
// ожидание перечитывается до MaxOpen): раньше он не читался никогда. Кусок не рвёт ожидание: каждое — ровно один раз.
func TestRunChunksBig(t *testing.T) {
	projects := t.TempDir()
	small := filepath.Join(projects, "p", "small.jsonl")
	big := filepath.Join(projects, "p", "big.jsonl")
	appendLines(t, small, call(0, "s1", "sleep 5", false), result(1, "s1", ""))
	const n = 40
	for i := range n {
		id := fmt.Sprintf("b%d", i)
		appendLines(t, big, call(2*i, id, fmt.Sprintf("sleep %d", 60+i), false), result(2*i+1, id, ""), call(2*i+1, id+"l", "ls", false), result(2*i+1, id+"l", "a"))
	}
	info, err := os.Stat(big)
	if err != nil {
		t.Fatal(err)
	}
	p := &Pass{Projects: projects, Workers: t.TempDir(), Backfill: 7 * 24 * time.Hour, Budget: info.Size() / 5, MaxOpen: 6 * time.Hour}
	st := &State{Files: map[string]*File{}}
	got := 0
	for pass := range 10 {
		ws, err := p.Run(st, stamp.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		got += len(ws)
		if s := st.Files["claude/p/big.jsonl"]; s.Offset == info.Size() {
			break
		}
		if pass == 9 {
			t.Fatalf("большой файл не дочитан: %+v из %d, ожиданий %d", st.Files["claude/p/big.jsonl"], info.Size(), got)
		}
	}
	if got != n {
		t.Errorf("ожиданий %d, нужно %d", got, n)
	}
}

// Открытое дольше MaxOpen закрывается с исходом none, смещение уходит в конец.
func TestRunFlushesStale(t *testing.T) {
	projects := t.TempDir()
	file := filepath.Join(projects, "p", "s1", "subagents", "agent-a1.jsonl")
	appendLines(t, file, call(0, "t1", "make e2e", true), result(0, "t1", "Command running in background with ID: b1."))
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
	projects, workers := t.TempDir(), t.TempDir()
	old := filepath.Join(projects, "p", "old.jsonl")
	appendLines(t, old, call(0, "t1", "sleep 10", false), result(1, "t1", ""))
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
