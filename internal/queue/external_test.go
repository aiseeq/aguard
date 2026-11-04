package queue

import (
	"strings"
	"testing"
	"time"
)

func extTask(prio, status string) ExternalTask {
	return ExternalTask{Key: "TRK-1", Title: "внешняя", Status: status, Prio: prio, FirstSeen: at("2026-11-04T09:00:00+01:00")}
}

func extTicket(prio, status string) Ticket {
	return extTask(prio, status).Ticket("m", "/h/work/m", "m-master", "git-admin")
}

func count(ns []Notice, kind string) int {
	n := 0
	for _, x := range ns {
		if x.Kind == kind {
			n++
		}
	}
	return n
}

// Постановка: High без местного файла даёт одно письмо мастеру, повторно не дублирует.
func TestExternalHighOnce(t *testing.T) {
	w := newWorld(ModeOn)
	w.free["m-master"] = true
	s := seeded(w)
	ts := []Ticket{extTicket("HIGH", "To Do")}
	ns := step(s, ts, w)
	if count(ns, KindHigh) != 1 || !has(ns, KindHigh, "m-master") {
		t.Fatalf("первое письмо: %+v", ns)
	}
	if !strings.Contains(ns[0].Line, "m/TRK-1") || !strings.Contains(ns[0].Line, "tk next") {
		t.Fatalf("строка: %q", ns[0].Line)
	}
	w.now = w.now.Add(time.Minute)
	if ns := step(s, ts, w); len(ns) != 0 {
		t.Fatalf("повтор: %+v", ns)
	}
}

// Занятому мастеру письмо ждёт, пока он освободится; задача уже в работе — тоже письмо, живой случай постановки.
func TestExternalStartedWaitsForFree(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	ts := []Ticket{extTicket("HIGH", "In Progress")}
	w.free["m-master"] = false
	if ns := step(s, ts, w); len(ns) != 0 {
		t.Fatalf("мастер занят: %+v", ns)
	}
	w.free["m-master"] = true
	w.now = w.now.Add(time.Minute)
	if ns := step(s, ts, w); count(ns, KindHigh) != 1 {
		t.Fatalf("освободился: %+v", ns)
	}
}

// To Do → In Progress: письмо уже было, второго нет. В работе 5 ч: админу не «не взят». Вернулась в To Do: отсчёт 4 ч заново, письмо мастеру не повторяется.
func TestExternalStartedNoAdmin(t *testing.T) {
	w := newWorld(ModeOn)
	w.free["m-master"] = true
	s := seeded(w)
	if ns := step(s, []Ticket{extTicket("HIGH", "To Do")}, w); count(ns, KindHigh) != 1 {
		t.Fatalf("письмо: %+v", ns)
	}
	started := []Ticket{extTicket("HIGH", "In Progress")}
	w.now = w.now.Add(5 * time.Minute)
	if ns := step(s, started, w); len(ns) != 0 {
		t.Fatalf("начата: %+v", ns)
	}
	w.now = w.now.Add(5 * time.Hour)
	if ns := step(s, started, w); len(ns) != 0 {
		t.Fatalf("в работе 5 ч: %+v", ns)
	}
	back := []Ticket{extTicket("HIGH", "To Do")}
	w.now = w.now.Add(5 * time.Minute)
	if ns := step(s, back, w); len(ns) != 0 {
		t.Fatalf("вернулась: %+v", ns)
	}
	w.now = w.now.Add(4*time.Hour + time.Minute)
	if ns := step(s, back, w); count(ns, KindHighAdmin) != 1 || count(ns, KindHigh) != 0 {
		t.Fatalf("4 ч после возврата: %+v", ns)
	}
}

// Мастера нет: админу сразу и о начатой, отметка одна.
func TestExternalStartedNoMaster(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	w.live["m-master"] = false
	ts := []Ticket{extTicket("HIGH", "In Progress")}
	if ns := step(s, ts, w); count(ns, KindHighAdmin) != 1 {
		t.Fatalf("нет мастера: %+v", ns)
	}
	w.now = w.now.Add(time.Minute)
	if ns := step(s, ts, w); len(ns) != 0 {
		t.Fatalf("повтор: %+v", ns)
	}
}

// В выходной в папках weekend_crit только CRIT, в понедельник строка идёт.
func TestExternalWeekend(t *testing.T) {
	w := restWorld()
	s := seeded(w)
	ts := []Ticket{inDir(extTicket("HIGH", "In Progress"), restRoot+"/m")}
	if ns := step(s, ts, w); len(ns) != 0 {
		t.Fatalf("суббота: %+v", ns)
	}
	w.now = at("2026-11-09T09:00:00+01:00")
	if ns := step(s, ts, w); count(ns, KindHigh) != 1 {
		t.Fatalf("понедельник: %+v", ns)
	}
}

// У внешней задачи нет файла: строка CRIT не посылает в tk show.
func TestExternalCritLine(t *testing.T) {
	w := newWorld(ModeOn)
	w.free["m-master"] = true
	s := seeded(w)
	ns := step(s, []Ticket{extTicket("CRIT", "To Do")}, w)
	if count(ns, KindCrit) != 1 || strings.Contains(ns[0].Line, "tk show") || !strings.Contains(ns[0].Line, "tk next") {
		t.Fatalf("CRIT: %+v", ns)
	}
}

// Противник, сценарий 1: задача появилась днём — утренней строки сегодня нет, завтра есть; при закрытых воротах нет и завтра.
func TestExternalMorning(t *testing.T) {
	task := extTask("MED", "To Do")
	task.FirstSeen = at("2026-11-04T15:45:00+01:00")
	ts := []Ticket{task.Ticket("m", "/h/work/m", "m-master", "git-admin")}
	w := newWorld(ModeOn)
	w.free["m-master"] = true
	w.open = []string{"MED-DO", "MED-HUNT", "MED-ARCH"}
	w.now = at("2026-11-04T15:46:00+01:00")
	s := seeded(w)
	if ns := step(s, ts, w); count(ns, KindMorning) != 0 {
		t.Fatalf("днём: %+v", ns)
	}
	w.now = at("2026-11-05T07:01:00+01:00")
	if ns := step(s, ts, w); count(ns, KindMorning) != 1 {
		t.Fatalf("утром: %+v", ns)
	}
	w = newWorld(ModeOn)
	w.free["m-master"] = true
	w.open = []string{"MED-DO"} // без сложности видна, только когда открыты все пары приоритета, как у tk next
	w.now = at("2026-11-05T07:01:00+01:00")
	s = seeded(w)
	if ns := step(s, ts, w); count(ns, KindMorning) != 0 {
		t.Fatalf("закрыта пара: %+v", ns)
	}
}

func TestSetExternalKeepsFirstSeen(t *testing.T) {
	var s State
	t1, t2 := at("2026-11-04T10:00:00+01:00"), at("2026-11-04T10:05:00+01:00")
	s.SetExternal("/p", []ExternalTask{{Key: "A"}}, t1)
	s.SetExternal("/p", []ExternalTask{{Key: "A", Title: "новый"}, {Key: "B"}}, t2)
	got := s.External["/p"]
	if len(got) != 2 || !got[0].FirstSeen.Equal(t1) || got[0].Title != "новый" || !got[1].FirstSeen.Equal(t2) {
		t.Fatalf("%+v", got)
	}
	s.SetExternal("/p", nil, t2)
	if _, ok := s.External["/p"]; ok {
		t.Fatalf("пустой ответ оставил список: %+v", s.External)
	}
}

func TestKeepExternal(t *testing.T) {
	var s State
	now := at("2026-11-04T10:00:00+01:00")
	s.SetExternal("/a", []ExternalTask{{Key: "A"}}, now)
	s.SetExternal("/b", []ExternalTask{{Key: "B"}}, now)
	s.KeepExternal([]string{"/a"})
	if len(s.External) != 1 || s.External["/a"] == nil {
		t.Fatalf("%+v", s.External)
	}
}
