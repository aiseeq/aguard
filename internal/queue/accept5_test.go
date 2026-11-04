package queue

import (
	"testing"
	"time"
)

// Приёмка, пятый заход: находки проверки 1104-1119 и умолчание skip. Написана мастером.

// HIGH моложе 4 ч попал в журнал админу при мёртвом мастере, потом проект вошёл в live, мастер ожил и не взял:
// журнальная отметка не заменяет боевую строку через 4 ч.
func TestHighAdminLogThenLiveMasterAlive(t *testing.T) {
	for name, s := range map[string]func(*world) *State{
		"состояние цело":  seeded,
		"посев с чистого": func(*world) *State { return &State{} },
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(ModeLog)
			w.live["m-master"] = false
			st := s(w)
			ts := []Ticket{freshHigh(w, 0)}
			if ns := step(st, ts, w); !has(ns, KindHighAdmin, "git-admin") {
				t.Fatalf("журнал: %+v", ns)
			}
			w.mode, w.live["m-master"], w.free["m-master"] = ModeOn, true, true
			w.now = w.now.Add(time.Hour)
			ns := step(st, ts, w)
			if !has(ns, KindHigh, "m-master") || has(ns, KindHighAdmin, "git-admin") {
				t.Fatalf("вошёл в live, 1 ч: %+v", ns)
			}
			w.now = w.now.Add(2*time.Hour + 59*time.Minute)
			if ns := step(st, ts, w); has(ns, KindHighAdmin, "git-admin") {
				t.Fatalf("3 ч 59 мин: %+v", ns)
			}
			w.now = w.now.Add(2 * time.Minute)
			if ns := step(st, ts, w); !has(ns, KindHighAdmin, "git-admin") {
				t.Fatalf("4 ч 01 мин: %+v", ns)
			}
			if ns := step(st, ts, w); has(ns, KindHighAdmin, "git-admin") {
				t.Fatalf("боевая один раз: %+v", ns)
			}
		})
	}
}

// Состояние потеряно, HIGH в работе, потом вернулся в «открыт»: посев не гасит его навсегда. Мастеру строка, админу через 4 ч.
func TestSeedTakenHighBackToOpen(t *testing.T) {
	for _, age := range []time.Duration{time.Hour, 5 * time.Hour} {
		w := newWorld(ModeOn)
		w.free["m-master"] = true
		s := &State{}
		high := freshHigh(w, age)
		high.Status = "в работе"
		if ns := step(s, []Ticket{high}, w); len(ns) != 0 {
			t.Fatalf("возраст %s, в работе: %+v", age, ns)
		}
		w.now = w.now.Add(time.Hour)
		high.Status = "открыт"
		if ns := step(s, []Ticket{high}, w); !has(ns, KindHigh, "m-master") || has(ns, KindHighAdmin, "git-admin") {
			t.Fatalf("возраст %s, вернулся: %+v", age, ns)
		}
		w.now = w.now.Add(4*time.Hour + time.Minute)
		if ns := step(s, []Ticket{high}, w); !has(ns, KindHighAdmin, "git-admin") {
			t.Fatalf("возраст %s, 4 ч после возврата: %+v", age, ns)
		}
	}
}

// Репозиторий публичный: папки, которые очередь не читает, живут только в конфиге машины.
func TestDefaultSkipEmpty(t *testing.T) {
	if got := Default("/h").Skip; len(got) != 0 {
		t.Fatalf("%v", got)
	}
}
