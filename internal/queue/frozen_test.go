package queue

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFrozenListParse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "frozen")
	if err := os.WriteFile(path, []byte("# комментарий\n\n~/git/p\tчеловек\t2026-11-09\tпричина\n/opt/q\tлид\t2026-11-08\tожидание\n"), 0600); err != nil {
		t.Fatal(err)
	}
	list, problems, err := ReadFrozen(path, "/home/u")
	if err != nil || len(problems) != 0 || len(list) != 2 || !list["/home/u/git/p"] || !list["/opt/q"] {
		t.Fatal(list, problems, err)
	}
	const good = "/ok\tлид\t2026-11-08\tпричина\n"
	cases := []struct {
		bad    string
		frozen string
	}{
		{"/p\tчеловек\t2026-11-09", "/p"},
		{"/p человек 2026-11-09", "/p"},
		{"/p\tx\tне дата\twhy", "/p"},
		{"/p\t\t2026-11-09\twhy", "/p"},
		{"/p\tx\t2026-11-09\t", "/p"},
		{"relative\tx\t2026-11-09\twhy", ""},
	}
	for _, c := range cases {
		if err = os.WriteFile(path, []byte(c.bad+"\n"+good), 0600); err != nil {
			t.Fatal(err)
		}
		list, problems, err = ReadFrozen(path, "/home/u")
		if err != nil || len(problems) != 1 || !list["/ok"] {
			t.Fatalf("%q: другая строка пострадала: %v %v %v", c.bad, list, problems, err)
		}
		if c.frozen != "" && !list[c.frozen] || c.frozen == "" && len(list) != 1 {
			t.Fatalf("%q: %v", c.bad, list)
		}
	}
	if _, _, err = ReadFrozen(dir, "/home/u"); err == nil {
		t.Fatal("ошибка чтения папки не замечена")
	}
	if _, _, err = ReadFrozen(path+"-нет", "/home/u"); err == nil {
		t.Fatal("нет файла — ошибка")
	}
}
func freezeWorld() *wakeWorld {
	w := newWakeWorld()
	w.view.Frozen = func(string) (bool, error) { return false, nil }
	w.view.Alive = func(n string) bool { return n != "master" }
	w.view.AdminNext = func(admin, dir string) (Ticket, bool, error) { w.calls++; return w.first, w.found, w.err }
	return w
}
func TestWakeFrozenNoMasterNoLetters(t *testing.T) {
	w := freezeWorld()
	w.view.Frozen = func(string) (bool, error) { return true, nil }
	crit := w.ts[0]
	crit.Prio = "CRIT"
	crit.Key = "p/crit"
	high := w.ts[0]
	high.Prio = "HIGH"
	high.Key = "p/high"
	back := w.ts[0]
	back.Status = "готово"
	back.Back = "admin"
	back.Key = "p/closed"
	w.ts = []Ticket{crit, high, back}
	w.state.Wakes = map[string]Wake{"master": {Ticket: "p/crit", Session: "s"}}
	w.state.ProjectWakes = map[string]ProjectWake{"/p": {Ticket: "p/crit"}}
	ns := w.step()
	if len(ns) != 1 || !has(ns, KindBack, "admin") {
		t.Fatal(ns)
	}
	if len(w.state.Wakes) != 0 || len(w.state.ProjectWakes) != 0 || w.state.Tickets[crit.Key] != nil || w.state.Tickets[high.Key] != nil || w.calls != 0 {
		t.Fatal(w.state)
	}
	w.confirm(ns)
	w.view.Alive = func(string) bool { return true }
	ns = w.step()
	if !has(ns, KindCrit, "master") || !has(ns, KindHigh, "master") {
		t.Fatal(ns)
	}
}
func TestWakeFrozenLiveMasterWakes(t *testing.T) {
	w := newWakeWorld()
	w.view.Frozen = func(string) (bool, error) { return true, nil }
	w.view.AdminNext = func(string, string) (Ticket, bool, error) {
		t.Fatal("вызов для живого мастера")
		return Ticket{}, false, nil
	}
	if ns := w.step(); !has(ns, KindWake, "master") {
		t.Fatal(ns)
	}
}
func TestWakeNoMasterLettersAdmin(t *testing.T) {
	w := freezeWorld()
	w.ts[0].Master = ""
	ns := w.step()
	if len(ns) != 1 || !has(ns, KindWakeAdmin, "admin") || !strings.Contains(ns[0].Line, "проект p: живого мастера нет, есть работа: cd /p && tk next --by admin") || len(w.state.ProjectWakes) != 0 {
		t.Fatal(ns, w.state.ProjectWakes)
	}
	if ns2 := w.step(); !has(ns2, KindWakeAdmin, "admin") {
		t.Fatal("недоставленное письмо не повторилось")
	}
	w.confirm(ns)
	if ns = w.step(); len(ns) != 0 {
		t.Fatal(ns)
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := w.state.Save(path); err != nil {
		t.Fatal(err)
	}
	st, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	w.state = st
	if ns = w.step(); len(ns) != 0 {
		t.Fatal("повтор после рестарта", ns)
	}
	w.first.Key = "p/second"
	ns = w.step()
	if !has(ns, KindWakeAdmin, "admin") {
		t.Fatal(ns)
	}
	w.confirm(ns)
	w.err = errors.New("сбой")
	before := w.state.ProjectWakes["/p"]
	if ns = w.step(); len(ns) != 0 || w.state.ProjectWakes["/p"] != before {
		t.Fatal(ns)
	}
	w.err = nil
	w.found = false
	if ns = w.step(); len(ns) != 0 || len(w.state.ProjectWakes) != 0 {
		t.Fatal(ns, w.state.ProjectWakes)
	}
}
func TestWakeNoMasterAdminBusyOrDead(t *testing.T) {
	for _, reason := range []string{"занят", "задачи", "мёртв"} {
		t.Run(reason, func(t *testing.T) {
			w := freezeWorld()
			switch reason {
			case "занят":
				w.view.Free = func(string) bool { return false }
			case "задачи":
				w.view.WakeFree = func(string) bool { return false }
			case "мёртв":
				w.view.Alive = func(string) bool { return false }
			}
			if ns := w.step(); len(ns) != 0 || len(w.state.ProjectWakes) != 0 || w.calls != 0 {
				t.Fatal(ns, w.calls)
			}
		})
	}
}
func TestWakeFrozenListErrorNoAdminLetter(t *testing.T) {
	w := freezeWorld()
	w.view.Frozen = func(string) (bool, error) { return false, errors.New("список не читается") }
	high := w.ts[0]
	high.Prio = "CRIT"
	high.Key = "p/crit"
	if ns := w.state.Step([]Ticket{high}, w.view); !has(ns, KindCrit, "admin") || has(ns, KindWakeAdmin, "admin") {
		t.Fatal(ns)
	}
	w.view.Alive = func(string) bool { return true }
	w.view.Now = w.view.Now.Add(time.Minute)
	if ns := w.step(); !has(ns, KindWake, "master") {
		t.Fatal(ns)
	}
}

func TestWakeNoMasterLogThenLive(t *testing.T) {
	w := freezeWorld()
	live := false
	w.view.Serious = func(string) bool { return live }
	ns := w.step()
	if !has(ns, KindWakeAdmin, "admin") || ns[0].Serious {
		t.Fatal(ns)
	}
	w.confirm(ns)
	if ns = w.step(); len(ns) != 0 {
		t.Fatal(ns)
	}
	live = true
	ns = w.step()
	if !has(ns, KindWakeAdmin, "admin") || !ns[0].Serious {
		t.Fatal(ns)
	}
	w.confirm(ns)
	if ns = w.step(); len(ns) != 0 {
		t.Fatal(ns)
	}
}

func TestWakeNoMasterAdminNotReplacedByLead(t *testing.T) {
	w := freezeWorld()
	w.view.AdminFor = func(string) string { return "" }
	if ns := w.step(); len(ns) != 0 || w.calls != 0 {
		t.Fatal(ns, w.calls)
	}
}

// Непринятое предложение не повторяется, поэтому пачка за проход потеряла бы остальные проекты.
func TestWakeNoMasterOneOfferPerPass(t *testing.T) {
	w := freezeWorld()
	w.ts[0].Master = ""
	second := w.ts[0]
	second.Project, second.Dir, second.Key = "q", "/q", "q/base"
	w.ts = append(w.ts, second)
	w.view.AdminNext = func(_, dir string) (Ticket, bool, error) {
		return Ticket{Key: filepath.Base(dir) + "/first"}, true, nil
	}
	ns := w.step()
	if len(ns) != 1 || ns[0].Dir != "/p" {
		t.Fatal("первый проход", ns)
	}
	w.confirm(ns)
	ns = w.step()
	if len(ns) != 1 || ns[0].Dir != "/q" {
		t.Fatal("второй проход", ns)
	}
}

// Живой мастер ведёт проект сам: админу предлагать нечего.
func TestWakeLiveMasterNoAdminOffer(t *testing.T) {
	w := freezeWorld()
	w.view.Alive = func(string) bool { return true }
	w.view.Free = func(name string) bool { return name == "admin" }
	if ns := w.step(); len(ns) != 0 {
		t.Fatal(ns)
	}
}

// Занятый работой админ (боец, фоновые задачи) предложения не получает.
func TestWakeNoMasterAdminNotWakeFree(t *testing.T) {
	w := freezeWorld()
	w.ts[0].Master = ""
	w.view.WakeFree = func(string) bool { return false }
	if ns := w.step(); len(ns) != 0 {
		t.Fatal(ns)
	}
}

// Админ, которого в этом проходе будит письмо мастера, предложения не получает.
func TestWakeNoMasterAdminWokenAsMaster(t *testing.T) {
	w := freezeWorld()
	w.ts[0].Master = ""
	own := w.ts[0]
	own.Project, own.Dir, own.Key, own.Master = "own", "/own", "own/base", "admin"
	w.ts = append(w.ts, own)
	w.view.Alive = func(name string) bool { return name == "admin" }
	ns := w.step()
	if len(ns) != 1 || !has(ns, KindWake, "admin") {
		t.Fatal(ns)
	}
}

// Без CRIT в том же проходе: админа молчать заставляет именно ошибка списка.
func TestWakeFrozenErrorNoOfferWithoutCrit(t *testing.T) {
	w := freezeWorld()
	w.ts[0].Master = ""
	w.view.Frozen = func(string) (bool, error) { return false, errors.New("список не читается") }
	if ns := w.step(); len(ns) != 0 || w.calls != 0 {
		t.Fatal(ns, w.calls)
	}
}

func TestWakeNoMasterWeekendOnlyCrit(t *testing.T) {
	w := freezeWorld()
	w.ts[0].Master = ""
	w.view.Now = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	w.view.WeekendCrit = []string{"/p"}
	if ns := w.step(); len(ns) != 0 {
		t.Fatal(ns)
	}
	w.first.Prio = "CRIT"
	if ns := w.step(); !has(ns, KindWakeAdmin, "admin") {
		t.Fatal(ns)
	}
}

func TestWakeNoMasterFirstTakenSkipped(t *testing.T) {
	w := freezeWorld()
	w.ts[0].Master = ""
	w.first.Status, w.first.Executor = "в работе", "worker-1"
	if ns := w.step(); len(ns) != 0 {
		t.Fatal(ns)
	}
	w.first.Executor = "ADMIN"
	if ns := w.step(); !has(ns, KindWakeAdmin, "admin") {
		t.Fatal("свой тикет админа предлагается", ns)
	}
}
