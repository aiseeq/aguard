package queue

import (
	"strings"
	"testing"
	"time"
)

// HIGH открыт 5 ч без записей мастера, мастер поставил перед ним другой тикет (tk set X --blocks Y): пока блокер движется
// («в работе» у живого исполнителя, «ждёт» до срока), письма админу «не взят» нет; блокер закрыт, отклонён, открыт,
// журнальный, у мёртвого исполнителя, ждёт без срока или после него, из чужой папки — письмо как раньше. Случай 23.11 10:06.
func TestHighAdminBlockedByMoving(t *testing.T) {
	blocker := func(status, executor, dir string) Ticket {
		b := tick("MED", status)
		b.Key, b.Slug, b.Executor, b.Dir = "m/20261104-0900-y", "y", executor, dir
		return b
	}
	overdue := func(b Ticket) Ticket { b.Overdue = true; return b }
	due := func(b Ticket) Ticket { b.WaitDue = true; return b }
	journal := func(b Ticket) Ticket { b.Journal = true; return b }
	cases := []struct {
		name   string
		block  Ticket
		letter bool
	}{
		{"в-работе", blocker("в работе", "m-master", "/h/m"), false},
		{"ждёт-у-исполнителя", due(blocker("ждёт", "m-master", "/h/m")), false},
		{"ждёт-без-срока", blocker("ждёт", "m-master", "/h/m"), true},
		{"готово", blocker("готово", "m-master", "/h/m"), true},
		{"отклонён", blocker("отклонён", "m-master", "/h/m"), true},
		{"открыт", blocker("открыт", "m-master", "/h/m"), true},
		{"ждёт-без-исполнителя-до-срока", due(blocker("ждёт", "", "/h/m")), false},
		{"ждёт-просрочен", overdue(due(blocker("ждёт", "m-master", "/h/m"))), true},
		{"исполнитель-мёртв", blocker("в работе", "dead-master", "/h/m"), true},
		{"журнальный", journal(blocker("в работе", "m-master", "/h/m")), true},
		{"другая-папка", blocker("в работе", "m-master", "/h/other"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(ModeOn)
			w.live["git-admin"], w.free["git-admin"], w.free["m-master"] = true, true, true
			s := seeded(w)
			x := tick("HIGH", "открыт")
			x.Dir = "/h/m"
			x.BlockedBy = []string{"z", "y"} // движется второй
			ts := []Ticket{x, c.block}
			if ns := step(s, ts, w); !has(ns, KindHigh, "m-master") {
				t.Fatalf("первое письмо мастеру идёт и заблокированному: %+v", ns)
			}
			w.now = w.now.Add(5 * time.Hour)
			if ns := step(s, ts, w); has(ns, KindHighAdmin, "git-admin") != c.letter {
				t.Fatalf("письмо админу = %v, ждали %v: %+v", !c.letter, c.letter, ns)
			}
		})
	}
	t.Run("блокер-закрылся-позже", func(t *testing.T) {
		w := newWorld(ModeOn)
		w.live["git-admin"], w.free["git-admin"] = true, true
		s := seeded(w)
		x := tick("HIGH", "открыт")
		x.Dir = "/h/m"
		x.BlockedBy = []string{"y"}
		b := blocker("в работе", "m-master", "/h/m")
		step(s, []Ticket{x, b}, w)
		w.now = w.now.Add(5 * time.Hour)
		if ns := step(s, []Ticket{x, b}, w); has(ns, KindHighAdmin, "git-admin") {
			t.Fatalf("блокер в работе — без письма: %+v", ns)
		}
		b.Status = "готово"
		w.now = w.now.Add(time.Minute)
		if ns := step(s, []Ticket{x, b}, w); !has(ns, KindHighAdmin, "git-admin") {
			t.Fatalf("блокер закрыт — письмо: %+v", ns)
		}
	})
}

func TestHighAdminCrossProjectBlockers(t *testing.T) {
	home := "/srv/u7"
	makeWorld := func() *world {
		w := newWorld(ModeOn)
		w.home = home
		w.live["git-admin"], w.free["git-admin"], w.free["m-master"] = true, true, true
		return w
	}
	check := func(t *testing.T, ref, status string, includeBlocker, moving, wantLetter, wantMissing bool) {
		t.Helper()
		w := makeWorld()
		s := seeded(w)
		x := tick("HIGH", "открыт")
		x.Dir, x.BlockedBy = home+"/git/main", []string{ref}
		ts := []Ticket{x}
		if includeBlocker {
			b := tick("MED", status)
			b.Key, b.Project, b.Slug, b.Dir, b.Executor = "other/20261124-1057-slug-x", "other", "slug-x", home+"/git/other", "other-master"
			if !moving {
				b.Executor = ""
			}
			w.live["other-master"] = true
			ts = append(ts, b)
		}
		step(s, ts, w)
		w.now = w.now.Add(5 * time.Hour)
		var admin Notice
		for _, n := range step(s, ts, w) {
			if n.Kind == KindHighAdmin {
				admin = n
			}
		}
		if (admin.Kind == KindHighAdmin) != wantLetter {
			t.Fatalf("письмо админу = %v, ждали %v: %+v", admin.Kind == KindHighAdmin, wantLetter, admin)
		}
		if wantLetter && strings.Contains(admin.Line, "связь "+ref+" не найдена") != wantMissing || !wantMissing && strings.Contains(admin.Line, "не найдена") {
			t.Fatalf("строка письма = %q, пометка не найдена = %v", admin.Line, wantMissing)
		}
	}
	t.Run("git движется по slug", func(t *testing.T) {
		check(t, "git/other/slug-x", "в работе", true, true, false, false)
	})
	t.Run("git закрыт", func(t *testing.T) {
		check(t, "git/other/slug-x", "готово", true, false, true, false)
	})
	t.Run("git не найден", func(t *testing.T) {
		check(t, "git/other/slug-x", "", false, false, true, true)
	})
	if dir, id, ok := blockerLocation("work/project/task", Ticket{}, home); !ok || dir != home+"/work/project" || id != "task" {
		t.Fatalf("work-ссылка разобрана как %q, %q, %v", dir, id, ok)
	}
}
