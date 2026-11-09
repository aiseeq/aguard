package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/config"
	"github.com/aiseeq/aguard/internal/queue"
)

func wakeSocketWorld(t *testing.T, worker bool) (*app, queue.View, []queue.Ticket, <-chan string) {
	t.Helper()
	a, _ := testIOApp(t)
	received := liveLead(t, a)
	dir := filepath.Join(a.home, "project")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	tk := filepath.Join(a.home, "tk")
	script := "#!/bin/sh\nprintf '%s\\n' '{\"source\":\"local\",\"path\":\"docs/tasks/20261104-0950-foo.md\",\"slug\":\"foo\",\"prio\":\"MED\",\"status\":\"открыт\"}'\n"
	if err := os.WriteFile(tk, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	a.queueWorkers = func() ([]string, error) {
		if worker {
			return []string{"worker-job.service"}, nil
		}
		return nil, nil
	}
	if worker {
		path := filepath.Join(a.home, ".local/state/worker/job/job.json")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"owner":{"name":"lead"}}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	v := queue.View{Now: now, Lead: "lead", Serious: func(string) bool { return true }, Alive: func(string) bool { return true }, Free: func(string) bool { return true }, Session: func(string) string { return "session-1" }}
	v.WakeFree = func(name string) bool { owners, err := a.workerOwners(); return err == nil && !owners[name] }
	v.Next = func(name, dir string) (queue.Ticket, bool, error) {
		return a.nextForWake(context.Background(), tk, name, dir, now, func(time.Time) bool { return false })
	}
	ts := []queue.Ticket{{Key: "project/base", Project: "project", Dir: dir, Master: "lead", Prio: "MED", Status: "открыт"}}
	return a, v, ts, received
}
func TestQueueWakeLetterDelivered(t *testing.T) {
	a, v, ts, received := wakeSocketWorld(t, false)
	alive, err := a.reg.Alive()
	if err != nil {
		t.Fatal(err)
	}
	state := queue.State{Seeded: true}
	ns := state.Step(ts, v)
	a.deliverQueueNotices(&config.Config{Queue: queue.Config{Mode: queue.ModeOn}}, alive, &state, v, ns, v.Now)
	select {
	case msg := <-received:
		if !strings.Contains(msg, "есть работа: tk next") {
			t.Fatal(msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("письмо не пришло")
	}
	select {
	case msg := <-received:
		t.Fatalf("лишнее письмо: %s", msg)
	case <-time.After(100 * time.Millisecond):
	}
	if state.Wakes["lead"].Ticket != "project/20261104-0950-foo" {
		t.Fatal(state.Wakes)
	}
}
func TestQueueWakeBusyWorkerNoLetter(t *testing.T) {
	a, v, ts, received := wakeSocketWorld(t, true)
	alive, err := a.reg.Alive()
	if err != nil {
		t.Fatal(err)
	}
	state := queue.State{Seeded: true}
	ns := state.Step(ts, v)
	a.deliverQueueNotices(&config.Config{Queue: queue.Config{Mode: queue.ModeOn}}, alive, &state, v, ns, v.Now)
	if len(ns) != 0 || len(state.Wakes) != 0 {
		t.Fatal(ns, state.Wakes)
	}
	select {
	case msg := <-received:
		t.Fatalf("лишнее письмо: %s", msg)
	case <-time.After(100 * time.Millisecond):
	}
}

// Администратор опрашивает несколько проектов: ответ каждого имеет собственный кэш.
func TestQueueAdminNextCacheByProject(t *testing.T) {
	a, _ := testIOApp(t)
	now := time.Now()
	tk := filepath.Join(a.home, "tk")
	script := "#!/bin/sh\nprintf x >> calls\nprintf '%s\\n' '{\"source\":\"external\",\"key\":\"TASK-1\",\"prio\":\"MED\",\"status\":\"open\"}'\n"
	if err := os.WriteFile(tk, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one", "two"} {
		dir := filepath.Join(a.home, name)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			ticket, found, err := a.nextForAdmin(context.Background(), tk, "admin", dir, now)
			if err != nil || !found || ticket.Key != name+"/TASK-1" {
				t.Fatal(ticket, found, err)
			}
		}
		calls, err := os.ReadFile(filepath.Join(dir, "calls"))
		if err != nil || string(calls) != "x" {
			t.Fatalf("%s: %q %v", dir, calls, err)
		}
	}
}

// Мастер за минуту поставил tk wait: кэш пятиминутной давности не должен назвать его вставшим.
func TestQueueWakeNoFalseStuckAfterTurn(t *testing.T) {
	a, v, ts, _ := wakeSocketWorld(t, false)
	tk := filepath.Join(a.home, "tk")
	first := filepath.Join(a.home, "first")
	script := "#!/bin/sh\nprintf x >> " + filepath.Join(a.home, "calls") + "\nprintf '{\"source\":\"local\",\"path\":\"docs/tasks/20261104-0950-%s.md\",\"slug\":\"%s\",\"prio\":\"MED\",\"status\":\"открыт\"}\\n' \"$(cat " + first + ")\" \"$(cat " + first + ")\"\n"
	if err := os.WriteFile(tk, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	setFirst := func(name string) {
		t.Helper()
		if err := os.WriteFile(first, []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	calls := func() int {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(a.home, "calls"))
		if err != nil {
			t.Fatal(err)
		}
		return len(b)
	}
	turn := false
	v.TurnAfter = func(string, time.Time) bool { return turn }
	v.Next = func(name, dir string) (queue.Ticket, bool, error) {
		return a.nextForWake(context.Background(), tk, name, dir, v.Now, func(time.Time) bool { return turn })
	}
	setFirst("foo")
	state := queue.State{Seeded: true}
	for _, n := range state.Step(ts, v) {
		state.Confirm(n, v)
	}
	if state.Wakes["lead"].Ticket != "project/20261104-0950-foo" || calls() != 1 {
		t.Fatal(state.Wakes, calls())
	}
	setFirst("bar")
	if ns := state.Step(ts, v); len(ns) != 0 || calls() != 1 {
		t.Fatal("без хода — кэш и тишина", ns, calls())
	}
	turn = true
	ns := state.Step(ts, v)
	if len(ns) != 1 || ns[0].Kind != queue.KindWake || ns[0].Key != "project/20261104-0950-bar" || calls() != 2 {
		t.Fatal("после хода — свежий ответ и новое письмо, не сигнал", ns, calls())
	}
}

// Другая папка мастера — не ошибка и не чужой кэш: мастер сменил cwd.
func TestQueueWakeCacheOtherDirAsksAgain(t *testing.T) {
	a, _ := testIOApp(t)
	now := time.Now()
	tk := filepath.Join(a.home, "tk")
	script := "#!/bin/sh\nprintf '%s\\n' '{\"source\":\"external\",\"key\":\"TASK-1\",\"prio\":\"MED\",\"status\":\"open\"}'\n"
	if err := os.WriteFile(tk, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	never := func(time.Time) bool { return false }
	for _, name := range []string{"one", "two"} {
		dir := filepath.Join(a.home, name)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		ticket, found, err := a.nextForWake(context.Background(), tk, "m", dir, now, never)
		if err != nil || !found || ticket.Key != name+"/TASK-1" {
			t.Fatal(ticket, found, err)
		}
	}
}

// Код 1 у tk — «работы нет», любой другой — сбой: запись о письме от него не меняется.
func TestQueueNextExitCodes(t *testing.T) {
	a, _ := testIOApp(t)
	tk := filepath.Join(a.home, "tk")
	never := func(time.Time) bool { return false }
	for code, wantErr := range map[string]bool{"1": false, "2": true} {
		if err := os.WriteFile(tk, []byte("#!/bin/sh\nexit "+code+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
		_, found, err := a.nextForWake(context.Background(), tk, "m"+code, a.home, time.Now(), never)
		if found || (err != nil) != wantErr {
			t.Errorf("код %s: found=%v err=%v", code, found, err)
		}
	}
}

// Настоящий проход: строка списка глушит HIGH проекта без живого мастера, убрали строку — тикет снова виден.
func TestQueueFrozenListSilencesRound(t *testing.T) {
	path := "docs/tasks/" + time.Now().Add(-time.Hour).Format("20060102-1504") + "-HIGH-c.md"
	b := newQueueBench(t, `[{"repo":"p","slug":"c","status":"открыт","prio":"HIGH","title":"t","path":"`+path+`"}]`, "[]")
	frozen := filepath.Join(b.dir, "frozen")
	b.c.Queue.Frozen = frozen
	writeList := func(body string) {
		t.Helper()
		if err := os.WriteFile(frozen, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	marked := func() int {
		return b.journal(t, queue.KindHigh) + b.journal(t, queue.KindHighAdmin) + b.journal(t, queue.KindUnreachable)
	}
	writeList(filepath.Join(b.dir, "git", "p") + "\tпользователь\t2026-11-09\tтест\n")
	b.round(t)
	if n := marked(); n != 0 {
		t.Fatalf("замороженный проект дал строк: %d", n)
	}
	writeList("# пусто\n")
	b.round(t)
	if n := marked(); n == 0 {
		t.Fatal("проект вне списка молчит")
	}
}
