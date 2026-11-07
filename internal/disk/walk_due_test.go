package disk

import (
	"path/filepath"
	"strings"
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
		want := WalkReason("")
		if tc.want {
			want = WalkPlan
		}
		if got := st().WalkDue(&c, t0.Add(tc.after), tc.free); got != want {
			t.Errorf("%s: WalkDue = %q, want %q", tc.name, got, want)
		}
	}
	// Состояние до правки (нет last_walk_free): обход по старому сроку walk, он и запишет last_walk_free.
	old := &State{LastWalk: t0}
	if got := old.WalkDue(&c, t0.Add(time.Hour), free); got != WalkPlan {
		t.Errorf("state without last_walk_free: WalkDue = %q, want %q", got, WalkPlan)
	}
	// Быстрое падение: fast (5 ГБ) за fast_window — обход через walkGap, как раньше.
	fast := &State{LastWalk: t0, LastWalkFree: free, Free: []Point{{At: t0.Add(5 * time.Minute), Bytes: free}}}
	if got := fast.WalkDue(&c, t0.Add(10*time.Minute), free-6<<30); got != WalkFast {
		t.Errorf("fast drop: WalkDue = %q, want %q", got, WalkFast)
	}
}

func validateWalkTestConfig(t *testing.T, c *Config) {
	t.Helper()
	if err := c.Validate(t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

func TestWalkWindowReasonsAndClosure(t *testing.T) {
	oldLocal := time.Local
	time.Local = time.UTC
	defer func() { time.Local = oldLocal }()

	c := Default()
	c.WalkWindow = "02:00-05:00"
	c.WalkMax = policy.Duration(24 * time.Hour)
	validateWalkTestConfig(t, &c)
	t0 := time.Date(2026, 11, 7, 10, 36, 0, 0, time.Local)
	s := &State{LastWalk: t0, LastWalkFree: 100}
	cases := []struct {
		at   time.Time
		want WalkReason
	}{
		{time.Date(2026, 11, 8, 10, 40, 0, 0, time.Local), ""},
		{time.Date(2026, 11, 9, 1, 59, 0, 0, time.Local), ""},
		{time.Date(2026, 11, 9, 2, 0, 0, 0, time.Local), WalkWindow},
	}
	for _, tc := range cases {
		if got := s.WalkDue(&c, tc.at, 100); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.at, got, tc.want)
		}
	}
	s.LastWindow = "2026-11-09"
	if got := s.WalkDue(&c, time.Date(2026, 11, 9, 2, 3, 0, 0, time.Local), 100); got != "" {
		t.Errorf("closed window: got %q", got)
	}

	fastAtDay := &State{LastWalk: t0, LastWalkFree: 10 << 30,
		Free: []Point{{At: time.Date(2026, 11, 8, 13, 55, 0, 0, time.Local), Bytes: 10 << 30}}}
	if got := fastAtDay.WalkDue(&c, time.Date(2026, 11, 8, 14, 0, 0, 0, time.Local), 4<<30); got != WalkFast {
		t.Errorf("daytime fast: got %q, want fast", got)
	}
	fastAtNight := &State{LastWalk: t0, LastWalkFree: 10 << 30, Told: map[string]time.Time{},
		Free: []Point{{At: time.Date(2026, 11, 9, 1, 55, 0, 0, time.Local), Bytes: 10 << 30}}}
	now := time.Date(2026, 11, 9, 2, 0, 0, 0, time.Local)
	if got := fastAtNight.WalkDue(&c, now, 4<<30); got != WalkFast {
		t.Errorf("fast in window: got %q, want fast", got)
	}
	entries := func() ([]string, error) { return nil, nil }
	measure := func([]string) map[string]Usage { return map[string]Usage{} }
	if _, _, err := fastAtNight.Pass(&c, now, 4<<30, entries, measure, func(string) (string, string) { return "", "" }); err != nil {
		t.Fatal(err)
	}
	if got := fastAtNight.WalkDue(&c, now.Add(5*time.Minute), 4<<30); got != "" {
		t.Errorf("fast should close window: got %q", got)
	}

	missed := &State{LastWalk: time.Date(2026, 11, 1, 2, 0, 0, 0, time.Local), LastWalkFree: 100}
	if got := missed.WalkDue(&c, time.Date(2026, 11, 3, 10, 0, 0, 0, time.Local), 100); got != WalkMissed {
		t.Errorf("missed window: got %q, want missed", got)
	}
}

func TestWalkWindowOvernightAndClockRollback(t *testing.T) {
	oldLocal := time.Local
	time.Local = time.UTC
	defer func() { time.Local = oldLocal }()

	c := Default()
	c.WalkWindow = "23:00-01:00"
	c.WalkMax = policy.Duration(24 * time.Hour)
	validateWalkTestConfig(t, &c)
	s := &State{LastWalk: time.Date(2026, 11, 1, 23, 10, 0, 0, time.Local), LastWalkFree: 100, LastWindow: "2026-11-01"}
	if got := s.WalkDue(&c, time.Date(2026, 11, 2, 0, 30, 0, 0, time.Local), 100); got != "" {
		t.Errorf("same overnight instance: got %q", got)
	}
	if got := s.WalkDue(&c, time.Date(2026, 11, 2, 23, 0, 0, 0, time.Local), 100); got != WalkWindow {
		t.Errorf("next overnight instance: got %q, want window", got)
	}

	c.WalkWindow = "02:00-05:00"
	validateWalkTestConfig(t, &c)
	rollback := &State{LastWalk: time.Date(2026, 11, 9, 2, 0, 0, 0, time.Local), LastWalkFree: 100, LastWindow: "2026-11-09"}
	if got := rollback.WalkDue(&c, time.Date(2026, 11, 8, 2, 30, 0, 0, time.Local), 100); got != WalkWindow {
		t.Errorf("clock rollback: got %q, want window", got)
	}
}

func TestWalkWindowDSTRepeatedHour(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Belgrade")
	if err != nil {
		t.Fatal(err)
	}
	oldLocal := time.Local
	time.Local = loc
	defer func() { time.Local = oldLocal }()

	first, err := time.Parse(time.RFC3339, "2026-10-25T02:10:00+02:00")
	if err != nil {
		t.Fatal(err)
	}
	second, err := time.Parse(time.RFC3339, "2026-10-25T02:10:00+01:00")
	if err != nil {
		t.Fatal(err)
	}
	if first.In(loc).Format("-07:00") == second.In(loc).Format("-07:00") {
		t.Fatal("expected repeated times with different offsets")
	}
	c := Default()
	c.WalkWindow = "02:00-03:00"
	validateWalkTestConfig(t, &c)
	s := &State{LastWalk: time.Date(2026, 10, 24, 2, 0, 0, 0, loc), LastWalkFree: 100}
	if got := s.WalkDue(&c, first, 100); got != WalkWindow {
		t.Fatalf("first repeated hour: got %q, want window", got)
	}
	s.LastWindow = "2026-10-25"
	if got := s.WalkDue(&c, second, 100); got != "" {
		t.Errorf("second repeated hour: got %q, want closed", got)
	}
}

func TestWalkWindowValidationAndLoadNewestWalk(t *testing.T) {
	oldLocal := time.Local
	time.Local = time.UTC
	defer func() { time.Local = oldLocal }()

	for _, value := range []string{"2:00-05:00", "24:00-01:00", "02:00-02:00", "02:00/05:00"} {
		c := Default()
		c.WalkWindow = value
		if err := c.Validate(t.TempDir()); err == nil || !strings.Contains(err.Error(), "disk.walk_window") {
			t.Errorf("Validate(%q) error = %v, want disk.walk_window error", value, err)
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	old := &State{LastWalk: time.Date(2026, 11, 8, 1, 0, 0, 0, time.Local), LastWalkFree: 10, LastWindow: "2026-11-08"}
	if err := old.Save(path); err != nil {
		t.Fatal(err)
	}
	newer := &State{LastWalk: time.Date(2026, 11, 8, 2, 0, 0, 0, time.Local), LastWalkFree: 20, LastWindow: "2026-11-08"}
	if err := newer.SaveWalk(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.LastWalk.Equal(newer.LastWalk) || loaded.LastWalkFree != newer.LastWalkFree || loaded.LastWindow != newer.LastWindow {
		t.Fatalf("Load got walk=%v free=%d window=%q, want newest %+v", loaded.LastWalk, loaded.LastWalkFree, loaded.LastWindow, newer)
	}
	c := Default()
	c.WalkWindow = "02:00-05:00"
	validateWalkTestConfig(t, &c)
	if got := loaded.WalkDue(&c, time.Date(2026, 11, 8, 2, 10, 0, 0, time.Local), 20); got != "" {
		t.Errorf("walk after recovered save: got %q", got)
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
