package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/evictors"
)

func TestMemoryEvictorsJSONFieldsAndOrderMatchText(t *testing.T) {
	a, out := testIOApp(t)
	at := time.Now().Truncate(time.Hour)
	agg := evictors.NewAggregate()
	agg.Add(&evictors.Interval{At: at, Changes: []evictors.Change{
		{Path: "/app.slice/frequent.service", Process: "chrome", Anon: 1 << 20, OtherSwap: 100 << 20, Sessions: 7, Candidate: true},
		{Path: "/agents.slice/claude-2.scope", Process: "claude", Session: true, Anon: 400 << 20, OtherSwap: 100 << 20, Sessions: 3, Candidate: true},
	}})
	path := evictorStorePath(a)
	if err := evictors.Save(path, agg); err != nil {
		t.Fatal(err)
	}

	if err := a.memoryCmd([]string{"evictors", "-since", "24h", "-top", "15", "-json"}); err != nil {
		t.Fatal(err)
	}
	var got evictorJSON
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("JSON decode: %v: %s", err, out)
	}
	if got.From != at.Format(time.RFC3339) || got.To != at.Format(time.RFC3339) || got.Bad != 0 || got.Note != "кандидаты, не доказанная причина" || len(got.Rows) != 2 {
		t.Fatalf("поля сводки: %+v", got)
	}
	if got.Rows[0] != (evictorJSONRow{Cgroup: "/agents.slice/claude-2.scope", Session: true, Process: "claude", NetAnonB: 400 << 20, MatchedSwapB: 100 << 20, OtherSwapB: 100 << 20, Intervals: 1, Sessions: 3}) ||
		got.Rows[1] != (evictorJSONRow{Cgroup: "/app.slice/frequent.service", Process: "chrome", NetAnonB: 1 << 20, MatchedSwapB: 1 << 20, OtherSwapB: 100 << 20, Intervals: 1, Sessions: 7}) {
		t.Fatalf("поля или порядок кандидатов: %+v", got.Rows)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"from", "to", "bad", "note", "rows"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("отсутствует JSON поле %q", key)
		}
	}
	var rowFields []map[string]json.RawMessage
	if err := json.Unmarshal(fields["rows"], &rowFields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"cgroup", "session", "process", "net_anon_b", "matched_swap_b", "other_swap_b", "intervals", "sessions"} {
		if _, ok := rowFields[0][key]; !ok {
			t.Errorf("отсутствует поле строки %q", key)
		}
	}

	out.Reset()
	if err := a.memoryCmd([]string{"evictors", "-since", "24h", "-top", "15"}); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	first, second := strings.Index(text, got.Rows[0].Cgroup), strings.Index(text, got.Rows[1].Cgroup)
	if first < 0 || second < 0 || first >= second {
		t.Fatalf("JSON и текст должны сохранять порядок строк: %s", text)
	}
}

func TestMemoryEvictorsJSONEmptyRowsIsArray(t *testing.T) {
	a, out := testIOApp(t)
	if err := os.MkdirAll(filepath.Dir(evictorStorePath(a)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := a.memoryCmd([]string{"evictors", "-json"}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		From string            `json:"from"`
		To   string            `json:"to"`
		Bad  int               `json:"bad"`
		Note string            `json:"note"`
		Rows []json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("JSON decode: %v: %s", err, out)
	}
	if got.Rows == nil || len(got.Rows) != 0 || got.Bad != 0 || got.Note != "кандидаты, не доказанная причина" {
		t.Fatalf("пустой отчёт должен содержать rows: []: %+v", got)
	}
	if !strings.Contains(out.String(), `"rows": []`) {
		t.Fatalf("rows сериализован не пустым массивом: %s", out)
	}
}
