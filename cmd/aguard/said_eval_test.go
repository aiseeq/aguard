package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/config"
	"github.com/aiseeq/aguard/internal/said"
	"github.com/aiseeq/akit/oneshot"
	"github.com/aiseeq/akit/price"
	"github.com/aiseeq/akit/session"
)

func TestSaidEvalReplay(t *testing.T) {
	dir := t.TempDir()
	projects := filepath.Join(dir, "projects")
	folder := filepath.Join(projects, "-home-user-git-project")
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	transcript := strings.Join([]string{
		`{"type":"agent-name","agentName":"lead"}`,
		`{"type":"assistant","timestamp":"2026-11-16T09:59:30Z","cwd":"` + dir + `/git/project","message":{"content":[{"type":"text","text":"Первый ответ."}]}}`,
		`{"type":"user","origin":{"kind":"human"},"timestamp":"2026-11-16T09:59:40Z","cwd":"` + dir + `/git/project","message":{"content":"первая реплика раньше"}}`,
		`{"type":"assistant","timestamp":"2026-11-16T10:00:00Z","cwd":"` + dir + `/git/project","message":{"content":[{"type":"text","text":"Ответ именно на выбранную реплику."}]}}`,
		`{"type":"user","origin":{"kind":"human"},"timestamp":"2026-11-16T10:00:00.987Z","cwd":"` + dir + `/git/project","message":{"content":"нужная реплика проверяем выбор"}}`,
		`{"type":"assistant","timestamp":"2026-11-16T10:00:01Z","cwd":"` + dir + `/git/project","message":{"content":[{"type":"text","text":"Ответ после выбранной реплики."}]}}`,
		`{"type":"user","origin":{"kind":"human"},"timestamp":"2026-11-16T10:00:02Z","cwd":"` + dir + `/git/project","message":{"content":"следующая реплика после цели"}}`,
	}, "\n") + "\n"
	transcriptPath := filepath.Join(folder, "12345678-session.jsonl")
	if err := os.WriteFile(transcriptPath, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	secondDir := filepath.Join(projects, "-home-user-git-other")
	if err := os.MkdirAll(secondDir, 0o700); err != nil {
		t.Fatal(err)
	}
	secondTranscript := strings.Join([]string{
		`{"type":"agent-name","agentName":"lead"}`,
		`{"type":"assistant","timestamp":"2026-11-16T10:59:00Z","cwd":"` + dir + `/git/other","message":{"content":[{"type":"text","text":"Ответ перед вторым случаем."}]}}`,
		`{"type":"user","origin":{"kind":"human"},"timestamp":"2026-11-16T11:00:00Z","cwd":"` + dir + `/git/other","message":{"content":"второй уникальный эталон"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(secondDir, "abcdefgh-session.jsonl"), []byte(secondTranscript), 0o600); err != nil {
		t.Fatal(err)
	}

	casesPath := filepath.Join(dir, "cases.tsv")
	if err := os.WriteFile(casesPath, []byte("12345678:5\trule\t~/git/project\nabcdefgh:3\trule\t~/git/other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	priorPath := filepath.Join(dir, "said.tsv")
	prior := []said.Entry{
		{Time: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC), Session: "old", Name: "lead", Scope: "~/git/project",
			Kind: said.KindRule, Words: "нужная реплика проверяем выбор", Quote: "нужная реплика", Ref: "older000:1"},
		{Time: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC), Session: "12345678-session", Name: "lead",
			Scope: "~/git/project", Kind: said.KindRule, Words: "нужная реплика проверяем выбор", Quote: "нужная реплика", Ref: "12345678:5"},
		{Time: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC), Session: "12345678-session", Name: "lead",
			Scope: "~/git/project", Kind: said.KindRule, Words: "нужная реплика проверяем выбор", Quote: "нужная реплика", Ref: "12345678:5#2"},
		{Time: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC), Session: "same-second", Name: "lead", Scope: "~/git/project",
			Kind: said.KindRule, Words: "нужная реплика проверяем выбор", Quote: "нужная реплика", Ref: "same0000:1"},
		{Time: time.Date(2026, 11, 16, 10, 1, 0, 0, time.UTC), Session: "future", Name: "lead", Scope: "~/git/project",
			Kind: said.KindRule, Words: "нужная реплика проверяем выбор", Quote: "нужная реплика", Ref: "future000:1"},
	}
	var data strings.Builder
	data.WriteString(said.Header + "\n")
	for i := range prior {
		data.WriteString(prior[i].Line())
	}
	if err := os.WriteFile(priorPath, []byte(data.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	var calls int
	a := &app{home: dir, projects: projects, reg: session.ForHome(dir), out: &output,
		log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))}
	a.saidModel = func(_, prompt string) said.Model {
		if prompt != "temporary prompt" {
			t.Errorf("prompt override = %q", prompt)
		}
		return func(_ context.Context, input string) (oneshot.Result, error) {
			calls++
			if strings.Contains(input, "нужная реплика проверяем выбор") {
				for _, want := range []string{"Ответ именно на выбранную реплику", "older000:1"} {
					if !strings.Contains(input, want) {
						t.Errorf("вход модели не содержит %q: %s", want, input)
					}
				}
				for _, excluded := range []string{"первая реплика раньше", "следующая реплика после цели", "12345678:5", "same0000:1", "future000:1"} {
					if strings.Contains(input, excluded) {
						t.Errorf("лишнее в модели-входе %q: %s", excluded, input)
					}
				}
				return oneshot.Result{Text: `{"items":[
					{"kind":"rule","quote":"нужная реплика","scope":"~/elsewhere","words":"нужная реплика"},
					{"kind":"decision","quote":"проверяем выбор","scope":"~/git/project","words":"проверяем выбор"},
					{"kind":"promise","quote":"выбор","scope":"~/another","words":"выбор"}]}`,
					ID: "stub", Usage: price.Usage{}}, nil
			}
			if !strings.Contains(input, "второй уникальный эталон") ||
				!strings.Contains(input, "Ответ перед вторым случаем") {
				t.Errorf("неверный вход второй реплики: %s", input)
			}
			return oneshot.Result{Text: `{"items":[
				{"kind":"rule","quote":"второй уникальный эталон","scope":"~/git/other","words":"второй уникальный эталон"},
				{"kind":"promise","quote":"второй уникальный","scope":"~/wrong","words":"второй уникальный"},
				{"kind":"decision","quote":"уникальный эталон","scope":"global","words":"уникальный эталон"}]}`,
				ID: "stub", Usage: price.Usage{}}, nil
		}
	}
	c := &config.Config{Said: said.Default()}
	c.Said.Model, c.Said.File = "opus", priorPath
	if err := a.saidEval(context.Background(), c, casesPath, writeEvalPrompt(t, dir), true); err != nil {
		t.Fatal(err)
	}
	var got saidEvalSummary
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("вызовов модели %d, ожидалось два разных refs", calls)
	}
	if len(got.Cases) != 2 || got.ExpectedN != 2 || got.FoundN != 6 || got.MatchedN != 1 || got.ExtraRecords != 5 || got.Errors != 0 {
		t.Fatalf("итог eval: %+v", got)
	}
	if got.Cases[0].MatchedN != 0 || got.Cases[0].Extra != 3 || got.Cases[1].MatchedN != 1 || got.Cases[1].Extra != 2 {
		t.Fatalf("пары и лишние записи: %+v", got.Cases)
	}
}

func TestSaidEvalAllErrorsReturnsError(t *testing.T) {
	dir := t.TempDir()
	casesPath := filepath.Join(dir, "cases.tsv")
	if err := os.WriteFile(casesPath, []byte("missing0:3\trule\t~/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	a := &app{home: dir, projects: filepath.Join(dir, "projects"), out: &output}
	c := &config.Config{Said: said.Default()}
	c.Said.Model, c.Said.File = "opus", filepath.Join(dir, "said.tsv")
	err := a.saidEval(context.Background(), c, casesPath, "", false)
	if err == nil || !strings.Contains(output.String(), "ошибок 1") {
		t.Fatalf("all-failed error=%v output=%q", err, output.String())
	}
}

func TestSaidEvalOnlyAndMissingContinues(t *testing.T) {
	dir := t.TempDir()
	projects := filepath.Join(dir, "projects")
	for _, tc := range []struct{ folder, id, text string }{
		{"-aaa-foreign", "foreign0", "чужая сессия"},
		{"-zzz-target", "target00", "реплика своей сессии"},
	} {
		folder := filepath.Join(projects, tc.folder)
		if err := os.MkdirAll(folder, 0o700); err != nil {
			t.Fatal(err)
		}
		transcript := strings.Join([]string{
			`{"type":"agent-name","agentName":"lead"}`,
			`{"type":"assistant","timestamp":"2026-11-16T10:00:00Z","cwd":"` + dir + `/git/project","message":{"content":[{"type":"text","text":"ответ ` + tc.text + `"}]}}`,
			`{"type":"user","origin":{"kind":"human"},"timestamp":"2026-11-16T10:01:00Z","cwd":"` + dir + `/git/project","message":{"content":"` + tc.text + `"}}`,
		}, "\n") + "\n"
		if err := os.WriteFile(filepath.Join(folder, tc.id+"-session.jsonl"), []byte(transcript), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	casesPath := filepath.Join(dir, "cases.tsv")
	if err := os.WriteFile(casesPath, []byte("missing0:3\trule\t~/git/project\ntarget00:3\t-\t-\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	var calls int
	a := &app{home: dir, projects: projects, reg: session.ForHome(dir), out: &output,
		log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))}
	a.saidModel = func(_, _ string) said.Model {
		return func(_ context.Context, input string) (oneshot.Result, error) {
			calls++
			if !strings.Contains(input, "реплика своей сессии") || strings.Contains(input, "чужая сессия") {
				t.Errorf("eval выбрал не ту сессию: %s", input)
			}
			return oneshot.Result{Text: `{"items":[]}`, ID: "stub"}, nil
		}
	}
	c := &config.Config{Said: said.Default()}
	c.Said.Model, c.Said.File = "opus", filepath.Join(dir, "said.tsv")
	if err := a.saidEval(context.Background(), c, casesPath, "", true); err != nil {
		t.Fatalf("ошибка одного ref остановила прогон: %v", err)
	}
	var got saidEvalSummary
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(got.Cases) != 2 || got.Errors != 1 || got.Cases[0].Error == "" ||
		got.Cases[1].Error != "" || got.Cases[1].ExpectedN != 0 || got.Cases[1].FoundN != 0 || got.Cases[1].MatchedN != 0 {
		t.Fatalf("ошибка и пустое ожидание: calls=%d summary=%+v", calls, got)
	}
}

func TestSaidEvalDashExpectation(t *testing.T) {
	empty := saidEvalResult{Expected: []saidEvalPair{{Kind: "-", Scope: "-"}}}
	compareSaidEval(&empty)
	if empty.ExpectedN != 0 || empty.FoundN != 0 || empty.MatchedN != 0 || empty.Extra != 0 {
		t.Errorf("пустой ответ должен совпасть с '-': %+v", empty)
	}
	unexpected := saidEvalResult{Expected: empty.Expected, Actual: []said.Item{{Kind: said.KindRule, Scope: "global"}}}
	compareSaidEval(&unexpected)
	if unexpected.ExpectedN != 0 || unexpected.FoundN != 1 || unexpected.MatchedN != 0 || unexpected.Extra != 1 {
		t.Errorf("непустой ответ для '-' должен быть лишним: %+v", unexpected)
	}
}

func TestSaidEvalAugmentingMatch(t *testing.T) {
	row := saidEvalResult{
		Expected: []saidEvalPair{{Kind: said.KindDecision, Scope: "-"}, {Kind: said.KindDecision, Scope: "global"}},
		Actual:   []said.Item{{Kind: said.KindDecision, Scope: "global"}, {Kind: said.KindDecision, Scope: "~/git/other"}},
	}
	compareSaidEval(&row)
	if row.ExpectedN != 2 || row.FoundN != 2 || row.MatchedN != 2 || row.Extra != 0 {
		t.Fatalf("нужно переназначить wildcard на ~/git/other: %+v", row)
	}
}

func TestSaidEvalTextShowsExpectedAndActualPairs(t *testing.T) {
	var output bytes.Buffer
	summary := saidEvalSummary{Cases: []saidEvalResult{{
		Ref: "abc12345:7", Expected: []saidEvalPair{{Kind: said.KindRule, Scope: "~/project"}},
		Actual:    []said.Item{{Kind: said.KindRule, Scope: "~/elsewhere"}, {Kind: said.KindDecision, Scope: "~/project"}},
		ExpectedN: 1, FoundN: 2, MatchedN: 0, Extra: 2,
	}}, ExpectedN: 1, FoundN: 2, ExtraRecords: 2}
	if err := printSaidEval(&output, summary); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"abc12345:7", "rule/~/project", "rule/~/elsewhere", "decision/~/project", "ожидалось 1, пришло 2, совпало 0, лишних 2"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("текстовый вывод не содержит %q: %s", want, output.String())
		}
	}
}

func writeEvalPrompt(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(path, []byte("temporary prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
