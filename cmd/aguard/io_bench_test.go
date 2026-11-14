package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/pressure"
)

type fakeBench struct {
	now              time.Time
	mem              []float64
	memIndex         int
	waitCalls        int
	memErrAfterWait  int
	writeback        []float64
	pressure         []int64
	pressureIndex    int
	setCalls         int
	failSetAt        int
	quiet            time.Time
	cleared          bool
	lockReleased     bool
	lockCalls        int
	lockErr          error
	syncDirty        float64
	syncDirties      []float64
	syncCalls        int
	disk             []int64
	diskIndex        int
	started          []string
	scopeCodes       []int
	scopeSelf        []int64
	scopePSI         []int64
	stopped          int
	fadvise          int
	fadviseErr       error
	scopeErr         bool
	waitForever      bool
	deadlineSeen     bool
	takeover         bool
	startUnthrottled []bool
	sequence         []string
}

func (f *fakeBench) Now() time.Time { return f.now }
func (f *fakeBench) Wait(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.now = f.now.Add(d)
	f.waitCalls++
	if f.memIndex < len(f.mem)-1 {
		f.memIndex++
	}
	return nil
}
func (f *fakeBench) ReadMeminfo() ([]byte, error) {
	if f.memErrAfterWait > 0 && f.waitCalls >= f.memErrAfterWait {
		return nil, errors.New("подставной meminfo после срока")
	}
	v := f.mem[f.memIndex]
	wb := 0.0
	if f.memIndex < len(f.writeback) {
		wb = f.writeback[f.memIndex]
	}
	return fmt.Appendf(nil, "Dirty: %d kB\nWriteback: %d kB\n", int(v*1024), int(wb*1024)), nil
}
func (f *fakeBench) ReadPressure() ([]byte, error) {
	n := int64(0)
	if f.pressureIndex < len(f.pressure) {
		n = f.pressure[f.pressureIndex]
		f.pressureIndex++
	}
	return fmt.Appendf(nil, "some avg10=0 avg60=0 avg300=0 total=%d\nfull avg10=0 avg60=0 avg300=0 total=%d\n", n, n), nil
}
func (f *fakeBench) Sync() error {
	f.syncCalls++
	f.syncDirty = f.mem[f.memIndex]
	if f.memIndex < len(f.writeback) {
		f.syncDirty += f.writeback[f.memIndex]
	}
	f.syncDirties = append(f.syncDirties, f.syncDirty)
	if f.memIndex < len(f.mem)-1 {
		f.memIndex++
	}
	return nil
}
func (f *fakeBench) Fadvise(string) error { f.fadvise++; return f.fadviseErr }
func (f *fakeBench) Lock(_ context.Context, _ string, _ time.Duration) (func() error, error) {
	f.lockCalls++
	if f.lockErr != nil {
		return nil, f.lockErr
	}
	return func() error { f.lockReleased = true; return nil }, nil
}
func (f *fakeBench) QuietPath() (string, error)          { return "fake-quiet", nil }
func (f *fakeBench) ReadQuiet(string) (time.Time, error) { return f.quiet, nil }
func (f *fakeBench) SetQuiet(_ string, n pressure.QuietNote) error {
	f.setCalls++
	if f.failSetAt == f.setCalls {
		return errors.New("refused")
	}
	f.quiet = n.Until.Truncate(time.Second)
	return nil
}
func (f *fakeBench) ClearQuiet(string) error { f.quiet = time.Time{}; f.cleared = true; return nil }
func (f *fakeBench) Start(_ context.Context, v benchVariant, _ string, _, cap string, unthrottled bool) (benchScope, error) {
	g := "с потолком"
	if unthrottled {
		g = "без потолка"
	}
	f.started = append(f.started, v.label+"/"+g+"/"+cap)
	f.startUnthrottled = append(f.startUnthrottled, unthrottled)
	i := len(f.started) - 1
	return &fakeBenchScope{f: f, i: i}, nil
}
func (f *fakeBench) DeviceBytes(string) (int64, error) {
	if f.diskIndex >= len(f.disk) {
		return 0, nil
	}
	v := f.disk[f.diskIndex]
	f.diskIndex++
	return v, nil
}

type fakeBenchScope struct {
	f *fakeBench
	i int
}

func (s *fakeBenchScope) Wait(ctx context.Context) (int, error) {
	if s.f.waitForever {
		_, s.f.deadlineSeen = ctx.Deadline()
		if !s.f.deadlineSeen {
			return 0, errors.New("run context has no deadline")
		}
		<-ctx.Done()
		return 0, ctx.Err()
	}
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	s.f.now = s.f.now.Add(time.Second)
	if s.f.takeover {
		s.f.quiet = s.f.now.Add(time.Minute).Truncate(time.Second)
	}
	if s.i < len(s.f.scopeCodes) {
		return s.f.scopeCodes[s.i], nil
	}
	return 0, nil
}
func (s *fakeBenchScope) Stats() (int64, int64, error) {
	if s.f.scopeErr {
		return 0, 0, errors.New("нет io.stat")
	}
	self, psi := int64(0), int64(0)
	if s.i < len(s.f.scopeSelf) {
		self = s.f.scopeSelf[s.i]
	}
	if s.i < len(s.f.scopePSI) {
		psi = s.f.scopePSI[s.i]
	}
	return self, psi, nil
}
func (s *fakeBenchScope) CloseInput() error { s.f.sequence = append(s.f.sequence, "stdin"); return nil }
func (s *fakeBenchScope) Close() error      { s.f.sequence = append(s.f.sequence, "close"); return nil }
func (s *fakeBenchScope) Stop(context.Context) error {
	s.f.sequence = append(s.f.sequence, "stop")
	s.f.stopped++
	return nil
}

func baseFake() *fakeBench {
	return &fakeBench{now: time.Date(2026, 11, 13, 20, 0, 0, 0, time.UTC), mem: []float64{0, 0}, pressure: []int64{0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, disk: []int64{0, 1 << 20, 0, 1 << 20, 0, 1 << 20, 0, 1 << 20, 0, 1 << 20, 0, 1 << 20}}
}
func testOpts(t *testing.T, runs int, variants ...string) benchOptions {
	t.Helper()
	o := benchOptions{runs: runs, cap: "30M", dirtyFloor: 4 * benchMiB, runTimeout: time.Minute, prepTimeout: time.Minute, prepStall: time.Second}
	for _, v := range variants {
		o.variants = append(o.variants, benchVariant{label: v, command: "true"})
	}
	return o
}
func runFake(f *fakeBench, o benchOptions, out *bytes.Buffer) error {
	return runIOBench(context.Background(), out, f, o, "lock", "nvme0n1", 999)
}

func TestBenchABBAOrder(t *testing.T) {
	for _, tc := range []struct {
		names []string
		want  string
	}{{[]string{"A", "B"}, "A B B A A B"}, {[]string{"A", "B", "C"}, "A B C C B A A B C"}} {
		o := testOpts(t, 3, tc.names...)
		got := benchOrder(o)
		labels := make([]string, len(got))
		for i, r := range got {
			labels[i] = r.variant.label
		}
		if strings.Join(labels, " ") != tc.want {
			t.Fatalf("order %v, want %s", labels, tc.want)
		}
		var output bytes.Buffer
		printBenchOrder(&output, got)
		if !strings.Contains(output.String(), "порядок: "+tc.want) {
			t.Fatalf("order was not printed: %q", output.String())
		}
		fake := baseFake()
		var report bytes.Buffer
		if err := runFake(fake, o, &report); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(report.String(), "порядок: "+tc.want) {
			t.Fatalf("runner did not print order %q: %s", tc.want, report.String())
		}
	}
}

func TestBenchWaitsForDirtyBeforeSync(t *testing.T) {
	f := baseFake()
	f.mem = []float64{400, 64, 0}
	var out bytes.Buffer
	o := testOpts(t, 1, "A", "B")
	err := runFake(f, o, &out)
	if err != nil {
		t.Fatal(err)
	}
	if f.syncCalls != 2 || len(f.syncDirties) != 2 || f.syncDirties[0] != 64 || f.syncDirties[1] != 0 || f.waitCalls < 1 {
		t.Fatalf("sync calls=%d dirty at sync=%v waits=%d", f.syncCalls, f.syncDirties, f.waitCalls)
	}
	if len(f.started) == 0 {
		t.Fatal("no runs")
	}
	f = baseFake()
	f.mem = []float64{64, 64, 0}
	f.writeback = []float64{0, 1, 0}
	if err := prepareBench(context.Background(), f, f.now.Add(time.Minute), testOpts(t, 1, "A", "B")); err != nil {
		t.Fatal(err)
	}
	if f.syncDirty != 64 || f.waitCalls != 1 {
		t.Fatalf("Dirty+Writeback at sync=%v MiB, waits=%d", f.syncDirty, f.waitCalls)
	}
}

func TestBenchDirtyFloorUsesInclusiveKiBBoundary(t *testing.T) {
	for _, tc := range []struct {
		name      string
		postSync  []float64
		waitCalls int
	}{{"at floor", []float64{0, 4}, 0}, {"one KiB above", []float64{0, 4 + 1.0/1024, 4}, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			f := baseFake()
			f.mem = tc.postSync
			f.pressure = make([]int64, 8)
			o := testOpts(t, 1, "A", "B")
			if err := prepareBench(context.Background(), f, f.now.Add(time.Minute), o); err != nil {
				t.Fatal(err)
			}
			if f.waitCalls != tc.waitCalls {
				t.Fatalf("waits=%d, want %d", f.waitCalls, tc.waitCalls)
			}
		})
	}
}

func TestBenchDirtyFloorIncludesWriteback(t *testing.T) {
	f := baseFake()
	f.mem = []float64{0, 4, 4}
	f.writeback = []float64{0, 1.0 / 1024, 0}
	f.pressure = make([]int64, 8)
	o := testOpts(t, 1, "A", "B")
	if err := prepareBench(context.Background(), f, f.now.Add(time.Minute), o); err != nil {
		t.Fatal(err)
	}
	if f.waitCalls != 1 {
		t.Fatalf("Dirty+Writeback did not wait above floor: waits=%d", f.waitCalls)
	}
}

func TestBenchPreparationPSIAbortsAndClearsWindow(t *testing.T) {
	f := baseFake()
	f.pressure = []int64{0, 4_000_000}
	var out bytes.Buffer
	err := runFake(f, testOpts(t, 1, "A", "B"), &out)
	if code, _ := exitStatus(err); code != 2 {
		t.Fatalf("status=%v output=%s", err, out.String())
	}
	if len(f.started) != 0 || !f.cleared {
		t.Fatalf("started=%v cleared=%v", f.started, f.cleared)
	}
}

func TestBenchPrepTimeoutIsEnforcedByFakeClock(t *testing.T) {
	f := baseFake()
	f.mem = []float64{100, 100, 100}
	f.memErrAfterWait = 2
	f.pressure = make([]int64, 10)
	err := prepareBench(context.Background(), f, f.now.Add(2*time.Second), testOpts(t, 1, "A", "B"))
	if err == nil || !strings.Contains(err.Error(), "--prep-timeout") || f.waitCalls != 2 {
		t.Fatalf("prep timeout error=%v waits=%d", err, f.waitCalls)
	}
}

func TestBenchPrepTimeoutReportsConcurrentStall(t *testing.T) {
	f := baseFake()
	f.mem = []float64{100, 100}
	f.memErrAfterWait = 1
	f.pressure = []int64{0, 0, 0, time.Second.Microseconds()}
	err := prepareBench(context.Background(), f, f.now.Add(time.Second), testOpts(t, 1, "A", "B"))
	if !errors.Is(err, errBenchPrepStall) || strings.Contains(err.Error(), "--prep-timeout") {
		t.Fatalf("stall at deadline error=%v", err)
	}
}

func TestBenchPrepPollsPSIDuringDirtyWait(t *testing.T) {
	f := baseFake()
	f.mem = []float64{100, 100}
	f.pressure = []int64{0, 0, time.Second.Microseconds()}
	deadline := f.now.Add(5 * time.Second)
	err := prepareBench(context.Background(), f, deadline, testOpts(t, 1, "A", "B"))
	if !errors.Is(err, errBenchPrepStall) || f.waitCalls != 1 || !f.now.Before(deadline) {
		t.Fatalf("mid-wait stall error=%v waits=%d now=%v deadline=%v", err, f.waitCalls, f.now, deadline)
	}
}

func TestBenchForeignBytesAndScopeMissing(t *testing.T) {
	f := baseFake()
	f.disk = []int64{0, 8 << 20, 0, 1 << 20}
	f.scopeSelf = []int64{1 << 20, 1 << 20}
	var out bytes.Buffer
	err := runFake(f, testOpts(t, 1, "A", "B"), &out)
	code, _ := exitStatus(err)
	if code != 1 || !strings.Contains(out.String(), "гипотеза") {
		t.Fatalf("status=%d output=%s", code, out.String())
	}
	f = baseFake()
	f.scopeErr = true
	out.Reset()
	err = runFake(f, testOpts(t, 1, "A", "B"), &out)
	code, _ = exitStatus(err)
	if code != 2 || strings.Contains(out.String(), "чистый") {
		t.Fatalf("scope failure status=%d output=%s", code, out.String())
	}
}

func TestBenchForeignThresholdIncludesFivePercent(t *testing.T) {
	f := baseFake()
	f.disk = []int64{0, 106 << 20, 0, 106 << 20}
	f.scopeSelf = []int64{100 << 20, 100 << 20}
	var out bytes.Buffer
	err := runFake(f, testOpts(t, 1, "A", "B"), &out)
	code, _ := exitStatus(err)
	if code != 1 || !strings.Contains(out.String(), "гипотеза") {
		t.Fatalf("foreign threshold status=%d output=%s", code, out.String())
	}
}

func TestBenchForeignBytesAtMinimumThresholdStayClean(t *testing.T) {
	f := baseFake()
	f.disk = []int64{0, 5 << 20, 0, 5 << 20}
	f.scopeSelf = []int64{1 << 20, 1 << 20}
	var out bytes.Buffer
	err := runFake(f, testOpts(t, 1, "A", "B"), &out)
	code, _ := exitStatus(err)
	if code != 0 || strings.Contains(out.String(), "\tгипотеза") {
		t.Fatalf("foreign bytes at threshold: code=%d output=%s", code, out.String())
	}
}

func TestBenchQuietRenewFailureStopsNextRun(t *testing.T) {
	f := baseFake()
	f.failSetAt = 2
	var out bytes.Buffer
	err := runFake(f, testOpts(t, 1, "A", "B"), &out)
	code, _ := exitStatus(err)
	if code != 2 || len(f.started) != 1 || !f.cleared {
		t.Fatalf("status=%d started=%v cleared=%v output=%s", code, f.started, f.cleared, out.String())
	}
}

func TestBenchMedianExcludesHypothesis(t *testing.T) {
	var out bytes.Buffer
	o := testOpts(t, 3, "A", "B")
	rs := []benchRun{{variant: "A", group: "с потолком", seconds: 1, clean: true}, {variant: "A", group: "с потолком", seconds: 1.1, clean: true}, {variant: "A", group: "с потолком", seconds: 8, mark: "гипотеза"}, {variant: "B", group: "с потолком", seconds: 1.2, clean: true}}
	printBenchResults(&out, rs, o)
	s := out.String()
	for _, want := range []string{"медиана 1,05 с", "n=2/3", "выборка неполная"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in %s", want, s)
		}
	}
}

func TestBenchZeroCleanSamplesAreMarkedIncomplete(t *testing.T) {
	var out bytes.Buffer
	o := testOpts(t, 3, "A", "B")
	rs := []benchRun{
		{variant: "A", group: "с потолком", mark: "гипотеза"},
		{variant: "A", group: "с потолком", mark: "гипотеза"},
		{variant: "A", group: "с потолком", mark: "гипотеза"},
	}
	printBenchResults(&out, rs, o)
	if !strings.Contains(out.String(), "итог A/с потолком: нет чистых, n=0/3; выборка неполная") {
		t.Fatalf("zero-clean summaries missing: %s", out.String())
	}
}

func TestBenchPrepTimeoutMustBePositive(t *testing.T) {
	args := []string{"--variant", "A: cat", "--variant", "B: cat", "--prep-timeout", "0"}
	if _, err := parseBenchOptions(args); err == nil {
		t.Fatalf("disabled prep timeout accepted: %v", err)
	}
}

func TestBenchTimeoutStopsScopeAndClearsWindow(t *testing.T) {
	f := baseFake()
	f.waitForever = true
	o := testOpts(t, 1, "A", "B")
	o.runTimeout = 5 * time.Millisecond
	var out bytes.Buffer
	err := runIOBench(context.Background(), &out, f, o, "lock", "dev", 1)
	code, _ := exitStatus(err)
	if code != 2 || f.stopped == 0 || !f.cleared || !f.deadlineSeen || strings.Join(f.sequence, ",") != "stdin,stop,close" {
		t.Fatalf("code=%d stopped=%d cleared=%v deadline=%v sequence=%v out=%s", code, f.stopped, f.cleared, f.deadlineSeen, f.sequence, out.String())
	}
}

func TestBenchForeignQuietWindowIsUntouched(t *testing.T) {
	f := baseFake()
	f.quiet = f.now.Add(time.Minute)
	var out bytes.Buffer
	o := testOpts(t, 1, "A", "B")
	err := runFake(f, o, &out)
	code, _ := exitStatus(err)
	if code != 2 || f.cleared || f.setCalls != 0 || len(f.started) != 0 || !strings.Contains(out.String(), "чужое тихое окно") {
		t.Fatalf("code=%d cleared=%v sets=%d starts=%v output=%s", code, f.cleared, f.setCalls, f.started, out.String())
	}
}

func TestBenchDoesNotClearWindowTakenOverByAnotherOwner(t *testing.T) {
	f := baseFake()
	f.takeover = true
	var out bytes.Buffer
	err := runFake(f, testOpts(t, 1, "A", "B"), &out)
	code, _ := exitStatus(err)
	if code != 2 || f.cleared || f.quiet.IsZero() || len(f.started) != 1 {
		t.Fatalf("code=%d cleared=%v quiet=%v started=%v output=%s", code, f.cleared, f.quiet, f.started, out.String())
	}
}

func TestBenchFileValidationAndFadvisePolicy(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	args := []string{"--variant", "A: cat", "--variant", "B: cat", "--file", missing}
	if _, err := parseBenchOptions(args); err == nil {
		t.Fatal("missing probe accepted")
	}
	path := filepath.Join(dir, "large")
	fh, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := fh.Truncate(64*benchMiB + 1); err != nil {
		t.Fatal(err)
	}
	if err := fh.Close(); err != nil {
		t.Fatal(err)
	}
	args[len(args)-1] = path
	if _, err := parseBenchOptions(args); err == nil {
		t.Fatal("oversized probe accepted")
	}
	f := baseFake()
	var out bytes.Buffer
	if err := runFake(f, testOpts(t, 1, "A", "B"), &out); err != nil {
		t.Fatal(err)
	}
	if f.fadvise != 0 {
		t.Fatalf("fadvise without --file: %d", f.fadvise)
	}
	small := filepath.Join(dir, "small")
	if err := os.WriteFile(small, []byte("probe"), 0600); err != nil {
		t.Fatal(err)
	}
	f = baseFake()
	o := testOpts(t, 1, "A", "B")
	o.file = small
	out.Reset()
	if err := runFake(f, o, &out); err != nil {
		t.Fatal(err)
	}
	if f.fadvise != 2 || !strings.Contains(out.String(), "метаданные ФС не сбрасываются") {
		t.Fatalf("fadvise=%d output=%s", f.fadvise, out.String())
	}
	f = baseFake()
	f.fadviseErr = errors.New("DONTNEED отказан")
	o = testOpts(t, 1, "A", "B")
	o.file = small
	out.Reset()
	err = runFake(f, o, &out)
	code, _ := exitStatus(err)
	if code != 2 || len(f.started) != 0 || !strings.Contains(out.String(), "fadvise") || !f.cleared {
		t.Fatalf("fadvise failure code=%d starts=%v cleared=%v output=%s", code, f.started, f.cleared, out.String())
	}

	invalid := &app{out: &out, benchIO: f}
	f.lockCalls = 0
	args = []string{"--variant", "A: cat", "--variant", "B: cat", "--file", missing}
	out.Reset()
	err = invalid.ioBench(context.Background(), args)
	code, _ = exitStatus(err)
	if code != 2 || f.lockCalls != 0 || !strings.Contains(out.String(), "файл пробы") {
		t.Fatalf("invalid probe code=%d lock calls=%d output=%s", code, f.lockCalls, out.String())
	}
}

func TestBenchNonzeroVariantExitIsReportedAsFailure(t *testing.T) {
	f := baseFake()
	f.scopeCodes = []int{3, 0}
	var out bytes.Buffer
	err := runFake(f, testOpts(t, 1, "A", "B"), &out)
	code, _ := exitStatus(err)
	if code != 2 || !strings.Contains(out.String(), "завершился с кодом 3") || strings.Contains(out.String(), "чистый") {
		t.Fatalf("code=%d output=%s", code, out.String())
	}
}

func TestBenchLockUsesFlockAndDoesNotRemoveHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bench.lock")
	b := realBenchBackend{}
	unlock, err := b.Lock(context.Background(), path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Lock(context.Background(), path, 0); err == nil || !strings.Contains(err.Error(), "идёт другой замер") {
		t.Fatalf("second lock error=%v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("lock file disappeared: %v", err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	unlock2, err := b.Lock(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	if err := unlock2(); err != nil {
		t.Fatal(err)
	}
}

func TestBenchBusyLockReturnsCodeTwoWithoutStarting(t *testing.T) {
	f := baseFake()
	f.lockErr = errors.New("идёт другой замер")
	var out bytes.Buffer
	err := runFake(f, testOpts(t, 1, "A", "B"), &out)
	code, _ := exitStatus(err)
	if code != 2 || f.lockCalls != 1 || f.lockReleased || len(f.started) != 0 || !strings.Contains(out.String(), "идёт другой замер") {
		t.Fatalf("code=%d lock calls=%d released=%v starts=%v output=%s", code, f.lockCalls, f.lockReleased, f.started, out.String())
	}
}

func TestBenchOrderGroupsAndThrottle(t *testing.T) {
	o := testOpts(t, 2, "A", "B")
	o.unthrottled = true
	got := benchOrder(o)
	if len(got) != 8 || got[4].group != "без потолка" || got[4].variant.label != "A" || got[5].variant.label != "B" {
		t.Fatalf("order=%v", got)
	}
	if strings.Contains(strings.Join([]string{got[4].group}, " "), "с потолком") {
		t.Fatal("unthrottled group mislabeled")
	}
	f := baseFake()
	var out bytes.Buffer
	if err := runFake(f, o, &out); err != nil {
		t.Fatal(err)
	}
	if len(f.startUnthrottled) != len(got) {
		t.Fatalf("started %d runs, want %d", len(f.startUnthrottled), len(got))
	}
	for i, unthrottled := range f.startUnthrottled {
		if unthrottled != (i >= 4) {
			t.Fatalf("run %d unthrottled=%v", i, unthrottled)
		}
	}
}

func TestBenchScopeArgvHasThrottleOnlyForCappedRuns(t *testing.T) {
	argv := benchScopeArgv("systemd-run", "/tmp/aguard", "aguard-bench-1-2", "nvme0n1", "30M", false, "cat file")
	joined := strings.Join(argv, "\n")
	for _, want := range []string{"--scope", "--collect", "--property=IOAccounting=yes", "--unit=aguard-bench-1-2", "--property=IOReadBandwidthMax=/dev/nvme0n1 30M", "--property=IOWriteBandwidthMax=/dev/nvme0n1 30M", "io\nbench-hold", "cat file"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv missing %q: %v", want, argv)
		}
	}
	argv = benchScopeArgv("systemd-run", "/tmp/aguard", "unit", "nvme0n1", "30M", true, "cat file")
	for _, arg := range argv {
		if strings.Contains(arg, "BandwidthMax") {
			t.Fatalf("unthrottled argv contains %q", arg)
		}
	}
}

func TestBenchCommandUsesSystemdRunAsExecutable(t *testing.T) {
	argv := benchScopeArgv("systemd-run", "/tmp/aguard", "unit", "nvme0n1", "30M", false, "cat file")
	var executable string
	var args []string
	cmd := newBenchCommand(argv, func(name string, values ...string) *exec.Cmd {
		executable, args = name, values
		return exec.Command("true")
	})
	if executable != "systemd-run" || len(args) == 0 || args[0] != "--user" || cmd == nil {
		t.Fatalf("Command(%q, %v), want systemd-run --user ...", executable, args)
	}
}

func TestBenchCommandEnvironmentRemovesStaleProbe(t *testing.T) {
	got := benchCommandEnv([]string{"PATH=/bin", "AGUARD_BENCH_FILE=stale"}, "")
	if strings.Contains(strings.Join(got, "\n"), "AGUARD_BENCH_FILE=") {
		t.Fatalf("stale probe leaked to child: %v", got)
	}
	got = benchCommandEnv([]string{"AGUARD_BENCH_FILE=stale"}, "/tmp/probe")
	if strings.Join(got, "\n") != "AGUARD_BENCH_FILE=/tmp/probe" {
		t.Fatalf("probe environment=%v", got)
	}
}

func TestBenchDeviceBytesUsesInjectedProcRoot(t *testing.T) {
	root := t.TempDir()
	data := []byte("8 0 sda 10 0 20 0 30 0 40 0\n")
	if err := os.WriteFile(filepath.Join(root, "diskstats"), data, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := (realBenchBackend{procRoot: root}).DeviceBytes("sda")
	if err != nil {
		t.Fatal(err)
	}
	if got != (20+40)*512 {
		t.Fatalf("disk bytes=%d, want %d", got, (20+40)*512)
	}
}

func TestBenchDirtyFloorFlag(t *testing.T) {
	base := []string{"--variant", "A: cat", "--variant", "B: cat"}
	defaults, err := parseBenchOptions(base)
	if err != nil || defaults.dirtyFloor != 4*benchMiB {
		t.Fatalf("default dirty floor=%d, error=%v", defaults.dirtyFloor, err)
	}
	custom, err := parseBenchOptions(append(base, "--dirty-floor", "8M"))
	if err != nil || custom.dirtyFloor != 8*benchMiB {
		t.Fatalf("custom dirty floor=%d, error=%v", custom.dirtyFloor, err)
	}
}

func TestBenchHoldSeparatesChildOutputFromExitCode(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runBenchHold(context.Background(), nil, []string{"--", "sh", "-c", "printf child-output; exit 3"}, &stdout, &stderr, strings.NewReader(""), startBenchChild)
	if code != 0 || stdout.String() != "exit 3\n" || !strings.HasPrefix(stderr.String(), "child-output") || !strings.Contains(stderr.String(), "exit status 3") {
		t.Fatalf("helper=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestBenchReadExitSkipsNonProtocolLines(t *testing.T) {
	code, err := readBenchExit(strings.NewReader("noise from systemd\nexit 3\n"))
	if err != nil || code != 3 {
		t.Fatalf("read exit=%d, error=%v", code, err)
	}
}

type holdGateReader struct {
	entered chan struct{}
	release chan struct{}
}

func (r *holdGateReader) Read([]byte) (int, error) { close(r.entered); <-r.release; return 0, io.EOF }

func TestBenchHoldWaitsForStdinEOF(t *testing.T) {
	var stdout, stderr bytes.Buffer
	r := &holdGateReader{entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan int, 1)
	go func() {
		done <- runBenchHold(context.Background(), nil, []string{"--", "sh", "-c", "exit 3"}, &stdout, &stderr, r, startBenchChild)
	}()
	select {
	case <-r.entered:
	case <-time.After(time.Second):
		t.Fatal("holder не начал читать stdin")
	}
	select {
	case code := <-done:
		t.Fatalf("holder exited before stdin EOF: %d", code)
	default:
	}
	if stdout.String() != "exit 3\n" {
		t.Fatalf("holder protocol output=%q", stdout.String())
	}
	close(r.release)
	if code := <-done; code != 0 {
		t.Fatalf("holder returned %d", code)
	}
}

type fakeBenchChild struct {
	wait   <-chan error
	signal chan os.Signal
}

func (c fakeBenchChild) Wait() error              { return <-c.wait }
func (c fakeBenchChild) Signal(s os.Signal) error { c.signal <- s; return nil }

func TestBenchHoldForwardsSignalAndDoesNotWaitForStdin(t *testing.T) {
	wait := make(chan error, 1)
	forwarded := make(chan os.Signal, 1)
	signals := make(chan os.Signal, 1)
	signals <- os.Interrupt
	stdout := &bytes.Buffer{}
	start := func([]string, io.Writer) (benchChild, error) {
		return fakeBenchChild{wait: wait, signal: forwarded}, nil
	}
	done := make(chan int, 1)
	go func() {
		done <- runBenchHold(context.Background(), signals, []string{"sh", "-c", "fake"}, stdout, io.Discard, strings.NewReader("never read"), start)
	}()
	select {
	case sig := <-forwarded:
		if sig != os.Interrupt {
			t.Fatalf("forwarded %v", sig)
		}
	case <-time.After(time.Second):
		t.Fatal("signal was not forwarded")
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("helper returned %d", code)
		}
	case <-time.After(time.Second):
		t.Fatal("helper waited after signal")
	}
	wait <- errors.New("reaped")
}
