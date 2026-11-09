package queue

import (
	"encoding/json"
	"slices"
	"testing"
	"time"
)

func spaceTable() Table {
	var t Table
	for _, pair := range []string{"MED-DO", "MED-HUNT", "MED-ARCH", "LOW-DO", "LOW-HUNT", "LOW-ARCH"} {
		t = append(t, TableRow{Pair: pair})
	}
	return t
}

func TestSpaceOpen(t *testing.T) {
	medAll := []string{"MED-DO", "MED-HUNT", "MED-ARCH"}
	all := append(slices.Clone(medAll), "LOW-DO", "LOW-HUNT", "LOW-ARCH")
	for name, c := range map[string]struct {
		open  []string
		shift int
		want  []string
	}{
		"сдвиг 0 повторяет общие":             {[]string{"MED-DO", "LOW-DO"}, 0, []string{"MED-DO", "LOW-DO"}},
		"сдвиг 0 при закрытых":                {nil, 0, []string{}},
		"сдвиг 1 при override «только HIGH»":  {nil, 1, medAll},
		"сдвиг 1: LOW как MED того же DIFF":   {[]string{"MED-HUNT"}, 1, append(slices.Clone(medAll), "LOW-HUNT")},
		"сдвиг 1: LOW общих MED не открывает": {[]string{"LOW-DO"}, 1, medAll},
		"сдвиг 2 открывает всё":               {nil, 2, all},
	} {
		got := SpaceOpen(spaceTable(), c.open, map[string]int{"/h/work": c.shift}, false)["/h/work"]
		if !slices.Equal(got, c.want) || got == nil {
			t.Errorf("%s: %v, нужно %v", name, got, c.want)
		}
	}
	if got := SpaceOpen(spaceTable(), nil, nil, false); got == nil || len(got) != 0 {
		t.Errorf("без пространств — пустой объект: %v", got)
	}
}

func TestSpaceOpenNegativeMargin(t *testing.T) {
	spaces := map[string]int{"/work": 1}
	table := spaceTable()
	cases := []struct {
		name   string
		supply Supply
		other  *Supply
		open   []string
		want   bool
	}{
		{"отрицательный", Supply{Margin: -1}, nil, nil, false},
		{"восстановился", Supply{Margin: 0}, nil, nil, true},
		{"сгорает", Supply{Margin: -1, Burning: true}, nil, nil, true},
		{"override", Supply{Margin: -1}, nil, []string{"MED-DO"}, true},
		{"второй поставщик", Supply{Margin: 1}, &Supply{Margin: -1}, nil, false},
		{"второй сгорает", Supply{Margin: 1}, &Supply{Margin: -1, Burning: true}, nil, true},
	}
	for _, c := range cases {
		got := SpaceOpen(table, c.open, spaces, NegativeMargin(c.supply, c.other))["/work"]
		if slices.Contains(got, "MED-DO") != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}

func TestSpacesConfig(t *testing.T) {
	c := Default("/h")
	c.Spaces = map[string]int{"~/work/": 1}
	if err := c.Validate("/h"); err != nil {
		t.Fatal(err)
	}
	if c.Spaces["/h/work"] != 1 {
		t.Fatalf("корень не раскрыт: %v", c.Spaces)
	}
	for _, bad := range []int{-1, MaxSpaceShift + 1} {
		c := Default("/h")
		c.Spaces = map[string]int{"~/work": bad}
		if err := c.Validate("/h"); err == nil {
			t.Fatalf("сдвиг %d принят", bad)
		}
	}
}

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
	for name, spaces := range map[string]map[string]int{
		"относительный корень": {"work": 1},
		"два написания":        {"~/work": 1, "/h/work/": 2},
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
