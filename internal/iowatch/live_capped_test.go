package iowatch

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// liveRun — 8,5 с шагов по 100 мс и детальные записи раз в секунду (1…8 с) через Recorder в Live; step и
// cgroups задают шаг и cgroup записи секунды sec.
func liveRun(t *testing.T, step func(s *Step), cgroups func(sec int) []Cgroup) Now {
	t.Helper()
	r := NewRecorder(Default())
	live := NewLive()
	r.Attach(nil, nil, live)
	base := time.Date(2026, 11, 8, 14, 0, 0, 0, time.Local)
	for i := 1; i <= 85; i++ {
		at := base.Add(time.Duration(i) * 100 * time.Millisecond)
		if i%10 == 0 {
			r.Detail(Step{At: at, Cgroups: cgroups(i / 10)})
		}
		s := Step{At: at}
		step(&s)
		r.Step(s)
	}
	now, ok := live.Now()
	if !ok {
		t.Fatal("нет шагов")
	}
	return now
}

func TestLiveCappedWindowAndOpenCase(t *testing.T) {
	probe := Cap{Path: "/app.slice/run-p1-i2.scope", RBps: 30_000_000}
	stalled := func(s *Step) { s.IOFullUS, s.ReadBytes, s.IOTicks = 70_000, 3_000_000, 5 }
	capped := func(int) []Cgroup {
		return []Cgroup{{Path: probe.Path, ReadBytes: 30_000_000, IOFullUS: 700_000, Cap: probe},
			{Path: "/agents.slice/agent-lead-1.scope", IOFullUS: 2000}}
	}
	t.Run("проба под потолком: окно и идущий случай", func(t *testing.T) {
		now := liveRun(t, stalled, capped)
		// окно (3,5; 8,5]: записи 4 (на половину) … 8; последняя запись — 8 с, шаги 8,1–8,5 деталей ещё не имеют
		c := now.W5s.Capped
		if !c.Capped || c.OutsideMS != 9 || c.CappedMS != 3150 || now.W5s.BusyMS != 250 || !now.Open.Open || !now.Open.Capped {
			t.Fatalf("под потолком: окно %+v занят %d, случай %+v", c, now.W5s.BusyMS, now.Open)
		}
	})
	t.Run("пять cgroup без потолка по 200 мс за окно", func(t *testing.T) {
		now := liveRun(t, stalled, func(int) []Cgroup {
			var out []Cgroup
			for _, name := range []string{"a", "b", "c", "d", "e"} {
				out = append(out, Cgroup{Path: "/agents.slice/agent-" + name + "-1.scope", IOFullUS: 44_445})
			}
			return out
		})
		if c := now.W5s.Capped; c.Capped || c.CappedMS != 0 || c.OutsideMS != 200 || now.Open.Capped {
			t.Fatalf("потолков нет — не под потолком: %+v %+v", c, now.Open)
		}
	})
	t.Run("граничная запись: вне потолка — только её доля в окне", func(t *testing.T) {
		now := liveRun(t, stalled, func(sec int) []Cgroup {
			out := capped(sec)
			if sec == 4 { // запись (3, 4 с] внутри окна (3,5; 8,5] на половину
				out = append(out, Cgroup{Path: "/session.slice/plasmashell.service", IOFullUS: 300_000})
			}
			return out
		})
		if c := now.W5s.Capped; !c.Capped || c.OutsideMS != 150 {
			t.Fatalf("300 мс записи (3, 4 с] — 150 в окне: %+v", c)
		}
	})
	t.Run("диск занят больше половины окна", func(t *testing.T) {
		now := liveRun(t, func(s *Step) { stalled(s); s.IOTicks = 60 }, capped)
		if c := now.W5s.Capped; c.Capped || now.W5s.BusyMS != 3000 || now.Open.Capped {
			t.Fatalf("занятый диск — не оправдание: %+v занят %d, случай %+v", c, now.W5s.BusyMS, now.Open)
		}
	})
	t.Run("простой вне потолка у идущего случая", func(t *testing.T) {
		now := liveRun(t, stalled, func(sec int) []Cgroup {
			return append(capped(sec), Cgroup{Path: "/session.slice/plasmashell.service", IOFullUS: 300_000})
		})
		if now.W5s.Capped.Capped || !now.Open.Open || now.Open.Capped {
			t.Fatalf("интерфейс ждал — фриз: %+v %+v", now.W5s.Capped, now.Open)
		}
	})
}

// Без детальных записей поле признания есть, с нулями: отсутствие поля значит старый сторож.
func TestLiveCappedFieldsPresentWithoutDetails(t *testing.T) {
	live := NewLive()
	r := NewRecorder(Default())
	r.Attach(nil, nil, live)
	r.Step(Step{At: time.Now()})
	now, ok := live.Now()
	if !ok {
		t.Fatal("нет шагов")
	}
	b, err := json.Marshal(now)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"5s":{`, `"capped":{"capped":false,"outside_ms":0,"capped_ms":0}`, `"open":{"open":false,"capped":false}`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("нет %s в %s", want, b)
		}
	}
}

// Диск забит последние 2,3 с (проверка 08.11 14:27): за 5 с занят 2300 мс — меньше половины, но последняя секунда
// занята целиком — окно не под потолком.
func TestLiveCappedBusyTail(t *testing.T) {
	probe := Cap{Path: "/app.slice/run-p1-i2.scope", RBps: 30_000_000}
	i := 0
	now := liveRun(t, func(s *Step) {
		i++
		s.IOFullUS, s.ReadBytes, s.IOTicks = 70_000, 3_000_000, 5
		if i > 62 {
			s.IOTicks = 100
		}
	}, func(int) []Cgroup {
		return []Cgroup{{Path: probe.Path, ReadBytes: 30_000_000, IOFullUS: 700_000, Cap: probe}}
	})
	if now.W5s.BusyMS >= 2500 || now.W1s.BusyMS != 1000 || now.W5s.Capped.Capped || now.W30s.Capped.Capped || now.W1s.Capped.Capped {
		t.Fatalf("занятый хвост: 5 с занят %d, 1 с %d, признание %+v %+v %+v", now.W5s.BusyMS, now.W1s.BusyMS, now.W1s.Capped, now.W5s.Capped, now.W30s.Capped)
	}
}

// Признание идущего случая сбрасывается со сменой случая: новый до своей детальной записи — не под потолком.
func TestLiveOpenCappedResetOnNewCase(t *testing.T) {
	r := NewRecorder(Default())
	live := NewLive()
	r.Attach(nil, nil, live)
	base := time.Date(2026, 11, 8, 14, 0, 0, 0, time.Local)
	probe := Cap{Path: "/app.slice/run-p1-i2.scope", RBps: 30_000_000}
	for i := 1; i <= 35; i++ {
		at := base.Add(time.Duration(i) * 100 * time.Millisecond)
		if i%10 == 0 {
			r.Detail(Step{At: at, Cgroups: []Cgroup{{Path: probe.Path, ReadBytes: 30_000_000, IOFullUS: 700_000, Cap: probe}}})
		}
		s := Step{At: at, IOFullUS: 70_000, ReadBytes: 3_000_000}
		if i > 30 && i < 35 { // 400 мс без простоя — больше gap: случай закрыт, на 3,5 с — новый
			s.IOFullUS = 0
		}
		r.Step(s)
		if i == 30 {
			if now, _ := live.Now(); !now.Open.Open || !now.Open.Capped {
				t.Fatalf("первый случай под потолком: %+v", now.Open)
			}
		}
	}
	if now, _ := live.Now(); !now.Open.Open || now.Open.Capped {
		t.Fatalf("новый случай без своих записей — не под потолком: %+v", now.Open)
	}
}

// Запоздавшая детальная запись в Live (как TestCappedWaitLateRecordInterval): проход стоял 3 с, 90 МБ за 3 с при
// 80M — в потолок не упирались, окно не под потолком.
func TestLiveCappedLateRecordInterval(t *testing.T) {
	r := NewRecorder(Default())
	live := NewLive()
	r.Attach(nil, nil, live)
	base := time.Date(2026, 11, 8, 14, 0, 0, 0, time.Local)
	evals := Cap{Path: "/agents.slice/agents-evals.slice", RBps: 80_000_000}
	for i := 1; i <= 45; i++ {
		at := base.Add(time.Duration(i) * 100 * time.Millisecond)
		switch i {
		case 10:
			r.Detail(Step{At: at})
		case 40:
			r.Detail(Step{At: at, Cgroups: []Cgroup{{Path: evals.Path + "/a.scope", ReadBytes: 90_000_000, IOFullUS: 2_500_000, Cap: evals}}})
		}
		r.Step(Step{At: at, IOFullUS: 70_000, ReadBytes: 3_000_000})
	}
	now, _ := live.Now()
	if c := now.W5s.Capped; c.Capped || c.CappedMS != 0 || c.OutsideMS != 2500 {
		t.Fatalf("скорость — за весь промежуток записи: %+v", c)
	}
}

// Диск занят целиком первые 3 с окна 5 с, последняя секунда свободна (проверка 08.11 14:43): хвост окно не
// отбивает — его отбивает занятость всего окна, 3000 из 5000 мс.
func TestLiveCappedBusyHead(t *testing.T) {
	probe := Cap{Path: "/app.slice/run-p1-i2.scope", RBps: 30_000_000}
	i := 0
	now := liveRun(t, func(s *Step) {
		i++
		s.IOFullUS, s.ReadBytes = 70_000, 3_000_000
		if i > 35 && i <= 65 {
			s.IOTicks = 100
		}
	}, func(int) []Cgroup {
		return []Cgroup{{Path: probe.Path, ReadBytes: 30_000_000, IOFullUS: 700_000, Cap: probe}}
	})
	if now.W5s.BusyMS != 3000 || now.W1s.BusyMS != 0 || now.W5s.Capped.Capped || !now.W1s.Capped.Capped {
		t.Fatalf("занятое начало окна: 5 с занят %d, 1 с %d, признание 1 с %+v, 5 с %+v", now.W5s.BusyMS, now.W1s.BusyMS, now.W1s.Capped, now.W5s.Capped)
	}
}
