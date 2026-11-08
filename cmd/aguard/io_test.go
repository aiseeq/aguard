package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/activity"
	"github.com/aiseeq/aguard/internal/ioroute"
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

// delaySink держит цикл на первом шаге и запоминает время шагов.
type delaySink struct {
	delay  time.Duration
	at     []time.Time
	enough func()
}

func (s *delaySink) step(st iowatch.Step) {
	s.at = append(s.at, st.At)
	if len(s.at) == 1 {
		time.Sleep(s.delay)
	}
	if len(s.at) == 2 {
		s.enough()
	}
}
func (s *delaySink) detail(iowatch.Step)           {}
func (s *delaySink) sampleErr(_ string, err error) { panic(err) }

// Цикл задержан дольше шага: буфер тикера отдаёт старый тик, а время шага — момент чтения, иначе простой
// за всю задержку лёг бы на 100 мс и случай вышел бы короче стены.
func TestIOSamplingStepTimeIsReadTime(t *testing.T) {
	dir := t.TempDir()
	proc, block := filepath.Join(dir, "proc"), filepath.Join(dir, "block")
	for name, body := range map[string]string{
		"pressure/io":     "some avg10=0 total=1\nfull avg10=0 total=1\n",
		"pressure/memory": "some avg10=0 total=1\nfull avg10=0 total=1\n",
		"pressure/cpu":    "some avg10=0 total=1\n",
		"loadavg":         "1.00 2.00 3.00 1/5201 77\n",
		"vmstat":          "pswpin 0\npswpout 0\npgmajfault 0\n",
		"diskstats":       "259 0 nvme0n1 0 0 0 0 0 0 0 0 0 0 0\n",
	} {
		path := filepath.Join(proc, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(block, "nvme0n1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(block, "nvme0n1", "dev"), []byte("259:0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := iowatch.Default()
	cfg.Device, cfg.Step, cfg.Detail = "nvme0n1", policy.Duration(20*time.Millisecond), policy.Duration(time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const delay = 300 * time.Millisecond
	sink := &delaySink{delay: delay, enough: cancel}
	err := runIOSampling(ctx, cfg, iowatch.Roots{Proc: proc, Cgroup: filepath.Join(dir, "cgroup"), Block: block}, sink, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("цикл: %v", err)
	}
	if len(sink.at) < 2 || sink.at[1].Sub(sink.at[0]) < delay {
		t.Fatalf("второй шаг после задержки %v: времена %v", delay, sink.at)
	}
}

// recordFreeze — тот же Recorder, что у службы: секунда тишины с детальной записью, 3 шага по 60 мс
// простоя и спокойствие, затем ручной снимок буфера в dir. Отдаёт случай.
func recordFreeze(t *testing.T, dir string, base time.Time) iowatch.Case {
	t.Helper()
	cfg := iowatch.Default()
	cfg.CaseMin = policy.Duration(200 * time.Millisecond)
	r := iowatch.NewRecorder(cfg)
	var cases []iowatch.Case
	for i := range 30 {
		at := base.Add(time.Duration(i) * 100 * time.Millisecond)
		if i%10 == 0 {
			r.Detail(iowatch.Step{At: at, Processes: []iowatch.Process{{PID: 2, Start: 9, Comm: "dd", WriteBytes: 15 << 20}},
				Cgroups: []iowatch.Cgroup{{Path: "/user.slice/agent-sess7-123.scope", WriteBytes: 15 << 20, WriteOps: 865, IOFullUS: 120_000}}})
		}
		s := iowatch.Step{At: at, WriteBytes: 1 << 20, IOTicks: 40, WIOS: 1, WMS: 29, InFlight: 12}
		if i >= 10 && i < 13 {
			s.IOFullUS = 60_000
			s.FIOS = 1
		}
		cases = append(cases, r.Step(s)...)
	}
	if err := r.Dump(dir, base.Add(3*time.Second)).Wait(); err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 {
		t.Fatalf("случаев %d", len(cases))
	}
	return cases[0]
}

func TestAtWindowFormatCgroupFromIOStatAndManualSnapshot(t *testing.T) {
	a, out := testIOApp(t)
	dir := t.TempDir()
	base := time.Date(2026, 10, 28, 15, 54, 0, 0, time.Local)
	c := recordFreeze(t, dir, base)
	if !c.Start.Equal(base.Add(900*time.Millisecond)) || c.DurMS != 300 {
		t.Fatalf("случай: %+v", c)
	}
	if err := a.ioAt(dir, c.End, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{
		"15:54:00  io full 0 мс (пик 0)  mem full 0 мс  swapin 0 стр  диск R/W 0/10 МБ  занят 40%  ожид R/W 0/29 мс  очередь 12  flush 0\n",
		"15:54:01  io full 180 мс (пик 60)",
		"flush 3\n",
		"запись процессов: dd(2) 45 МБ\n",
		"запись cgroup: sess7 45 МБ\n",
		"простой IO cgroup: sess7 360 мс\n",
		"операции записи cgroup: sess7 2595 оп\n",
		"своп процессов: нет\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("нет %q в выводе:\n%s", want, text)
		}
	}
	if !regexp.MustCompile(`(?m)^\d\d:\d\d:\d\d  io full `).MatchString(text) {
		t.Errorf("формат строки секунды:\n%s", text)
	}
	out.Reset()
	if err := a.ioAt(dir, base.Add(-24*time.Hour), time.Minute); err != nil {
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

// alertCase — случай от alert_min с виновником sess7.
func alertCase(at time.Time, ms int64) iowatch.Case {
	return iowatch.Case{Start: at, End: at.Add(time.Duration(ms) * time.Millisecond), DurMS: ms, IdleMS: ms * 3 / 4, PeakMS: 99,
		IOFullMS: ms * 3 / 4, SwapIn: 1200, WriteB: 300 << 20, WAwaitMS: 40, InFlight: 30,
		Culprits: &iowatch.Culprit{From: at.Add(-5 * time.Second), Main: "/user.slice/agent-sess7-1.scope", MainProc: "dd",
			ProcWrite: []iowatch.Ranked{{Name: "dd(2)", Value: 300 << 20}}, CgroupWrite: []iowatch.Ranked{{Name: "sess7", Value: 300 << 20}}}}
}

func TestAlertLogModeText(t *testing.T) {
	a, _ := testIOApp(t)
	var logged bytes.Buffer
	a.log = slog.New(slog.NewTextHandler(&logged, nil))
	cfg := iowatch.Default()
	cfg.Mode, cfg.Lead = iowatch.ModeLog, "lead"
	base := time.Date(2026, 10, 29, 7, 9, 28, 0, time.Local)
	full := []iowatch.Case{alertCase(base, 2100), alertCase(base.Add(time.Minute), 3400)}
	act := ioroute.Action{Kind: ioroute.Letter, To: "sess7", Why: ioroute.WhyUrgent}
	a.ioSend(cfg, act.To, string(act.Why), ioRouteLetter(act, full, cfg.AlertMin.D(), nil), 3400)
	text := logged.String()
	for _, want := range []string{"написал бы", "to=sess7", "фриз 29.10 07:10:28, 3,4 с (простой 2,5 с, io", "главная cgroup sess7",
		"запись процессов (данные и метаданные): dd(2) 300 МБ", "своп in/out 1200/0 стр", "Всего случаев от 2,0 с с 07:09: 2, вместе 5,5 с", "aguard io report -since"} {
		if !strings.Contains(text, want) {
			t.Errorf("нет %q в журнале:\n%s", want, text)
		}
	}
}

// routeOwner — хозяин случая, которому положено письмо: у возможной жертвы (читала, удаление снимка) — "", ей только тикет.
func routeOwner(t *testing.T, a *app, c iowatch.Case) string {
	t.Helper()
	cfg := iowatch.Default()
	rc, err := a.ioOwners(&cfg).routeCase(&c)
	if err != nil {
		t.Fatal(err)
	}
	if rc.Victim {
		return ""
	}
	return rc.Owner
}

// Служба: короче alert_min, тихий, под потолком — не в маршрут; короткий от 2 с — тикет хозяину без письма; длинный
// при хозяине не в сети — письмо лиду и тикет хозяину; следующий длинный в паузе — ждёт её конца, а по концу, когда
// вред кончился, уходит заметкой в тикет, без письма.
func TestIOServiceRoutes(t *testing.T) {
	a, _ := testIOApp(t)
	var logged bytes.Buffer
	a.log = slog.New(slog.NewTextHandler(&logged, nil))
	s := newIOService(context.Background(), a)
	cfg := iowatch.Default()
	cfg.Mode, cfg.Lead = iowatch.ModeLog, "lead"
	s.cfg = cfg
	s.setRoute(cfg)
	base := time.Now().Truncate(time.Second)
	jobs := func() []ioTicketJob {
		var out []ioTicketJob
		for {
			select {
			case j := <-s.tickets.queue:
				out = append(out, j)
			default:
				return out
			}
		}
	}
	letters := func() int {
		t.Helper()
		time.Sleep(50 * time.Millisecond) // письма — своими горутинами
		return strings.Count(logged.String(), "написал бы")
	}
	s.closed(alertCase(base, 1900))
	quiet := alertCase(base, 5000)
	quiet.Quiet = true
	s.closed(quiet)
	capped := alertCase(base, 12600)
	capped.Capped = true
	s.closed(capped)
	if n, j := letters(), jobs(); n != 0 || len(j) != 0 || len(s.cases) != 3 {
		t.Fatalf("не в маршрут: писем %d, тикетов %d, случаев %d", n, len(j), len(s.cases))
	}
	s.closed(alertCase(base, 3000))
	if n, j := letters(), jobs(); n != 0 || len(j) != 1 || j[0].owner != "sess7" || j[0].class != "dd" || j[0].tk == "" {
		t.Fatalf("короткий: писем %d, тикеты %+v", n, j)
	}
	s.closed(alertCase(base.Add(time.Minute), 12000))
	if n, j := letters(), jobs(); n != 1 || !strings.Contains(logged.String(), "to=lead") || len(j) != 1 || j[0].why != ioroute.WhyOffline {
		t.Fatalf("длинный, хозяин не в сети: писем %d, тикеты %+v, журнал:\n%s", n, j, logged.String())
	}
	s.closed(alertCase(base.Add(2*time.Minute), 12000))
	if n := letters(); n != 1 || s.route.Pending() != 1 {
		t.Fatalf("в паузе: писем %d, ждёт %d", n, s.route.Pending())
	}
	jobs()
	s.act(s.route.Tick(base.Add(2 * time.Hour)))
	if n, j := letters(), jobs(); n != 1 || len(j) != 1 || len(j[0].cases) != 1 || s.route.Pending() != 0 {
		t.Fatalf("конец паузы, вред кончился: писем %d, тикеты %+v", n, j)
	}
}

// Письмо живой сессии в режиме alert доходит; сбоя в журнале нет.
func TestIOSendAlive(t *testing.T) {
	a, _ := testIOApp(t)
	var logged bytes.Buffer
	a.log = slog.New(slog.NewTextHandler(&logged, nil))
	cfg := iowatch.Default()
	cfg.Mode, cfg.Lead = iowatch.ModeAlert, "lead"
	got := liveLead(t, a)
	act := ioroute.Action{Kind: ioroute.Letter, To: "lead", Why: ioroute.WhyNoOwner}
	a.ioSend(cfg, "lead", string(act.Why), ioRouteLetter(act, []iowatch.Case{alertCase(time.Now().Add(-time.Minute), 12500)}, cfg.AlertMin.D(), nil), 12500)
	if msg := <-got; !strings.Contains(msg, "хозяина виновника нет") || !strings.Contains(msg, "dd(2)") || strings.Contains(logged.String(), "level=WARN") {
		t.Fatalf("письмо лиду %.300q, журнал:\n%s", msg, logged.String())
	}
}

// Сводки переживают перезапуск: остановка пишет файл, старт читает; старше cases_keep — не берутся;
// битый файл откладывается, история начинается заново.
func TestCasesSurviveRestartKeepAndBrokenFile(t *testing.T) {
	a, _ := testIOApp(t)
	var logged bytes.Buffer
	a.log = slog.New(slog.NewTextHandler(&logged, nil))
	now := time.Now()
	s := newIOService(context.Background(), a)
	s.cfg = iowatch.Default()
	s.cases = []iowatch.Case{{Start: now.Add(-15 * 24 * time.Hour)}, {Start: now.Add(-time.Hour), DurMS: 2500}}
	if err := iowatch.SaveCases(s.path, s.kept(now)); err != nil {
		t.Fatal(err)
	}
	next := newIOService(context.Background(), a)
	next.load()
	if len(next.cases) != 1 || next.cases[0].DurMS != 2500 {
		t.Fatalf("после перезапуска: %+v", next.cases)
	}
	if err := os.WriteFile(s.path, []byte("{битая\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	broken := newIOService(context.Background(), a)
	broken.load()
	moved, _ := filepath.Glob(s.path + ".broken-*")
	if len(broken.cases) != 0 || len(moved) != 1 || !strings.Contains(logged.String(), "файл отложен") {
		t.Fatalf("битый файл: cases=%d, отложено %v, журнал:\n%s", len(broken.cases), moved, logged.String())
	}
}

func TestReportTextAndJSONFromFile(t *testing.T) {
	a, out := testIOApp(t)
	path := filepath.Join(t.TempDir(), "cases.jsonl")
	base := time.Now().Add(-2 * time.Hour).Truncate(time.Hour)
	quiet := iowatch.Case{Start: base.Add(30 * time.Minute), DurMS: 1100, IdleMS: 1000, Quiet: true,
		Culprits: &iowatch.Culprit{CgroupIdle: []iowatch.Ranked{{Name: "thinkfan", Value: 1000}}}}
	capped := alertCase(base.Add(40*time.Minute), 12600)
	capped.IdleMS, capped.Capped, capped.OutsideIdleMS = 8100, true, 51
	capped.Culprits.CappedIdle = []iowatch.Ranked{{Name: "run-p2016228-i69065841.scope", Value: 9133}}
	cases := []iowatch.Case{{Start: base, DurMS: 100, IdleMS: 60}, alertCase(base.Add(time.Minute), 2500), quiet, capped, alertCase(base.Add(time.Hour), 700)}
	if err := iowatch.SaveCases(path, cases); err != nil {
		t.Fatal(err)
	}
	if err := a.ioReport(path, false, base.Add(-time.Minute), time.Now(), 0, false); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{": 3, простой всего 2,5 с\n", "по длительности: <300 мс 1 · 300–500 мс 0 · 0,5–1 с 1 · 1–2 с 0 · 2–5 с 1 · ≥5 с 0\n",
		"  " + base.Format("02.01 15:00") + "  2 / 1 / 2,5 с\n", "главная cgroup (случаев): sess7 2\n", "запись процессов: dd 600 МБ в 2\n",
		"самые длинные:\n- " + base.Add(time.Minute).Format("02.01 15:04:05") + ", 2,5 с",
		"тихих (простой без обмена с диском, не фриз): 1, простой 1,0 с; кто ждал (случаев от 500 мс): thinkfan 1\n",
		"под потолком io.max (ждали придержанный потолком IO, остальные — нет; не фриз): 1, простой 8,1 с; чей потолок: run-p2016228-i69065841.scope 1\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("нет %q в итоге:\n%s", want, text)
		}
	}
	out.Reset()
	if err := a.ioReport(path, false, base.Add(-time.Minute), time.Now(), time.Second, true); err != nil {
		t.Fatal(err)
	}
	var sum iowatch.Summary
	if err := json.Unmarshal(out.Bytes(), &sum); err != nil || sum.Total != 3 || len(sum.Cases) != 1 || sum.Min != 1000 || len(sum.Hours) != 2 ||
		sum.Quiet != 1 || len(sum.QuietWho) != 1 || sum.Capped != 1 || sum.CappedIdleMS != 8100 || len(sum.CappedWho) != 1 {
		t.Fatalf("JSON: %+v %v", sum, err)
	}
}

// Старые снимки событий — через тот же Recorder в сводки; в файл службы — только при остановленной
// службе (в тесте её нет), с -remove снимки удаляются, ручные остаются. Не файл службы — службу не спрашивают:
// systemctl в PATH подменён и оставляет метку.
func TestImportEventSnapshotsToCases(t *testing.T) {
	a, out := testIOApp(t)
	bin := t.TempDir()
	called := filepath.Join(bin, "called")
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\n: >'"+called+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	dir := filepath.Join(t.TempDir(), "events")
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	var buf bytes.Buffer
	for i := range 40 {
		at := base.Add(time.Duration(i) * 100 * time.Millisecond)
		step := iowatch.Step{At: at}
		if i >= 5 && i < 15 {
			step.IOFullUS = 90_000
		}
		if err := iowatch.WriteRecord(&buf, iowatch.Record{Kind: iowatch.KindStep, Step: step}); err != nil {
			t.Fatal(err)
		}
		if i%10 == 0 {
			if err := iowatch.WriteRecord(&buf, iowatch.Record{Kind: iowatch.KindDetail, Step: iowatch.Step{At: at,
				Processes: []iowatch.Process{{PID: 7, Comm: "rm", WriteBytes: 1 << 20}}}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	event, manual := filepath.Join(dir, base.Format("2006-01-02T150405.000")+".jsonl"), filepath.Join(dir, iowatch.ManualName(base))
	for _, p := range []string{event, manual} {
		if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "cases.jsonl")
	if err := a.ioImport(dir, path, false, true); err != nil {
		t.Fatal(err)
	}
	got, err := iowatch.LoadCases(path)
	if err != nil || len(got) != 1 || got[0].DurMS != 1000 || got[0].Culprits == nil || got[0].Culprits.ProcWrite[0].Name != "rm(7)" {
		t.Fatalf("случаи: %+v %v", got, err)
	}
	if _, err := os.Stat(event); !os.IsNotExist(err) {
		t.Fatalf("снимок события не удалён: %v", err)
	}
	if _, err := os.Stat(manual); err != nil || !strings.Contains(out.String(), "снимков 1, случаев 1") {
		t.Fatalf("ручной снимок: %v; вывод %q", err, out.String())
	}
	if _, err := os.Stat(called); err == nil {
		t.Error("не файл службы, а systemctl вызван")
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
func TestIONoDeviceWarnsDaily(t *testing.T) {
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

// Цикл io — не safePass: проход отмечает сам, раз в ioPassEvery, иначе отчёт и страница Сна видят
// «ни одного прохода» (29.10).
func TestIOStepNotesPassEveryMinute(t *testing.T) {
	a, _ := testIOApp(t)
	s := newIOService(context.Background(), a)
	s.cfg = iowatch.Default()
	s.rec = iowatch.NewRecorder(s.cfg)
	base := time.Now()
	for i := range 1500 { // 2,5 мин шагов по 100 мс
		s.step(iowatch.Step{At: base.Add(time.Duration(i) * 100 * time.Millisecond)})
	}
	a.flush()
	passes := 0
	if _, err := activity.NewStore(filepath.Join(a.home, "activity")).Read(time.Time{}, func(r *activity.Record) {
		if r.Watcher == "io" && r.Kind == activity.KindPass {
			passes += r.Count
		}
	}); err != nil {
		t.Fatal(err)
	}
	if passes != 3 {
		t.Fatalf("отметок прохода io за 2,5 мин: %d, ждал 3", passes)
	}
}

func TestIOCaseIncludesProcessCommand(t *testing.T) {
	c := alertCase(time.Date(2026, 10, 31, 12, 0, 0, 0, time.Local), 7400)
	c.Culprits.ProcWrite = []iowatch.Ranked{{Name: "dd(10)", Value: 40}, {Name: "cp(12)", Value: 30}, {Name: "rg(13)", Value: 20}, {Name: "tar(14)", Value: 10}}
	c.Culprits.ProcRead = []iowatch.Ranked{{Name: "cat(11)", Value: 20}}
	c.Culprits.ProcessDetails = map[string]iowatch.ProcessDetails{
		"dd(10)":  {Cmdline: "dd --write", CWD: "/workspace/project"},
		"cp(12)":  {Cmdline: "cp a b"},
		"rg(13)":  {Cmdline: "rg needle"},
		"tar(14)": {Cmdline: "tar cf x"},
		"cat(11)": {Cmdline: "cat input"},
	}
	var b strings.Builder
	writeIOCase(&b, &c, 5)
	text := b.String()
	for _, want := range []string{"  cmd dd(10): dd --write (в /workspace/project)\n", "  cmd cp(12): cp a b\n", "  cmd rg(13): rg needle\n", "  cmd cat(11): cat input\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("нет %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "tar cf x") {
		t.Errorf("команда четвёртого процесса — только у топ-3:\n%s", text)
	}

	// aguard io report: под самыми длинными случаями — команды топ-3 записи; случай без команд (до 31.10) — без строки процессов.
	old := alertCase(time.Date(2026, 10, 30, 12, 0, 0, 0, time.Local), 3000)
	old.Culprits.ProcWrite = []iowatch.Ranked{{Name: "go(5)", Value: 10}}
	b.Reset()
	writeIOReport(&b, &iowatch.Summary{Total: 2, Cases: []iowatch.Case{c, old}, Min: 1000})
	text = b.String()
	for _, want := range []string{"  cmd dd(10): dd --write (в /workspace/project)\n", "  cmd rg(13): rg needle\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("итог без команды %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "tar cf x") || strings.Contains(text, "go(5)") {
		t.Errorf("итог: команда вне топ-3 или процессы случая без команд:\n%s", text)
	}

	// aguard io at: итог окна с командой ушедшего процесса.
	b.Reset()
	writeIOSummary(&b, iowatch.Window{Details: []iowatch.Step{{Processes: []iowatch.Process{
		{PID: 10, Start: 1, Comm: "dd", WriteBytes: 4096, Cmdline: "dd --write", CWD: "/workspace/project"}}}}})
	if !strings.Contains(b.String(), "  cmd dd(10): dd --write (в /workspace/project)\n") {
		t.Errorf("aguard io at без команды:\n%s", b.String())
	}
}

// Письмо называет фон ядра и помечает io.stat, набранный чужим коммитом по fsync; своя запись в пределах — без пометки.
func TestIOCaseFsyncAndKernel(t *testing.T) {
	c := alertCase(time.Date(2026, 10, 30, 12, 15, 12, 0, time.Local), 7400)
	c.Culprits.CgroupWrite = []iowatch.Ranked{{Name: "postgresql.service", Value: 870 << 20}, {Name: "sess7", Value: 300 << 20}}
	c.Culprits.CgroupWrite = append(c.Culprits.CgroupWrite, iowatch.Ranked{Name: "systemd-journald.service", Value: 2 << 20})
	c.Culprits.CgroupOwn = map[string]int64{"postgresql.service": 5 << 20, "sess7": 290 << 20}
	c.Culprits.Foreign = iowatch.Ranked{Name: "docker.service", Value: 900 << 20}
	c.Culprits.CgroupDirty = []iowatch.Ranked{{Name: "sess7", Value: 290 << 20}}
	c.Culprits.DeviceWrite, c.Culprits.KernelWrite = 1900<<20, 730<<20
	var b strings.Builder
	writeIOCase(&b, &c, 3)
	text := b.String()
	for _, want := range []string{"запись диска за окно 1900 МБ, из них фон ядра 730 МБ", "запись процессов по cgroup: sess7 290 МБ",
		"postgresql.service 870 МБ (своих 5 — коммит btrfs по её fsync), sess7 300 МБ, systemd-journald.service 2 МБ (процессы чужого uid — своя запись не видна)\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("нет %q:\n%s", want, text)
		}
	}
	b.Reset() // своих 0 — процессы cgroup не попали в топ шага: доли не знаем, пометки нет
	writeIOCgroupWrite(&b, []iowatch.Ranked{{Name: "sess8", Value: 100 << 20}}, map[string]int64{"sess8": 0})
	if b.String() != "запись cgroup (io.stat): sess8 100 МБ\n" {
		t.Errorf("своих 0: %q", b.String())
	}
	if line := ioCaseLine(&c); !strings.Contains(line, "чужая cgroup docker.service (чужой uid, I/O по io.stat): 900 МБ; главная cgroup sess7") {
		t.Errorf("шапка не называет чужую cgroup первой: %s", line)
	}
}

func TestIOCaseCleanerAttributionAndAlertRecipient(t *testing.T) {
	base := time.Date(2026, 11, 3, 1, 20, 0, 0, time.Local)
	c := alertCase(base, 2100)
	c.Culprits.MainValue = 100 << 20
	c.Culprits.DeviceWrite, c.Culprits.KernelWrite, c.Culprits.CleanerMS = 1000<<20, 960<<20, 2000
	want := "удаление снапшотов btrfs (btrfs-cleaner, CPU 2000 мс за окно): фон ядра 960 МБ, 96% записи устройства; главная cgroup sess7"
	if got := ioCaseAttribution(&c); got != want {
		t.Fatalf("подпись cleaner: %s", got)
	}
	c.Culprits.CleanerMS = 0
	if got := ioCaseAttribution(&c); !strings.HasPrefix(got, "фон ядра (writeback, коммит btrfs)") {
		t.Fatalf("без CPU cleaner должен остаться фон ядра: %s", got)
	}
	c.Culprits.CleanerMS, c.Culprits.MainValue = 2000, 970<<20
	if got := ioCaseAttribution(&c); got != "главная cgroup sess7" {
		t.Fatalf("без перевеса ядра должна остаться главная cgroup: %s", got)
	}
	c.Culprits.MainValue = 100 << 20
	var detail strings.Builder
	writeIOCase(&detail, &c, 3)
	if !strings.Contains(detail.String(), "CPU btrfs-cleaner за окно: 2000 мс") {
		t.Fatalf("разбор не содержит CPU cleaner:\n%s", detail.String())
	}

	a, _ := testIOApp(t)
	if o := routeOwner(t, a, c); o != "" {
		t.Fatalf("письмо cleaner адресовано только лиду: хозяин %q", o)
	}
}

func TestIOCaseAttributionWithoutMainAndReadMostlyAlert(t *testing.T) {
	base := time.Date(2026, 11, 3, 1, 20, 0, 0, time.Local)
	c := alertCase(base, 2100)
	c.Culprits.Main = ""
	c.Culprits.MainValue = 0
	c.Culprits.KernelWrite = 500 << 20
	if got, want := ioCaseAttribution(&c), "фон ядра (writeback, коммит btrfs): 500 МБ, 0% записи устройства"; got != want {
		t.Fatalf("подпись фона ядра без главной: %q, want %q", got, want)
	}
	alertText, _ := ioAlertText([]iowatch.Case{c}, time.Second)
	if !strings.Contains(alertText, "фон ядра (writeback, коммит btrfs)") || strings.Contains(alertText, "главная cgroup") {
		t.Fatalf("текст письма без главной не содержит подпись ядра или содержит пустую главную: %q", alertText)
	}
	c.Culprits.KernelWrite = 0
	c.Culprits.CgroupSwapIn = []iowatch.Ranked{{Name: "victim-a", Value: 30}, {Name: "victim-b", Value: 20}, {Name: "victim-c", Value: 10}, {Name: "victim-d", Value: 1}}
	if got, want := ioCaseAttribution(&c), "главной нет, жертвы вытеснения: victim-a, victim-b, victim-c"; got != want {
		t.Fatalf("подпись жертв вытеснения: %q, want %q", got, want)
	}
	c.Culprits.CgroupSwapIn = nil
	if got := ioCaseAttribution(&c); got != "" {
		t.Fatalf("без главной и признаков виновника ожидалась пустая подпись, получено %q", got)
	}
	c.Culprits.KernelWrite, c.Culprits.CleanerMS = 500<<20, 100
	if got := ioCaseAttribution(&c); !strings.HasPrefix(got, "удаление снапшотов btrfs (") || strings.Contains(got, "главная cgroup") {
		t.Fatalf("снапшоты без главной: %q", got)
	}
	alertText, _ = ioAlertText([]iowatch.Case{c}, time.Second)
	if !strings.Contains(alertText, "удаление снапшотов btrfs (") || strings.Contains(alertText, "главная cgroup") {
		t.Fatalf("текст письма о снапшотах без главной: %q", alertText)
	}
	kernelCase := alertCase(base.Add(time.Second), 1800)
	kernelCase.Culprits.Main, kernelCase.Culprits.MainValue = "", 0
	kernelCase.Culprits.KernelWrite, kernelCase.Culprits.CleanerMS = 200<<20, 0
	alertText, _ = ioAlertText([]iowatch.Case{c, kernelCase}, time.Second)
	if !strings.Contains(alertText, "удаление снапшотов btrfs (") || !strings.Contains(alertText, "фон ядра (writeback, коммит btrfs)") || strings.Contains(alertText, "главная cgroup") {
		t.Fatalf("письмо без главных не содержит обе подписи или содержит пустую главную: %q", alertText)
	}
	snapApp, _ := testIOApp(t)
	if o := routeOwner(t, snapApp, c); o != "" {
		t.Fatalf("снапшоты без главной отправлены не только лиду: хозяин %q", o)
	}

	c = alertCase(base, 2100)
	c.Culprits.MainRead, c.Culprits.MainWrite = 200, 100
	if got, want := ioCaseAttribution(&c), "главная cgroup sess7 (в основном чтение)"; got != want {
		t.Fatalf("подпись читающей главной: %q, want %q", got, want)
	}
	a, _ := testIOApp(t)
	if o := routeOwner(t, a, c); o != "" {
		t.Fatalf("читающей главной отправлено письмо сессии: хозяин %q", o)
	}
	c.Culprits.MainWrite = 200 // запись равна чтению — адресат сессии сохраняется
	if o := routeOwner(t, a, c); o != "sess7" {
		t.Fatalf("пишущей главной не отправлено письмо сессии: хозяин %q", o)
	}
	c.Culprits.MainWrite = 201 // запись строго больше чтения
	if o := routeOwner(t, a, c); o != "sess7" {
		t.Fatalf("главной с записью больше чтения не отправлено письмо сессии: хозяин %q", o)
	}
}

func TestIOCaseForeignWithoutMainAttributionAndRecipient(t *testing.T) {
	c := alertCase(time.Now(), 2100)
	c.Culprits.Main = ""
	c.Culprits.MainValue = 0
	c.Culprits.Foreign = iowatch.Ranked{Name: "postgresql.service", Value: 12 << 20}
	c.Culprits.CgroupSwapIn = []iowatch.Ranked{{Name: "victim", Value: 25_600}}
	want := "чужая cgroup postgresql.service (чужой uid, I/O по io.stat): 12 МБ"
	if got := ioCaseAttribution(&c); got != want {
		t.Fatalf("подпись чужой cgroup без главной: %q, want %q", got, want)
	}
	a, _ := testIOApp(t)
	if o := routeOwner(t, a, c); o != "" {
		t.Fatalf("чужая cgroup без главной отправлена не только лиду: хозяин %q", o)
	}
}

func TestIOCaseOldJSONWithoutMainMeasures(t *testing.T) {
	const data = `{"main":"/user.slice/agent-old-1.scope","main_value":1048576}`
	var culprit iowatch.Culprit
	if err := json.Unmarshal([]byte(data), &culprit); err != nil {
		t.Fatal(err)
	}
	c := iowatch.Case{Culprits: &culprit}
	if got, want := ioCaseAttribution(&c), "главная cgroup old"; got != want {
		t.Fatalf("подпись старого случая: %q, want %q", got, want)
	}
	encoded, err := json.Marshal(&culprit)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["main_write"]; ok {
		t.Fatalf("повторная сериализация добавила main_write: %s", encoded)
	}
	if _, ok := fields["main_read"]; ok {
		t.Fatalf("повторная сериализация добавила main_read: %s", encoded)
	}
}

func TestIOCaseSwapInLabelsIdentifyVictims(t *testing.T) {
	c := alertCase(time.Now(), 2100)
	c.Culprits.CgroupSwapIn = []iowatch.Ranked{{Name: "victim", Value: 25}}
	var detail strings.Builder
	writeIOCase(&detail, &c, 3)
	if !strings.Contains(detail.String(), "жертвы вытеснения: victim 25 стр") || strings.Contains(detail.String(), "своп cgroup") {
		t.Fatalf("разбор не обозначает swap-in как жертву вытеснения:\n%s", detail.String())
	}
	var report strings.Builder
	writeIOReport(&report, &iowatch.Summary{Total: 1, Cases: []iowatch.Case{c}, Culprits: iowatch.SumCulprit{
		CgroupSwapIn: []iowatch.RankedCases{{Name: "victim", Value: 25, Cases: 1}},
	}})
	if !strings.Contains(report.String(), "жертвы вытеснения: victim") || strings.Contains(report.String(), "своп cgroup") {
		t.Fatalf("сводка не обозначает swap-in как жертву вытеснения:\n%s", report.String())
	}
}

func TestIOCaseAttribution(t *testing.T) {
	base := time.Date(2026, 10, 30, 12, 15, 12, 0, time.Local)
	t.Run("ядро больше главной", func(t *testing.T) {
		c := alertCase(base, 7400)
		c.Culprits.MainValue = 100 << 20
		c.Culprits.DeviceWrite, c.Culprits.KernelWrite = 847<<20, 383<<20
		line := ioCaseLine(&c)
		if !strings.Contains(line, "фон ядра (writeback, коммит btrfs): 383 МБ, 45% записи устройства; главная cgroup sess7") {
			t.Fatalf("шапка не называет фон ядра первым: %s", line)
		}
	})
	const head = "30.10 12:15:12, 7,4 с (простой 5,5 с, io; пик шага 99 мс), "
	kernel := head + "фон ядра (writeback, коммит btrfs): 383 МБ, 45% записи устройства; главная cgroup sess7"
	foreign := head + "чужая cgroup snapperd (чужой uid, I/O по io.stat): 870 МБ; главная cgroup sess7"
	mainOnly := head + "главная cgroup sess7"
	// Foreign в случае — только больше меры главной (ForeignOver); MainValue 0 — случай до 31.10 без меры главной.
	for _, tc := range []struct {
		name                 string
		mainMB, kernelMB, fr int64
		want                 string
	}{
		{"ядро больше главной", 100, 383, 0, kernel},
		{"ядро равно главной", 383, 383, 0, mainOnly},
		{"главная больше ядра", 900, 383, 0, mainOnly},
		{"чужая больше главной", 560, 0, 870, foreign},
		{"ядро больше главной, чужая больше ядра", 100, 383, 870, foreign},
		{"ядро больше главной и чужой", 100, 383, 300, kernel},
		{"ядро больше главной и равно чужой", 100, 383, 383, kernel},
		{"старый случай: ядро больше чужой", 0, 383, 300, kernel},
		{"старый случай: чужая больше ядра", 0, 383, 870, foreign},
		{"старый случай без чужой", 0, 383, 0, mainOnly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := alertCase(base, 7400)
			c.Culprits.MainValue = tc.mainMB << 20
			c.Culprits.DeviceWrite, c.Culprits.KernelWrite = 847<<20, tc.kernelMB<<20
			if tc.fr > 0 {
				c.Culprits.Foreign = iowatch.Ranked{Name: "snapperd", Value: tc.fr << 20}
			}
			if got := ioCaseLine(&c); got != tc.want {
				t.Fatalf("шапка:\nполучено: %s\nожидалось: %s", got, tc.want)
			}
		})
	}
}
