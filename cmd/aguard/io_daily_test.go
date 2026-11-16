package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/iowatch"
)

func newDailyTestService(t *testing.T) *ioService {
	t.Helper()
	a, _ := testIOApp(t)
	s := newIOService(context.Background(), a)
	s.cfg = iowatch.Default()
	s.rec = iowatch.NewRecorder(s.cfg)
	s.rec.Attach(s.series, s.profiles, s.live)
	s.dailyPath = filepath.Join(t.TempDir(), "graft-mem-daily.jsonl")
	return s
}

func addDailyTestSecond(s *ioService, at time.Time, graft int64, known bool) {
	s.series.Add(iowatch.MachineSecond{At: at, MemAvail: 2 << 30, GraftSliceMem: graft, GraftSliceMemKnown: known})
}

func waitDailyTestSave(t *testing.T, s *ioService) {
	t.Helper()
	if s.dailySaveDone == nil {
		return
	}
	select {
	case err := <-s.dailySaveDone:
		s.dailySaveDone = nil
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("daily save timed out")
	}
}

func readDailyTestRows(t *testing.T, path string) []iowatch.DailyMemory {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rows []iowatch.DailyMemory
	for line := range bytes.SplitSeq(bytes.TrimSpace(data), []byte{'\n'}) {
		var row iowatch.DailyMemory
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	return rows
}

func TestIODailyRollupUsesLastSeriesDateAndDoesNotDuplicate(t *testing.T) {
	loc := time.Local
	dayD := time.Date(2026, 11, 14, 0, 0, 0, 0, loc)
	d1 := dayD.AddDate(0, 0, 1)
	s := newDailyTestService(t)
	addDailyTestSecond(s, dayD.Add(23*time.Hour+59*time.Minute+57*time.Second), 0, false)
	addDailyTestSecond(s, dayD.Add(23*time.Hour+59*time.Minute+58*time.Second), 10<<20, true)
	addDailyTestSecond(s, dayD.Add(23*time.Hour+59*time.Minute+59*time.Second), 20<<20, true)
	s.seedDailyDate(d1.Add(time.Second))
	if s.dailyDate != "2026-11-14" {
		t.Fatalf("initial date from last stored second = %s", s.dailyDate)
	}
	s.step(iowatch.Step{At: d1.Add(time.Second)})
	s.step(iowatch.Step{At: d1.Add(2 * time.Second)})
	waitDailyTestSave(t, s)
	rows := readDailyTestRows(t, s.dailyPath)
	if len(rows) != 1 || rows[0].Date != "2026-11-14" || rows[0].Seconds != 2 || rows[0].GraftFrom != dayD.Add(23*time.Hour+59*time.Minute+58*time.Second).Format(time.RFC3339) {
		t.Fatalf("rollup after midnight: %+v", rows)
	}

	addDailyTestSecond(s, d1.Add(3*time.Second), 30<<20, true)
	s.dailyDate = "" // emulate a restart after the new date has entered the series
	s.seedDailyDate(d1.Add(4 * time.Second))
	s.step(iowatch.Step{At: d1.Add(4 * time.Second)})
	waitDailyTestSave(t, s)
	rows = readDailyTestRows(t, s.dailyPath)
	if len(rows) != 1 || rows[0].Date != "2026-11-14" {
		t.Fatalf("restart in D+1 wrote another date: %+v", rows)
	}

	early := newDailyTestService(t)
	addDailyTestSecond(early, dayD.Add(23*time.Hour+59*time.Minute+59*time.Second), 40<<20, true)
	early.seedDailyDate(d1.Add(2 * time.Second))
	early.step(iowatch.Step{At: d1.Add(2 * time.Second)})
	waitDailyTestSave(t, early)
	rows = readDailyTestRows(t, early.dailyPath)
	if len(rows) != 1 || rows[0].Date != "2026-11-14" || rows[0].Seconds != 1 {
		t.Fatalf("early restart failed to finish prior date: %+v", rows)
	}
}

func TestIODailyBrokenFileIsQuarantined(t *testing.T) {
	s := newDailyTestService(t)
	if err := os.WriteFile(s.dailyPath, []byte("{broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.load()
	moved, err := filepath.Glob(s.dailyPath + ".broken-*")
	if err != nil || len(moved) != 1 {
		t.Fatalf("broken daily files: %v, err %v", moved, err)
	}
}
