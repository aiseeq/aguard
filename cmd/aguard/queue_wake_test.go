package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/session"

	"github.com/aiseeq/aguard/internal/config"
	"github.com/aiseeq/aguard/internal/queue"
	"github.com/aiseeq/aguard/internal/restart"
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

type fakeJudge struct {
	free     bool
	wakeFree bool
}

func (j fakeJudge) Free(*restart.Config, int, time.Duration, time.Time) (bool, string) {
	return j.free, "подставной"
}
func (j fakeJudge) WakeFree(*restart.Config, int, time.Duration, time.Time) (bool, error) {
	return j.wakeFree, nil
}

// Настоящий View: занятость бойцом, ход по транскрипту, папки проектов и список заморозки.
func TestQueueViewWakeWiring(t *testing.T) {
	b := newQueueBench(t, "[]", "[]")
	b.a.projects = filepath.Join(b.dir, "projects")
	transcript := filepath.Join(b.a.projects, "-x", "s1.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(transcript)
	if err != nil {
		t.Fatal(err)
	}
	busy := map[string]bool{}
	b.a.queueWorkers = func() ([]string, error) {
		if busy["m"] {
			return []string{"worker-job.service"}, nil
		}
		return nil, nil
	}
	jobPath := filepath.Join(b.dir, ".local/state/worker/job/job.json")
	if err = os.MkdirAll(filepath.Dir(jobPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(jobPath, []byte(`{"owner":{"name":"m"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	alive := map[string]session.Info{"m": {Pid: 7, SessionID: "s1", Name: "m"}}
	project := filepath.Join(b.dir, "git", "p")
	view := func() queue.View {
		return queueView(b.c, alive, fakeJudge{free: true, wakeFree: true}, time.Now(), b.a, map[string]bool{project: true}, nil)
	}
	if !view().WakeFree("m") {
		t.Fatal("без бойца свободен")
	}
	busy["m"] = true
	if view().WakeFree("m") {
		t.Fatal("живой боец мастера — занят")
	}
	v := view()
	if !v.TurnAfter("m", info.ModTime().Add(-time.Second)) || v.TurnAfter("m", info.ModTime().Add(time.Second)) {
		t.Fatal("ход сверяется с транскриптом")
	}
	if yes, err := v.Frozen(project + "/"); err != nil || !yes {
		t.Fatal("папка из списка", yes, err)
	}
	if yes, _ := v.Frozen(filepath.Join(b.dir, "git", "q")); yes {
		t.Fatal("другая папка")
	}
	found := false
	for _, p := range v.Projects() {
		found = found || p.Dir == project && p.Project == "p" && p.Master == "p-master" && p.Admin == "git-admin"
	}
	if !found {
		t.Fatalf("проект не виден: %+v", v.Projects())
	}
}

// Поля ответа tk доходят до решения: без Prio CRIT в выходной молчал бы.
func TestQueueNextFieldsAndCacheAge(t *testing.T) {
	a, _ := testIOApp(t)
	tk := filepath.Join(a.home, "tk")
	script := "#!/bin/sh\nprintf '%s ' \"$@\" > " + filepath.Join(a.home, "args") + "\nprintf x >> " + filepath.Join(a.home, "calls") + "\nprintf '%s\\n' '{\"source\":\"local\",\"path\":\"docs/tasks/20261104-0950-foo.md\",\"slug\":\"foo\",\"prio\":\"CRIT\",\"status\":\"в работе\",\"executor\":\"m\"}'\n"
	if err := os.WriteFile(tk, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	never := func(time.Time) bool { return false }
	now := time.Now()
	first, found, err := a.nextForWake(context.Background(), tk, "m", a.home, now, never)
	if err != nil || !found || first.Prio != "CRIT" || first.Status != "в работе" || first.Executor != "m" || first.Slug != "foo" {
		t.Fatalf("%+v %v %v", first, found, err)
	}
	if args, readErr := os.ReadFile(filepath.Join(a.home, "args")); readErr != nil || string(args) != "next --json --by m " {
		t.Fatalf("tk спрошен не за мастера: %q %v", args, readErr)
	}
	calls := func() int {
		t.Helper()
		b, readErr := os.ReadFile(filepath.Join(a.home, "calls"))
		if readErr != nil {
			t.Fatal(readErr)
		}
		return len(b)
	}
	if _, _, err = a.nextForWake(context.Background(), tk, "m", a.home, now.Add(4*time.Minute), never); err != nil || calls() != 1 {
		t.Fatal("четыре минуты — кэш", err, calls())
	}
	if _, _, err = a.nextForWake(context.Background(), tk, "m", a.home, now.Add(6*time.Minute), never); err != nil || calls() != 2 {
		t.Fatal("шесть минут — новый вопрос", err, calls())
	}
}

// «Написал бы» в log ставит отметку, иначе одна и та же строка писалась бы каждый проход.
func TestQueueWakeLogModeMarks(t *testing.T) {
	a, v, ts, received := wakeSocketWorld(t, false)
	v.Serious = func(string) bool { return false }
	alive, err := a.reg.Alive()
	if err != nil {
		t.Fatal(err)
	}
	state := queue.State{Seeded: true}
	a.deliverQueueNotices(&config.Config{Queue: queue.Config{Mode: queue.ModeOn}}, alive, &state, v, state.Step(ts, v), v.Now)
	if state.Wakes["lead"].Mode != queue.ModeLog {
		t.Fatal(state.Wakes)
	}
	select {
	case msg := <-received:
		t.Fatalf("в log письмо: %s", msg)
	case <-time.After(100 * time.Millisecond):
	}
}

// Наступивший tk wait — другой ключ с тем же тикетом: мастер получит письмо, а админ не получит «встал» по старой отметке.
func TestQueueNextDueWaitOwnKey(t *testing.T) {
	a, _ := testIOApp(t)
	tk := filepath.Join(a.home, "tk")
	answer := func(status, due string) queue.Ticket {
		t.Helper()
		body := fmt.Sprintf(`{"source":"local","path":"docs/tasks/20261104-0950-foo.md","slug":"foo","prio":"MED","status":%q,"due":%q}`, status, due)
		if err := os.WriteFile(tk, []byte("#!/bin/sh\nprintf '%s\\n' '"+body+"'\n"), 0700); err != nil {
			t.Fatal(err)
		}
		a.queueNext = nil
		got, found, err := a.nextForWake(context.Background(), tk, "m", a.home, time.Now(), func(time.Time) bool { return false })
		if err != nil || !found {
			t.Fatal(found, err)
		}
		return got
	}
	open, first, second := answer("открыт", "2026-11-09T12:00:00+01:00"), answer("ждёт", "2026-11-09T12:00:00+01:00"), answer("ждёт", "2026-11-10T12:00:00+01:00")
	if open.Key == first.Key || first.Key == second.Key || !strings.HasPrefix(first.Key, open.Key+"@") {
		t.Fatalf("%q %q %q", open.Key, first.Key, second.Key)
	}
	if noDue := answer("ждёт", ""); noDue.Key != open.Key {
		t.Fatalf("срока нет: %q", noDue.Key)
	}
}

// Настоящий View: ответ tk живёт до хода мастера, сбой списков не прячется, админ — только своего пространства.
func TestQueueViewAnswersAndErrors(t *testing.T) {
	b := newQueueBench(t, "[]", "[]")
	b.a.projects = filepath.Join(b.dir, "projects")
	transcript := filepath.Join(b.a.projects, "-x", "s1.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(b.dir, "calls")
	b.c.Queue.Tk = filepath.Join(b.dir, "tk-next")
	script := "#!/bin/sh\nprintf x >> " + calls + "\nprintf '%s\\n' '{\"source\":\"local\",\"path\":\"docs/tasks/20261104-0950-foo.md\",\"slug\":\"foo\",\"prio\":\"MED\",\"status\":\"открыт\"}'\n"
	if err := os.WriteFile(b.c.Queue.Tk, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(b.dir, "git", "p")
	alive := map[string]session.Info{"m": {Pid: 7, SessionID: "s1", Name: "m", Cwd: project}}
	now := time.Now()
	view := func(at time.Time, frozenErr error) queue.View {
		return queueView(b.c, alive, fakeJudge{free: true, wakeFree: true}, at, b.a, map[string]bool{}, frozenErr)
	}
	asked := func(at time.Time, want int) {
		t.Helper()
		if _, found, err := view(at, nil).Next("m", project); err != nil || !found {
			t.Fatal(found, err)
		}
		got, err := os.ReadFile(calls)
		if err != nil || len(got) != want {
			t.Fatalf("вопросов %d, want %d: %v", len(got), want, err)
		}
	}
	touch := func(at time.Time) {
		t.Helper()
		if err := os.Chtimes(transcript, at, at); err != nil {
			t.Fatal(err)
		}
	}
	touch(now.Add(-time.Hour))
	asked(now, 1)
	asked(now.Add(time.Minute), 1) // хода не было — ответ годен
	touch(now.Add(2 * time.Minute))
	asked(now.Add(3*time.Minute), 2) // ход после ответа — спросить заново
	v := view(now, nil)
	if v.Cwd("m") != project || v.AdminFor(project) != "git-admin" || v.AdminFor(filepath.Join(b.dir, "other")) != "" {
		t.Fatalf("папка %q, админ %q, вне пространства %q", v.Cwd("m"), v.AdminFor(project), v.AdminFor(filepath.Join(b.dir, "other")))
	}
	wantErr := errors.New("нет файла")
	if _, err := view(now, wantErr).Frozen(project); !errors.Is(err, wantErr) {
		t.Fatal("сбой списка спрятан", err)
	}
	b.a.queueWorkers = func() ([]string, error) { return nil, errors.New("systemctl") }
	if view(now, nil).WakeFree("m") {
		t.Fatal("список бойцов не прочитан — нельзя считать свободным")
	}
}

// Плохая строка списка замороженных — предупреждение в журнале демона; строка «некому сообщить» — в журнале очереди.
func TestQueueFrozenWarningAndNoRecipientLine(t *testing.T) {
	b := newQueueBench(t, "[]", "[]")
	var logged bytes.Buffer
	b.a.log = slog.New(slog.NewTextHandler(&logged, nil))
	b.c.Queue.Frozen = filepath.Join(b.dir, "frozen")
	if err := os.WriteFile(b.c.Queue.Frozen, []byte("мусор\n"), 0600); err != nil {
		t.Fatal(err)
	}
	b.round(t)
	if !strings.Contains(logged.String(), "строка списка замороженных") {
		t.Fatal(logged.String())
	}
	const line = "некому сообщить: мастер встал"
	state := queue.State{Seeded: true}
	b.a.deliverQueueNotices(b.c, nil, &state, queue.View{Now: time.Now()}, []queue.Notice{{Kind: queue.KindUnreachable, Key: "p/x", Serious: true, Line: line}}, time.Now())
	files, err := filepath.Glob(filepath.Join(b.a.dir("queue"), "*"))
	if err != nil {
		t.Fatal(err)
	}
	inJournal := false
	for _, f := range files {
		body, readErr := os.ReadFile(f)
		if readErr == nil && bytes.Contains(body, []byte(line)) {
			inJournal = true
		}
	}
	if !inJournal {
		t.Fatal("строки нет в журнале очереди")
	}
}

// Папка из dirs с именем docs — сама проект, как у queueTasksProject; docs/tasks проекта — его корень.
func TestQueueWakeProjectsMatchTasksProject(t *testing.T) {
	b := newQueueBench(t, "[]", "[]")
	want := map[string]bool{filepath.Join(b.dir, "git", "p"): false, filepath.Join(b.dir, "docs"): false}
	for _, p := range b.a.queueWakeProjects(b.c) {
		if _, ok := want[p.Dir]; !ok {
			t.Fatalf("чужая папка: %+v", p)
		}
		want[p.Dir] = true
	}
	for dir, seen := range want {
		if !seen {
			t.Fatalf("нет %s", dir)
		}
	}
}

// Настоящий проход: отрицательный запас закрывает MED в пространстве со сдвигом.
func TestQueueNegativeMarginClosesShiftedMed(t *testing.T) {
	b := newQueueBench(t, "[]", "[]")
	root := b.c.Queue.GitRoots[0]
	b.c.Queue.Spaces = map[string]int{root: 1}
	now := time.Now()
	history := fmt.Sprintf(`{"at":%q,"sevenDayResetsAt":%q,"sevenDayUsedPercentage":10,"account":"a"}
{"at":%q,"sevenDayResetsAt":%q,"sevenDayUsedPercentage":95,"account":"a"}
`, now.Add(-24*time.Hour).Format(time.RFC3339), now.Add(72*time.Hour).Format(time.RFC3339), now.Add(-time.Hour).Format(time.RFC3339), now.Add(72*time.Hour).Format(time.RFC3339))
	if err := os.WriteFile(b.c.Queue.History, []byte(history), 0600); err != nil {
		t.Fatal(err)
	}
	b.round(t)
	gate, err := queue.ReadGate(b.c.Queue.Gate)
	if err != nil {
		t.Fatal(err)
	}
	if gate.Claude >= 0 || slices.Contains(gate.OpenByRoot[root], "MED-DO") {
		t.Fatalf("%+v", gate)
	}
}
