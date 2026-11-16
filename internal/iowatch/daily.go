package iowatch

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"time"

	"github.com/aiseeq/aguard/internal/quantile"
)

const DailyKeep = 30

// DailyMemory is a local-calendar-day summary of the available-memory-plus-graft measure.
type DailyMemory struct {
	Date             string `json:"date"`
	GraftFrom        string `json:"graft_from"`
	Seconds          int    `json:"seconds"`
	P1               int64  `json:"p1"`
	P5               int64  `json:"p5"`
	P50              int64  `json:"p50"`
	GraftSliceMemMax int64  `json:"graft_slice_mem_max"`
}

// DailySample holds one second with known graft memory for the local-date rollup.
type DailySample struct {
	At                 time.Time
	AvailablePlusGraft int64
	GraftSliceMem      int64
}

// DailySampleFor converts a series second only when its graft-memory value is known.
func DailySampleFor(row MachineSecond, date string, loc *time.Location) (DailySample, bool) {
	if row.At.In(loc).Format("2006-01-02") != date || (!row.GraftSliceMemKnown && row.GraftSliceMem == 0) {
		return DailySample{}, false
	}
	return DailySample{At: row.At, AvailablePlusGraft: row.MemAvail + row.GraftSliceMem, GraftSliceMem: row.GraftSliceMem}, true
}

// SummarizeDaily computes the local-date quantiles from its known samples.
func SummarizeDaily(samples []DailySample, date string, loc *time.Location) DailyMemory {
	values := make([]int64, 0)
	var maxGraft int64
	var graftFrom time.Time
	for i := range samples {
		sample := &samples[i]
		if sample.At.In(loc).Format("2006-01-02") != date {
			continue
		}
		values = append(values, sample.AvailablePlusGraft)
		maxGraft = max(maxGraft, sample.GraftSliceMem)
		if graftFrom.IsZero() || sample.At.Before(graftFrom) {
			graftFrom = sample.At
		}
	}
	slices.Sort(values)
	graftFromText := ""
	if !graftFrom.IsZero() {
		graftFromText = graftFrom.In(loc).Format(time.RFC3339)
	}
	return DailyMemory{Date: date, GraftFrom: graftFromText, Seconds: len(values), P1: quantile.Of(values, 0.01),
		P5: quantile.Of(values, 0.05), P50: quantile.Of(values, 0.5), GraftSliceMemMax: maxGraft}
}

// SaveDaily appends a day once, keeping only the newest 30 dates.
func SaveDaily(path string, entry DailyMemory) error {
	rows, err := LoadDaily(path)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.Date == entry.Date {
			return nil
		}
	}
	rows = append(rows, entry)
	sort.Slice(rows, func(i, j int) bool { return rows[i].Date < rows[j].Date })
	if len(rows) > DailyKeep {
		rows = rows[len(rows)-DailyKeep:]
	}
	return saveLines(path, "суточная свёртка памяти", rows)
}

func LoadDaily(path string) ([]DailyMemory, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open daily memory %s: %w", path, err)
	}
	defer f.Close()
	var rows []DailyMemory
	s := bufio.NewScanner(f)
	for s.Scan() {
		var row DailyMemory
		if err := json.Unmarshal(s.Bytes(), &row); err != nil {
			return nil, fmt.Errorf("decode daily memory row %d: %w", len(rows)+1, err)
		}
		rows = append(rows, row)
	}
	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("read daily memory %s: %w", path, err)
	}
	return rows, nil
}
