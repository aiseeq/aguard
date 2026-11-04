package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
		open, err := gateOpen(b.a.queueGatePass(b.c, now, false))
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
		open, err := gateOpen(b.a.queueGatePass(b.c, now, false))
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

func gateOpen(g queue.Gate, err error) ([]string, error) { return g.Open, err }

func (b *queueBench) writeOverrideAll(t *testing.T) {
	t.Helper()
	var body strings.Builder
	for _, pair := range []string{"MED-DO", "MED-HUNT", "MED-ARCH", "LOW-DO", "LOW-HUNT", "LOW-ARCH"} {
		body.WriteString(pair + "\tнет\t2030-01-01\tx\ty\n")
	}
	if err := os.WriteFile(b.c.Queue.Override, []byte(body.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Сдвиг меняет только настройка, без пересборки и без изменения общих ворот: за один проход карта доходит до gate.json.
func TestQueueGateOpenByRoot(t *testing.T) {
	b := newQueueBench(t, "[]", "[]")
	b.round(t)
	gate, err := queue.ReadGate(b.c.Queue.Gate)
	if err != nil {
		t.Fatal(err)
	}
	if gate.OpenByRoot == nil || len(gate.OpenByRoot) != 0 {
		t.Fatalf("без пространств — пустой объект: %v", gate.OpenByRoot)
	}
	root := filepath.Join(b.dir, "work")
	b.writeOverrideAll(t)
	b.c.Queue.Spaces = map[string]int{root: 0}
	b.round(t)
	gate, err = queue.ReadGate(b.c.Queue.Gate)
	if err != nil {
		t.Fatal(err)
	}
	if pairs, ok := gate.OpenByRoot[root]; !ok || len(pairs) != 0 || len(gate.Open) != 0 {
		t.Fatalf("сдвиг 0: %+v", gate)
	}
	b.c.Queue.Spaces[root] = 1
	lines := b.journal(t, "gate")
	b.round(t)
	gate, err = queue.ReadGate(b.c.Queue.Gate)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(gate.OpenByRoot[root], "MED-DO") || slices.Contains(gate.OpenByRoot[root], "LOW-DO") || len(gate.Open) != 0 {
		t.Fatalf("сдвиг 1: %+v", gate)
	}
	if n := b.journal(t, "gate"); n != lines+1 {
		t.Fatalf("смена сдвига не в журнале: %d, было %d", n, lines)
	}
	// Вход сломан: письма читают из прошлого файла и карту, как tk.
	b.writeHistory(t, map[string]time.Duration{"a": 25 * time.Hour})
	got, err := b.a.queueGatePass(b.c, time.Now(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got.OpenByRoot[root], "MED-DO") {
		t.Fatalf("сломанный вход: %+v", got)
	}
}

// Файл ворот старого формата без поля переписывается сразу: новый tk без поля видит только CRIT и HIGH.
func TestQueueGateOldFormatRewritten(t *testing.T) {
	b := newQueueBench(t, "[]", "[]")
	b.round(t)
	raw, err := os.ReadFile(b.c.Queue.Gate)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "open_by_root")
	raw, err = json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.c.Queue.Gate, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	b.round(t)
	if gate, err := queue.ReadGate(b.c.Queue.Gate); err != nil || gate.OpenByRoot == nil {
		t.Fatalf("поле не появилось: %+v %v", gate, err)
	}
}

func TestQueueTransfers(t *testing.T) {
	b := newQueueBench(t, "[]", "[]")
	tasks := filepath.Join(b.dir, "git", "p", "docs", "tasks")
	body := `{"at":"2026-11-04T10:00:00+01:00","slug":"x","stem":"20261104-1000-HIGH-x","back":"lead","key":"TRK-7","url":"https://x/TRK-7"}
{"at":"2026-11-04T10:00:00+01:00","slug":"nb","stem":"20261104-1001-MED-nb","back":"","key":"TRK-8","url":"https://x/TRK-8"}
не json
{"slug":"nokey","stem":"20261104-1002-MED-nokey","back":"lead"}
{"slug":"badstem","stem":"bad","back":"lead","key":"TRK-9"}
`
	if err := os.WriteFile(filepath.Join(tasks, ".transferred.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got := b.a.queueTransfers(b.c, nil)
	if len(got) != 1 || got[0].Key != "p/20261104-1000-x" || got[0].Back != "lead" || got[0].Transfer != "TRK-7 https://x/TRK-7" ||
		got[0].Status != queue.StatusTransferred || got[0].Dir != filepath.Join(b.dir, "git", "p") {
		t.Fatalf("%+v", got)
	}
}

func TestQueueTasksProject(t *testing.T) {
	dirs := []string{"/h/docs"}
	for tasks, want := range map[string]string{
		"/h/git/p/docs/tasks": "/h/git/p",
		"/h/git/docs/tasks":   "/h/git",
		"/h/docs/tasks":       "/h/docs",
		"/h/docs/docs/tasks":  "/h/docs",
	} {
		if got := queueTasksProject(tasks, dirs); got != want {
			t.Errorf("%s: %s, нужно %s", tasks, got, want)
		}
	}
}
