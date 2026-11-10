package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/queue"
)

// Приёмка прохода queue, третий заход: написана мастером, боец её не правит.

func TestQueueTicketCreatedFromName(t *testing.T) {
	b := newQueueBench(t, `[{"repo":"p","slug":"c","status":"открыт","prio":"LOW","title":"t","path":"docs/tasks/20261103-1205-LOW-c.md"}]`, "[]")
	ts, err := b.a.queueTickets(context.Background(), b.c)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 11, 3, 12, 5, 0, 0, time.Local)
	if len(ts) != 1 || !ts[0].Created.Equal(want) {
		t.Fatalf("%+v", ts)
	}
}

// gate.json не записан: смены ворот не было, журнал не копит строку на каждый проход.
func TestQueueGateWriteFailNoJournal(t *testing.T) {
	b := newQueueBench(t, "[]", "[]")
	block := filepath.Join(b.dir, "file")
	if err := os.WriteFile(block, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	b.c.Queue.Gate = filepath.Join(block, "gate.json")
	b.round(t)
	b.round(t)
	if n := b.journal(t, "gate"); n != 0 {
		t.Fatalf("строк о смене: %d", n)
	}
}

// Реестр не прочитан — неизвестно, кто жив: «никто не доступен» и отметки о нём были бы ложью.
func TestQueueRegistryFailNoUnreachable(t *testing.T) {
	b := newQueueBench(t, `[{"repo":"p","slug":"c","status":"открыт","prio":"CRIT","title":"t","path":"docs/tasks/20261103-1200-CRIT-c.md"}]`, "[]")
	block := filepath.Join(b.dir, "file")
	if err := os.WriteFile(block, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	b.a.reg.Dir = block
	if _, err := b.a.reg.Alive(); err == nil {
		t.Fatal("реестр на файле читается без ошибки: тест ничего не проверяет")
	}
	b.round(t)
	if n := b.journal(t, queue.KindUnreachable); n != 0 {
		t.Fatalf("журнал: %d", n)
	}
}
