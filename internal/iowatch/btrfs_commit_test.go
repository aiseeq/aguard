package iowatch

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const sampleCommitStats = "commits %d\ncur_commit_ms %d\nlast_commit_ms %d\nmax_commit_ms 12000\ntotal_commit_ms 21000\n"

func writeCommitStats(t *testing.T, root, uuid string, commits, cur, last int64) {
	t.Helper()
	dir := filepath.Join(root, uuid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "commit_stats"), fmt.Appendf(nil, sampleCommitStats, commits, cur, last), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBtrfsCommitSamplerSequenceAndFilesystems(t *testing.T) {
	root := t.TempDir()
	const one = "11111111-1111-1111-1111-111111111111"
	const two = "22222222-2222-2222-2222-222222222222"
	sampler := BtrfsCommitSampler{Root: root}
	if err := os.WriteFile(filepath.Join(root, "not-a-fs"), []byte("ignored"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i, cur := range []int64{0, 0, 3500, 7000, 0} {
		commits := int64(4)
		last := int64(0)
		if i == 4 {
			commits, last = 5, 7200
		}
		writeCommitStats(t, root, one, commits, cur, last)
		gotCur, gotCommits, gotLast, err := sampler.Sample()
		if err != nil {
			t.Fatal(err)
		}
		wantCount := int64(0)
		if i == 4 {
			wantCount = 1
		}
		if gotCur != cur || gotCommits != wantCount || gotLast != last {
			t.Fatalf("sample %d = cur %d commits %d last %d", i, gotCur, gotCommits, gotLast)
		}
	}
	writeCommitStats(t, root, one, 5, 0, 7200) // unchanged count must not report stale last_commit_ms
	if cur, commits, last, err := sampler.Sample(); err != nil || cur != 0 || commits != 0 || last != 0 {
		t.Fatalf("unchanged counter sample = %d/%d/%d err=%v", cur, commits, last, err)
	}

	writeCommitStats(t, root, one, 5, 9000, 7200)
	writeCommitStats(t, root, two, 8, 8000, 11000)
	if cur, commits, last, err := sampler.Sample(); err != nil || cur != 9000 || commits != 0 || last != 0 {
		t.Fatalf("new filesystem baseline = %d/%d/%d err=%v", cur, commits, last, err)
	}
	writeCommitStats(t, root, one, 6, 7000, 13000)
	writeCommitStats(t, root, two, 9, 6000, 9000)
	cur, commits, last, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if cur != 7000 || commits != 2 || last != 13000 {
		t.Fatalf("multi-filesystem max sample = %d/%d/%d", cur, commits, last)
	}
}

func TestBtrfsCommitSamplerMissingAndMalformed(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	sampler := BtrfsCommitSampler{Root: root}
	if _, _, _, err := sampler.Sample(); err != nil {
		t.Fatalf("missing sysfs root: %v", err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	const uuid = "11111111-1111-1111-1111-111111111111"
	if err := os.Mkdir(filepath.Join(root, uuid), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, uuid, "commit_stats"), []byte("commits nope\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := sampler.Sample(); err == nil {
		t.Fatal("malformed file did not produce warning error")
	}
}

func TestLegacySeriesWithoutBtrfsFieldsLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "series.jsonl")
	if err := os.WriteFile(path, []byte(`{"at":"2026-11-21T10:00:00Z","io_full_ms":7}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, err := LoadSeries(path, time.Time{})
	if err != nil || len(rows) != 1 || rows[0].IOFullMS != 7 || rows[0].BtrfsCommits != 0 {
		t.Fatalf("legacy series rows=%+v err=%v", rows, err)
	}
}

func TestBtrfsCommitOverlappingCaseUsesMaximum(t *testing.T) {
	series := NewSeries(time.Minute)
	start := time.Unix(100, 0)
	for i, cur := range []int64{0, 0, 3500, 7000, 0} {
		last := int64(0)
		if i == 4 {
			last = 7200
		}
		series.Add(MachineSecond{At: start.Add(time.Duration(i) * time.Second), BtrfsCommitCurMS: cur, BtrfsCommitLastMS: last})
	}
	r := NewRecorder(Default())
	r.Attach(series, nil, nil)
	c := Case{Start: start.Add(1500 * time.Millisecond), End: start.Add(4500 * time.Millisecond)}
	r.finish(&c)
	if c.BtrfsCommitMaxMS != 7200 {
		t.Fatalf("case commit max %d, want 7200", c.BtrfsCommitMaxMS)
	}
}

func TestBtrfsCommitSeriesKeepsMaximumWithinSecond(t *testing.T) {
	base := time.Unix(200, 0)
	series := NewSeries(time.Minute)
	r := NewRecorder(Default())
	r.Attach(series, nil, nil)
	for i, cur := range []int64{3500, 7000, 1000} {
		commitCount, last := int64(0), int64(0)
		if i == 0 {
			commitCount, last = 1, 7000
		} else if i == 1 {
			commitCount, last = 2, 7200
		}
		r.Step(Step{At: base.Add(100*time.Millisecond + time.Duration(i)*100*time.Millisecond), BtrfsCommitCurMS: cur, BtrfsCommits: commitCount, BtrfsCommitLastMS: last})
	}
	r.Step(Step{At: base.Add(time.Second), BtrfsCommitCurMS: 0})
	rows := series.Rows(time.Time{}).Seconds()
	if len(rows) != 1 || rows[0].BtrfsCommitCurMS != 7000 || rows[0].BtrfsCommits != 3 || rows[0].BtrfsCommitLastMS != 7200 {
		t.Fatalf("second max = %+v", rows)
	}
}

func TestBtrfsCommitLiveCaseProbe(t *testing.T) {
	for _, tc := range []struct {
		name         string
		commitEnd    float64
		wantCommitMS int64
		tickOff      float64
	}{
		{"case-ends-mid-second", 109.2, 7200, 0.5},
		{"case-ends-early-second", 109.05, 7050, 0.5},
		{"case-ends-late-second", 109.9, 7900, 0.5},
		{"tick-late-phase", 109.97, 7970, 0.96},
		{"tick-early-phase", 109.2, 7200, 0.01},
	} {
		t.Run(tc.name, func(t *testing.T) {
			series := NewSeries(time.Hour)
			r := NewRecorder(Default())
			r.Attach(series, nil, nil)
			sec := func(f float64) time.Time { return time.Unix(0, int64(f*1e9)) }
			var pendCur, pendN, pendLast int64
			nextTick, nextDetail := 100+tc.tickOff, 100.3
			commits, done := int64(10), false
			var cases []Case
			for i := range 300 {
				at := 100.0 + float64(i)*0.1 + 0.05
				for nextTick <= at {
					cur := int64(0)
					if nextTick > 102 && nextTick < tc.commitEnd {
						cur = int64((nextTick - 102) * 1000)
					}
					pendCur, pendN, pendLast = cur, 0, 0
					if !done && nextTick >= tc.commitEnd {
						done = true
						commits++
						pendN, pendLast = 1, tc.wantCommitMS
					}
					nextTick++
				}
				for nextDetail <= at {
					r.Detail(Step{At: sec(nextDetail)})
					nextDetail++
				}
				sample := Step{At: sec(at), BtrfsCommitCurMS: pendCur, BtrfsCommits: pendN, BtrfsCommitLastMS: pendLast}
				pendCur, pendN, pendLast = 0, 0, 0
				if at > 102 && at-0.1 < tc.commitEnd {
					sample.IOFullUS = 100000
				}
				cases = append(cases, r.Step(sample)...)
			}
			if len(cases) != 1 || cases[0].BtrfsCommitMaxMS != tc.wantCommitMS {
				t.Fatalf("cases=%+v, want one commit max %dms", cases, tc.wantCommitMS)
			}
			needClosed := cases[0].End.Truncate(time.Second).Add(time.Second)
			last, ok := series.LastAt()
			if !ok || last.Before(needClosed) {
				t.Fatalf("case finished before row %s was closed (last row %s)", needClosed, last)
			}
		})
	}
}

func TestBtrfsCommitCloseFinishesWaitingAndOpenCases(t *testing.T) {
	base := time.Unix(700, 0)
	series := NewSeries(time.Hour)
	series.Add(MachineSecond{At: base.Add(-10 * time.Second), BtrfsCommitCurMS: 4500})
	series.Add(MachineSecond{At: base.Add(3 * time.Second), BtrfsCommitLastMS: 7200})
	r := NewRecorder(Default())
	r.Attach(series, nil, nil)
	r.waiting = []Case{{Start: base.Add(-10 * time.Second), End: base.Add(-9500 * time.Millisecond), DurMS: 500}}
	r.open = &openCase{start: base.Add(3200 * time.Millisecond), last: base.Add(3500 * time.Millisecond)}

	got := r.Close()
	if len(got) != 2 {
		t.Fatalf("Close returned %d waiting/open cases, want 2: %+v", len(got), got)
	}
	if got[0].BtrfsCommitMaxMS != 4500 || got[1].BtrfsCommitMaxMS != 7200 {
		t.Fatalf("Close commit maxima = %d, %d; want existing-row values 4500, 7200", got[0].BtrfsCommitMaxMS, got[1].BtrfsCommitMaxMS)
	}
}

func TestBtrfsCommitCloseProbe(t *testing.T) {
	for _, attach := range []bool{false, true} {
		r := NewRecorder(Default())
		if attach {
			r.Attach(NewSeries(time.Hour), nil, nil)
		}
		base := time.Unix(1000, 0)
		var got []Case
		for i := 1; i <= 15; i++ {
			s := Step{At: base.Add(time.Duration(i) * 100 * time.Millisecond)}
			if i <= 10 {
				s.IOFullUS = 100000
			}
			got = append(got, r.Step(s)...)
		}
		got = append(got, r.Close()...)
		if len(got) != 1 {
			t.Errorf("attached series=%v: Close returned %d cases, want 1", attach, len(got))
		}
	}
}

func TestBtrfsCommitOverlapsEndRowAndUsesCurOrLastMaximum(t *testing.T) {
	base := time.Unix(300, 0)
	series := NewSeries(time.Minute)
	for _, row := range []MachineSecond{
		{At: base, BtrfsCommitCurMS: 7000},
		{At: base.Add(time.Second), BtrfsCommitLastMS: 7200},
		{At: base.Add(2 * time.Second), BtrfsCommitCurMS: 100},
	} {
		series.Add(row)
	}
	if got := btrfsCommitMaxForCase(series, base, base.Add(1500*time.Millisecond)); got != 7200 {
		t.Fatalf("max overlapping commit = %d, want 7200", got)
	}
	if got := btrfsCommitMaxForCase(series, base, base.Add(500*time.Millisecond)); got != 7200 {
		t.Fatalf("max commit overlap = %d, want 7200", got)
	}
}

func TestBtrfsCommitOverlapsCaseUsesCurrentOnlyAndEndBoundary(t *testing.T) {
	base := time.Unix(400, 0)
	curSeries := NewSeries(time.Minute)
	curSeries.Add(MachineSecond{At: base, BtrfsCommitCurMS: 7200})
	if got := btrfsCommitMaxForCase(curSeries, base, base.Add(500*time.Millisecond)); got != 7200 {
		t.Fatalf("current-only overlap = %d, want 7200", got)
	}
	endSeries := NewSeries(time.Minute)
	endSeries.Add(MachineSecond{At: base.Add(time.Second), BtrfsCommitLastMS: 7200})
	if got := btrfsCommitMaxForCase(endSeries, base, base.Add(900*time.Millisecond)); got != 7200 {
		t.Fatalf("commit ending in floor(End)+1 row = %d, want 7200", got)
	}
}

func TestBtrfsCommitOverlapIntervalSlackAndStrictBoundaries(t *testing.T) {
	at := time.Unix(800, 0)
	if !BtrfsCommitOverlaps(at, 2000, at.Add(-2500*time.Millisecond), at.Add(-2250*time.Millisecond)) {
		t.Fatal("one-second polling slack at commit start was not included")
	}
	commitStart := at.Add(-3 * time.Second)
	commitEnd := at.Add(time.Second)
	if BtrfsCommitOverlaps(at, 2000, commitStart.Add(-time.Second), commitStart) {
		t.Fatal("touching the case end must not count as overlap")
	}
	if BtrfsCommitOverlaps(at, 2000, commitEnd, commitEnd.Add(time.Second)) {
		t.Fatal("touching the case start must not count as overlap")
	}
}

func TestBtrfsCommitStatsOptionalCurAndFailedFSKeepsBaseline(t *testing.T) {
	root := t.TempDir()
	const uuid = "11111111-1111-1111-1111-111111111111"
	const minimal = "commits 4\nlast_commit_ms 7200\nmax_commit_ms 7200\ntotal_commit_ms 9000\n"
	path := filepath.Join(root, uuid, "commit_stats")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(minimal), 0o644); err != nil {
		t.Fatal(err)
	}
	sampler := BtrfsCommitSampler{Root: root}
	if cur, commits, last, err := sampler.Sample(); err != nil || cur != 0 || commits != 0 || last != 0 {
		t.Fatalf("baseline without cur: %d/%d/%d err=%v", cur, commits, last, err)
	}
	if err := os.WriteFile(path, []byte("broken\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := sampler.Sample(); err == nil {
		t.Fatal("broken sysfs should return a warning error")
	}
	if err := os.WriteFile(path, []byte("commits 5\nlast_commit_ms 7200\nmax_commit_ms 7200\ntotal_commit_ms 16200\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cur, commits, last, err := sampler.Sample(); err != nil || cur != 0 || commits != 1 || last != 7200 {
		t.Fatalf("recovered FS sample: %d/%d/%d err=%v", cur, commits, last, err)
	}
}

func TestBtrfsCommitCounterResetStartsNewBaseline(t *testing.T) {
	root := t.TempDir()
	const uuid = "11111111-1111-1111-1111-111111111111"
	sampler := BtrfsCommitSampler{Root: root}
	writeCommitStats(t, root, uuid, 10, 0, 9000)
	if _, _, _, err := sampler.Sample(); err != nil {
		t.Fatal(err)
	}
	writeCommitStats(t, root, uuid, 2, 0, 9000)
	if _, commits, last, err := sampler.Sample(); err != nil || commits != 0 || last != 0 {
		t.Fatalf("counter reset sample = commits %d last %d err=%v", commits, last, err)
	}
	writeCommitStats(t, root, uuid, 3, 0, 1000)
	if _, commits, last, err := sampler.Sample(); err != nil || commits != 1 || last != 1000 {
		t.Fatalf("post-reset delta = commits %d last %d err=%v", commits, last, err)
	}
}

func BenchmarkReadBtrfsCommitStats(b *testing.B) {
	path := filepath.Join(b.TempDir(), "commit_stats")
	if err := os.WriteFile(path, []byte("commits 8\ncur_commit_ms 3500\nlast_commit_ms 3200\nmax_commit_ms 12000\ntotal_commit_ms 21000\n"), 0o644); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := readBtrfsCommitStats(path); err != nil {
			b.Fatal(err)
		}
	}
}
