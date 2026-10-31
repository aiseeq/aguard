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
	if !strings.Contains(logs.String(), "перегрузка кончилась") || !strings.Contains(logs.String(), "lasted=40m0s") {
		t.Errorf("длительность без пропуска — 40 минут:\n%s", logs.String())
	}
}

// Сбой замера в проходе рвёт перегрузку «подряд», а интервал проход отдаёт окну машины.
func TestCpuRoundBreak(t *testing.T) {
	var logs strings.Builder
	a := cpuApp(t, &logs)
	cfg := cpuwatch.Default(a.home)
	cfg.Mode, cfg.Hosts, cfg.Lead = cpuwatch.ModeLog, nil, "lead"
	h := &cpuHost{root: filepath.Join(a.home, "absent"), win: cpuwatch.Host{Name: cpuwatch.HostLocal}}
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
