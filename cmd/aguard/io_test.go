package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/activity"
	"github.com/aiseeq/aguard/internal/iowatch"
	"github.com/aiseeq/aguard/internal/policy"
	"github.com/aiseeq/akit/proc"
	"github.com/aiseeq/akit/session"
)

// testIOApp — app без конфига на диске (действует Default) с выводом в буфер.
func testIOApp(t *testing.T) (*app, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	out := &bytes.Buffer{}
	return &app{home: dir, cfgPath: filepath.Join(dir, "нет.toml"), stateDir: filepath.Join(dir, "state"), out: out,
		act: activity.New(filepath.Join(dir, "activity"), time.Now()), log: slog.New(slog.NewTextHandler(io.Discard, nil))}, out
}

// recordFreeze пишет снимок события тем же Recorder, что у службы: секунда тишины с детальной
// записью (буфер до события), затем 3 шага по 60 мс простоя и спокойствие до закрытия.
func recordFreeze(t *testing.T, dir string, base time.Time) iowatch.Event {
	t.Helper()
	cfg := iowatch.Default()
	cfg.Calm = policy.Duration(300 * time.Millisecond)
	r := iowatch.NewRecorder(cfg, dir)
	r.Detail(iowatch.Step{At: base, Processes: []iowatch.Process{{PID: 2, Start: 9, Comm: "dd", WriteBytes: 15 << 20}},
		Cgroups: []iowatch.Cgroup{{Path: "/user.slice/agent-sess7-123.scope", WriteBytes: 15 << 20, WriteOps: 865, IOFullUS: 120_000}}})
	for i := range 20 {
		s := iowatch.Step{At: base.Add(time.Duration(i) * 100 * time.Millisecond), WriteBytes: 1 << 20, IOTicks: 40, WIOS: 1, WMS: 29, InFlight: 12}
		if i >= 10 && i < 13 {
			s.IOFullUS = 60_000
			s.FIOS = 1
		}
		ev, err := r.Step(s)
		if err != nil {
			t.Fatal(err)
		}
		if ev != nil {
			if _, _, err := r.Close(); err != nil { // дождаться дописи файла
				t.Fatal(err)
			}
			return *ev
		}
	}
	t.Fatal("событие не закрылось")
	return iowatch.Event{}
}

func TestAtWindowFormatCgroupFromIOStatAndEventBuffer(t *testing.T) {
	a, out := testIOApp(t)
	dir := t.TempDir()
	base := time.Date(2026, 10, 28, 15, 54, 0, 0, time.Local)
	ev := recordFreeze(t, dir, base)
	if !ev.Start.Equal(base.Add(time.Second)) {
		t.Fatalf("начало события %s", ev.Start)
	}
	if err := a.ioAt(dir, ev.Start, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{
		"15:54:00  io full 0 мс (пик 0)  mem full 0 мс  swapin 0 стр  диск R/W 0/10 МБ  занят 40%  ожид R/W 0/29 мс  очередь 12  flush 0\n", // буфер до события
		"15:54:01  io full 180 мс (пик 60)",
		"flush 3\n",
		"запись процессов: dd(2) 15 МБ\n",
		"запись cgroup: sess7 15 МБ\n",
		"простой IO cgroup: sess7 120 мс\n",
		"операции записи cgroup: sess7 865 оп\n",
		"своп процессов: нет\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("нет %q в выводе:\n%s", want, text)
		}
	}
	if strings.Contains(text, "(процессы)") || strings.Contains(text, "io.stat") {
		t.Errorf("лишняя строка итога:\n%s", text)
	}
	if !regexp.MustCompile(`(?m)^\d\d:\d\d:\d\d  io full `).MatchString(text) {
		t.Errorf("формат строки секунды:\n%s", text)
	}
}

func TestAtManualSnapshotAndNoData(t *testing.T) {
	a, out := testIOApp(t)
	dir := t.TempDir()
	r := iowatch.NewRecorder(iowatch.Default(), dir)
	at := time.Now().Truncate(time.Second)
	if _, err := r.Step(iowatch.Step{At: at, IOFullUS: 20_000}); err != nil {
		t.Fatal(err)
	}
	snap := r.Dump(at.Add(2 * time.Minute)) // время в имени — момент снимка, строки — буфер до него
	if err := snap.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := a.ioAt(dir, at, time.Minute); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "io full 20 мс") {
		t.Fatalf("ручной снимок не прочитан:\n%s", out.String())
	}
	out.Reset()
	if err := a.ioAt(dir, at.Add(-24*time.Hour), time.Minute); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "нет данных в окне\nближайший снимок: ") {
		t.Fatalf("пустое окно:\n%s", out.String())
	}
}

func TestAtTimeUsesLocalTimezone(t *testing.T) {
	now := time.Date(2026, 10, 28, 23, 10, 0, 0, time.FixedZone("MSK", 3*3600))
	got, err := parseIOTime("15:04:05", now)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 10, 28, 15, 4, 5, 0, now.Location()); !got.Equal(want) {
		t.Fatalf("время %s, хотели %s", got, want)
	}
	if _, err := parseIOTime("вчера", now); err == nil {
		t.Fatal("непонятное время — должна быть ошибка")
	}
}

func TestFreezesOutputWithCulprits(t *testing.T) {
	a, out := testIOApp(t)
	dir := t.TempDir()
	base := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	recordFreeze(t, dir, base)
	if err := a.ioFreezes(dir, base.Add(-time.Minute), base.Add(time.Minute), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	head := base.Add(time.Second).Format("2006-01-02 15:04:05.000") + " — " + base.Add(1200*time.Millisecond).Format("15:04:05.000") +
		"  длительность 300 мс, простой 180 мс, пик 60 мс\n"
	if !strings.HasPrefix(text, head) || !strings.Contains(text, "запись процессов: dd(2) 15 МБ\n") {
		t.Fatalf("вывод freezes:\n%s", text)
	}
	out.Reset()
	if err := a.ioFreezes(dir, base.Add(-time.Minute), base.Add(time.Minute), 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if out.String() != "фризов нет\n" {
		t.Fatalf("порог выше пика:\n%s", out.String())
	}
}

func TestAnalyzeCommandResolution(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "bin", "aguard-io-test-worker")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got, err := findIOCommand("aguard-io-test-worker", dir); err != nil || got != path {
		t.Fatalf("команда %q, err %v", got, err)
	}
	if _, err := findIOCommand("aguard-io-test-нет", dir); err == nil {
		t.Fatal("нет команды — должна быть ошибка")
	}
}

func TestAnalyzePromptArgsAndRecipients(t *testing.T) {
	if got := ioPromptArgs([]string{"worker", "--msg", "до {prompt} после"}, "диагноз"); len(got) != 3 || got[2] != "до диагноз после" {
		t.Fatalf("подстановка: %#v", got)
	}
	if got := ioPromptArgs([]string{"worker", "ask"}, "диагноз"); len(got) != 3 || got[2] != "диагноз" {
		t.Fatalf("без {prompt}: %#v", got)
	}
	if got := strings.Join(ioRecipients("lead", "sess7"), ","); got != "lead,sess7" {
		t.Fatalf("адресаты: %s", got)
	}
	if got := strings.Join(ioRecipients("lead", "lead"), ","); got != "lead" {
		t.Fatalf("лид — один раз: %s", got)
	}
}

// 10 событий по 15 минут с простоем в каждом шаге: промпт режется по строкам до 64 КБ, список событий цел.
func TestAnalyzePromptLimitTenLongEvents(t *testing.T) {
	a, _ := testIOApp(t)
	dir := t.TempDir()
	base := time.Now().Add(-4 * time.Hour).Truncate(time.Second)
	events := make([]iowatch.Event, 10)
	for i := range events {
		start := base.Add(time.Duration(i) * 20 * time.Minute)
		path := filepath.Join(dir, iowatch.EventName(start))
		var buf bytes.Buffer
		for s := range 15 * 60 {
			at := start.Add(time.Duration(s) * time.Second)
			if err := iowatch.WriteRecord(&buf, iowatch.Record{Kind: iowatch.KindStep, Step: iowatch.Step{At: at, IOFullUS: 60_000, WriteBytes: 1 << 20, WIOS: 3, WMS: 30}}); err != nil {
				t.Fatal(err)
			}
			if err := iowatch.WriteRecord(&buf, iowatch.Record{Kind: iowatch.KindDetail, Step: iowatch.Step{At: at,
				Processes: []iowatch.Process{{PID: 7, Comm: "dd", WriteBytes: 1 << 20}}, Cgroups: []iowatch.Cgroup{{Path: "/user.slice/agent-sess7-123.scope", WriteBytes: 1 << 20}}}}); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		events[i] = iowatch.Event{Start: start, End: start.Add(15*time.Minute - time.Second), IdleUS: int64(i+1) * 1_000_000, PeakUS: 60_000, Path: path}
	}
	in, err := a.ioAnalysisInput(dir, iowatch.Default(), events, events[9])
	if err != nil {
		t.Fatal(err)
	}
	if len(in.prompt) > ioPromptLimit || len(in.prompt) < ioPromptLimit-1024 {
		t.Fatalf("промпт %d байт", len(in.prompt))
	}
	if !strings.HasSuffix(in.prompt, "\n") || strings.Count(in.prompt, "главный виновник: cgroup sess7") != 10 || in.owner != "sess7" || in.cause != "io" {
		t.Fatalf("промпт: владелец %q, причина %q, начало:\n%.2000s", in.owner, in.cause, in.prompt)
	}
}

// writeAnalyzer — команда разбора: аргументы — в файл, на выход — ответ.
func writeAnalyzer(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "analyzer")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAnalyzeLogModeWritesInsteadOfSending(t *testing.T) {
	a, _ := testIOApp(t)
	var logged bytes.Buffer
	a.log = slog.New(slog.NewTextHandler(&logged, nil))
	dir := t.TempDir()
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	ev := recordFreeze(t, dir, base)
	argsFile := filepath.Join(t.TempDir(), "args")
	cfg := iowatch.Default()
	cfg.Mode, cfg.Lead = iowatch.ModeLog, "lead"
	cfg.Analyze = []string{writeAnalyzer(t, `printf '%s' "$2" > "$1"; echo "виновник dd в sess7"`), argsFile, "{prompt}"}
	if err := a.analyzeIO(context.Background(), cfg, dir, []iowatch.Event{ev}); err != nil {
		t.Fatal(err)
	}
	prompt, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(prompt), ioAnalyzePrompt) || !strings.Contains(string(prompt), "запись процессов: dd(2) 15 МБ") {
		t.Fatalf("промпт:\n%s", prompt)
	}
	text := logged.String()
	if !strings.Contains(text, "написал бы") || !strings.Contains(text, "to=lead,sess7") || !strings.Contains(text, "виновник dd в sess7") {
		t.Fatalf("журнал:\n%s", text)
	}
}

func TestAnalyzeTimeoutKillsDescendantHoldingOutput(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep недоступен")
	}
	path := writeAnalyzer(t, "sleep 600 &\nsleep 600\n")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := runIOAnalyzer(ctx, path)
	if err == nil || time.Since(started) > 3*time.Second {
		t.Fatalf("таймаут не остановил дерево процессов: err=%v, время=%s", err, time.Since(started))
	}
}

func TestAnalyzeMinimumAndPausedEventsAccumulate(t *testing.T) {
	a, _ := testIOApp(t)
	var logged bytes.Buffer
	a.log = slog.New(slog.NewTextHandler(&logged, nil))
	s := newIOService(context.Background(), a)
	s.cfg = iowatch.Default()
	s.cfg.Mode, s.cfg.Lead = iowatch.ModeLog, "lead"
	s.cfg.AnalyzeMin = policy.Duration(100 * time.Millisecond)
	s.cfg.AnalyzeEvery = policy.Duration(time.Hour)
	s.cfg.Analyze = []string{writeAnalyzer(t, "echo ok\n")}
	base := time.Now().Truncate(time.Millisecond)
	event := func(at time.Time, idle int64) iowatch.Event {
		return iowatch.Event{Start: at, End: at, IdleUS: idle, PeakUS: idle, Path: filepath.Join(s.dir, iowatch.EventName(at))}
	}
	s.closed(event(base, 99_000))
	if len(s.pending) != 0 {
		t.Fatalf("событие ниже analyze_min в очереди: %+v", s.pending)
	}
	s.analyzed = base
	s.closed(event(base.Add(time.Second), 100_000))
	s.closed(event(base.Add(2*time.Second), 150_000))
	s.launchAnalysis(base.Add(3 * time.Second))
	if len(s.pending) != 2 || s.analyzing {
		t.Fatalf("пауза не сохранила события: pending=%d analyzing=%v", len(s.pending), s.analyzing)
	}
	s.analyzed = base.Add(-2 * time.Hour)
	s.launchAnalysis(base.Add(3 * time.Second))
	if len(s.pending) != 0 || !s.analyzing {
		t.Fatalf("накопленные события не запущены: pending=%d analyzing=%v", len(s.pending), s.analyzing)
	}
	s.launchAnalysis(base.Add(4 * time.Second)) // второй разбор, пока идёт первый, не запускается
	select {
	case f := <-s.extra:
		f()
	case <-time.After(5 * time.Second):
		t.Fatal("разбор не завершился")
	}
	if s.analyzing || !strings.Contains(logged.String(), "написал бы") || strings.Contains(logged.String(), "разбор не удался") {
		t.Fatalf("итог разбора: analyzing=%v, журнал:\n%s", s.analyzing, logged.String())
	}
}

// Событие на 8 минут: секунды не влезают в 64 КБ, но итог виновников в промпте есть — режутся только
// секунды (проба ревью rv2).
func TestAnalyzeLongEventPromptKeepsCulprits(t *testing.T) {
	a, _ := testIOApp(t)
	dir := t.TempDir()
	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	var buf bytes.Buffer
	for s := range 8 * 60 {
		at := start.Add(time.Duration(s) * time.Second)
		for k := range 10 {
			if err := iowatch.WriteRecord(&buf, iowatch.Record{Kind: iowatch.KindStep, Step: iowatch.Step{At: at.Add(time.Duration(k) * 100 * time.Millisecond), IOFullUS: 60_000, WriteBytes: 1 << 20, WIOS: 3, WMS: 30}}); err != nil {
				t.Fatal(err)
			}
		}
		if err := iowatch.WriteRecord(&buf, iowatch.Record{Kind: iowatch.KindDetail, Step: iowatch.Step{At: at,
			Processes: []iowatch.Process{{PID: 7, Comm: "dd", WriteBytes: 1 << 20}}, Cgroups: []iowatch.Cgroup{{Path: "/user.slice/agent-sess7-123.scope", WriteBytes: 1 << 20}}}}); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, iowatch.EventName(start))
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	ev := iowatch.Event{Start: start, End: start.Add(8*time.Minute - time.Second), IdleUS: 28_000_000, PeakUS: 60_000, Path: path}
	in, err := a.ioAnalysisInput(dir, iowatch.Default(), []iowatch.Event{ev}, ev)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.prompt) > ioPromptLimit || !strings.Contains(in.prompt, "запись процессов: dd(7) ") || !strings.Contains(in.prompt, "запись cgroup: sess7 ") ||
		strings.Count(in.prompt, "  io full ") < 100 {
		t.Fatalf("промпт %d байт, строк секунд %d, начало:\n%.3000s", len(in.prompt), strings.Count(in.prompt, "  io full "), in.prompt)
	}
}

// Сессия-виновник закрыта к моменту письма: лиду ушло, это не провал разбора.
func TestAnalyzeClosedCulpritSessionIsNotFailure(t *testing.T) {
	a, _ := testIOApp(t)
	var logged bytes.Buffer
	a.log = slog.New(slog.NewTextHandler(&logged, nil))
	dir := t.TempDir()
	ev := recordFreeze(t, dir, time.Now().Add(-time.Hour).Truncate(time.Second))
	cfg := iowatch.Default()
	cfg.Mode, cfg.Lead = iowatch.ModeAlert, "lead"
	cfg.Analyze = []string{writeAnalyzer(t, "echo ok\n")}
	got := liveLead(t, a) // лид жив (сессия — этот процесс), sess7 в реестре нет
	if err := a.analyzeIO(context.Background(), cfg, dir, []iowatch.Event{ev}); err != nil {
		t.Fatal(err)
	}
	if msg := <-got; !strings.Contains(msg, "aguard io: фриз") || !strings.Contains(logged.String(), "сессия-виновник уже закрыта") ||
		strings.Contains(logged.String(), "level=WARN") {
		t.Fatalf("письмо лиду %.200q, журнал:\n%s", msg, logged.String())
	}
}

// liveLead — сессия lead в реестре a: pid этого процесса и абстрактный сокет; канал отдаёт принятое письмо.
func liveLead(t *testing.T, a *app) <-chan string {
	t.Helper()
	st, err := proc.ReadStat("/proc", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	sock := fmt.Sprintf("@aguard-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	a.reg = session.Registry{Dir: filepath.Join(t.TempDir(), "sessions"), ProcRoot: "/proc"}
	info, err := json.Marshal(session.Info{Pid: os.Getpid(), ProcStart: st.Start, Name: "lead", SocketPath: sock})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(a.reg.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.reg.Dir, strconv.Itoa(os.Getpid())+".json"), info, 0o600); err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			got <- err.Error()
			return
		}
		defer conn.Close()
		b, err := io.ReadAll(conn)
		if err != nil {
			got <- err.Error()
			return
		}
		got <- string(b)
	}()
	return got
}

// Нет диска: предупреждение один раз, повтор — через сутки.
func TestAnalyzeNoDeviceWarnsDaily(t *testing.T) {
	a, _ := testIOApp(t)
	var logged bytes.Buffer
	a.log = slog.New(slog.NewTextHandler(&logged, nil))
	s := newIOService(context.Background(), a)
	now := time.Now()
	for i := range 3 {
		s.warnEvery(now.Add(time.Duration(i)*time.Hour), "device", ioNoDeviceEvery, "нет диска", iowatch.ErrNoDevice)
	}
	s.warnEvery(now.Add(25*time.Hour), "device", ioNoDeviceEvery, "нет диска", iowatch.ErrNoDevice)
	if n := strings.Count(logged.String(), "level=WARN"); n != 2 {
		t.Fatalf("предупреждений %d:\n%s", n, logged.String())
	}
}
