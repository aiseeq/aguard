package agentwait

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Второе сохранение кеша идёт журналом statefile; LoadState читает только снимок через os.ReadFile.
func TestReviewSidecarJournalLoaded(t *testing.T) {
	projects, workers := t.TempDir(), t.TempDir()
	dir := t.TempDir()
	offsets := filepath.Join(dir, "offsets.json")
	p := &Pass{Projects: projects, Workers: workers, Budget: 1 << 20, MaxOpen: time.Hour}
	rvTask(t, workers, "a", "готово", "s", 1)
	st, err := LoadState(offsets)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Run(st, stamp); err != nil {
		t.Fatal(err)
	}
	if err := st.Save(offsets); err != nil {
		t.Fatal(err)
	}
	rvTask(t, workers, "b", "готово", "t", 1)
	st, err = LoadState(offsets)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Run(st, stamp.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if st.Workers.Tasks["b"] == nil {
		t.Fatal("b не в кеше после прохода")
	}
	if err := st.Save(offsets); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		i, _ := e.Info()
		t.Logf("%s %d", e.Name(), i.Size())
	}
	st, err = LoadState(offsets)
	if err != nil {
		t.Fatal(err)
	}
	if st.Workers.Tasks["b"] == nil {
		t.Fatalf("кеш после LoadState без задачи b: %d задач, root_mod %d", len(st.Workers.Tasks), st.Workers.RootMod)
	}
}
