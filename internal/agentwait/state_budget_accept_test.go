package agentwait

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/aiseeq/aguard/internal/testutil"
)

// Мера тикета small-writers: проход меняет единицы записей из тысяч — на диск идёт порядка изменения.

const (
	acceptFiles  = 9000
	acceptCycles = 200
	acceptBudget = 32 << 10
)

func TestStateBudgetAccept(t *testing.T) {
	testutil.SkipBudgetUnderRace(t)
	path := filepath.Join(t.TempDir(), "offsets.json")
	want := make(map[string]*File, acceptFiles)
	raw := make(map[string]any, acceptFiles)
	keys := make([]string, 0, acceptFiles)
	for i := range acceptFiles {
		rel := fmt.Sprintf("/home/aisee/.claude/projects/-home-aisee-git-p%04d/01e2b979-a4d0-4a8b-9f6e-%012d.jsonl", i%40, i)
		want[rel] = &File{Offset: int64(i) * 100}
		raw[rel] = map[string]any{"offset": int64(i) * 100}
		keys = append(keys, rel)
	}
	b, err := json.Marshal(map[string]any{"started": true, "files": raw})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	testutil.StateBudget(t, acceptCycles, acceptBudget, path, func() int { return len(want) }, LoadState,
		func(st *State) bool { return st.Started }, func(st *State) int { return len(st.Files) },
		func(st *State, c int) error {
			testutil.ThreeKeys(c, keys, func(k string) {
				st.Files[k].Offset += 1000
				st.Files[k].Seen = map[string]int64{"toolu_01": int64(c)}
				want[k] = &File{Offset: st.Files[k].Offset, Seen: map[string]int64{"toolu_01": int64(c)}}
			})
			return st.Save(path)
		})
	got := testutil.CheckCount(t, path, LoadState, func(st *State) int { return len(st.Files) }, len(want))
	for k, w := range want {
		g, ok := got.Files[k]
		if !ok || g.Offset != w.Offset || !maps.Equal(g.Seen, w.Seen) {
			t.Fatalf("%s: %+v, ждали %+v", k, g, w)
		}
	}
}
