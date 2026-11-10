package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/names"
	"github.com/aiseeq/aguard/internal/queue"
)

const extHigh = `[{"key":"TRK-1","title":"внешняя","status":"To Do","prio":"HIGH","url":"https://x/TRK-1"}]`

// externalTk — tk, у которого external печатает файл или падает; папка p есть в корне git у стенда.
func (b *queueBench) externalTk(t *testing.T, answer string) (answerPath, calls string) {
	t.Helper()
	answerPath, calls = filepath.Join(b.dir, "ext.json"), filepath.Join(b.dir, "ext-calls.log")
	if err := os.WriteFile(answerPath, []byte(answer), 0o644); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = external ]; then\n pwd >> %s\n [ -e %s.fail ] && { echo трекер недоступен >&2; exit 1; }\n cat %s\n exit 0\nfi\nexec %s \"$@\"\n", calls, answerPath, answerPath, b.c.Queue.Tk)
	wrapper := filepath.Join(b.dir, "tk-external")
	if err := os.WriteFile(wrapper, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	b.c.Queue.Tk = wrapper
	b.c.Queue.Live = []string{"p"}
	return answerPath, calls
}

func (b *queueBench) external(t *testing.T, state *queue.State, now time.Time) []queue.Ticket {
	t.Helper()
	ts, err := b.a.queueExternal(context.Background(), b.c, state, now)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func callCount(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

func TestQueueExternalTickets(t *testing.T) {
	b := newQueueBench(t, "[]", "[]")
	b.externalTk(t, extHigh)
	var state queue.State
	ts := b.external(t, &state, time.Now())
	if len(ts) != 1 || ts[0].Key != "p/TRK-1" || !ts[0].External || ts[0].Status != "открыт" || ts[0].Dir != filepath.Join(b.dir, "git", "p") {
		t.Fatalf("%+v", ts)
	}
}

// Опрос раз в 5 мин, между опросами список из state.json.
func TestQueueExternalCadence(t *testing.T) {
	b := newQueueBench(t, "[]", "[]")
	_, calls := b.externalTk(t, extHigh)
	var state queue.State
	now := time.Now()
	for _, later := range []time.Duration{0, 30 * time.Second, 4*time.Minute + 59*time.Second} {
		ts := b.external(t, &state, now.Add(later))
		if len(ts) != 1 {
			t.Fatalf("+%v: %+v", later, ts)
		}
	}
	if n := callCount(t, calls); n != 1 {
		t.Fatalf("запросов до 5 мин: %d", n)
	}
	b.external(t, &state, now.Add(5*time.Minute))
	if n := callCount(t, calls); n != 2 {
		t.Fatalf("запросов к 5 мин: %d", n)
	}
}

// Постановка: сбой трекера виден в журнале демона, прошлый список цел, задача не считается новой.
func TestQueueExternalFailureKeepsList(t *testing.T) {
	for name, breakIt := range map[string]func(t *testing.T, answer string){
		"код": func(t *testing.T, answer string) {
			if err := os.WriteFile(answer+".fail", nil, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"не JSON": func(t *testing.T, answer string) {
			if err := os.WriteFile(answer, []byte("{не json"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			b := newQueueBench(t, "[]", "[]")
			answer, _ := b.externalTk(t, extHigh)
			log := b.captureLog()
			var state queue.State
			now := time.Now()
			b.external(t, &state, now)
			first := state.External[filepath.Join(b.dir, "git", "p")][0].FirstSeen
			breakIt(t, answer)
			ts := b.external(t, &state, now.Add(5*time.Minute))
			if len(ts) != 1 || !ts[0].Created.Equal(first) {
				t.Fatalf("список после сбоя: %+v", ts)
			}
			if !strings.Contains(log.String(), "внешний трекер") {
				t.Fatalf("журнал: %s", log.String())
			}
		})
	}
}

// Рестарт демона: список и первая встреча читаются из state.json, сбой на старте не стирает отметки о письмах.
func TestQueueExternalSurvivesRestart(t *testing.T) {
	b := newQueueBench(t, "[]", "[]")
	answer, _ := b.externalTk(t, extHigh)
	var state queue.State
	now := time.Now()
	b.external(t, &state, now)
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var loaded queue.State
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatal(err)
	}
	b.a.queueExternalAt = time.Time{}
	if err := os.WriteFile(answer+".fail", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ts := b.external(t, &loaded, now.Add(time.Minute))
	if len(ts) != 1 || ts[0].Key != "p/TRK-1" {
		t.Fatalf("после рестарта: %+v", ts)
	}
}

// Противник, сценарий 6: проект ушёл в skip или из live — опроса нет, старого списка нет, писем по нему нет.
func TestQueueExternalLeftConfig(t *testing.T) {
	for name, change := range map[string]func(b *queueBench){
		"skip": func(b *queueBench) { b.c.Queue.Skip = []string{filepath.Join(b.dir, "git", "p")} },
		"live": func(b *queueBench) { b.c.Queue.Live = []string{"другой"} },
	} {
		t.Run(name, func(t *testing.T) {
			b := newQueueBench(t, "[]", "[]")
			_, calls := b.externalTk(t, extHigh)
			var state queue.State
			now := time.Now()
			b.external(t, &state, now)
			change(b)
			ts := b.external(t, &state, now.Add(5*time.Minute))
			if len(ts) != 0 || len(state.External) != 0 || callCount(t, calls) != 1 {
				t.Fatalf("%+v %+v", ts, state.External)
			}
		})
	}
}

// Служба стартует с коротким PATH, а источник зовёт tk по имени: папка tk должна быть в PATH потомка (проверка 04.11: код 2, command not found).
func TestQueueExternalSeesTkDir(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{"tk": "#!/bin/sh\nexec tk-sibling\n", "tk-sibling": "#!/bin/sh\necho '[]'\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", "/usr/bin:/bin")
	if _, err := readQueueExternal(context.Background(), filepath.Join(dir, "tk"), dir); err != nil {
		t.Fatal(err)
	}
}

// Проверка 04.11: внешние без мастера стёрли бы отметки о письмах. С 10.11 (queue-names-table-stops) прогретый демон идёт
// на прошлой таблице: внешние с теми же хозяевами, отметки целы.
func TestQueueExternalBrokenNames(t *testing.T) {
	b := newQueueBench(t, "[]", "[]")
	b.externalTk(t, extHigh)
	log := b.captureLog()
	b.round(t)
	before, err := os.ReadFile(b.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(before), "TRK-1") {
		t.Fatalf("state.json: %s", before)
	}
	table := filepath.Join(b.a.home, names.File)
	if err := os.MkdirAll(filepath.Dir(table), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(table, []byte("строка без табуляции\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b.round(t)
	after, err := os.ReadFile(b.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("состояние изменилось:\n%s\n%s", before, after)
	}
	if !strings.Contains(log.String(), "таблица имён битая, иду на прошлой") {
		t.Fatalf("журнал: %s", log.String())
	}
}

// Проверка 04.11: сбой одной папки не трогает другие проекты; его причину показывает опрос, а список этой папки цел.
func TestQueueExternalBadDir(t *testing.T) {
	b := newQueueBench(t, "[]", "[]")
	b.externalTk(t, extHigh)
	loop := filepath.Join(b.dir, "git", "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	b.c.Queue.Live = []string{"loop", "p"}
	log := b.captureLog()
	var state queue.State
	state.External = map[string][]queue.ExternalTask{loop: {{Key: "TRK-2", Prio: "HIGH", Status: "To Do"}}}
	ts := b.external(t, &state, time.Now())
	if len(ts) != 2 {
		t.Fatalf("%+v", ts)
	}
	if !strings.Contains(log.String(), "внешний трекер") || !strings.Contains(log.String(), loop) {
		t.Fatalf("журнал: %s", log.String())
	}
}

// Проверка 04.11: обход идёт до писем, поэтому срок общий; зависший потомок с открытыми трубами срок не отменяет.
func TestQueueExternalSweepBudget(t *testing.T) {
	b := newQueueBench(t, "[]", "[]")
	hung := filepath.Join(b.dir, "tk-hung")
	if err := os.WriteFile(hung, []byte("#!/bin/sh\nsleep 8\necho '[]'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	log := b.captureLog()
	var state queue.State
	started := time.Now()
	b.a.pollQueueExternal(context.Background(), hung, []string{b.dir, filepath.Join(b.dir, "git")}, &state, started, 200*time.Millisecond)
	if took := time.Since(started); took > 6*time.Second {
		t.Fatalf("обход шёл %v", took)
	}
	if got := strings.Count(log.String(), "внешний трекер"); got != 2 {
		t.Fatalf("сбоев в журнале %d: %s", got, log.String())
	}
}

// Полный проход: у внешней задачи нет файла, tk note не вызывается, состояние сохраняет список и отметку.
func TestQueueExternalRound(t *testing.T) {
	b := newQueueBench(t, "[]", "[]")
	b.externalTk(t, extHigh)
	notes, _ := b.noteTk(t)
	b.round(t)
	b.round(t)
	data, err := os.ReadFile(b.statePath())
	if err != nil {
		t.Fatal(err)
	}
	var state queue.State
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if len(state.External) != 1 {
		t.Fatalf("state.json: %s", data)
	}
	if n := noteCount(t, notes); n != 0 {
		t.Fatalf("tk note вызван %d раз", n)
	}
}
