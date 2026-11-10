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

// Мастер ведёт «открытый» HIGH без tk start (заметки в журнале): запись мастера моложе 4 ч — админу «не взят» нет;
// старше 4 ч или запись не мастера — тревога, как раньше (aguard-high-not-taken-false, случаи 07.11).
func TestHighMasterNoteCountsAsTaken(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	high := tick("HIGH", "открыт")
	step(s, []Ticket{high}, w)
	w.now = w.now.Add(4*time.Hour + time.Minute)
	noted := high
	noted.MasterNote = w.now.Add(-highWait + time.Minute)
	if ns := step(s, []Ticket{noted}, w); has(ns, KindHighAdmin, "git-admin") {
		t.Fatalf("заметка мастера 3 ч 59 мин назад: %+v", ns)
	}
	noted.MasterNote = w.now.Add(-highWait - time.Minute)
	if ns := step(s, []Ticket{noted}, w); !has(ns, KindHighAdmin, "git-admin") {
		t.Fatalf("заметка мастера старше 4 ч: %+v", ns)
	}
	// Запись из будущего (сдвиг часов) не глушит тревогу: новое состояние, тот же срок.
	s = seeded(w)
	w.now = w.now.Add(-4*time.Hour - time.Minute)
	step(s, []Ticket{high}, w)
	w.now = w.now.Add(4*time.Hour + time.Minute)
	noted.MasterNote = w.now.Add(time.Hour)
	if ns := step(s, []Ticket{noted}, w); !has(ns, KindHighAdmin, "git-admin") {
		t.Fatalf("заметка мастера из будущего глушит тревогу: %+v", ns)
	}
}
