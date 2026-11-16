package iowatch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func dailyTestSamples(rows []MachineSecond, date string, loc *time.Location) []DailySample {
	var samples []DailySample
	for _, row := range rows {
		if sample, ok := DailySampleFor(row, date, loc); ok {
			samples = append(samples, sample)
		}
	}
	return samples
}

func TestGraftSliceMemSumAndMissing(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "user.slice", "user-1000.slice", "user@1000.service")
	write := func(rel, value string) {
		t.Helper()
		path := filepath.Join(base, rel, "memory.current")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join("agents.slice", "agents-graft.slice"), "123\n")
	write(filepath.Join("app.slice", "app-graft.slice"), "456\n")
	r := NewGraftSliceMemReader(root, 1000)
	got, err := r.Sample(time.Now())
	if err != nil || got != 579 {
		t.Fatalf("sum %d, err %v", got, err)
	}
	if err := os.Remove(filepath.Join(base, "app.slice", "app-graft.slice", "memory.current")); err != nil {
		t.Fatal(err)
	}
	if got, err = r.Sample(time.Now()); err != nil || got != 123 {
		t.Fatalf("vanished slice should be ignored: %d, err %v", got, err)
	}
	r = NewGraftSliceMemReader(t.TempDir(), 1000)
	if got, err = r.Sample(time.Now()); err != nil || got != 0 {
		t.Fatalf("missing slices: %d, err %v", got, err)
	}
	write(filepath.Join("broken.slice", "bad-graft.slice"), "not-a-number")
	r = NewGraftSliceMemReader(root, 1000)
	if got, err = r.Sample(time.Now()); err == nil || got != 123 {
		t.Fatalf("read error must retain readable sum: %d, err %v", got, err)
	}
}

func TestGraftSliceMemStoredAsKiBAndSurvivesDetail(t *testing.T) {
	series := NewSeries(time.Hour)
	at := time.Date(2026, 11, 14, 12, 0, 0, 0, time.Local)
	series.detail(&Step{Dirty: 1 << 10, GraftSliceMem: 17 << 10, GraftSliceMemKnown: true})
	series.step(&Step{At: at}, 0, time.Second)
	series.step(&Step{At: at.Add(time.Second)}, 0, time.Second)
	rows := series.Rows(time.Time{}).Seconds()
	if len(rows) != 1 || rows[0].GraftSliceMem != 17<<10 {
		t.Fatalf("graft slice memory row: %+v", rows)
	}
}

func TestSeriesKeepsGraftMemoryUnknownBeforeFirstDetail(t *testing.T) {
	series := NewSeries(time.Hour)
	at := time.Date(2026, 11, 14, 12, 0, 0, 0, time.Local)
	series.step(&Step{At: at}, 0, time.Second)
	series.step(&Step{At: at.Add(time.Second)}, 0, time.Second)
	rows := series.Rows(time.Time{}).Seconds()
	if len(rows) != 1 || rows[0].GraftSliceMemKnown || rows[0].GraftSliceMem != 0 {
		t.Fatalf("pre-detail graft memory must remain unknown: %+v", rows)
	}
}

func TestSeriesDropsStepsUntilClockPassesLastRow(t *testing.T) {
	series := NewSeries(time.Hour)
	base := time.Unix(1_800_000_000, 0)
	series.Add(MachineSecond{At: base.Add(5 * time.Second), WriteB: 55})
	for i := range 7 {
		series.step(&Step{At: base.Add(time.Duration(i) * time.Second), WriteBytes: int64(i + 1)}, 0, time.Second)
	}
	rows := series.Rows(time.Time{}).Seconds()
	if len(rows) != 1 || rows[0].WriteB != 55 {
		t.Fatalf("backward-clock steps must be discarded through the last stored second: %+v", rows)
	}
	series.step(&Step{At: base.Add(7 * time.Second), WriteBytes: 8}, 0, time.Second)
	rows = series.Rows(time.Time{}).Seconds()
	if len(rows) != 2 || !rows[0].At.Equal(base.Add(5*time.Second)) || !rows[1].At.Equal(base.Add(6*time.Second)) || rows[1].WriteB != 7 {
		t.Fatalf("series after the clock catches up: %+v", rows)
	}
}

func TestDailyMemorySummaryAndRetention(t *testing.T) {
	loc := time.FixedZone("test", 2*60*60)
	start := time.Date(2026, 11, 14, 0, 0, 0, 0, loc)
	var rows []MachineSecond
	for day := range 2 {
		for i := 99; i >= 0; i-- {
			at := start.AddDate(0, 0, day).Add(time.Duration(i) * time.Second)
			rows = append(rows, MachineSecond{At: at, MemAvail: int64(i) * 1024, GraftSliceMem: int64(1000 + i), GraftSliceMemKnown: true})
			rows = append(rows, MachineSecond{At: at.Add(200 * time.Second)}) // legacy row: field absent
		}
	}
	first := SummarizeDaily(dailyTestSamples(rows, "2026-11-14", loc), "2026-11-14", loc)
	second := SummarizeDaily(dailyTestSamples(rows, "2026-11-15", loc), "2026-11-15", loc)
	for _, got := range []DailyMemory{first, second} {
		if got.Seconds != 100 || got.P1 != 1000 || got.P5 != 5100 || got.P50 != 51225 || got.GraftSliceMemMax != 1099 || got.GraftFrom == "" {
			t.Fatalf("daily summary: %+v", got)
		}
	}
	path := filepath.Join(t.TempDir(), "graft-mem-daily.jsonl")
	if err := SaveDaily(path, first); err != nil {
		t.Fatal(err)
	}
	if err := SaveDaily(path, first); err != nil {
		t.Fatal(err)
	}
	duplicates, err := LoadDaily(path)
	if err != nil || len(duplicates) != 1 {
		t.Fatalf("duplicate day rows: %d, err %v", len(duplicates), err)
	}
	for day := 1; day <= 31; day++ {
		entry := first
		entry.Date = start.AddDate(0, 0, day-1).Format("2006-01-02")
		if err := SaveDaily(path, entry); err != nil {
			t.Fatal(err)
		}
	}
	got, err := LoadDaily(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != DailyKeep || got[0].Date != "2026-11-15" || got[len(got)-1].Date != "2026-12-14" {
		t.Fatalf("retained dates: %d, %v", len(got), got)
	}
}

func TestDailyMemoryWithoutKnownSamplesIsEmpty(t *testing.T) {
	loc := time.Local
	at := time.Date(2026, 11, 14, 12, 0, 0, 0, loc)
	got := SummarizeDaily(dailyTestSamples([]MachineSecond{{At: at}}, at.Format("2006-01-02"), loc), at.Format("2006-01-02"), loc)
	if got.Seconds != 0 || got.GraftFrom != "" {
		t.Fatalf("legacy-only rollup should be empty: %+v", got)
	}
}

func TestSeriesRowsSinceAfterRingWrap(t *testing.T) {
	series := NewSeries(5 * time.Second)
	base := time.Unix(1_800_000_000, 0)
	for i := range 8 {
		series.Add(MachineSecond{At: base.Add(time.Duration(i) * time.Second), GraftSliceMem: int64(i+1) << 10, GraftSliceMemKnown: true})
	}
	rows := series.Rows(base.Add(6 * time.Second)).Seconds()
	if len(rows) != 2 || rows[0].GraftSliceMem != 7<<10 || rows[1].GraftSliceMem != 8<<10 {
		t.Fatalf("rows after ring wrap: %+v", rows)
	}
}

func TestSeriesJSONKeepsKnownZeroAndReadsLegacy(t *testing.T) {
	known := MachineSecond{At: time.Unix(1, 0), GraftSliceMemKnown: true}
	path := filepath.Join(t.TempDir(), "series.jsonl")
	if err := SaveSeries(path, []MachineSecond{known, {At: time.Unix(2, 0)}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), `"graft_slice_mem":0`) || strings.Contains(strings.Split(string(data), "\n")[1], `graft_slice_mem`) {
		t.Fatalf("series JSON zero/legacy lines %s, err %v", data, err)
	}
	decoded, err := LoadSeries(path, time.Time{})
	if err != nil || len(decoded) != 2 || !decoded[0].GraftSliceMemKnown || decoded[1].GraftSliceMemKnown {
		t.Fatalf("known/legacy decode %+v, err %v", decoded, err)
	}
	legacyPath := filepath.Join(t.TempDir(), "old-series.jsonl")
	if err := os.WriteFile(legacyPath, []byte(`{"at":"1970-01-01T00:00:03Z"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	decoded, err = LoadSeries(legacyPath, time.Time{})
	if err != nil || len(decoded) != 1 || decoded[0].GraftSliceMemKnown {
		t.Fatalf("old series line known=%v err=%v", decoded, err)
	}
	stepData, err := json.Marshal(Step{At: time.Unix(4, 0), GraftSliceMemKnown: true})
	if err != nil {
		t.Fatal(err)
	}
	var step Step
	if err := json.Unmarshal(stepData, &step); err != nil || !step.GraftSliceMemKnown {
		t.Fatalf("detail snapshot graft-known marker lost: %s, err %v", stepData, err)
	}
}

func TestLoadSeriesHeadFormatWithoutLoss(t *testing.T) {
	path := filepath.Join(t.TempDir(), "series.jsonl")
	writeFile(t, path, `{"at":"2026-11-14T23:59:59Z","io_full_ms":17,"rd_b":12345,"mem_avail":987654,"graft_slice_mem":0}`+"\n")
	got, err := LoadSeries(path, time.Time{})
	if err != nil || len(got) != 1 {
		t.Fatalf("HEAD series line: %+v, err %v", got, err)
	}
	row := got[0]
	if !row.At.Equal(time.Date(2026, 11, 14, 23, 59, 59, 0, time.UTC)) || row.IOFullMS != 17 || row.ReadB != 12345 ||
		row.MemAvail != 987654 || row.GraftSliceMem != 0 || !row.GraftSliceMemKnown {
		t.Fatalf("HEAD series line lost fields: %+v", row)
	}
}

func TestLoadSeriesAcceptsLineLongerThan64KiB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "series.jsonl")
	line := append([]byte(`{"at":"2026-11-14T23:59:59Z","extra":"`), strings.Repeat("x", 70*1024)...)
	line = append(line, []byte(`"}`+"\n")...)
	if err := os.WriteFile(path, line, 0o600); err != nil {
		t.Fatal(err)
	}
	rows, err := LoadSeries(path, time.Time{})
	if err != nil || len(rows) != 1 || !rows[0].At.Equal(time.Date(2026, 11, 14, 23, 59, 59, 0, time.UTC)) {
		t.Fatalf("large series line: rows=%+v err=%v", rows, err)
	}
}
