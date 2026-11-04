package queue

import (
	"testing"
	"time"
)

func dueTick(prio string) Ticket {
	t := tick(prio, "ждёт")
	t.WaitDue = true
	t.Diff = "DO"
	return t
}

func dueWorld() *world {
	w := newWorld(ModeOn)
	w.free["m-master"] = true
	w.now = at("2026-11-04T16:30:00+01:00")
	return w
}

// Приёмка лида 16:08: наступивший ждущий любого приоритета будит мастера при закрытых воротах.
func TestDueWaitWakesWithClosedGates(t *testing.T) {
	for _, prio := range []string{"CRIT", "HIGH", "MED", "LOW"} {
		t.Run(prio, func(t *testing.T) {
			w := dueWorld()
			s := seeded(w)
			ns := step(s, []Ticket{dueTick(prio)}, w)
			if count(ns, KindMorning) != 1 || !has(ns, KindMorning, "m-master") {
				t.Fatalf("%+v", ns)
			}
		})
	}
}

// Не чаще раза за день на мастера, сколько бы ни наступило; на другой день снова.
func TestDueWaitOncePerDay(t *testing.T) {
	w := dueWorld()
	s := seeded(w)
	a, b := dueTick("MED"), dueTick("LOW")
	b.Key = "m/20261104-0951-y"
	if ns := step(s, []Ticket{a, b}, w); count(ns, KindMorning) != 1 {
		t.Fatalf("два сразу: %+v", ns)
	}
	w.now = w.now.Add(time.Hour)
	if ns := step(s, []Ticket{a, b}, w); len(ns) != 0 {
		t.Fatalf("тот же день: %+v", ns)
	}
	w.now = at("2026-11-05T07:01:00+01:00")
	if ns := step(s, []Ticket{a, b}, w); count(ns, KindMorning) != 1 {
		t.Fatalf("на другой день: %+v", ns)
	}
}

// Проверка 04.11, ворота: письмо не зависит от сложности и от того, какие пары открыты.
func TestDueWaitAnyGates(t *testing.T) {
	for name, diff := range map[string]string{"без сложности": "", "со сложностью": "HUNT"} {
		t.Run(name, func(t *testing.T) {
			w := dueWorld()
			w.open = []string{"LOW-DO"}
			s := seeded(w)
			task := dueTick("LOW")
			task.Diff = diff
			if ns := step(s, []Ticket{task}, w); count(ns, KindMorning) != 1 {
				t.Fatalf("%+v", ns)
			}
		})
	}
}

// Проверка 04.11: письмо мёртвому не доставится, повтор шёл бы каждые 30 с.
func TestDueWaitDeadMaster(t *testing.T) {
	w := dueWorld()
	w.live["m-master"] = false
	s := seeded(w)
	if ns := step(s, []Ticket{dueTick("MED")}, w); len(ns) != 0 {
		t.Fatalf("%+v", ns)
	}
}

// Проверка 04.11: раз в день считается на мастера, а не на всех.
func TestDueWaitPerMaster(t *testing.T) {
	w := dueWorld()
	w.live["n-master"], w.free["n-master"] = true, true
	s := seeded(w)
	a, b := dueTick("MED"), dueTick("MED")
	b.Key, b.Project, b.Master = "n/20261104-0951-y", "n", "n-master"
	ns := step(s, []Ticket{a, b}, w)
	if count(ns, KindMorning) != 2 || !has(ns, KindMorning, "m-master") || !has(ns, KindMorning, "n-master") {
		t.Fatalf("%+v", ns)
	}
}

// Заведён сегодня после границы, срок наступил днём: письмо сегодня, дата создания его не держит.
func TestDueWaitCreatedToday(t *testing.T) {
	w := dueWorld()
	s := seeded(w)
	task := dueTick("MED")
	task.Created = at("2026-11-04T10:00:00+01:00")
	if ns := step(s, []Ticket{task}, w); count(ns, KindMorning) != 1 {
		t.Fatalf("%+v", ns)
	}
}

// Журнал: наступивший будит, как в visibleInNext, ненаступивший и открытый журнал нет.
func TestDueWaitJournal(t *testing.T) {
	w := dueWorld()
	s := seeded(w)
	journal := dueTick("HIGH")
	journal.Journal = true
	if ns := step(s, []Ticket{journal}, w); count(ns, KindMorning) != 1 {
		t.Fatalf("наступивший: %+v", ns)
	}
	w = dueWorld()
	s = seeded(w)
	notDue := journal
	notDue.WaitDue = false
	opened := tick("HIGH", "открыт")
	opened.Journal = true
	if ns := step(s, []Ticket{notDue, opened}, w); len(ns) != 0 {
		t.Fatalf("ненаступивший и открытый: %+v", ns)
	}
}

// Границы: срок не наступил, мастер занят, до 07:00, выходной в weekend_crit (кроме CRIT), «в работе» при закрытых воротах.
func TestDueWaitBoundaries(t *testing.T) {
	notDue := dueTick("MED")
	notDue.WaitDue = false
	inWork := tick("MED", "в работе")
	inWork.Executor = "m-master"
	inWork.Diff = "DO"
	for name, task := range map[string]Ticket{"не наступил": notDue, "в работе": inWork} {
		w := dueWorld()
		s := seeded(w)
		if ns := step(s, []Ticket{task}, w); len(ns) != 0 {
			t.Fatalf("%s: %+v", name, ns)
		}
	}
	w := dueWorld()
	w.free["m-master"] = false
	s := seeded(w)
	if ns := step(s, []Ticket{dueTick("MED")}, w); len(ns) != 0 {
		t.Fatalf("занят: %+v", ns)
	}
	w.free["m-master"] = true
	w.now = w.now.Add(time.Minute)
	if ns := step(s, []Ticket{dueTick("MED")}, w); count(ns, KindMorning) != 1 {
		t.Fatalf("освободился: %+v", ns)
	}
	w = dueWorld()
	w.now = at("2026-11-05T06:59:00+01:00")
	s = seeded(w)
	if ns := step(s, []Ticket{dueTick("MED")}, w); len(ns) != 0 {
		t.Fatalf("до границы: %+v", ns)
	}
	w = restWorld()
	s = seeded(w)
	if ns := step(s, []Ticket{inDir(dueTick("HIGH"), restRoot+"/p")}, w); len(ns) != 0 {
		t.Fatalf("выходной: %+v", ns)
	}
	if ns := step(s, []Ticket{inDir(dueTick("CRIT"), restRoot+"/p")}, w); count(ns, KindMorning) != 1 {
		t.Fatalf("выходной, CRIT: %+v", ns)
	}
}
