package said

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/testutil"
)

// Мера тикета small-writers: said/state.json не переписывается целиком на каждом проходе, все части состояния
// переживают чтение с диска.

const (
	acceptFiles  = 5000
	acceptCycles = 200
	acceptBudget = 32 << 10
)

func acceptOldFile(t *testing.T, path string) {
	t.Helper()
	files := make(map[string]any, acceptFiles)
	for i := range acceptFiles {
		files[fmt.Sprintf("-home-aisee-git-p%04d/01e2b979-a4d0-4a8b-9f6e-%012d.jsonl", i%40, i)] = map[string]any{"offset": i * 100, "from": 0}
	}
	last := make(map[string]string, 400)
	for i := range 400 {
		last[fmt.Sprintf("session-%d", i)] = strings.Repeat("текст ", 60)
	}
	pending := make([]map[string]any, 300)
	for i := range pending {
		pending[i] = map[string]any{"event": map[string]any{"Text": strings.Repeat("реплика ", 40), "Line": i}, "in_reply": strings.Repeat("ответ ", 40), "attempts": 0}
	}
	b, err := json.Marshal(map[string]any{"tail": map[string]any{"started": true, "files": files}, "last": last, "pending": pending})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Обычный проход: хвост и последний текст агента двигаются, очередь не меняется.
func TestStateBudgetAccept(t *testing.T) {
	testutil.SkipBudgetUnderRace(t)
	path := filepath.Join(t.TempDir(), "state.json")
	acceptOldFile(t, path)
	st, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(st.Tail.Files))
	for k := range st.Tail.Files {
		keys = append(keys, k)
	}
	wantOffset := make(map[string]int64, len(keys))
	for _, k := range keys {
		wantOffset[k] = st.Tail.Files[k].Offset
	}
	wantLast := map[string]string{}
	testutil.WriteBudget(t, acceptCycles, acceptBudget, func(c int) error {
		st, err := LoadState(path)
		if err != nil {
			return err
		}
		if len(st.Pending) != 300 || len(st.Last) != 400 || !st.Tail.Started {
			t.Fatalf("цикл %d: очередь %d, last %d, started %v", c, len(st.Pending), len(st.Last), st.Tail.Started)
		}
		for i := range 3 {
			k := keys[(c*7+i)%len(keys)]
			st.Tail.Files[k].Offset += 1000
			wantOffset[k] = st.Tail.Files[k].Offset
		}
		s := fmt.Sprintf("session-%d", c%400)
		st.Last[s] = fmt.Sprintf("новый конец %d", c)
		wantLast[s] = st.Last[s]
		return st.Save(path)
	})
	got, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	for k, w := range wantOffset {
		if got.Tail.Files[k] == nil || got.Tail.Files[k].Offset != w {
			t.Fatalf("смещение %s: %+v, ждали %d", k, got.Tail.Files[k], w)
		}
	}
	for k, w := range wantLast {
		if got.Last[k] != w {
			t.Fatalf("last %s: %q, ждали %q", k, got.Last[k], w)
		}
	}
}

// Очередь и пауза меняются — переживают чтение с диска, в том числе очистка очереди и снятие паузы.
func TestStatePendingPauseAccept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	acceptOldFile(t, path)
	pause := time.Now().Add(time.Hour).Truncate(time.Second)

	st, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	st.Pending = st.Pending[1:]
	st.Pending[0].Attempts = 2
	st.Pause = pause
	if err := st.Save(path); err != nil {
		t.Fatal(err)
	}
	st, err = LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Pending) != 299 || st.Pending[0].Attempts != 2 || !st.Pause.Equal(pause) {
		t.Fatalf("очередь %d, попыток %d, пауза %v", len(st.Pending), st.Pending[0].Attempts, st.Pause)
	}

	st.Pending = nil
	st.Pause = time.Time{}
	delete(st.Last, "session-1")
	if err := st.Save(path); err != nil {
		t.Fatal(err)
	}
	st, err = LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Pending) != 0 || !st.Pause.IsZero() {
		t.Fatalf("очередь %d, пауза %v", len(st.Pending), st.Pause)
	}
	if _, ok := st.Last["session-1"]; ok {
		t.Fatal("удалённый last воскрес")
	}
}
