package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/cpuwatch"
	"github.com/aiseeq/akit/pressure"
)

func spinnerRoot(t *testing.T) string { t.Helper(); return t.TempDir() }

func spinnerDetails(t *testing.T, root string, pid int, master string, io string) {
	t.Helper()
	dir := filepath.Join(root, fmt.Sprint(pid))
	fields := make([]string, 20)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0] = "S"
	fields[19] = "1"
	if err := os.MkdirAll(filepath.Join(dir, "task", "1"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stat"), fmt.Appendf(nil, "%d (pipe-reader) %s\n", pid, strings.Join(fields, " ")), 0600); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		"cgroup":  "0::/user.slice/session-" + master + ".scope\n",
		"environ": "CLAUDE_PID=0\x00",
		"cmdline": "pipe-reader --input token=do-not-leak\x00",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, path)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, path), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if io != "" {
		if err := os.WriteFile(filepath.Join(dir, "io"), []byte(io), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func spinnerProc(pid int, start int64, cpu float64, age time.Duration, now time.Time) cpuwatch.Process {
	return cpuwatch.Process{Task: cpuwatch.Task{Pid: pid, Comm: "pipe-reader"}, Start: start, CPU: cpu, Age: age, StartedAt: now.Add(-age)}
}

func spinnerSetStart(t *testing.T, root string, pid int, start int64) {
	t.Helper()
	path := filepath.Join(root, fmt.Sprint(pid), "stat")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Fields(string(b))
	f[len(f)-1] = fmt.Sprint(start)
	if err := os.WriteFile(path, []byte(strings.Join(f, " ")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func spinnerOwner(t *cpuwatch.Task) cpuwatch.Owner {
	if strings.Contains(t.Cgroup, "alpha-master") {
		return cpuwatch.Owner{Master: "alpha-master", Kind: cpuwatch.KindSession}
	}
	if strings.Contains(t.Cgroup, "beta-master") {
		return cpuwatch.Owner{Master: "beta-master", Kind: cpuwatch.KindSession}
	}
	return cpuwatch.Owner{}
}

func TestSpinnerFirstPassAgeCPUAndIO(t *testing.T) {
	cfg := cpuwatch.Default("")
	now := time.Date(2026, 11, 12, 3, 46, 0, 0, time.UTC)
	root := spinnerRoot(t)
	spinnerDetails(t, root, 101, "alpha-master", "rchar: 15360\nwchar: 0\n") // чтение из pipe учитывается в rchar
	spinnerDetails(t, root, 102, "alpha-master", "rchar: 100\nwchar: 1048576\n")
	spinnerDetails(t, root, 103, "alpha-master", "") // отсутствующий io — неизвестный, не нулевой
	spinnerDetails(t, root, 105, "alpha-master", "rchar: 1\nwchar: 0\n")
	spinnerDetails(t, root, 106, "alpha-master", "rchar: 1048576\nwchar: 0\n")
	var w spinnerWatch
	ps := []cpuwatch.Process{
		spinnerProc(101, 1, 540, 12*time.Minute, now), // первый замер: 75% ниже порога
		spinnerProc(102, 1, 700, 12*time.Minute, now),
		spinnerProc(103, 1, 700, 12*time.Minute, now),
		spinnerProc(104, 1, 0, 12*time.Minute, now),
		spinnerProc(105, 1, 600, 9*time.Minute, now),  // моложе порога, несмотря на высокий CPU
		spinnerProc(106, 1, 700, 12*time.Minute, now), // rchar один достигает порога IO
	}
	got, err := w.find(now, root, ps, spinnerOwner, &cfg)
	if err != nil {
		t.Fatalf("missing IO is an ordinary proc race: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("low CPU, high wchar, unknown IO named: %+v", got)
	}
	ps[0].CPU = 612 // рост между проходами превышает порог; возраст взят из времени старта
	ps[1].CPU += 50
	ps[2].CPU += 50
	got, err = w.find(now.Add(time.Minute), root, ps, spinnerOwner, &cfg)
	if err != nil {
		t.Fatalf("missing IO should be skipped quietly: %v", err)
	}
	if len(got) != 1 || got[0].Pid != 101 || got[0].Rchar != 15360 || got[0].Wchar != 0 {
		t.Fatalf("pipe reader should be named: %+v", got)
	}
	v := cpuwatch.Verdict{Spinners: got}
	m := cpuwatch.Master{Name: "alpha-master"}
	letter := cpuwatch.MasterMessage("ноут", &v, &m, now.Add(time.Minute), true)
	for _, want := range []string{"pid 101", "работает 12 мин (с 03:34)", "15 КБ", "wchar 0 Б", "token=***", "ничего не останавливает"} {
		if !strings.Contains(letter, want) {
			t.Errorf("letter lacks %q: %s", want, letter)
		}
	}
	if strings.Count(letter, "aguard ничего не останавливает") != 1 {
		t.Fatalf("advice repeated in lines: %s", letter)
	}
}

func TestSpinnerFirstObservationUsesLifetimeCPU(t *testing.T) {
	cfg := cpuwatch.Default("")
	now := time.Date(2026, 11, 12, 3, 46, 0, 0, time.UTC)
	root := spinnerRoot(t)
	spinnerDetails(t, root, 150, "alpha-master", "rchar: 15360\nwchar: 0\n")
	var w spinnerWatch
	got, err := w.find(now, root, []cpuwatch.Process{spinnerProc(150, 1, 612, 12*time.Minute, now)}, spinnerOwner, &cfg)
	if err != nil || len(got) != 1 || got[0].Pid != 150 {
		t.Fatalf("first observation should use lifetime CPU and kernel age: %+v, %v", got, err)
	}
}

func TestSpinnerUpdatesDeltaOnEveryPass(t *testing.T) {
	cfg := cpuwatch.Default("")
	root := spinnerRoot(t)
	start := time.Date(2026, 11, 12, 3, 0, 0, 0, time.UTC)
	spinnerDetails(t, root, 160, "alpha-master", "rchar: 15\nwchar: 0\n")
	var w spinnerWatch
	for i, cpu := range []float64{0, 30, 80} {
		now := start.Add(time.Duration(i) * time.Minute)
		got, err := w.find(now, root, []cpuwatch.Process{spinnerProc(160, 1, cpu, 12*time.Minute, now)}, spinnerOwner, &cfg)
		if err != nil {
			t.Fatal(err)
		}
		if i < 2 && len(got) != 0 {
			t.Fatalf("early pass %d named process: %+v", i, got)
		}
		if i == 2 && (len(got) != 1 || got[0].Pid != 160) {
			t.Fatalf("third pass delta was lost: %+v", got)
		}
	}
}

func TestSpinnerLongGapFallsBackToLifetimeRate(t *testing.T) {
	cfg := cpuwatch.Default("")
	root := spinnerRoot(t)
	start := time.Date(2026, 11, 12, 3, 0, 0, 0, time.UTC)
	spinnerDetails(t, root, 161, "alpha-master", "rchar: 15\nwchar: 0\n")
	var w spinnerWatch
	first := spinnerProc(161, 1, 500, 12*time.Minute, start)
	if got, _ := w.find(start, root, []cpuwatch.Process{first}, spinnerOwner, &cfg); len(got) != 0 {
		t.Fatalf("first rate below threshold: %+v", got)
	}
	late := start.Add(3 * time.Minute)
	second := spinnerProc(161, 1, 600, 12*time.Minute, late)
	got, err := w.find(late, root, []cpuwatch.Process{second}, spinnerOwner, &cfg)
	if err != nil || len(got) != 1 {
		t.Fatalf("long gap should use CPU/age, got %+v err=%v", got, err)
	}
}

func TestSpinnerExpectedIOErrorsDoNotWarn(t *testing.T) {
	cfg := cpuwatch.Default("")
	root := spinnerRoot(t)
	now := time.Now()
	spinnerDetails(t, root, 165, "alpha-master", "rchar: 10\nwchar: 0\n")
	var w spinnerWatch
	ps := []cpuwatch.Process{spinnerProc(162, 1, 600, 12*time.Minute, now), spinnerProc(165, 2, 600, 12*time.Minute, now)} // Gone и переиспользованный PID
	got, err := w.find(now, root, ps, spinnerOwner, &cfg)
	if err != nil || len(got) != 0 {
		t.Fatalf("gone candidate should be skipped quietly: %+v %v", got, err)
	}
}

func TestSpinnerMalformedEnvironIsReportedWithoutStoppingScan(t *testing.T) {
	cfg := cpuwatch.Default("")
	root := spinnerRoot(t)
	now := time.Now()
	spinnerDetails(t, root, 163, "alpha-master", "rchar: 10\nwchar: 0\n")
	if err := os.WriteFile(filepath.Join(root, "163", "environ"), []byte("CLAUDE_PID=not-a-pid\x00"), 0600); err != nil {
		t.Fatal(err)
	}
	spinnerDetails(t, root, 164, "alpha-master", "rchar: 10\nwchar: 0\n")
	var w spinnerWatch
	ps := []cpuwatch.Process{spinnerProc(163, 1, 600, 12*time.Minute, now), spinnerProc(164, 1, 600, 12*time.Minute, now)}
	got, err := w.find(now, root, ps, spinnerOwner, &cfg)
	if err == nil {
		t.Fatal("malformed CLAUDE_PID was not reported")
	}
	if len(got) != 1 || got[0].Pid != 164 {
		t.Fatalf("one bad environment stopped rest of scan: %+v", got)
	}
}

func TestSpinnerCPUAndPIDIdentityAndDeath(t *testing.T) {
	cfg := cpuwatch.Default("")
	root := spinnerRoot(t)
	now := time.Date(2026, 11, 12, 4, 0, 0, 0, time.UTC)
	spinnerDetails(t, root, 201, "alpha-master", "rchar: 1\nwchar: 0\n")
	var w spinnerWatch
	old := spinnerProc(201, 11, 0, 12*time.Minute, now)
	if got, _ := w.find(now, root, []cpuwatch.Process{old}, spinnerOwner, &cfg); len(got) != 0 {
		t.Fatalf("zero CPU named: %+v", got)
	}
	// Новый starttime при том же PID начинает расчёт заново по cputime/возрасту.
	reused := spinnerProc(201, 22, 60, 12*time.Minute, now.Add(time.Minute))
	spinnerSetStart(t, root, 201, 22)
	if got, _ := w.find(now.Add(time.Minute), root, []cpuwatch.Process{reused}, spinnerOwner, &cfg); len(got) != 0 {
		t.Fatalf("reused pid inherited old CPU: %+v", got)
	}
	if _, ok := w["201:11"]; ok {
		t.Fatal("old PID/start identity retained")
	}
	if _, ok := w["201:22"]; !ok {
		t.Fatal("new PID/start identity not recorded")
	}
	if _, err := w.find(now.Add(2*time.Minute), root, nil, spinnerOwner, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(w) != 0 {
		t.Fatalf("dead process remains in state: %+v", w)
	}
}

func TestSpinnerDeltaOwnerFilteringAndLetterLimit(t *testing.T) {
	cfg := cpuwatch.Default("")
	root := spinnerRoot(t)
	now := time.Date(2026, 11, 12, 5, 0, 0, 0, time.UTC)
	var w spinnerWatch
	var ps []cpuwatch.Process
	for i := 1; i <= 7; i++ {
		pid := 300 + i
		master := "alpha-master"
		if i == 7 {
			master = "beta-master"
		}
		spinnerDetails(t, root, pid, master, "rchar: 15360\nwchar: 0\n")
		ps = append(ps, spinnerProc(pid, 1, 0, 12*time.Minute, now))
	}
	w.find(now, root, ps, spinnerOwner, &cfg)
	for i := range ps {
		ps[i].CPU = 50
	}
	got, err := w.find(now.Add(time.Minute), root, ps, spinnerOwner, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 7 {
		t.Fatalf("busy delta should qualify all: %+v", got)
	}
	letter := cpuwatch.MasterMessage("ноут", &cpuwatch.Verdict{Spinners: got}, &cpuwatch.Master{Name: "alpha-master"}, now, true)
	if strings.Contains(letter, "pid 307") || !strings.Contains(letter, "ещё 1 долгий процесс") || strings.Count(letter, "Долгий процесс без") != 5 {
		t.Fatalf("master filtering/limit: %s", letter)
	}
}

func spinnerMachine(t *testing.T) (*app, *cpuHost, cpuwatch.Config, time.Time, *bytes.Buffer, *strings.Builder) {
	t.Helper()
	var logs strings.Builder
	a := cpuApp(t, &logs)
	output := &bytes.Buffer{}
	a.out = output
	root := t.TempDir()
	now := time.Now()
	boot := now.Add(-time.Hour).Unix()
	fields := make([]string, 20)
	for i := range fields {
		fields[i] = "0"
	}
	fields[11] = "300000"
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "stat"), fmt.Sprintf("btime %d\ncpu0 1 2 3\nprocs_running 2\n", boot))
	write(filepath.Join(root, "loadavg"), "1.00 1.00 1.00 2/100 100\n")
	dir := filepath.Join(root, "500")
	write(filepath.Join(dir, "stat"), "500 (pipe-reader) "+strings.Join(fields, " ")+"\n")
	write(filepath.Join(dir, "comm"), "pipe-reader\n")
	write(filepath.Join(dir, "cgroup"), "0::/user.slice/user-1000.slice/agent-alpha-master-123.scope\n")
	write(filepath.Join(dir, "environ"), "CLAUDE_PID=0\x00")
	write(filepath.Join(dir, "cmdline"), "\x00")
	write(filepath.Join(dir, "io"), "rchar: 15360\nwchar: 0\n")
	write(filepath.Join(dir, "task", "501", "stat"), "501 (pipe-reader) R 1 2 3\n")
	h := &cpuHost{root: root, state: filepath.Join(t.TempDir(), "local.json"), win: cpuwatch.Host{Name: cpuwatch.HostLocal}}
	cfg := cpuwatch.Default(a.home)
	cfg.Mode = cpuwatch.ModeLog
	cfg.Lead = "lead"
	cfg.Hosts = nil
	h.win.Every = cfg.Interval.D()
	a.cpu = map[string]*cpuHost{"": h}
	a.strayRead = func(context.Context, *cpuHost) ([]byte, error) { return []byte("##procs\n##strays\n##end\n"), nil }
	return a, h, cfg, now, output, &logs
}

func TestSpinnerReportGeneratedByCPURound(t *testing.T) {
	a, h, cfg, now, _, logs := spinnerMachine(t)
	for i := 11; i >= 1; i-- {
		at := now.Add(-time.Duration(i) * time.Minute)
		h.win.Judge(&cpuwatch.Sample{At: at, Load: pressure.LoadAvg{Avg1: 1}, Threads: 1}, func(*cpuwatch.Task) cpuwatch.Owner { return cpuwatch.Owner{Master: "alpha-master"} })
	}
	if err := a.cpuRound(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "pid 500") || !strings.Contains(logs.String(), "15 КБ") || !strings.Contains(logs.String(), "команда `pipe-reader`") {
		t.Fatalf("cpuRound letter did not carry candidate: %s", logs.String())
	}
}

func TestCpuDryShowsSpinnerFromFirstPass(t *testing.T) {
	a, _, cfg, _, out, _ := spinnerMachine(t)
	if err := a.cpuDry(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "pid 500") || !strings.Contains(out.String(), "15 КБ") {
		t.Fatalf("dry output omits long process: %s", out.String())
	}
}

// Необычная ошибка io (битый файл) — в предупреждение, а не молча: обычные — только Gone, EACCES, переиспользованный pid.
func TestSpinnerMalformedIOIsReportedWithoutStoppingScan(t *testing.T) {
	cfg := cpuwatch.Default("")
	root := spinnerRoot(t)
	now := time.Now()
	spinnerDetails(t, root, 166, "alpha-master", "rchar: битое\nwchar: 0\n")
	spinnerDetails(t, root, 167, "alpha-master", "rchar: 10\nwchar: 0\n")
	var w spinnerWatch
	ps := []cpuwatch.Process{spinnerProc(166, 1, 600, 12*time.Minute, now), spinnerProc(167, 1, 600, 12*time.Minute, now)}
	got, err := w.find(now, root, ps, spinnerOwner, &cfg)
	if err == nil || !strings.Contains(err.Error(), "pid 166") {
		t.Fatalf("битый io не попал в предупреждение: %v", err)
	}
	if len(got) != 1 || got[0].Pid != 167 {
		t.Fatalf("битый io одного процесса остановил обход: %+v", got)
	}
}
