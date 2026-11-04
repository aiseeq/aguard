package queue

import (
	"strings"
	"testing"
	"time"
)

// Приёмка, третий заход: дефекты второго ревью queue-order-guard. Написана мастером, боец её не правит.

// Тикет самого админа: админ мёртв, идёт лиду, а не теряется молча.
func TestHighOwnAdminDead(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	own := tick("HIGH", "открыт")
	own.Master = "git-admin"
	w.live["git-admin"] = false
	if ns := step(s, []Ticket{own}, w); !has(ns, KindHighAdmin, "lead") {
		t.Fatalf("%+v", ns)
	}
}

// Тикет самого админа, он жив и не берёт: через 4 ч строка лиду.
func TestHighOwnAdminNotTaken(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	own := tick("HIGH", "открыт")
	own.Master = "git-admin"
	ts := []Ticket{own}
	step(s, ts, w)
	w.now = w.now.Add(4*time.Hour + time.Minute)
	if ns := step(s, ts, w); !has(ns, KindHighAdmin, "lead") {
		t.Fatalf("%+v", ns)
	}
}

// Лид уже получил CRIT как последний живой: поднялся мастер — копии лиду не будет.
func TestCritLeadGotItNoCopyLater(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	w.live["m-master"], w.live["git-admin"] = false, false
	ts := []Ticket{tick("CRIT", "открыт")}
	if ns := step(s, ts, w); !has(ns, KindCrit, "lead") {
		t.Fatalf("%+v", ns)
	}
	w.live["m-master"] = true
	if ns := step(s, ts, w); len(ns) != 0 {
		t.Fatalf("%+v", ns)
	}
}

// Письмо о CRIT дошло, потом сессии закрылись: «никто не доступен» в журнале было бы ложью.
func TestCritSentThenAllGoneSilent(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	ts := []Ticket{tick("CRIT", "открыт")}
	step(s, ts, w)
	w.live = map[string]bool{}
	if ns := step(s, ts, w); len(ns) != 0 {
		t.Fatalf("%+v", ns)
	}
}

// Взятый HIGH вернулся в «открыт»: 4 ч считаются заново, иначе админу ложное «не взят за 4 ч». Журнал не обновляется без смены статуса.
func TestHighReturnedRestartsClock(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	high := tick("HIGH", "открыт")
	step(s, []Ticket{high}, w)
	taken := tick("HIGH", "в работе")
	step(s, []Ticket{taken}, w)
	marks := func() string {
		p := t.TempDir() + "/state.json"
		if err := s.Save(p); err != nil {
			t.Fatal(err)
		}
		loaded, err := LoadState(p)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join([]string{loaded.Tickets[high.Key].Prio, loaded.Tickets[high.Key].Seen.Format(time.RFC3339)}, " ")
	}
	before := marks()
	w.now = w.now.Add(5 * time.Hour)
	step(s, []Ticket{taken}, w)
	if after := marks(); after != before {
		t.Fatalf("состояние меняется на каждый проход, пока тикет взят: %s, %s", before, after)
	}
	if ns := step(s, []Ticket{high}, w); has(ns, KindHighAdmin, "git-admin") {
		t.Fatalf("вернулся только что: %+v", ns)
	}
	w.now = w.now.Add(4*time.Hour + time.Minute)
	if ns := step(s, []Ticket{high}, w); !has(ns, KindHighAdmin, "git-admin") {
		t.Fatalf("4 ч от возврата: %+v", ns)
	}
}

// Новый MED или LOW никого не будит: созданный после утренней границы в счёт строки не идёт.
func TestMorningIgnoresNewTicket(t *testing.T) {
	w := newWorld(ModeOn)
	w.now = at("2026-11-04T15:00:00+01:00")
	w.free["m-master"] = true
	w.open = []string{"LOW-DO"}
	s := seeded(w)
	fresh := tick("LOW", "открыт")
	fresh.Diff, fresh.Created = "DO", at("2026-11-04T14:50:00+01:00")
	if ns := step(s, []Ticket{fresh}, w); len(ns) != 0 {
		t.Fatalf("новый днём: %+v", ns)
	}
	old := tick("LOW", "открыт")
	old.Diff, old.Key, old.Created = "DO", "m/20261103-0900-old", at("2026-11-03T09:00:00+01:00")
	if ns := step(s, []Ticket{fresh, old}, w); !has(ns, KindMorning, "m-master") {
		t.Fatalf("старый тикет, мастер освободился: %+v", ns)
	}
	// На следующее утро вчерашний новый уже в счёт.
	w.now = at("2026-11-05T07:01:00+01:00")
	if ns := step(s, []Ticket{fresh}, w); !has(ns, KindMorning, "m-master") {
		t.Fatalf("следующее утро: %+v", ns)
	}
}

// Таблица правится руками: значение берётся из клетки поставщика, а не из худшей полосы.
func TestComputeEditedTable(t *testing.T) {
	tb, err := ParseTable(strings.NewReader("LOW-DO\tнет\tда\tда\nMED-DO\tда\tбез мастера\tнет\n"))
	if err != nil {
		t.Fatal(err)
	}
	now := at("2026-11-03T12:00:00+01:00")
	mid := &Supply{Margin: 0.5}
	if open, lane := Compute(tb, nil, Supply{Margin: 5}, mid, now); len(open) != 0 || !eq(lane, []string{"MED-DO"}) {
		t.Errorf("Claude полон, OpenAI средний: LOW-DO нет и да, MED-DO да и без мастера: open %v, lane %v", open, lane)
	}
	if open, _ := Compute(tb, nil, Supply{Margin: -3, Burning: true}, nil, now); !eq(open, []string{"LOW-DO", "MED-DO"}) {
		t.Errorf("сгорает: открыто всё, и клетка «нет» тоже: %v", open)
	}
}

// Заказчик ждёт и закрытый HIGH и CRIT, а не только MED и LOW.
func TestBackOnHighAndCrit(t *testing.T) {
	for _, prio := range []string{"HIGH", "CRIT"} {
		w := newWorld(ModeOn)
		w.live["c-master"], w.free["c-master"] = true, true
		s := seeded(w)
		tk := tick(prio, "готово")
		tk.Back = "c-master"
		if ns := step(s, []Ticket{tk}, w); !has(ns, KindBack, "c-master") {
			t.Errorf("%s: %+v", prio, ns)
		}
	}
}
