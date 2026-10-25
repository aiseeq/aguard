package waits

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // пояс для TestHoursDST без системной базы поясов
)

const (
	lineName    = `{"type":"agent-name","agentName":"x-master","sessionId":"s1"}`
	lineHook    = `{"type":"attachment","timestamp":"2026-10-24T22:10:00.000Z","attachment":{"type":"hook_cancelled","hookName":"PostToolBatch","hookEvent":"PostToolBatch","durationMs":23900,"timedOut":true,"timeoutMs":3000}}`
	lineHookOK  = `{"type":"attachment","timestamp":"2026-10-24T22:11:00.000Z","attachment":{"type":"hook_cancelled","hookEvent":"Stop","durationMs":10,"timedOut":false}}`
	lineAPI     = `{"type":"assistant","timestamp":"2026-10-25T08:08:23.301Z","isApiErrorMessage":true,"error":"server_error","apiErrorStatus":522}`
	lineCompact = `{"type":"system","subtype":"compact_boundary","timestamp":"2026-10-24T14:37:44.836Z","compactMetadata":{"trigger":"auto","preTokens":268110,"durationMs":53988}}`
	// маркер внутри текста результата инструмента — не событие
	lineQuoted = `{"type":"user","timestamp":"2026-10-24T22:12:00.000Z","message":{"content":"grep \"hook_cancelled\" ..."}}`
	linePlain  = `{"type":"assistant","timestamp":"2026-10-24T22:13:00.000Z"}`
)

func TestParse(t *testing.T) {
	cases := map[string]struct {
		kind, detail, name string
	}{
		lineName:    {name: "x-master"},
		lineHook:    {kind: KindHookTimeout, detail: "PostToolBatch"},
		lineHookOK:  {},
		lineAPI:     {kind: KindAPIError, detail: "server_error 522"},
		lineCompact: {kind: KindCompact, detail: "auto"},
		lineQuoted:  {},
		linePlain:   {},
	}
	for line, want := range cases {
		ev, ok, name, err := parse([]byte(line))
		if err != nil || ok != (want.kind != "") || ev.Kind != want.kind || ev.Detail != want.detail || name != want.name {
			t.Errorf("%.60s: %+v ok=%v name=%q err=%v", line, ev, ok, name, err)
		}
	}
}

func write(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

func kinds(evs []Event) string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Kind+"/"+e.Name+"/"+e.Agent)
	}
	return strings.Join(out, ",")
}

func TestTailPass(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "-home-u-git-x", "s1.jsonl")
	write(t, old, lineName, lineHook, linePlain, lineCompact)
	now := time.Now()
	tl := &Tail{Projects: dir, Backfill: 7 * 24 * time.Hour, BackfillBytes: 1 << 20}
	st := &State{Files: map[string]*File{}}

	// первый проход: старый файл — с конца, его начало — догрузкой
	evs, err := tl.Pass(st, now)
	if err != nil || kinds(evs) != "hook_timeout/x-master/,compact/x-master/" {
		t.Fatalf("первый проход: %s, %v", kinds(evs), err)
	}
	if evs, _ := tl.Pass(st, now); len(evs) != 0 {
		t.Errorf("догрузка повторилась: %s", kinds(evs))
	}

	// дописанное, недописанный хвост и новый файл суб-агента
	write(t, old, lineAPI)
	f, err := os.OpenFile(old, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(lineHook[:40]); err != nil { // строка ещё пишется
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "-home-u-git-x", "s1", "subagents", "agent-a1.jsonl"), lineHook)
	evs, err = tl.Pass(st, now)
	if err != nil || kinds(evs) != "api_error/x-master/,hook_timeout/x-master/a1" {
		t.Fatalf("дописанное: %s, %v", kinds(evs), err)
	}
	write(t, old, lineHook[40:])
	if evs, err := tl.Pass(st, now); err != nil || kinds(evs) != "hook_timeout/x-master/" {
		t.Errorf("дописанный хвост: %s, %v", kinds(evs), err)
	}
}

// TestBackfillBudget — догрузка кусками по бюджету, с целых строк, без повторов.
func TestBackfillBudget(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "-home-u-git-x", "s1.jsonl")
	var lines []string
	for range 20 {
		lines = append(lines, lineHook)
	}
	write(t, p, lines...)
	tl := &Tail{Projects: dir, Backfill: time.Hour, BackfillBytes: int64(len(lineHook)) * 3}
	st := &State{Files: map[string]*File{}}
	total := 0
	for range 20 {
		evs, err := tl.Pass(st, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		total += len(evs)
	}
	if total != 20 || st.Files["-home-u-git-x/s1.jsonl"].From != 0 {
		t.Errorf("догружено %d из 20, from=%d", total, st.Files["-home-u-git-x/s1.jsonl"].From)
	}
}

// TestHoursDST — в ночь перехода на зимнее время два часа 02:00 различимы.
func TestHoursDST(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	old := time.Local
	time.Local = loc
	t.Cleanup(func() { time.Local = old })
	r, err := Collect(func(fn func(*Event)) (int, error) {
		for _, at := range []string{"2026-10-25T00:30:00Z", "2026-10-25T01:30:00Z"} { // 02:30 CEST и 02:30 CET
			ts, err := time.Parse(time.RFC3339, at)
			if err != nil {
				return 0, err
			}
			fn(&Event{At: ts, Kind: KindAPIError, Detail: "server_error 522"})
		}
		return 0, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := r.Print(&out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"25.10 02:00 CEST  1", "25.10 02:00 CET  1"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("нет %q:\n%s", want, out.String())
		}
	}
}

func TestStateSaveReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "offsets.json")
	st := &State{Started: true, Files: map[string]*File{"p/s.jsonl": {Offset: 10, Name: "x"}}}
	if err := st.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(path)
	if err != nil || !got.Started || got.Files["p/s.jsonl"].Offset != 10 {
		t.Fatalf("состояние: %+v, %v", got, err)
	}
	store := NewStore(t.TempDir())
	at := time.Date(2026, 10, 24, 23, 10, 0, 0, time.Local)
	if err := store.Append([]Event{
		{At: at, Kind: KindHookTimeout, Detail: "PostToolBatch", Session: "s1", Name: "x-master", Ms: 23900},
		{At: at.Add(time.Minute), Kind: KindHookTimeout, Detail: "PostToolBatch", Session: "s2", Name: "y-master", Ms: 4300},
		{At: at, Kind: KindCompact, Detail: "auto", Session: "s1"},
	}); err != nil {
		t.Fatal(err)
	}
	r, err := Collect(func(fn func(*Event)) (int, error) { return store.Read(at.Add(-time.Hour), fn) })
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := r.Print(&out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"hook_timeout  PostToolBatch  2", "23.9s", "x-master  1"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("нет %q в сводке:\n%s", want, out.String())
		}
	}
}
