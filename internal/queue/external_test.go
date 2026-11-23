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

// В выходной в папках rest_crit только CRIT, в понедельник строка идёт.
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

// tk transfer перенёс тикет во внешний трекер: задача приходит под ключом трекера, но это та же работа — отметки писем
// она берёт у перенесённого тикета. Порядок как в работе: проход с местным тикетом, проход с одной строкой переноса
// (приоритета у неё нет), затем задача из трекера. Письма, которого у тикета не было, задача шлёт как новая. Случай 23.11.
func TestExternalTransferredInheritsMarks(t *testing.T) {
	moved := func(prio, back string) (Ticket, Ticket) {
		local := tick(prio, "открыт")
		gone := local
		gone.Prio, gone.Status, gone.Back, gone.Transfer = "", StatusTransferred, back, "TRK-1 https://tracker/TRK-1"
		return local, gone
	}
	ext := func(prio string) Ticket { return extTicket(prio, "To Do") }
	keyed := func(ns []Notice, key, kind string) bool {
		for _, n := range ns {
			if n.Key == key && n.Kind == kind {
				return true
			}
		}
		return false
	}
	// transfer — проход с одной строкой переноса, затем проход с задачей; возвращает письма второго.
	transfer := func(s *State, w *world, gone Ticket, rest ...Ticket) []Notice {
		w.now = w.now.Add(time.Minute)
		step(s, []Ticket{gone}, w)
		w.now = w.now.Add(5 * time.Minute)
		return step(s, append([]Ticket{gone}, rest...), w)
	}
	for _, back := range []string{"c-master", ""} {
		t.Run("high-sent/back="+back, func(t *testing.T) {
			w := newWorld(ModeOn)
			w.free["m-master"] = true
			s := seeded(w)
			local, gone := moved("HIGH", back)
			if ns := step(s, []Ticket{local}, w); !keyed(ns, local.Key, KindHigh) {
				t.Fatalf("местный HIGH — письмо: %+v", ns)
			}
			if ns := transfer(s, w, gone, ext("HIGH")); keyed(ns, "m/TRK-1", KindHigh) {
				t.Fatalf("перенесённый HIGH — без повторного первого письма: %+v", ns)
			}
		})
		t.Run("crit-sent/back="+back, func(t *testing.T) {
			w := newWorld(ModeOn)
			w.free["m-master"] = true
			s := seeded(w)
			local, gone := moved("CRIT", back)
			if ns := step(s, []Ticket{local}, w); !keyed(ns, local.Key, KindCrit) {
				t.Fatalf("местный CRIT — письмо: %+v", ns)
			}
			if ns := transfer(s, w, gone, ext("CRIT")); keyed(ns, "m/TRK-1", KindCrit) || keyed(ns, "m/TRK-1", KindCritCopy) {
				t.Fatalf("перенесённый CRIT — без повтора: %+v", ns)
			}
		})
	}
	t.Run("high-waited-busy-master", func(t *testing.T) {
		w := newWorld(ModeOn)
		s := seeded(w)
		local, gone := moved("HIGH", "")
		if ns := step(s, []Ticket{local}, w); keyed(ns, local.Key, KindHigh) {
			t.Fatalf("мастер занят — письмо ждёт: %+v", ns)
		}
		w.free["m-master"] = true
		if ns := transfer(s, w, gone, ext("HIGH")); !keyed(ns, "m/TRK-1", KindHigh) {
			t.Fatalf("письма не было — задача шлёт его: %+v", ns)
		}
	})
	t.Run("admin-deadline", func(t *testing.T) {
		w := newWorld(ModeOn)
		w.free["m-master"], w.live["git-admin"], w.free["git-admin"] = true, true, true
		s := seeded(w)
		local, gone := moved("HIGH", "")
		step(s, []Ticket{local}, w)
		w.now = w.now.Add(highWait + time.Minute)
		if ns := step(s, []Ticket{local}, w); !keyed(ns, local.Key, KindHighAdmin) {
			t.Fatalf("местный HIGH не взят за срок — письмо админу: %+v", ns)
		}
		for _, n := range transfer(s, w, gone, ext("HIGH")) {
			if n.Key == "m/TRK-1" {
				t.Fatalf("после переноса админ второй раз не получает «не взят»: %+v", n)
			}
		}
		// Срок админу идёт от местного тикета, а не от прихода задачи.
		w2 := newWorld(ModeOn)
		w2.free["m-master"], w2.live["git-admin"], w2.free["git-admin"] = true, true, true
		s2 := seeded(w2)
		step(s2, []Ticket{local}, w2)
		start := w2.now
		w2.now = w2.now.Add(time.Hour)
		transfer(s2, w2, gone, ext("HIGH"))
		w2.now = start.Add(highWait + time.Minute)
		if ns := step(s2, []Ticket{gone, ext("HIGH")}, w2); !keyed(ns, "m/TRK-1", KindHighAdmin) {
			t.Fatalf("срок от местного тикета — письмо админу: %+v", ns)
		}
	})
	t.Run("taken-then-todo", func(t *testing.T) {
		// Местный HIGH был в работе дольше срока: задача «To Do» начинает срок админу заново, а не шлёт «не взят» сразу.
		w := newWorld(ModeOn)
		w.free["m-master"], w.live["git-admin"], w.free["git-admin"] = true, true, true
		s := seeded(w)
		local, gone := moved("HIGH", "")
		step(s, []Ticket{local}, w)
		local.Status = "в работе"
		w.now = w.now.Add(time.Minute)
		step(s, []Ticket{local}, w)
		w.now = w.now.Add(highWait + time.Hour)
		step(s, []Ticket{local}, w)
		if ns := transfer(s, w, gone, ext("HIGH")); keyed(ns, "m/TRK-1", KindHighAdmin) {
			t.Fatalf("взятый тикет после переноса — без ложного «не взят»: %+v", ns)
		}
	})
	t.Run("other-project-same-key", func(t *testing.T) {
		w := newWorld(ModeOn)
		w.free["m-master"], w.free["n-master"], w.live["n-master"] = true, true, true
		s := seeded(w)
		local, gone := moved("HIGH", "")
		local.Key, local.Project, local.Dir, local.Master = "n/20261104-0950-x", "n", "/h/work/n", "n-master"
		gone.Key, gone.Project, gone.Dir, gone.Master = local.Key, "n", "/h/work/n", "n-master"
		if ns := step(s, []Ticket{local}, w); !keyed(ns, local.Key, KindHigh) {
			t.Fatalf("местный HIGH в n — письмо: %+v", ns)
		}
		inN := extTask("HIGH", "To Do").Ticket("n", "/h/work/n", "n-master", "git-admin")
		ns := transfer(s, w, gone, inN, ext("HIGH"))
		if keyed(ns, "n/TRK-1", KindHigh) {
			t.Fatalf("перенос в n — задача n/TRK-1 без повтора: %+v", ns)
		}
		if !keyed(ns, "m/TRK-1", KindHigh) {
			t.Fatalf("тот же ключ в проекте m — новая задача: %+v", ns)
		}
	})
	t.Run("no-transfer-line", func(t *testing.T) {
		w := newWorld(ModeOn)
		w.free["m-master"] = true
		s := seeded(w)
		if ns := step(s, []Ticket{ext("HIGH")}, w); !keyed(ns, "m/TRK-1", KindHigh) {
			t.Fatalf("внешняя без переноса — письмо, как прежде: %+v", ns)
		}
	})
	t.Run("marks-expire", func(t *testing.T) {
		w := newWorld(ModeOn)
		s := seeded(w)
		local, gone := moved("HIGH", "c-master")
		step(s, []Ticket{local}, w)
		w.now = w.now.Add(time.Minute)
		step(s, []Ticket{gone}, w)
		if len(s.Transferred) != 1 {
			t.Fatalf("строка переноса снята: %+v", s.Transferred)
		}
		w.now = w.now.Add(transferKeep + time.Minute)
		step(s, []Ticket{gone}, w) // строка переноса вечная, состояние тикета (с заказчиком) живо
		if len(s.Transferred) != 0 {
			t.Fatalf("после transferKeep убрана: %+v", s.Transferred)
		}
		w.now = w.now.Add(time.Minute)
		step(s, []Ticket{gone}, w)
		if len(s.Transferred) != 0 {
			t.Fatalf("не снята заново с состояния без приоритета: %+v", s.Transferred)
		}
	})
}
