package queue

import (
	"encoding/json"
	"testing"
	"time"
)

// За выходные состояние пишется один раз: иначе state.json переписывался бы каждые 30 с.
func TestWeekendStateStable(t *testing.T) {
	w := restWorld()
	s := seeded(w)
	ts := []Ticket{inDir(tick("HIGH", "открыт"), restRoot+"/p")}
	step(s, ts, w)
	first, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, later := range []time.Duration{30 * time.Second, 30 * time.Hour} {
		w.now = at(saturday).Add(later)
		step(s, ts, w)
		got, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(first) {
			t.Fatalf("+%v: состояние изменилось\n%s\n%s", later, first, got)
		}
	}
}

// Закрыт в субботу, семь суток приходятся на следующую субботу: получатель на выходном, срок не идёт, строка в понедельник.
func TestWeekendBackNotLost(t *testing.T) {
	w := restWorld()
	s := seeded(w)
	own := inDir(tick("MED", "открыт"), restRoot+"/p")
	c := inDir(tick("HIGH", "готово"), "/h/git/q")
	c.Key, c.Back, c.Master = "q/closed", "m-master", "other-master"
	ts := []Ticket{own, c}
	w.now = at("2026-11-07T00:10:00+01:00")
	step(s, ts, w)
	w.now = at("2026-11-14T00:11:00+01:00")
	if ns := step(s, ts, w); has(ns, KindBackLost, "") || has(ns, KindBack, "m-master") {
		t.Fatalf("следующая суббота: %+v", ns)
	}
	w.now = at("2026-11-16T10:00:00+01:00")
	if ns := step(s, ts, w); !has(ns, KindBack, "m-master") || has(ns, KindBackLost, "") {
		t.Fatalf("понедельник: %+v", ns)
	}
}

func TestTransferLine(t *testing.T) {
	w := newWorld(ModeOn)
	w.free["lead"] = true
	s := seeded(w)
	tr := Ticket{Key: "m/20261104-0950-x", Project: "m", Slug: "x", Status: StatusTransferred, Back: "lead", Master: "m-master", Transfer: "TRK-7 https://x/TRK-7"}
	ns := step(s, []Ticket{tr}, w)
	if len(ns) != 1 || ns[0].Kind != KindBack || ns[0].To != "lead" || ns[0].Line != "aguard: перенесён m/x → TRK-7 https://x/TRK-7" {
		t.Fatalf("%+v", ns)
	}
	if ns := step(s, []Ticket{tr}, w); len(ns) != 0 {
		t.Fatalf("повтор: %+v", ns)
	}
	w2 := newWorld(ModeOn)
	w2.live["lead"] = false
	s2 := seeded(w2)
	step(s2, []Ticket{tr}, w2)
	w2.now = w2.now.Add(7*24*time.Hour + time.Minute)
	if ns := step(s2, []Ticket{tr}, w2); !has(ns, KindBackLost, "") {
		t.Fatalf("потеря: %+v", ns)
	}
}

// Строка о закрытом ждёт понедельника: состояние за выходные пишется один раз.
func TestWeekendBackStateStable(t *testing.T) {
	w := restWorld()
	s := seeded(w)
	c := inDir(tick("HIGH", "готово"), "/h/git/q")
	c.Key, c.Back, c.Master = "q/closed", "m-master", "other-master"
	ts := []Ticket{inDir(tick("MED", "открыт"), restRoot+"/p"), c}
	step(s, ts, w)
	first, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	w.now = at(saturday).Add(30 * time.Second)
	step(s, ts, w)
	got, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(first) {
		t.Fatalf("состояние изменилось\n%s\n%s", first, got)
	}
}

// Лид — мастер проекта в папке выходного: строка о закрытом ему всё равно идёт.
func TestWeekendBackLeadNotResting(t *testing.T) {
	w := restWorld()
	w.free["lead"] = true
	s := seeded(w)
	own := inDir(tick("MED", "открыт"), restRoot+"/p")
	own.Master = "lead"
	c := inDir(tick("HIGH", "готово"), "/h/git/q")
	c.Key, c.Back, c.Master = "q/closed", "lead", "other-master"
	if ns := step(s, []Ticket{own, c}, w); !has(ns, KindBack, "lead") {
		t.Fatalf("суббота: %+v", ns)
	}
}

// HIGH в работе в выходной, в понедельник вернулся в «открыт»: срок 4 ч считается от возврата.
func TestWeekendTakenBackToOpen(t *testing.T) {
	w := restWorld()
	s := seeded(w)
	work := inDir(tick("HIGH", "в работе"), restRoot+"/p")
	step(s, []Ticket{work}, w)
	w.now = at("2026-11-09T10:00:00+01:00")
	ns := step(s, []Ticket{inDir(tick("HIGH", "открыт"), restRoot+"/p")}, w)
	if !has(ns, KindHigh, "m-master") || has(ns, KindHighAdmin, "git-admin") {
		t.Fatalf("понедельник: %+v", ns)
	}
}

func TestSpacesConfigRoots(t *testing.T) {
	for name, spaces := range map[string]map[string]Level{
		"относительный корень": {"work": LevelMed},
		"два написания":        {"~/work": LevelMed, "/h/work/": LevelLow},
	} {
		c := Default("/h")
		c.Spaces = spaces
		if err := c.Validate("/h"); err == nil {
			t.Errorf("%s принято: %v", name, c.Spaces)
		}
	}
	c := Default("/h")
	c.WeekendCrit = []string{"/h/work/"}
	if err := c.Validate("/h"); err != nil {
		t.Fatal(err)
	}
	if c.WeekendCrit[0] != "/h/work" {
		t.Fatalf("корень с косой чертой: %v", c.WeekendCrit)
	}
}
