package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/codexwatch"
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

// Запас OpenAI: учётки из конфига, замер свежий, история старше суток; неизменившийся замер строку не добавляет, учётка без замера даёт null.
func TestQueueOpenAIMargin(t *testing.T) {
	b := newQueueBench(t, "[]", "[]")
	now := time.Now()
	resets := now.Add(72 * time.Hour).Truncate(time.Second)
	b.c.Codex.Accounts = []codexwatch.Account{{Name: "w"}, {Name: "p"}}
	codexDir := b.a.dir("codex")
	if err := os.MkdirAll(codexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sample := func(name string, used float64) {
		st := codexwatch.State{ResetsAt: resets, Samples: []codexwatch.Sample{{At: now.Add(-5 * time.Minute), Used: used}}}
		data, err := json.Marshal(st)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(codexDir, name+".json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sample("w", 40)
	b.round(t)
	gate, err := queue.ReadGate(b.c.Queue.Gate)
	if err != nil {
		t.Fatal(err)
	}
	if gate.OpenAI != nil {
		t.Fatalf("учётка без замера: %v", *gate.OpenAI)
	}
	sample("p", 10)
	hist := filepath.Join(b.a.dir("queue"), "openai-history.jsonl")
	var old strings.Builder
	for _, name := range []string{"w", "p"} {
		fmt.Fprintf(&old, `{"at":%q,"sevenDayResetsAt":%q,"sevenDayUsedPercentage":0,"account":%q}`+"\n", now.Add(-48*time.Hour).Format(time.RFC3339), resets.Format(time.RFC3339), name)
	}
	if err := os.WriteFile(hist, []byte(old.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := now.Add(-2 * time.Hour) // без смены open и lane файл обновляется раз в час
	if err := os.Chtimes(b.c.Queue.Gate, stale, stale); err != nil {
		t.Fatal(err)
	}
	b.round(t)
	gate, err = queue.ReadGate(b.c.Queue.Gate)
	if err != nil {
		t.Fatal(err)
	}
	// Остаток 60 + 90 = 150, расход 50 за 2 суток = 25 в сутки, до сброса 3 суток: 150 / 25 − 3 = 3.
	if gate.OpenAI == nil || math.Abs(*gate.OpenAI-3) > 0.05 {
		t.Fatalf("запас OpenAI: %+v", gate)
	}
	size := func() int64 {
		fi, err := os.Stat(hist)
		if err != nil {
			t.Fatal(err)
		}
		return fi.Size()
	}
	was := size()
	b.round(t)
	if size() != was {
		t.Fatal("замер не менялся, а строка в истории добавлена")
	}
}
