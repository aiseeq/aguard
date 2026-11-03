package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/cpuwatch"
	"github.com/aiseeq/akit/pressure"
	"github.com/aiseeq/akit/session"
)

// cpuApp — app наблюдателя cpu на временной папке: реестр сессий пуст, журнал — в logs.
func cpuApp(t *testing.T, logs *strings.Builder) *app {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	return &app{home: dir, stateDir: filepath.Join(dir, "state"), uid: 1000,
		reg: session.Registry{Dir: filepath.Join(dir, "sessions"), ProcRoot: dir}, log: slog.New(slog.NewTextHandler(logs, nil))}
}

// cpuSample — замер при 22 потоках: load average за минуту, готовые сейчас и готовые потоки по мастерам.
func cpuSample(at time.Time, avg1 float64, runnable int, by map[string]int) *cpuwatch.Sample {
	s := &cpuwatch.Sample{At: at, Load: pressure.LoadAvg{Avg1: avg1}, Runnable: runnable, Threads: 22}
	for m, n := range by {
		s.Tasks = append(s.Tasks, cpuwatch.Task{N: n, Comm: "go", Cgroup: m})
	}
	return s
}

func cpuOwner(t *cpuwatch.Task) cpuwatch.Owner {
	return cpuwatch.Owner{Master: t.Cgroup, Kind: cpuwatch.KindSession}
}

// Письма cpuAct: не раньше after подряд; сводка лиду и письмо ему же как мастеру — порознь; в сводке — все, кому
// писали за перегрузку; длительность кончившейся — без пропуска замеров.
func TestCpuAct(t *testing.T) {
	var logs strings.Builder
	a := cpuApp(t, &logs)
	cfg := cpuwatch.Default(a.home)
	cfg.Mode, cfg.Lead = cpuwatch.ModeLog, "lead"
	h := cpuwatch.Host{Name: cpuwatch.HostLocal} // замеры реже, чем раз в минуту: серию рвёт только Break
	start := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	act := func(at time.Time, s *cpuwatch.Sample) {
		v := h.Judge(s, cpuOwner)
		a.cpuAct(&cfg, &h, &v, at)
	}
	heavy := map[string]int{"lead": 20, "glint-master": 10}

	act(start, cpuSample(start, 30, 30, heavy))
	if strings.Contains(logs.String(), "написал бы") {
		t.Fatalf("перегрузка только началась — писем до after нет:\n%s", logs.String())
	}

	at := start.Add(10 * time.Minute)
	logs.Reset()
	act(at, cpuSample(at, 30, 30, heavy))
	if n := strings.Count(logs.String(), "написал бы\" session=lead "); n != 2 {
		t.Errorf("лиду — письмо как мастеру и сводка, а не %d:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "session=glint-master") {
		t.Errorf("мастеру с 10 задачами из 22 — письмо:\n%s", logs.String())
	}

	// через repeat у glint-master задач мало: ему не пишем, но в сводке он — среди тех, кому писали за перегрузку
	at = start.Add(40 * time.Minute)
	logs.Reset()
	act(at, cpuSample(at, 30, 30, map[string]int{"lead": 20, "glint-master": 1}))
	if strings.Contains(logs.String(), "session=glint-master") ||
		!strings.Contains(logs.String(), "Написал за перегрузку: glint-master, lead.") {
		t.Errorf("сводка — обо всех письмах за перегрузку:\n%s", logs.String())
	}

	// замер не удался, следующий — через 2 минуты: прошлая перегрузка длилась до последнего удачного замера
	h.Break()
	at = start.Add(42 * time.Minute)
	logs.Reset()
	act(at, cpuSample(at, 30, 30, heavy))
	if !strings.Contains(logs.String(), "перегрузка кончилась") || !strings.Contains(logs.String(), "lasted=40m0s") ||
		!strings.Contains(logs.String(), "load1=30 runnable=30 threads=22") {
		t.Errorf("длительность без пропуска — 40 минут:\n%s", logs.String())
	}
}

// Сбой замера в проходе рвёт перегрузку «подряд», а интервал проход отдаёт окну машины.
func TestCpuRoundBreak(t *testing.T) {
	var logs strings.Builder
	a := cpuApp(t, &logs)
	cfg := cpuwatch.Default(a.home)
	cfg.Mode, cfg.Hosts, cfg.Lead = cpuwatch.ModeLog, nil, "lead"
	h := &cpuHost{root: filepath.Join(a.home, "absent"), state: filepath.Join(a.dir("cpu"), "local.json"),
		win: cpuwatch.Host{Name: cpuwatch.HostLocal}}
	a.cpu = map[string]*cpuHost{"": h}
	start := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	h.win.Judge(cpuSample(start, 30, 30, nil), cpuOwner)
	if err := a.cpuRound(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	if !h.win.Down || !strings.Contains(logs.String(), "не удался") {
		t.Fatalf("сбой замера не замечен:\n%s", logs.String())
	}
	if h.win.Every != cfg.Interval.D() {
		t.Errorf("интервал окна %v, хотим %v", h.win.Every, cfg.Interval.D())
	}
	at := start.Add(time.Minute)
	if v := h.win.Judge(cpuSample(at, 30, 30, nil), cpuOwner); !v.Started || !v.Ended.Equal(start) {
		t.Errorf("после сбоя замера перегрузка — новая серия: %+v", v)
	}
}

// Пробник показывает сводку лиду такой, какой она уйдёт: с теми, кому написано строкой выше (cpu-dry-told).
func TestCpuDryLettersLeadNamesRecipients(t *testing.T) {
	cfg := cpuwatch.Default(t.TempDir())
	cfg.Lead = "lead"
	now := time.Date(2026, 10, 31, 15, 3, 0, 0, time.UTC)
	h := cpuwatch.Host{Name: "pc"}
	v := h.Judge(cpuSample(now, 33.9, 43, map[string]int{"sc2go-master": 20, "demu-master": 10, "chooks-master": 1}), cpuOwner)
	var out strings.Builder
	cpuDryLetters(&out, &cfg, &h, &v, now)
	if !strings.Contains(out.String(), "Написал за перегрузку: demu-master, sc2go-master") {
		t.Fatalf("сводка лиду без адресатов:\n%s", out.String())
	}
	if strings.Contains(out.String(), "никому") {
		t.Fatalf("в сводке «никому» при письмах строкой выше:\n%s", out.String())
	}
}

// Проход демона сам сохраняет окно, удачный и неудачный, а новый процесс читает его при старте.
func TestCpuRoundPersistsWindow(t *testing.T) {
	var logs strings.Builder
	a := cpuApp(t, &logs)
	cfg := cpuwatch.Default(a.home)
	cfg.Mode, cfg.Hosts, cfg.Lead = cpuwatch.ModeLog, nil, "lead"
	state := filepath.Join(a.dir("cpu"), "local.json")
	h := &cpuHost{root: filepath.Join(a.home, "absent"), state: state, win: cpuwatch.Host{Name: cpuwatch.HostLocal}}
	a.cpu = map[string]*cpuHost{"": h}
	start := time.Now().Add(-time.Minute).Truncate(time.Second)
	h.win.Judge(cpuSample(start, 30, 30, nil), cpuOwner)
	if err := a.cpuRound(context.Background(), &cfg); err != nil { // замер не удался: сохранён и сбой
		t.Fatal(err)
	}

	next := *a // новый процесс: окна в памяти нет, файл тот же
	next.cpu = map[string]*cpuHost{}
	loaded := next.cpuHosts(&cfg)
	if len(loaded) != 1 || loaded[0].state != state {
		t.Fatalf("машины нового процесса: %+v", loaded)
	}
	if got := loaded[0].win.State(); !got.Since.Equal(start) || !got.Broken {
		t.Fatalf("новый процесс не прочитал окно: %+v", got)
	}

	root := filepath.Join(a.home, "proc") // замер удался, load average ниже потоков: серия кончилась и это сохранено
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"loadavg": "1.00 1.00 1.00 1/100 1\n",
		"stat":    "cpu  1 1 1 1\ncpu0 1 1 1 1\ncpu1 1 1 1 1\nprocs_running 2\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h.root = root
	if err := a.cpuRound(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	var after cpuwatch.Host
	if err := loadCPUState(state, &after); err != nil {
		t.Fatal(err)
	}
	if got := after.State(); !got.Since.IsZero() || got.Broken || got.Last.IsZero() {
		t.Fatalf("удачный проход не сохранил окно: %+v", got)
	}

	cfg.Hosts = []string{"pc"} // у машины по ssh своё окно: файлы не перемешиваются
	remote := next.cpuHosts(&cfg)
	if want := filepath.Join(a.dir("cpu"), "host-pc.json"); remote[len(remote)-1].state != want {
		t.Errorf("файл окна pc: %q, хотим %q", remote[len(remote)-1].state, want)
	}
}

func TestCPUWindowSurvivesRestartAndKeepsRepeat(t *testing.T) {
	start := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "cpu", "local.json")
	h := cpuwatch.Host{Name: cpuwatch.HostLocal, Every: time.Minute}
	for i := range 10 {
		at := start.Add(time.Duration(i) * time.Minute)
		if v := h.Judge(cpuSample(at, 40, 30, nil), cpuOwner); !v.Over {
			t.Fatalf("замер %d: LA должен держать серию: %+v", i, v)
		}
	}
	h.Sent("lead", start.Add(5*time.Minute))
	if err := saveCPUState(path, &h); err != nil {
		t.Fatal(err)
	}
	var restored cpuwatch.Host
	if err := loadCPUState(path, &restored); err != nil {
		t.Fatal(err)
	}
	restored.Every = time.Minute
	at := start.Add(10 * time.Minute)
	v := restored.Judge(cpuSample(at, 40, 30, nil), cpuOwner)
	if !v.Long(at, 10*time.Minute) || !v.Since.Equal(start) {
		t.Fatalf("после рестарта серия продолжается от исходного времени: %+v", v)
	}
	if restored.Due("lead", at, 30*time.Minute) {
		t.Fatal("рестарт не должен сбрасывать интервал повторного письма")
	}
	if !restored.Due("lead", start.Add(35*time.Minute), 30*time.Minute) {
		t.Fatal("письмо снова доступно по истечении repeat")
	}
}

func TestCPUStateGapAfterRestartAndCorruptState(t *testing.T) {
	start := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "cpu", "local.json")
	h := cpuwatch.Host{Name: cpuwatch.HostLocal, Every: time.Minute}
	h.Judge(cpuSample(start, 40, 30, nil), cpuOwner)
	if err := saveCPUState(path, &h); err != nil {
		t.Fatal(err)
	}
	var restored cpuwatch.Host
	if err := loadCPUState(path, &restored); err != nil {
		t.Fatal(err)
	}
	restored.Every = time.Minute
	at := start.Add(4 * time.Minute)
	v := restored.Judge(cpuSample(at, 40, 30, nil), cpuOwner)
	if !v.Started || !v.Ended.Equal(start) || !v.Since.Equal(at) {
		t.Fatalf("пропуск больше двух интервалов после рестарта рвёт старую серию: %+v", v)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	var empty cpuwatch.Host
	if err := loadCPUState(path, &empty); err == nil {
		t.Fatal("битое состояние должно вернуть ошибку для журнала")
	}
	if v := empty.Judge(cpuSample(at, 40, 30, nil), cpuOwner); !v.Started || !v.Since.Equal(at) {
		t.Fatalf("после битого состояния окно начинается с нуля: %+v", v)
	}
}
