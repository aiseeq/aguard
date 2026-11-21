package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/iowatch"
	"github.com/aiseeq/aguard/internal/linesock"
)

func TestIOReportBtrfsCommitCountsAndLegacyRows(t *testing.T) {
	base := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	seconds := []iowatch.MachineSecond{
		{At: base, BtrfsCommits: 2, BtrfsCommitLastMS: 3000},
		{At: base.Add(time.Second), BtrfsCommits: 1, BtrfsCommitLastMS: 4000},
		{At: base.Add(2 * time.Second), BtrfsCommits: 1, BtrfsCommitLastMS: 5000},
		{At: base.Add(4 * time.Second), BtrfsCommits: 1, BtrfsCommitLastMS: 2000},
		{At: base.Add(5 * time.Second), BtrfsCommits: 1, BtrfsCommitLastMS: 2100},
		{At: base.Add(6 * time.Second), BtrfsCommits: 3, BtrfsCommitLastMS: 1500},
		{At: base.Add(7 * time.Second)},                                           // legacy row without Btrfs fields
		{At: base.Add(8 * time.Second), BtrfsCommitLastMS: 5000},                  // last value without counter delta
		{At: base.Add(9 * time.Second), BtrfsCommits: 1, BtrfsCommitLastMS: 8000}, // exactly at exclusive until
	}
	cases := []iowatch.Case{{Start: base, End: base.Add(time.Second)}}
	sum := iowatch.Summary{BtrfsCommitReportMS: 2000}
	sumBtrfsCommits(&sum, seconds, cases, base, base.Add(9*time.Second), 2000)
	if sum.BtrfsLongCommits != 5 || sum.BtrfsOutsideCases != 2 || sum.BtrfsLongestCommitMS != 5000 || !sum.BtrfsLongestCommitAt.Equal(base.Add(2*time.Second)) || !sum.BtrfsHasData {
		t.Fatalf("commit summary: %+v", sum)
	}
	var report bytes.Buffer
	writeIOReport(&report, &sum)
	if !bytes.Contains(report.Bytes(), []byte("долгие коммиты btrfs (≥ 2 с): 5, самый долгий 5,0 с в 10:00:02; из них вне случаев фриза: 2")) {
		t.Fatalf("commit report line: %s", report.String())
	}

}

func TestIOReportBtrfsCommitEndRowOverlapsFreeze(t *testing.T) {
	base := time.Unix(500, 0)
	seconds := []iowatch.MachineSecond{{At: base.Add(10 * time.Second), BtrfsCommits: 1, BtrfsCommitLastMS: 7200}}
	cases := []iowatch.Case{{Start: base.Add(2 * time.Second), End: base.Add(9*time.Second + 900*time.Millisecond)}}
	sum := iowatch.Summary{BtrfsCommitReportMS: 2000}
	sumBtrfsCommits(&sum, seconds, cases, base, base.Add(time.Minute), 2000)
	if sum.BtrfsLongCommits != 1 || sum.BtrfsOutsideCases != 0 {
		t.Fatalf("commit ending in floor(End)+1 row counted outside freeze: %+v", sum)
	}
}

func TestIOReportBtrfsNoDataPreservesLegacyTextAndJSON(t *testing.T) {
	var out bytes.Buffer
	writeIOReport(&out, &iowatch.Summary{})
	const legacy = "Случаи фриза 01.01 00:00 — 01.01 00:00: 0, простой всего 0 мс\n"
	if out.String() != legacy {
		t.Fatalf("no-commit report changed: %q, want %q", out.String(), legacy)
	}
	raw, err := json.Marshal(iowatch.Summary{})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("btrfs_commit_report_ms")) || bytes.Contains(raw, []byte("btrfs_unavailable")) {
		t.Fatalf("empty Btrfs fields in legacy JSON: %s", raw)
	}

	zero := iowatch.Summary{BtrfsCommitReportMS: 2000}
	sumBtrfsCommits(&zero, []iowatch.MachineSecond{{At: time.Now(), BtrfsCommitCurMS: 2200, BtrfsCommitLastMS: 5000}}, nil, time.Time{}, time.Now().Add(time.Hour), 2000)
	if !zero.BtrfsHasData || zero.BtrfsLongCommits != 0 {
		t.Fatalf("current-only or no-delta row should be data without a completed commit: %+v", zero)
	}
	out.Reset()
	writeIOReport(&out, &zero)
	if !bytes.Contains(out.Bytes(), []byte("долгие коммиты btrfs (≥ 2 с): 0")) {
		t.Fatalf("Btrfs data with no long commits lacks line: %s", out.String())
	}
}

func TestIOReportNoBtrfsDataOmitsClippedWindowFromJSON(t *testing.T) {
	a, out := testIOApp(t)
	path := filepath.Join(t.TempDir(), "io", "cases.jsonl")
	if err := iowatch.SaveCases(path, nil); err != nil {
		t.Fatal(err)
	}
	if err := iowatch.SaveSeries(filepath.Join(filepath.Dir(path), ioSeriesFile), nil); err != nil {
		t.Fatal(err)
	}
	until := time.Now()
	if err := a.ioReport(context.Background(), path, false, until.Add(-48*time.Hour), until, 0, "", true); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out.Bytes(), []byte("btrfs_window")) || bytes.Contains(out.Bytes(), []byte("btrfs_commit_report_ms")) {
		t.Fatalf("empty Btrfs row changed legacy JSON: %s", out.String())
	}
}

func TestIOBtrfsPendingAccumulatesUntilFastReadSucceeds(t *testing.T) {
	var pending ioBtrfsPending
	pending.add(7000, 1, 7200)
	pending.add(0, 0, 0) // a fast.Read failure leaves this pending
	pending.add(3500, 0, 0)
	if cur, commits, last := pending.take(); cur != 7000 || commits != 1 || last != 7200 {
		t.Fatalf("pending Btrfs samples = %d/%d/%d", cur, commits, last)
	}
	if cur, commits, last := pending.take(); cur != 0 || commits != 0 || last != 0 {
		t.Fatalf("pending Btrfs values repeated after take = %d/%d/%d", cur, commits, last)
	}
}

func TestIOReportBtrfsUnavailableAndRetentionWindow(t *testing.T) {
	var out bytes.Buffer
	writeIOReport(&out, &iowatch.Summary{BtrfsUnavailable: "сокет недоступен", BtrfsWindow: "за последние 26 ч"})
	if !bytes.Contains(out.Bytes(), []byte("коммиты btrfs: ряд недоступен: сокет недоступен; за последние 26 ч")) || bytes.Contains(out.Bytes(), []byte("долгие коммиты")) {
		t.Fatalf("unavailable series line: %s", out.String())
	}
	since, until := time.Now().Add(-48*time.Hour), time.Now()
	got, clipped := ioReportSeriesWindow(since, until, 26*time.Hour)
	if !clipped || until.Sub(got) != 26*time.Hour {
		t.Fatalf("series window cutoff=%s clipped=%v", got, clipped)
	}
	cutoff := until.Add(-26 * time.Hour)
	if got, clipped := ioReportSeriesWindow(cutoff, until, 26*time.Hour); clipped || !got.Equal(cutoff) {
		t.Fatalf("exact retention cutoff should not be clipped: since=%s clipped=%v", got, clipped)
	}
}

func TestIOReportKeepsGoingWhenSeriesReadFails(t *testing.T) {
	a, out := testIOApp(t)
	path := filepath.Join(t.TempDir(), "io", "cases.jsonl")
	if err := iowatch.SaveCases(path, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), ioSeriesFile), []byte("{broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := a.ioReport(context.Background(), path, false, now.Add(-time.Hour), now, 0, "", false); err != nil {
		t.Fatalf("report failed with unavailable series: %v", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("Случаи фриза")) || !bytes.Contains(out.Bytes(), []byte("коммиты btrfs: ряд недоступен:")) {
		t.Fatalf("report did not continue with a series warning: %s", out.String())
	}
}

func TestIOReportContinuesWhenLiveSeriesQueryFails(t *testing.T) {
	s, out, socket := ioLiveService(t, t.Context())
	ln, err := linesock.Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- linesock.Serve(ctx, ln, func(line []byte) any {
			var req iowatch.Request
			if err := json.Unmarshal(line, &req); err != nil {
				return iowatch.Answer{V: iowatch.ProtoVersion, Error: err.Error()}
			}
			if req.Q == iowatch.QuerySeries {
				return iowatch.Answer{V: iowatch.ProtoVersion, Error: "probe read failure"}
			}
			return iowatch.Answer{V: iowatch.ProtoVersion, Coverage: &iowatch.CoverRecord{Start: time.Now()}}
		}, slog.New(slog.NewTextHandler(io.Discard, nil)), "probe")
	}()
	acceptWaitSocket(t, socket)
	now := time.Now()
	if err := s.a.ioReport(context.Background(), s.path, true, now.Add(-time.Hour), now, 0, "", false); err != nil {
		t.Fatalf("report failed after live series error: %v", err)
	}
	cancel()
	<-serverDone
	if !bytes.Contains(out.Bytes(), []byte("коммиты btrfs: ряд недоступен: служба: probe read failure")) || !bytes.Contains(out.Bytes(), []byte("Случаи фриза")) {
		t.Fatalf("live series error did not preserve the report: %s", out.String())
	}
}

func TestIOCaseLineBtrfsThresholdPreservesLegacyText(t *testing.T) {
	start := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	c := iowatch.Case{Start: start, DurMS: 1000, IdleMS: 1000, PeakMS: 0, BtrfsCommitMaxMS: 999}
	legacy := "21.11 10:00:00, 1,0 с (простой 1,0 с, io; пик шага 0 мс)"
	if got := ioCaseLine(&c); got != legacy {
		t.Fatalf("without commit = %q, want byte-for-byte %q", got, legacy)
	}
	c.BtrfsCommitMaxMS = 1000
	if got := ioCaseLine(&c); !bytes.Contains([]byte(got), []byte("коммит btrfs до 1000 мс")) {
		t.Fatalf("1000ms commit missing from case line: %q", got)
	}
}
