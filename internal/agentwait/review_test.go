package agentwait

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func rvTask(t *testing.T, workers, task, phase, session string, lines int) string {
	t.Helper()
	base := filepath.Join(workers, task)
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "status.json"), fmt.Appendf(nil, `{"phase":%q}`, phase), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, "codex", "sessions", "2026", "11", "18", "rollout-"+session+".jsonl")
	l := make([]string, lines)
	for i := range l {
		l[i] = `{"timestamp":"2026-11-18T10:00:00Z","type":"session_meta","payload":{"id":"` + session + `"}}`
	}
	appendLines(t, path, l...)
	return path
}

// Бюджет кончился в проходе, где готовая задача появилась: следующий проход её пропускает навсегда.
func TestReviewBudgetDoneTail(t *testing.T) {
	projects, workers := t.TempDir(), t.TempDir()
	path := rvTask(t, workers, "a", "готово", "s", 50)
	info, _ := os.Stat(path)
	st := &State{Started: true, Files: map[string]*File{}}
	p := &Pass{Projects: projects, Workers: workers, Budget: 100, MaxOpen: 6 * time.Hour}
	if _, err := p.Run(st, stamp); err != nil {
		t.Fatal(err)
	}
	key := "codex/a/codex/sessions/2026/11/18/rollout-s.jsonl"
	t.Logf("after pass1 offset %d size %d", st.Files[key].Offset, info.Size())
	p.Budget = 1 << 20
	for i := range 3 {
		if _, err := p.Run(st, stamp.Add(time.Duration(i+1)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if st.Files[key].Offset != info.Size() {
		t.Fatalf("хвост готовой задачи не дочитан: offset %d size %d", st.Files[key].Offset, info.Size())
	}
}

// Нечитаемый status.json одной задачи: весь проход без ожиданий, а кеш соседней задачи уже сдвинут.
func TestReviewStatusErrorPartialCache(t *testing.T) {
	projects, workers := t.TempDir(), t.TempDir()
	st := &State{Started: true, Files: map[string]*File{}}
	p := &Pass{Projects: projects, Workers: workers, Budget: 1 << 20, MaxOpen: 6 * time.Hour}
	rvTask(t, workers, "a", "работает", "s", 1)
	if _, err := p.Run(st, stamp); err != nil {
		t.Fatal(err)
	}
	// a дописан и стал готов; b появился с нечитаемым статусом.
	path := filepath.Join(workers, "a", "codex", "sessions", "2026", "11", "18", "rollout-s.jsonl")
	appendLines(t, path, `{"timestamp":"2026-11-18T10:01:00Z","type":"session_meta","payload":{"id":"s"}}`)
	if err := os.WriteFile(filepath.Join(workers, "a", "status.json"), []byte(`{"phase":"готово"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rvTask(t, workers, "b", "готово", "t", 1)
	bs := filepath.Join(workers, "b", "status.json")
	if err := os.Chmod(bs, 0); err != nil {
		t.Fatal(err)
	}
	_, err := p.Run(st, stamp.Add(time.Minute))
	t.Logf("pass2 err: %v", err)
	if err := os.Chmod(bs, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Run(st, stamp.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	key := "codex/a/codex/sessions/2026/11/18/rollout-s.jsonl"
	if st.Files[key].Offset != info.Size() {
		t.Fatalf("хвост a потерян после ошибки статуса b: offset %d size %d", st.Files[key].Offset, info.Size())
	}
}

// Удалённая задача остаётся в кеше навсегда.
func TestReviewDeletedTaskGarbage(t *testing.T) {
	projects, workers := t.TempDir(), t.TempDir()
	st := &State{Started: true, Files: map[string]*File{}}
	p := &Pass{Projects: projects, Workers: workers, Budget: 1 << 20, MaxOpen: 6 * time.Hour}
	rvTask(t, workers, "gone", "готово", "s", 1)
	rvTask(t, workers, "keep", "готово", "k", 1)
	if _, err := p.Run(st, stamp); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(workers, "gone")); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if _, err := p.Run(st, stamp.Add(time.Duration(i+1)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	var keys []string
	for k := range st.Files {
		keys = append(keys, k)
	}
	t.Logf("files %s", strings.Join(keys, ","))
	if _, ok := st.Workers.Tasks["gone"]; ok {
		t.Fatalf("удалённая задача в кеше: %d задач", len(st.Workers.Tasks))
	}
}
