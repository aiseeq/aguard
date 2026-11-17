package guiwatch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// Дневные агрегаты сна берутся за местные даты окна [since, until): последний день окна входит, день после окна до
// полуночи — нет, а пояс аргументов окна не сдвигает даты.
func TestSleepReportDaysOfWindow(t *testing.T) {
	dir := t.TempDir()
	day := time.Date(2026, 11, 16, 0, 0, 0, 0, time.Local)
	for i, d := range []time.Time{day.AddDate(0, 0, -1), day, day.AddDate(0, 0, 1)} {
		date := d.Format("2006-01-02")
		raw, err := json.Marshal([]SleepStat{{Date: date, Target: "t", Wchan: "w", LongestMS: int64(i + 1)}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "sleeps-"+date+".json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	far := time.FixedZone("far", 14*3600)
	d := func(t time.Time) string { return t.Format("2006-01-02") }
	windows := []struct {
		since, until time.Time
		want         []string
	}{
		{day, day.AddDate(0, 0, 1), []string{d(day)}},
		{day.Add(23 * time.Hour), day.AddDate(0, 0, 1), []string{d(day)}},
		{day, day.AddDate(0, 0, 1).Add(12 * time.Hour), []string{d(day), d(day.AddDate(0, 0, 1))}},
	}
	for _, w := range windows {
		for _, zone := range []*time.Location{time.Local, far} {
			since, until := w.since.In(zone), w.until.In(zone)
			got, err := readSleepReport(dir, since, &until)
			if err != nil {
				t.Fatal(err)
			}
			var dates []string
			for _, st := range got {
				dates = append(dates, st.Date)
			}
			slices.Sort(dates)
			if !slices.Equal(dates, w.want) {
				t.Fatalf("окно %v–%v: дни %v, нужно %v", since, until, dates, w.want)
			}
		}
	}
}
