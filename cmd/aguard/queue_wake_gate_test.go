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

// Планка пространства открылась (OpenByRoot сменился): свободный мастер, чей tk next был пуст при закрытом MED,
// получает «есть работа» один раз. Ворота считает настоящий проход в gate.json, tk-заглушка видит MED только по нему.
func TestQueueWakeLetterAfterOpenByRootChange(t *testing.T) {
	a, v, ts, received := wakeSocketWorld(t, false)
	project := ts[0].Dir
	b := newQueueBench(t, "[]", "[]")
	b.writeOverrideAll(t) // общие ворота MED закрыты: открыть его может только планка пространства
	b.c.Queue.Spaces = map[string]int{project: 0}
	tk := filepath.Join(a.home, "tk")
	script := "#!/bin/sh\ngrep -q '\"MED-DO\"' " + b.c.Queue.Gate + " || exit 1\n" +
		"printf '%s\\n' '{\"source\":\"local\",\"path\":\"docs/tasks/20261104-0950-foo.md\",\"slug\":\"foo\",\"prio\":\"MED\",\"status\":\"открыт\"}'\n"
	if err := os.WriteFile(tk, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	v.TurnAfter = func(string, time.Time) bool { return false } // мастер с письма не работал
	v.Next = func(name, dir string) (queue.Ticket, bool, error) {
		return a.nextForWake(context.Background(), tk, name, dir, v.Now, func(time.Time) bool { return false })
	}
	cfg := &config.Config{Queue: queue.Config{Mode: queue.ModeOn}}
	state := queue.State{Seeded: true}
	start := v.Now
	pass := func(after time.Duration) []queue.Notice {
		t.Helper()
		gate, err := b.a.queueGatePass(b.c, time.Now(), false)
		if err != nil {
			t.Fatal(err)
		}
		if len(gate.Open) != 0 {
			t.Fatalf("общие ворота открыты: %v", gate.Open)
		}
		v.Now = start.Add(after)
		alive, err := a.reg.Alive()
		if err != nil {
			t.Fatal(err)
		}
		ns := state.Step(ts, v)
		a.deliverQueueNotices(cfg, alive, &state, v, ns, v.Now)
		return ns
	}
	silent := func(what string) {
		t.Helper()
		select {
		case msg := <-received:
			t.Fatalf("%s: лишнее письмо %s", what, msg)
		case <-time.After(100 * time.Millisecond):
		}
	}
	if ns := pass(0); len(ns) != 0 || len(state.Wakes) != 0 {
		t.Fatal("MED закрыт: tk next пуст, письма нет", ns, state.Wakes)
	}
	silent("MED закрыт")
	b.c.Queue.Spaces[project] = 1
	// Ответ tk кэширован на 5 минут: открытие планки видно, когда кэш истёк.
	ns := pass(6 * time.Minute)
	if len(ns) != 1 || ns[0].Kind != queue.KindWake || ns[0].Key != "project/20261104-0950-foo" {
		t.Fatal("MED открыт: письмо «есть работа»", ns)
	}
	select {
	case msg := <-received:
		if !strings.Contains(msg, "есть работа: tk next") {
			t.Fatal(msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("письмо не пришло")
	}
	if w := state.Wakes["lead"]; w.Ticket != "project/20261104-0950-foo" || !w.At.Equal(v.Now) {
		t.Fatal(state.Wakes)
	}
	for _, after := range []time.Duration{7 * time.Minute, 13 * time.Minute} { // из кэша и свежим ответом tk
		if ns := pass(after); len(ns) != 0 {
			t.Fatal("без изменений второго письма нет", after, ns)
		}
	}
	silent("без изменений")
}
