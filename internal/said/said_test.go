package said

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/oneshot"
	"github.com/aiseeq/akit/price"
	"github.com/aiseeq/akit/transcript"
)

const (
	lineName  = `{"type":"agent-name","agentName":"x-master","sessionId":"s1"}`
	lineHuman = `{"type":"user","origin":{"kind":"human"},"timestamp":"2026-10-26T13:01:33Z","cwd":"/h/git/x","message":{"content":"<system-reminder>обвязка</system-reminder>  Делаем без моков.  "}}`
	lineParts = `{"type":"user","origin":{"kind":"human"},"timestamp":"2026-10-26T13:02:00Z","cwd":"/h/git/x","message":{"content":[{"type":"text","text":"раз"},{"type":"image"},{"type":"text","text":"два"}]}}`
	// результат инструмента и письмо сессии — не реплики человека
	lineTool   = `{"type":"user","timestamp":"2026-10-26T13:03:00Z","message":{"content":[{"type":"tool_result","content":"\"origin\""}]}}`
	lineAgent  = `{"type":"assistant","timestamp":"2026-10-26T13:00:00Z","cwd":"/h/git/x","message":{"content":[{"type":"thinking","thinking":"т"},{"type":"text","text":"Мокать базу?"}]}}`
	lineSide   = `{"type":"user","isSidechain":true,"origin":{"kind":"human"},"timestamp":"2026-10-26T13:04:00Z","message":{"content":"суб-агент"}}`
	lineNoText = `{"type":"assistant","timestamp":"2026-10-26T13:05:00Z","message":{"content":[{"type":"tool_use","name":"Bash"},{"type":"text","text":"x"}]}}`
)

func TestParse(t *testing.T) {
	cases := map[string]struct {
		kind, text, name string
	}{
		lineName:   {name: "x-master"},
		lineHuman:  {kind: evUser, text: "Делаем без моков."},
		lineParts:  {kind: evUser, text: "раз\nдва"},
		lineTool:   {},
		lineAgent:  {kind: evAgent, text: "Мокать базу?"},
		lineSide:   {},
		lineNoText: {kind: evAgent, text: "x"},
	}
	for line, want := range cases {
		ev, ok, name, err := Parse(transcript.NewParser(), []byte(line))
		if err != nil || ok != (want.kind != "") || ev.Kind != want.kind || ev.Text != want.text || name != want.name {
			t.Errorf("%.60s: %+v ok=%v name=%q err=%v", line, ev, ok, name, err)
		}
	}
}

func TestTailAndCollect(t *testing.T) {
	projects := t.TempDir()
	dir := filepath.Join(projects, "-h-git-x")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "0123456789ab.jsonl")
	if err := os.WriteFile(path, []byte(lineName+"\n"), 0o600); err != nil { // имя прочитается в проходе
		t.Fatal(err)
	}
	c := Default()
	c.Context = 5
	st, err := LoadState(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	tl := NewTail(projects)
	st.Tail.Started = true // файл появился после первого прохода: читается с начала
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.WriteString(lineAgent + "\n" + lineTool + "\n" + lineHuman + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := fh.Close(); err != nil {
		t.Fatal(err)
	}
	evs, err := tl.Pass(st.Tail, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	c.Collect(st, evs)
	if len(st.Pending) != 1 {
		t.Fatalf("очередь %+v", st.Pending)
	}
	p := st.Pending[0]
	if p.Event.Name != "x-master" || p.Event.Session != "0123456789ab" || p.Event.Line != 4 || p.InReply != "базу?" {
		t.Errorf("реплика %+v", p)
	}
	c.Names = []string{"lead"}
	st.Pending = nil
	c.Collect(st, evs)
	if len(st.Pending) != 0 {
		t.Errorf("x-master не в names, а в очереди: %+v", st.Pending)
	}
}

func fake(out string, err error) Model {
	return func(context.Context, string) (oneshot.Result, error) {
		return oneshot.Result{Text: out, ID: "gpt-6-luna", Usage: price.Usage{Input: 6000, Output: 70}}, err
	}
}

func TestClassify(t *testing.T) {
	c := Default()
	home := "/h"
	p := &Pending{Event: Event{At: time.Date(2026, 10, 26, 13, 1, 33, 0, time.UTC), Text: "Ладно.\nДелаем   без\nмоков, всегда.",
		Cwd: "/h/git/x", Session: "0123456789ab", Name: "x-master", Line: 4}, Attempts: 1}
	prior := []Entry{{Ref: "aaaaaaaa:1", Scope: "~/git/x", Kind: KindRule, Words: "моков всегда", Quote: "с моками"}}
	out := `{"items":[
		{"kind":"rule","quote":"Делаем без моков, всегда.","scope":"project","words":"Моки  База","supersedes":"aaaaaaaa:1"},
		{"kind":"decision","quote":"чего нет в реплике","scope":"global","words":"","supersedes":""},
		{"kind":"promise","quote":"Ладно.","scope":"global","words":"","supersedes":"zzzzzzzz:9"}]}`
	rec, entries := c.Classify(context.Background(), fake(out, nil), p, home, prior, time.Now())
	if rec.Err != "" || len(rec.Items) != 2 || len(rec.Dropped) != 1 || rec.Ref != "01234567:4" || rec.Usage.Input != 6000 {
		t.Fatalf("журнал %+v", rec)
	}
	if e := entries[0]; e.Scope != "~/git/x" || e.Project != "~/git/x" || e.Supersedes != "aaaaaaaa:1" ||
		e.Words != "моки база" || e.Quote != "Делаем без моков, всегда." {
		t.Errorf("правило %+v", e)
	}
	if e := entries[1]; e.Scope != "global" || e.Supersedes != "" || e.Ref != "01234567:4#2" {
		t.Errorf("обещание: чужой supersedes должен сброситься, %+v", e)
	}
	rec, entries = c.Classify(context.Background(), fake("", errors.New("таймаут")), p, home, prior, time.Now())
	if rec.Err == "" || entries != nil {
		t.Errorf("сбой модели: %+v %+v", rec, entries)
	}
	rec, _ = c.Classify(context.Background(), fake("не json", nil), p, home, prior, time.Now())
	if !strings.Contains(rec.Err, "JSON") {
		t.Errorf("не JSON: %+v", rec)
	}
}

func TestWorktreeProject(t *testing.T) {
	c := Default()
	home := t.TempDir()
	repo := filepath.Join(home, "main-repo")
	wt := filepath.Join(home, "worktree")
	nested := filepath.Join(wt, "inside", "folder")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+filepath.Join(repo, ".git", "worktrees", "branch")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	foreignGitDir := filepath.Join(wt, "nested", ".git")
	if err := os.MkdirAll(foreignGitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	brokenNested := filepath.Join(wt, "broken-nested")
	if err := os.Mkdir(brokenNested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brokenNested, ".git"), []byte("not a gitdir"), 0o600); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(home, "broken")
	if err := os.Mkdir(broken, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, ".git"), []byte("not a gitdir"), 0o600); err != nil {
		t.Fatal(err)
	}
	ordinary := filepath.Join(home, "ordinary")
	if err := os.Mkdir(ordinary, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, cwd, want string }{
		{"worktree root", wt, repo},
		{"nested worktree folder", nested, repo},
		{"nested git directory", filepath.Join(wt, "nested"), filepath.Join(wt, "nested")},
		{"nested malformed git file", brokenNested, brokenNested},
		{"ordinary folder", ordinary, ordinary},
		{"broken git file", broken, broken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := tilde(tc.want, home)
			p := &Pending{Event: Event{At: time.Now(), Text: "правило", Cwd: tc.cwd, Session: "0123456789ab", Name: "x", Line: 1}}
			input := Input(p, home, nil)
			if !strings.Contains(input, "папка "+want) {
				t.Errorf("заголовок не содержит папку %q: %s", want, input)
			}
			_, entries := c.Classify(context.Background(), fake(`{"items":[{"kind":"rule","quote":"правило","scope":"project","words":"тест"}]}`, nil),
				p, home, nil, time.Now())
			if len(entries) != 1 || entries[0].Project != want {
				t.Errorf("project: got %+v, want %q", entries, want)
			}
		})
	}
}

func TestAppendRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "said.tsv")
	at := time.Date(2026, 10, 26, 14, 1, 33, 0, time.FixedZone("CET", 3600))
	in := []Entry{
		{Time: at, Session: "s1", Name: "lead", Project: "~", Scope: "global", Kind: KindDecision, Words: "луна",
			Quote: "строка\tс табом\nи \\ переводом", Ref: "s1:5"},
		{Time: at, Session: "s1", Name: "lead", Project: "~", Scope: "global", Kind: KindRule, Words: "луна дёшево",
			Quote: "гонять на луне", Ref: "s1:6", Supersedes: "s1:5"},
	}
	if err := Append(path, in[:1]); err != nil {
		t.Fatal(err)
	}
	if err := Append(path, in[1:]); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(b), Header) != 1 || strings.Count(string(b), "\n") != 3 {
		t.Errorf("файл:\n%s", b)
	}
	got, bad, err := Read(path)
	if err != nil || bad != 0 || len(got) != 2 || got[0].Quote != in[0].Quote || !got[0].Time.Equal(at) {
		t.Fatalf("прочитано %+v bad=%d err=%v", got, bad, err)
	}
	// отменённая запись не предлагается, прочие — по общим словам
	cands := Candidates(got, "~/git/x", "луна дёшево гонять", 8)
	if len(cands) != 1 || cands[0].Ref != "s1:6" {
		t.Errorf("кандидаты %+v", cands)
	}
}

func TestWatched(t *testing.T) {
	c := Default()
	for name, want := range map[string]bool{"lead": true, "git-admin": true, "aguard-master": true, "": false,
		"worker-3": false} {
		if c.Watched(name) != want {
			t.Errorf("%q: %v", name, !want)
		}
	}
}

func TestParseQueued(t *testing.T) {
	line := `{"type":"attachment","timestamp":"2026-10-26T13:06:00Z","cwd":"/h/git/x","attachment":{"type":"queued_command","prompt":"и ещё: без моков","origin":{"kind":"human"}}}`
	ev, ok, _, err := Parse(transcript.NewParser(), []byte(line))
	if err != nil || !ok || ev.Kind != evUser || ev.Text != "и ещё: без моков" {
		t.Errorf("реплика из очереди: %+v ok=%v err=%v", ev, ok, err)
	}
}

// TestAppendFileMode600 — said.tsv с цитатами пользователя: новый файл создаётся 600, старый 644 сужается при записи.
func TestAppendFileMode600(t *testing.T) {
	dir := t.TempDir()
	fresh := filepath.Join(dir, "said.tsv")
	if err := Append(fresh, []Entry{{Time: time.Now(), Session: "s1", Name: "lead", Kind: "rule", Quote: "x"}}); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "old.tsv")
	if err := os.WriteFile(old, []byte(Header+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(old, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Append(old, []Entry{{Time: time.Now(), Session: "s1", Name: "lead", Kind: "rule", Quote: "y"}}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{fresh, old} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s: права %o, want 600", filepath.Base(p), got)
		}
	}
}

// TestAppendChmodFailureWritesNothing — не сузились права старого файла: Append возвращает ошибку и не дописывает
// цитаты в файл, видный другим (реплика остаётся в очереди).
func TestAppendChmodFailureWritesNothing(t *testing.T) {
	old := filepath.Join(t.TempDir(), "said.tsv")
	if err := os.WriteFile(old, []byte(Header+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(old, 0o644); err != nil {
		t.Fatal(err)
	}
	orig := chmodFile
	t.Cleanup(func() { chmodFile = orig })
	chmodFile = func(*os.File, os.FileMode) error { return errors.New("отказ chmod") }
	err := Append(old, []Entry{{Time: time.Now(), Session: "s1", Name: "lead", Kind: "rule", Quote: "y"}})
	if err == nil || !strings.Contains(err.Error(), "права 600") {
		t.Fatalf("Append: err=%v, want ошибку «права 600»", err)
	}
	data, rerr := os.ReadFile(old)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(data) != Header+"\n" {
		t.Errorf("файл дописан при отказе chmod: %q", data)
	}
}
