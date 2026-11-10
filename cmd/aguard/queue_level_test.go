package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aiseeq/aguard/internal/queue"
)

// Ворота — только слово пользователя (10.11: «Мы просто меняем планку по моему слову без всяких таймеров»; «Я хотел для
// начала просто указывать через чат какие уровни выставить - и всё. Только этот механизм должен действовать сейчас»).
// Запас в истории — такой, при котором прежняя формула открывала все пары; таблица и поправки по старым путям — «да» на всё.
func TestQueueGateUserLevels(t *testing.T) {
	b := newQueueBench(t, "[]", "[]")
	cfgDir := filepath.Join(b.dir, ".config", "queue")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var table, override strings.Builder
	for _, pair := range []string{"MED-DO", "MED-HUNT", "MED-ARCH", "LOW-DO", "LOW-HUNT", "LOW-ARCH"} {
		table.WriteString(pair + "\tда\tда\tда\n")
		override.WriteString(pair + "\tда\t2099-12-31\tlead\tпроба\n")
	}
	for name, body := range map[string]string{"gate.tsv": table.String(), "override.tsv": override.String()} {
		if err := os.WriteFile(filepath.Join(cfgDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git, work := filepath.Join(b.dir, "git"), filepath.Join(b.dir, "work")
	b.c.Queue.Spaces = map[string]queue.Level{git: queue.LevelMed, work: queue.LevelMed}
	b.round(t)
	gate, err := queue.ReadGate(b.c.Queue.Gate)
	if err != nil {
		t.Fatal(err)
	}
	med := []string{"MED-DO", "MED-HUNT", "MED-ARCH"}
	if !slices.Equal(gate.OpenByRoot[git], med) || !slices.Equal(gate.OpenByRoot[work], med) || len(gate.OpenByRoot) != 2 {
		t.Fatalf("в пространствах только MED: %v", gate.OpenByRoot)
	}
	if len(gate.Open) != 0 || len(gate.Lane) != 0 {
		t.Fatalf("level по умолчанию HIGH — open пуст, lane пуст всегда: open %v, lane %v", gate.Open, gate.Lane)
	}
	raw, err := os.ReadFile(b.c.Queue.Gate)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"lane":[]`) {
		t.Fatalf("поле lane остаётся пустым списком, его читает lane llmark: %s", raw)
	}
	// Сломанная история лимитов ворот не касается: слово пользователя действует при любом запасе.
	if err := os.WriteFile(b.c.Queue.History, []byte("{битая\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b.c.Queue.Level = queue.LevelLow
	b.round(t)
	if gate, err = queue.ReadGate(b.c.Queue.Gate); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(gate.Open, queue.LevelLow.Pairs()) || !slices.Equal(gate.OpenByRoot[git], med) {
		t.Fatalf("level LOW вне корней, MED в корнях: %+v", gate)
	}
}

// Старый файл с непустым lane переписывается в тот же проход: lane llmark не берёт пары по прошлому расчёту ещё час.
func TestQueueGateOldLaneRewritten(t *testing.T) {
	b := newQueueBench(t, "[]", "[]")
	b.round(t)
	gate, err := queue.ReadGate(b.c.Queue.Gate)
	if err != nil {
		t.Fatal(err)
	}
	gate.Lane = []string{"MED-DO"}
	if err := gate.Write(b.c.Queue.Gate); err != nil {
		t.Fatal(err)
	}
	b.round(t)
	if gate, err = queue.ReadGate(b.c.Queue.Gate); err != nil || len(gate.Lane) != 0 {
		t.Fatalf("lane не переписан: %+v %v", gate.Lane, err)
	}
}
