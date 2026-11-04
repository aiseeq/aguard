package main

import (
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/queue"
)

// Приёмка, пятый заход: находка проверки 1104-1119. Написана мастером.

func (b *queueBench) futureGate(t *testing.T, at time.Time) {
	t.Helper()
	gate, err := queue.ReadGate(b.c.Queue.Gate)
	if err != nil {
		t.Fatal(err)
	}
	gate.At = at
	if err := gate.Write(b.c.Queue.Gate); err != nil {
		t.Fatal(err)
	}
}

// Метка gate.json в будущем (часы сдвинулись): tk next считает файл сломанным, письма тоже, а живой проход его чинит.
func TestQueueGateFromFuture(t *testing.T) {
	t.Run("вход сломан: письма не верят файлу", func(t *testing.T) {
		b := newQueueBench(t, "[]", "[]")
		b.round(t)
		now := time.Now()
		b.futureGate(t, now.Add(time.Hour))
		b.writeHistory(t, map[string]time.Duration{"a": 25 * time.Hour})
		open, err := b.a.queueGatePass(b.c, now, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(open) != 0 {
			t.Fatalf("метка в будущем: %v", open)
		}
	})
	t.Run("вход цел: файл переписан, строки о смене нет", func(t *testing.T) {
		b := newQueueBench(t, "[]", "[]")
		b.round(t)
		lines := b.journal(t, "gate")
		now := time.Now()
		b.futureGate(t, now.Add(time.Hour))
		open, err := b.a.queueGatePass(b.c, now, false)
		if err != nil {
			t.Fatal(err)
		}
		gate, err := queue.ReadGate(b.c.Queue.Gate)
		if err != nil {
			t.Fatal(err)
		}
		if gate.At.After(now) || len(open) == 0 {
			t.Fatalf("метка %s при %s, open %v", gate.At, now, open)
		}
		if n := b.journal(t, "gate"); n != lines {
			t.Fatalf("строк о смене: %d, было %d", n, lines)
		}
	})
}
