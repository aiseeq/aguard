package disk

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
)

// Мера тикета self-io-reads, вариант A (выбор лида 01.11 22:5x): обход записей читает 5–8 ГБ метаданных btrfs за
// проход, раз в час. Теперь — не раньше walk (1h) и только если свободное место сменилось на walk_change (2 ГБ) с
// прошлого обхода, иначе не реже walk_max (6h); быстрое падение (fast за fast_window) — как раньше.
func TestWalkDueByFreeChange(t *testing.T) {
	c := Default()
	if c.WalkChange != policy.Size(2<<30) || c.WalkMax.D() != 6*time.Hour || c.Walk.D() != time.Hour {
		t.Fatalf("defaults walk=%v walk_change=%d walk_max=%v, want 1h, 2 GiB, 6h", c.Walk.D(), c.WalkChange, c.WalkMax.D())
	}
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	const free = int64(250 << 30)
	st := func() *State { return &State{LastWalk: t0, LastWalkFree: free} }
	cases := []struct {
		name  string
		after time.Duration
		free  int64
		want  bool
	}{
		{"hour, free same", time.Hour, free, false},
		{"hour, 1 GiB used", time.Hour, free - 1<<30, false},
		{"hour, 2.5 GiB used", time.Hour, free - 5<<29, true},
		{"hour, 3 GiB freed", time.Hour, free + 3<<30, true},
		{"30 min, 3 GiB used (slowly)", 30 * time.Minute, free - 3<<30, false},
		{"5h59m, free same", 5*time.Hour + 59*time.Minute, free, false},
		{"6h, free same", 6 * time.Hour, free, true},
	}
	for _, tc := range cases {
		if got := st().WalkDue(&c, t0.Add(tc.after), tc.free); got != tc.want {
			t.Errorf("%s: WalkDue = %v, want %v", tc.name, got, tc.want)
		}
	}
	// Состояние до правки (нет last_walk_free): обход по старому сроку walk, он и запишет last_walk_free.
	old := &State{LastWalk: t0}
	if !old.WalkDue(&c, t0.Add(time.Hour), free) {
		t.Errorf("state without last_walk_free: WalkDue after walk = false, want true")
	}
	// Быстрое падение: fast (5 ГБ) за fast_window — обход через walkGap, как раньше.
	fast := &State{LastWalk: t0, LastWalkFree: free, Free: []Point{{At: t0.Add(5 * time.Minute), Bytes: free}}}
	if !fast.WalkDue(&c, t0.Add(10*time.Minute), free-6<<30) {
		t.Errorf("fast drop: WalkDue = false, want true")
	}
}

// Обход записывает свободное место в момент обхода, и оно переживает сохранение и загрузку круга (round.json).
func TestWalkRecordsFreeAtWalk(t *testing.T) {
	c := Default()
	dir := t.TempDir()
	c.Roots = []string{dir}
	if err := c.Validate(dir); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	s := &State{}
	const free = int64(200 << 30)
	entries := func() ([]string, error) { return nil, nil }
	measure := func([]string) map[string]Usage { return map[string]Usage{} }
	if _, _, err := s.Pass(&c, t0, free, entries, measure, func(string) (string, string) { return "", "" }); err != nil {
		t.Fatal(err)
	}
	if !s.LastWalk.Equal(t0) || s.LastWalkFree != free {
		t.Fatalf("after walk: last_walk=%v last_walk_free=%d, want %v and %d", s.LastWalk, s.LastWalkFree, t0, free)
	}
	path := filepath.Join(dir, "state.json")
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	r, _, err := LoadRound(path)
	if err != nil {
		t.Fatal(err)
	}
	if r.LastWalkFree != free {
		t.Fatalf("round reload: last_walk_free=%d, want %d", r.LastWalkFree, free)
	}
	full, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if full.LastWalkFree != free {
		t.Fatalf("full reload: last_walk_free=%d, want %d", full.LastWalkFree, free)
	}
}
