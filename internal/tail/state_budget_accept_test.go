package tail

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/aiseeq/aguard/internal/testutil"
)

// Мера тикета small-writers: проход меняет единицы записей из тысяч — на диск идёт порядка изменения, а не всё
// состояние. Цикл здесь как проход демона: чтение состояния с диска, правка, сохранение.

const (
	acceptFiles  = 5000
	acceptCycles = 200
	acceptBudget = 32 << 10 // байт записи на цикл, в среднем, с уплотнением
)

// acceptOldFile — состояние в нынешнем формате на диске: обновление не теряет смещения.
func acceptOldFile(t *testing.T, path string) map[string]*File {
	t.Helper()
	want := make(map[string]*File, acceptFiles)
	raw := make(map[string]any, acceptFiles)
	for i := range acceptFiles {
		rel := fmt.Sprintf("-home-aisee-git-project-%04d/01e2b979-a4d0-4a8b-9f6e-%012d.jsonl", i%40, i)
		want[rel] = &File{Offset: int64(i) * 100, From: 0, Name: "master"}
		raw[rel] = map[string]any{"offset": int64(i) * 100, "from": 0, "name": "master"}
	}
	b, err := json.Marshal(map[string]any{"started": true, "files": raw})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return want
}

func TestStateBudgetAccept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "offsets.json")
	want := acceptOldFile(t, path)
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	testutil.StateBudget(t, acceptCycles, acceptBudget, path, func() int { return len(want) }, LoadState,
		func(st *State) bool { return st.Started }, func(st *State) int { return len(st.Files) },
		func(st *State, c int) error {
			testutil.ThreeKeys(c, keys, func(k string) {
				st.Files[k].Offset += 1000
				want[k].Offset = st.Files[k].Offset
			})
			added := fmt.Sprintf("-home-aisee-new/%d.jsonl", c)
			st.Files[added] = &File{Offset: 10, Name: "new"}
			want[added] = &File{Offset: 10, Name: "new"}
			if c%10 == 5 { // запись может и уйти: удалённая не воскресает
				del := fmt.Sprintf("-home-aisee-new/%d.jsonl", c-1)
				delete(st.Files, del)
				delete(want, del)
			}
			return st.Save(path)
		})
	got := testutil.CheckCount(t, path, LoadState, func(st *State) int { return len(st.Files) }, len(want))
	for k, w := range want {
		g, ok := got.Files[k]
		if !ok || *g != *w {
			t.Fatalf("%s: %+v, ждали %+v", k, g, w)
		}
	}
}

// Сохранение без изменений — почти ничего на диске.
func TestStateNoChangeAccept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "offsets.json")
	acceptOldFile(t, path)
	st, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save(path); err != nil {
		t.Fatal(err)
	}
	st, err = LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	start := testutil.Wchar(t)
	if err := st.Save(path); err != nil {
		t.Fatal(err)
	}
	if spent := testutil.Wchar(t) - start; spent > 4<<10 {
		t.Fatalf("сохранение без изменений записало %d байт", spent)
	}
}
