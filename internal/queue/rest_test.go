package queue

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// cyRest — дни отдыха как на машине: Кипр, пояс Europe/Nicosia.
func cyRest(roots ...string) Rest {
	r, err := NewRest(roots, "Europe/Nicosia", "CY")
	if err != nil {
		panic(err)
	}
	return r
}

// Приёмка work-holiday-gate: суббота, праздник Кипра и будний день — планка ~/work CRIT / CRIT / обычная; ~/git не трогается.
func TestRestGateBar(t *testing.T) {
	spaces := map[string]Level{"/h/work": LevelMed, "/h/git": LevelMed}
	rest := cyRest("/h/work")
	for _, c := range []struct{ name, now, why string }{
		{"суббота", "2026-11-14T12:00:00+01:00", "суббота"},
		{"Рождество, пятница", "2026-12-25T12:00:00+01:00", "праздник: Χριστούγεννα"},
		{"Страстная пятница 2027 (православная Пасха 2 мая)", "2027-04-30T12:00:00+02:00", "праздник: Μεγάλη Παρασκευή"},
		{"Катаклизмос 2027, понедельник", "2027-06-21T12:00:00+02:00", "праздник: Αγίου Πνεύματος"},
		{"День независимости, четверг", "2026-10-01T12:00:00+02:00", "праздник: Ανεξαρτησία της Κύπρου"},
		{"будний день", "2026-11-12T12:00:00+01:00", ""},
		{"католическая Страстная пятница 2027 — рабочий", "2027-03-26T12:00:00+01:00", ""},
	} {
		g := UserGate(LevelHigh, spaces, rest, at(c.now))
		if got := g.CritOnly["/h/work"]; got != c.why {
			t.Errorf("%s: crit_only %q, нужно %q", c.name, got, c.why)
		}
		wantWork := LevelMed.Pairs()
		if c.why != "" {
			wantWork = []string{}
		}
		if !slices.Equal(g.OpenByRoot["/h/work"], wantWork) || !slices.Equal(g.OpenByRoot["/h/git"], LevelMed.Pairs()) {
			t.Errorf("%s: open_by_root %v", c.name, g.OpenByRoot)
		}
		if _, ok := g.CritOnly["/h/git"]; ok {
			t.Errorf("%s: ~/git в crit_only", c.name)
		}
	}
}

// Сутки — по Никосии: 23:30 CET 24.12 там уже Рождество, 22:30 CET — ещё нет.
func TestRestDayByNicosia(t *testing.T) {
	rest := cyRest("/h/work")
	if why := rest.Day(at("2026-12-24T23:30:00+01:00")); why == "" {
		t.Error("00:30 25.12 в Никосии — праздник")
	}
	if why := rest.Day(at("2026-12-24T22:30:00+01:00")); why != "" {
		t.Errorf("23:30 24.12 в Никосии: %q", why)
	}
	if why := rest.Day(at("2027-06-25T23:30:00+02:00")); why != "суббота" { // лето: Никосия +03:00, 00:30 субботы
		t.Errorf("летом 23:30 CEST пятницы — 00:30 субботы в Никосии: %q", why)
	}
}

// Вложенный корень spaces внутри корня отдыха тоже закрыт: tk берёт самый длинный корень (сценарий противника №4).
func TestRestGateNestedRoot(t *testing.T) {
	spaces := map[string]Level{"/h/work": LevelMed, "/h/work/client": LevelLow, "/h/workshop": LevelLow}
	g := UserGate(LevelHigh, spaces, cyRest("/h/work"), at("2026-11-14T12:00:00+01:00"))
	if g.CritOnly["/h/work/client"] != "суббота" || len(g.OpenByRoot["/h/work/client"]) != 0 {
		t.Fatalf("вложенный корень открыт: %v %v", g.CritOnly, g.OpenByRoot)
	}
	if _, ok := g.CritOnly["/h/workshop"]; ok || len(g.OpenByRoot["/h/workshop"]) == 0 {
		t.Fatalf("похожее имя закрыто: %v %v", g.CritOnly, g.OpenByRoot)
	}
}

// Корень отдыха вне spaces попадает в open_by_root: иначе tk взял бы для него общую планку open.
func TestRestGateRootOutsideSpaces(t *testing.T) {
	g := UserGate(LevelLow, nil, cyRest("/h/work"), at("2026-11-14T12:00:00+01:00"))
	if pairs, ok := g.OpenByRoot["/h/work"]; !ok || len(pairs) != 0 || g.CritOnly["/h/work"] == "" {
		t.Fatalf("%v %v", g.OpenByRoot, g.CritOnly)
	}
	if g := UserGate(LevelLow, nil, cyRest("/h/work"), at("2026-11-12T12:00:00+01:00")); len(g.OpenByRoot) != 0 || len(g.CritOnly) != 0 {
		t.Fatalf("будний день: %v %v", g.OpenByRoot, g.CritOnly)
	}
}

// Конец цепочки дней отдыха — 00:00 Никосии первого рабочего дня, одно значение на всю цепочку.
func TestRestEnd(t *testing.T) {
	rest := cyRest("/h/work")
	for _, c := range []struct{ now, want string }{
		{"2026-12-25T10:00:00+01:00", "2026-12-28T00:00:00+02:00"}, // пт Рождество, сб 26.12, вс → пн
		{"2026-12-27T20:00:00+01:00", "2026-12-28T00:00:00+02:00"},
		{"2026-04-10T10:00:00+02:00", "2026-04-14T00:00:00+03:00"}, // Страстная пятница → вт после Пасхального понедельника
		{"2026-11-07T10:00:00+01:00", "2026-11-09T00:00:00+02:00"},
	} {
		if got := rest.End(at(c.now)); !got.Equal(at(c.want)) {
			t.Errorf("%s: %v, нужно %s", c.now, got, c.want)
		}
	}
}

func TestRestConfigErrors(t *testing.T) {
	for name, c := range map[string]struct {
		roots          []string
		zone, holidays string
		err            string
	}{
		"без пояса":             {[]string{"/h/work"}, "", "CY", "rest_zone"},
		"неизвестный пояс":      {[]string{"/h/work"}, "Mars/Base", "CY", "rest_zone"},
		"неизвестный календарь": {[]string{"/h/work"}, "Europe/Nicosia", "GR", "rest_holidays"},
		"без календаря":         {[]string{"/h/work"}, "Europe/Nicosia", "", "rest_holidays"},
		"пояс без корней":       {nil, "Europe/Nicosia", "CY", "rest_crit"},
	} {
		if _, err := NewRest(c.roots, c.zone, c.holidays); err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if r, err := NewRest(nil, "", ""); err != nil || r.Day(time.Now()) != "" {
		t.Fatalf("пусто: %v", err)
	}
}
