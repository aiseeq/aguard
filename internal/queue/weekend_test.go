package queue

import (
	"testing"
	"time"
)

// Суббота 07.11.2026, понедельник 09.11.2026: в папках из rest_crit только CRIT, остальным как обычно. День — по
// Europe/Nicosia (+02:00 зимой), на час впереди CET машины.
const (
	restRoot = "/h/work"
	saturday = "2026-11-07T10:00:00+01:00"
)

func restWorld() *world {
	w := newWorld(ModeOn)
	w.rest = cyRest(restRoot)
	w.now = at(saturday)
	w.free["m-master"] = true
	return w
}

func inDir(t Ticket, dir string) Ticket {
	t.Dir = dir
	return t
}

func TestWeekendHighWaitsForMonday(t *testing.T) {
	w := restWorld()
	s := seeded(w)
	ts := []Ticket{inDir(tick("HIGH", "открыт"), restRoot+"/p")}
	for _, later := range []time.Duration{0, 5 * time.Hour, 24 * time.Hour} {
		w.now = at(saturday).Add(later)
		if ns := step(s, ts, w); len(ns) != 0 {
			t.Fatalf("выходной +%v: %+v", later, ns)
		}
	}
	w.now = at("2026-11-08T23:59:30+02:00") // проходы идут раз в 30 с, последний в воскресенье — перед полуночью Никосии
	if ns := step(s, ts, w); len(ns) != 0 {
		t.Fatalf("конец воскресенья: %+v", ns)
	}
	w.now = at("2026-11-09T00:01:00+02:00")
	ns := step(s, ts, w)
	if !has(ns, KindHigh, "m-master") || has(ns, KindHighAdmin, "git-admin") {
		t.Fatalf("понедельник, срок админу с начала дня: %+v", ns)
	}
	w.now = at("2026-11-09T03:59:00+02:00")
	if ns := step(s, ts, w); has(ns, KindHighAdmin, "git-admin") {
		t.Fatalf("3 ч 58 мин: %+v", ns)
	}
	w.now = at("2026-11-09T04:02:00+02:00")
	if ns := step(s, ts, w); !has(ns, KindHighAdmin, "git-admin") {
		t.Fatalf("4 ч 01 мин: %+v", ns)
	}
}

func TestWeekendOtherDirAsUsual(t *testing.T) {
	for name, dir := range map[string]string{"другой корень": "/h/git/p", "похожее имя": restRoot + "-old/p"} {
		w := restWorld()
		s := seeded(w)
		ts := []Ticket{inDir(tick("HIGH", "открыт"), dir)}
		if ns := step(s, ts, w); !has(ns, KindHigh, "m-master") {
			t.Fatalf("%s: %+v", name, ns)
		}
		w.now = w.now.Add(4*time.Hour + time.Minute)
		if ns := step(s, ts, w); !has(ns, KindHighAdmin, "git-admin") {
			t.Fatalf("%s, 4 ч: %+v", name, ns)
		}
	}
}

func TestWeekdayRestDirAsUsual(t *testing.T) {
	w := restWorld()
	w.now = at("2026-11-06T22:30:00+01:00") // 23:30 пятницы в Никосии; 23:00 CET там уже суббота
	s := seeded(w)
	if ns := step(s, []Ticket{inDir(tick("HIGH", "открыт"), restRoot+"/p")}, w); !has(ns, KindHigh, "m-master") {
		t.Fatalf("пятница: %+v", ns)
	}
}

func TestWeekendCritGoes(t *testing.T) {
	w := restWorld()
	w.free["m-master"] = false
	s := seeded(w)
	ns := step(s, []Ticket{inDir(tick("CRIT", "открыт"), restRoot+"/p")}, w)
	if !has(ns, KindCrit, "m-master") || has(ns, KindCritCopy, "lead") {
		t.Fatalf("CRIT в выходной: %+v", ns)
	}
}

// Строка о закрытом — по получателю: мастеру папки выходного ждёт понедельника, мастеру другой папки и лиду идёт.
func TestWeekendBackByRecipient(t *testing.T) {
	w := restWorld()
	w.live["other-master"], w.free["other-master"], w.free["lead"] = true, true, true
	s := seeded(w)
	own := inDir(tick("MED", "открыт"), restRoot+"/p")
	closedTo := func(to, key string) Ticket {
		c := inDir(tick("HIGH", "готово"), "/h/git/q")
		c.Key, c.Back, c.Master = key, to, "other-master"
		return c
	}
	ts := []Ticket{own, closedTo("m-master", "q/to-rest"), closedTo("other-master", "q/to-other"), closedTo("lead", "q/to-lead")}
	ns := step(s, ts, w)
	if has(ns, KindBack, "m-master") || !has(ns, KindBack, "other-master") || !has(ns, KindBack, "lead") {
		t.Fatalf("суббота: %+v", ns)
	}
	w.now = at("2026-11-09T10:00:00+01:00")
	if ns := step(s, ts, w); !has(ns, KindBack, "m-master") {
		t.Fatalf("понедельник: %+v", ns)
	}
}

func TestWeekendCritConfigExpanded(t *testing.T) {
	c := Default("/h")
	c.RestCrit, c.RestZone, c.RestHolidays = []string{"~/work"}, "Europe/Nicosia", "CY"
	if err := c.Validate("/h"); err != nil {
		t.Fatal(err)
	}
	if len(c.RestCrit) != 1 || c.RestCrit[0] != "/h/work" {
		t.Fatalf("%+v", c.RestCrit)
	}
	if len(Default("/h").RestCrit) != 0 {
		t.Fatal("умолчание должно быть пустым: список — в конфиге машины")
	}
}

func TestJournalNoLines(t *testing.T) {
	w := newWorld(ModeOn)
	w.free["m-master"] = true
	s := seeded(w)
	for _, prio := range []string{"CRIT", "HIGH"} {
		j := tick(prio, "открыт")
		j.Key, j.Journal = "m/"+prio, true
		if ns := step(s, []Ticket{j}, w); len(ns) != 0 {
			t.Fatalf("%s открытый журнал: %+v", prio, ns)
		}
	}
	w.now = w.now.Add(5 * time.Hour)
	high := tick("HIGH", "открыт")
	high.Key, high.Journal = "m/HIGH", true
	if ns := step(s, []Ticket{high}, w); len(ns) != 0 {
		t.Fatalf("журнал старше 4 ч: %+v", ns)
	}
	closedJournal := tick("HIGH", "готово")
	closedJournal.Key, closedJournal.Journal, closedJournal.Back = "m/closed", true, "lead"
	w.free["lead"] = true
	if ns := step(s, []Ticket{closedJournal}, w); !has(ns, KindBack, "lead") {
		t.Fatalf("закрытый журнал заказчику: %+v", ns)
	}
}
