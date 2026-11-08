package ioroute

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func testConfig() Config {
	return Config{UrgentMin: 10 * time.Second, SeriesN: 3, SeriesWindow: 15 * time.Minute, Repeat: time.Hour,
		MachineN: 6, MachineOwners: 3, CrossWindow: 24 * time.Hour, Lead: "lead"}
}

func at(min float64, sec int64, owner, class string) Case {
	end := t0.Add(time.Duration(min * float64(time.Minute)))
	return Case{Start: end.Add(-time.Duration(sec) * time.Second), End: end, DurMS: sec * 1000, Owner: owner, Class: class}
}

func alive(names ...string) func(string) bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return func(s string) bool { return m[s] }
}

func letters(acts []Action) []Action {
	var out []Action
	for _, a := range acts {
		if a.Kind == Letter {
			out = append(out, a)
		}
	}
	return out
}

func tickets(acts []Action) []Action {
	var out []Action
	for _, a := range acts {
		if a.Kind == Ticket {
			out = append(out, a)
		}
	}
	return out
}

// Одиночный короткий фриз пробы агента (как 08.11 13:12, 4 с) — тикет его хозяину, писем нет.
func TestShortCaseTicketNoLetter(t *testing.T) {
	r := New(testConfig())
	acts := r.Route(at(0, 4, "dream-master", "go"), alive("dream-master", "lead"))
	if len(letters(acts)) != 0 || len(tickets(acts)) != 1 || tickets(acts)[0].Owner != "dream-master" || tickets(acts)[0].Class != "go" {
		t.Fatalf("акты %+v", acts)
	}
}

// Длинный фриз — письмо хозяину сразу, лиду ничего.
func TestLongCaseLetterOwner(t *testing.T) {
	r := New(testConfig())
	acts := r.Route(at(0, 12, "dream-master", "go"), alive("dream-master", "lead"))
	l := letters(acts)
	if len(l) != 1 || l[0].To != "dream-master" || l[0].Why != WhyUrgent || len(tickets(acts)) != 0 {
		t.Fatalf("акты %+v", acts)
	}
}

// Серия: третий случай от 2 с за 15 мин — письмо; первые два — тикеты.
func TestSeriesThirdLetter(t *testing.T) {
	r := New(testConfig())
	a := alive("akit-master")
	for i, m := range []float64{0, 5} {
		if l := letters(r.Route(at(m, 3, "akit-master", "rg"), a)); len(l) != 0 {
			t.Fatalf("случай %d: письмо %+v", i, l)
		}
	}
	if l := letters(r.Route(at(10, 3, "akit-master", "rg"), a)); len(l) != 1 || l[0].To != "akit-master" {
		t.Fatalf("третий: %+v", l)
	}
	// Вне окна серия начинается заново.
	r2 := New(testConfig())
	r2.Route(at(0, 3, "akit-master", "rg"), a)
	r2.Route(at(5, 3, "akit-master", "rg"), a)
	if l := letters(r2.Route(at(21, 3, "akit-master", "rg"), a)); len(l) != 0 {
		t.Fatalf("за окном: %+v", l)
	}
}

// Шторм: письмо, затем случаи в паузе копятся; конец паузы при идущем вреде — одно письмо со всеми, при кончившемся — тикет.
func TestStormPendingAfterPause(t *testing.T) {
	r := New(testConfig())
	a := alive("dream-master")
	if l := letters(r.Route(at(0, 12, "dream-master", "adream"), a)); len(l) != 1 {
		t.Fatalf("первое: %+v", l)
	}
	for m := 1.0; m <= 59; m++ {
		if l := letters(r.Route(at(m, 3, "dream-master", "adream"), a)); len(l) != 0 {
			t.Fatalf("в паузе %v: письмо %+v", m, l)
		}
	}
	if r.Pending() == 0 {
		t.Fatal("в паузе ничего не ждёт")
	}
	if acts := r.Tick(t0.Add(59 * time.Minute)); len(acts) != 0 {
		t.Fatalf("до конца паузы: %+v", acts)
	}
	acts := r.Tick(t0.Add(60 * time.Minute))
	if l := letters(acts); len(l) != 1 || l[0].Why != WhyAfter || len(l[0].Cases) < 50 {
		t.Fatalf("конец паузы: %+v", acts)
	}

	r2 := New(testConfig())
	r2.Route(at(0, 12, "dream-master", "adream"), a)
	r2.Route(at(2, 12, "dream-master", "adream"), a)
	acts = r2.Tick(t0.Add(61 * time.Minute)) // вред кончился 59 мин назад
	if len(letters(acts)) != 0 || len(tickets(acts)) != 1 || len(tickets(acts)[0].Cases) != 1 {
		t.Fatalf("кончившийся шторм: %+v", acts)
	}
}

// Хозяин не в сети: вред сейчас — лиду письмо, хозяину тикет.
func TestOfflineOwner(t *testing.T) {
	r := New(testConfig())
	acts := r.Route(at(0, 12, "graft-master", "go"), alive("lead"))
	l, tk := letters(acts), tickets(acts)
	if len(l) != 1 || l[0].To != "lead" || l[0].Why != WhyOffline || len(tk) != 1 || tk[0].Owner != "graft-master" {
		t.Fatalf("акты %+v", acts)
	}
}

// Без хозяина: длинный — лиду письмо, одиночный короткий — только в сводку; серия одного класса (шторм службы) — лиду.
func TestNoOwner(t *testing.T) {
	r := New(testConfig())
	a := alive("lead")
	if l := letters(r.Route(at(0, 12, "", "plocate-updatedb"), a)); len(l) != 1 || l[0].To != "lead" || l[0].Why != WhyNoOwner {
		t.Fatalf("длинный: %+v", l)
	}
	r = New(testConfig())
	for m := 0.0; m < 2; m++ {
		acts := r.Route(at(m, 3, "", "plocate-updatedb"), a)
		if len(acts) != 1 || acts[0].Kind != Summary {
			t.Fatalf("короткий %v: %+v", m, acts)
		}
	}
	if acts := r.Route(at(2, 3, "", "snapperd"), a); len(acts) != 1 || acts[0].Kind != Summary {
		t.Fatalf("другой класс серию не продолжает: %+v", acts)
	}
	if l := letters(r.Route(at(3, 3, "", "plocate-updatedb"), a)); len(l) != 1 || l[0].To != "lead" || l[0].Why != WhyNoOwner {
		t.Fatalf("шторм службы: %+v", l)
	}
}

// Лид — сам хозяин: письмо ему, даже если реестр его не видит.
func TestLeadOwner(t *testing.T) {
	r := New(testConfig())
	l := letters(r.Route(at(0, 12, "lead", "go"), alive()))
	if len(l) != 1 || l[0].To != "lead" || l[0].Why != WhyUrgent {
		t.Fatalf("акты %+v", l)
	}
}

// Общий вред: 6 коротких случаев за 15 мин от 3 хозяев — одно письмо лиду, следующее — не раньше Repeat.
func TestMachineWide(t *testing.T) {
	r := New(testConfig())
	a := alive("a", "b", "c", "lead")
	owners := []string{"a", "b", "c", "d", "e", "f"}
	var got []Action
	for i, o := range owners {
		got = append(got, letters(r.Route(at(float64(i), 3, o, "x"+o), a))...)
	}
	if len(got) != 1 || got[0].To != "lead" || got[0].Why != WhyMachine || len(got[0].Cases) != 6 {
		t.Fatalf("письма %+v", got)
	}
	for i, o := range owners {
		if l := letters(r.Route(at(float64(10+i), 3, o, "y"+o), a)); len(l) != 0 {
			t.Fatalf("повтор до Repeat: %+v", l)
		}
	}
	// Один хозяин с серией — не общий вред.
	r = New(testConfig())
	got = nil
	for i := range 6 {
		got = append(got, letters(r.Route(at(float64(i), 3, "a", "x"), a))...)
	}
	for _, l := range got {
		if l.Why == WhyMachine {
			t.Fatalf("один хозяин дал общий вред: %+v", got)
		}
	}
}

// Класс у двух хозяев за сутки — письмо лиду раз в сутки на класс; общий класс (session) не сверяется.
func TestCrossOwners(t *testing.T) {
	r := New(testConfig())
	a := alive("a", "b", "c", "lead")
	if l := letters(r.Route(at(0, 3, "a", "go"), a)); len(l) != 0 {
		t.Fatalf("первый хозяин: %+v", l)
	}
	l := letters(r.Route(at(60, 3, "b", "go"), a))
	if len(l) != 1 || l[0].To != "lead" || l[0].Why != WhyCross || l[0].Class != "go" {
		t.Fatalf("второй хозяин: %+v", l)
	}
	if l := letters(r.Route(at(120, 3, "c", "go"), a)); len(l) != 0 {
		t.Fatalf("третий в те же сутки: %+v", l)
	}
	g := at(0, 3, "a", "session")
	g.Generic = true
	r = New(testConfig())
	r.Route(g, a)
	g = at(60, 3, "b", "session")
	g.Generic = true
	if l := letters(r.Route(g, a)); len(l) != 0 {
		t.Fatalf("общий класс: %+v", l)
	}
}

// Перезапуск: два случая до него (Seed), третий после — серия продолжается, письмо; письмо до перезапуска держит паузу.
func TestSeedRestoresSeries(t *testing.T) {
	r := New(testConfig())
	r.Seed([]Case{at(0, 3, "akit-master", "rg"), at(5, 3, "akit-master", "rg")})
	if l := letters(r.Route(at(10, 3, "akit-master", "rg"), alive("akit-master"))); len(l) != 1 {
		t.Fatalf("после перезапуска: %+v", l)
	}
	r = New(testConfig())
	r.Seed([]Case{at(0, 12, "akit-master", "rg")})
	if l := letters(r.Route(at(10, 12, "akit-master", "rg"), alive("akit-master"))); len(l) != 0 {
		t.Fatalf("пауза после перезапуска забыта: %+v", l)
	}
}

// Читающая главная (возможна жертва): срочное — лиду и тикет хозяину, хозяину письма нет; несрочное — тикет.
func TestVictimOwner(t *testing.T) {
	r := New(testConfig())
	c := at(0, 12, "dream-master", "fuse-overlayfs")
	c.Victim = true
	acts := r.Route(c, alive("dream-master", "lead"))
	l, tk := letters(acts), tickets(acts)
	if len(l) != 1 || l[0].To != "lead" || l[0].Why != WhyVictim || len(tk) != 1 || tk[0].Owner != "dream-master" {
		t.Fatalf("срочное: %+v", acts)
	}
	c = at(30, 3, "dream-master", "fuse-overlayfs")
	c.Victim = true
	if acts := r.Route(c, alive("dream-master", "lead")); len(letters(acts)) != 0 || len(tickets(acts)) != 1 {
		t.Fatalf("несрочное: %+v", acts)
	}
}

// Срочное жертвы или хозяина не в сети — тикет сразу; письмо лиду в паузе, вред к её концу кончился — второй
// заметки того же случая нет.
func TestPendingTicketedNoDoubleNote(t *testing.T) {
	for _, victim := range []bool{true, false} {
		r := New(testConfig())
		a := alive("lead")
		if victim {
			a = alive("lead", "dream-master")
		}
		r.Route(at(0, 12, "", "plocate"), a) // письмо лиду — пауза
		c := at(1, 12, "dream-master", "fuse")
		c.Victim = victim
		if tk := tickets(r.Route(c, a)); len(tk) != 1 {
			t.Fatalf("victim=%v: тикет сразу: %+v", victim, tk)
		}
		if r.Pending() != 1 {
			t.Fatalf("victim=%v: ждёт %d", victim, r.Pending())
		}
		if acts := r.Tick(t0.Add(2 * time.Hour)); len(tickets(acts)) != 0 {
			t.Fatalf("victim=%v: вторая заметка: %+v", victim, acts)
		}
	}
}
