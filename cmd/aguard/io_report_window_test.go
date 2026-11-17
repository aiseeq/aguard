package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/guiwatch"
	"github.com/aiseeq/aguard/internal/iowatch"
)

func TestIOReportWindowCounters(t *testing.T) {
	a, out := testIOApp(t)
	a.sudoJournalSource = func(context.Context, string, string, ...string) ([]journalRecord, error) {
		return nil, nil
	}
	base := time.Now().Round(0).Truncate(time.Second)
	since, until := base.Add(-5*time.Hour), base.Add(-4*time.Hour)
	insideSince, insideUntil := base.Add(-2*time.Hour), base.Add(-time.Hour)
	cases := []iowatch.Case{
		{Start: base.Add(-6 * time.Hour), DurMS: 3000, Culprits: &iowatch.Culprit{Main: "session-c101.scope"}},
		{Start: insideSince, DurMS: 3000, Culprits: &iowatch.Culprit{Main: "session-c104.scope"}},
		{Start: base.Add(-90 * time.Minute), DurMS: 3000, Culprits: &iowatch.Culprit{Main: "session-c102.scope"}},
		{Start: insideUntil, DurMS: 3000, Culprits: &iowatch.Culprit{Main: "session-c103.scope"}},
		{Start: base.Add(-30 * time.Minute), DurMS: 3000, Culprits: &iowatch.Culprit{Main: "session-c105.scope"}},
	}
	path := filepath.Join(t.TempDir(), "cases.jsonl")
	if err := iowatch.SaveCases(path, cases); err != nil {
		t.Fatal(err)
	}
	guiDir := a.dir("gui")
	if err := os.MkdirAll(guiDir, 0o700); err != nil {
		t.Fatal(err)
	}
	doc := guiwatch.Document{Version: 1, Slices: []guiwatch.Slice{
		{ID: "before", Start: base.Add(-6 * time.Hour), End: base.Add(-6*time.Hour + time.Minute)},
		{ID: "since", Start: insideSince, End: insideSince.Add(time.Minute)},
		{ID: "inside", Start: base.Add(-90 * time.Minute), End: base.Add(-90*time.Minute + time.Minute)},
		{ID: "until", Start: insideUntil, End: insideUntil.Add(time.Minute)},
		{ID: "overlap", Start: insideSince.Add(-10 * time.Minute), End: insideSince.Add(time.Minute)},
		{ID: "after", Start: base.Add(-30 * time.Minute), End: base.Add(-30*time.Minute + time.Minute)},
	}}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(guiDir, "slices.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"until", "after"} {
		if err := os.WriteFile(filepath.Join(guiDir, "journal-"+id+".json"), []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sleeps, err := json.Marshal([]guiwatch.SleepStat{{Date: base.AddDate(0, 0, 1).Format("2006-01-02"), Target: "test", Wchan: "sleep", LongestMS: (24*time.Hour + 27*time.Minute).Milliseconds()}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(guiDir, "sleeps-"+base.AddDate(0, 0, 1).Format("2006-01-02")+".json"), sleeps, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := a.ioReport(context.Background(), path, false, since, until, 0, "", false); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "root session-c*: сопоставлено 0 из 0 случаев") || !strings.Contains(got, "срезов интерфейса: 0, самый длинный S по wchan: 0s") {
		t.Fatalf("пустое окно не обнулило счётчики:\n%s", got)
	}

	out.Reset()
	if err := a.ioReport(context.Background(), path, false, insideSince, insideUntil, 0, "", false); err != nil {
		t.Fatal(err)
	}
	got = out.String()
	if !strings.Contains(got, "root session-c*: сопоставлено 0 из 2 случаев") || !strings.Contains(got, "срезов интерфейса: 2, самый длинный S по wchan: 0s") {
		t.Fatalf("счётчики окна с данными неверны:\n%s", got)
	}
}
