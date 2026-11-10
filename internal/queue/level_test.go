package queue

import (
	"slices"
	"strings"
	"testing"
)

// Уровни ворот — только слово пользователя в конфиге (10.11: «Мы просто меняем планку по моему слову без всяких таймеров»).

func TestLevelPairs(t *testing.T) {
	med := []string{"MED-DO", "MED-HUNT", "MED-ARCH"}
	all := []string{"MED-DO", "MED-HUNT", "MED-ARCH", "LOW-DO", "LOW-HUNT", "LOW-ARCH"}
	for level, want := range map[Level][]string{LevelHigh: {}, LevelMed: med, LevelLow: all, "": {}} {
		if got := level.Pairs(); !slices.Equal(got, want) || got == nil {
			t.Errorf("%q: %v, нужно %v", level, got, want)
		}
	}
	if got := append(LevelMed.Pairs(), "X"); got[3] != "X" || LevelLow.Pairs()[3] != "LOW-DO" {
		t.Fatalf("список ступени — копия: дописанный вызывающим не меняет следующий ответ: %v", LevelLow.Pairs())
	}
}

func TestParseLevel(t *testing.T) {
	for _, ok := range []string{"HIGH", "MED", "LOW"} {
		if l, err := ParseLevel(ok); err != nil || string(l) != ok {
			t.Errorf("%s: %q %v", ok, l, err)
		}
	}
	for _, bad := range []string{"", "1", "MEDIUM", "med", "CRIT", " MED"} {
		if _, err := ParseLevel(bad); err == nil {
			t.Errorf("%q принято", bad)
		}
	}
	if _, err := ParseLevel("1"); err == nil || !strings.Contains(err.Error(), "сдвиг") {
		t.Errorf("число — прежний сдвиг, текст должен это сказать: %v", err)
	}
}

// Ворота не зависят ни от чего, кроме конфига: open — ступень вне корней, open_by_root — ступени корней, lane пуст.
func TestUserGate(t *testing.T) {
	g := UserGate(LevelHigh, map[string]Level{"/h/git": LevelMed, "/h/work": LevelLow, "/h/x": LevelHigh}, Rest{}, at("2026-11-14T12:00:00+01:00"))
	if len(g.Open) != 0 || g.Open == nil || g.Lane == nil || len(g.Lane) != 0 {
		t.Fatalf("вне корней HIGH и пустой lane: %+v", g)
	}
	if !slices.Equal(g.OpenByRoot["/h/git"], LevelMed.Pairs()) || !slices.Equal(g.OpenByRoot["/h/work"], LevelLow.Pairs()) {
		t.Fatalf("ступени корней: %v", g.OpenByRoot)
	}
	if pairs, ok := g.OpenByRoot["/h/x"]; !ok || pairs == nil || len(pairs) != 0 {
		t.Fatalf("корень HIGH в карте пустым списком: tk видит, что планка посчитана: %v", g.OpenByRoot)
	}
	if g := UserGate(LevelMed, nil, Rest{}, at("2026-11-14T12:00:00+01:00")); !slices.Equal(g.Open, LevelMed.Pairs()) || len(g.Lane) != 0 || g.OpenByRoot == nil || len(g.OpenByRoot) != 0 {
		t.Fatalf("level MED без пространств: %+v", g)
	}
}

func TestLevelConfig(t *testing.T) {
	c := Default("/h")
	if c.Level != LevelHigh {
		t.Fatalf("умолчание вне корней — HIGH: %q", c.Level)
	}
	c.Spaces = map[string]Level{"~/work/": LevelMed}
	if err := c.Validate("/h"); err != nil {
		t.Fatal(err)
	}
	if c.Spaces["/h/work"] != LevelMed {
		t.Fatalf("корень не раскрыт: %v", c.Spaces)
	}
	for name, mod := range map[string]func(*Config){
		"ступень корня":      func(c *Config) { c.Spaces = map[string]Level{"~/work": "1"} },
		"пустая ступень":     func(c *Config) { c.Spaces = map[string]Level{"~/work": ""} },
		"ступень вне корней": func(c *Config) { c.Level = "MEDIUM" },
		"пустой level":       func(c *Config) { c.Level = "" },
	} {
		c := Default("/h")
		mod(&c)
		if err := c.Validate("/h"); err == nil {
			t.Errorf("%s: принято %q %v", name, c.Level, c.Spaces)
		}
	}
}
