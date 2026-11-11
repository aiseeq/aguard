package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/activity"
	"github.com/aiseeq/aguard/internal/ceiling"
	"github.com/aiseeq/akit/session"
)

func reviewScope(t *testing.T, slice, unit, events string) {
	dir := filepath.Join(slice, unit)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"memory.stat": "file 0\nshmem 0\n", "memory.current": "0\n", "memory.max": "1073741824\n", "memory.events": events} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func reviewApp(home string) *app {
	return &app{home: home, log: slog.New(slog.DiscardHandler), act: activity.New(filepath.Join(home, "act"), time.Now()),
		ceilingAlive: map[string]session.Info{}, ceilingAliveAt: time.Now().Add(time.Hour)}
}

// прежняя находка 1
func TestReviewDuplicateNoticeOnSetFailure(t *testing.T) {
	home := t.TempDir()
	slice := filepath.Join(home, "cg")
	unit := "agent-demo-1.scope"
	reviewScope(t, slice, unit, "oom_kill 1\n")
	if err := ceiling.Write(ceiling.StatePath(home, unit), ceiling.State{Unit: unit, Base: 2 << 30, By: "cl", OOMSeen: true, OOM: 0}); err != nil {
		t.Fatal(err)
	}
	old := ceilingSetProperty
	ceilingSetProperty = func(string, int64) error { return errors.New("unit not loaded") }
	t.Cleanup(func() { ceilingSetProperty = old })
	sent := 0
	a := reviewApp(home)
	a.ceilingJournalSource = func(context.Context, string, time.Time) ([]oomVictim, time.Time, error) {
		return []oomVictim{{PID: 5, Name: "grow", RSS: 1 << 30}}, time.Now(), nil
	}
	a.ceilingNoticeSend = func(ceiling.State, []oomVictim, int64, int64, int64, map[string]session.Info, error) error {
		sent++
		return nil
	}
	for range 3 {
		if err := a.ceilingRound(context.Background(), slice); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("notices for one OOM over 3 passes: %d", sent)
	if sent != 1 {
		t.Errorf("duplicate notices: %d", sent)
	}
}

// прежняя находка 4
func TestReviewMissingAtNotReset(t *testing.T) {
	home := t.TempDir()
	slice := filepath.Join(home, "cg")
	unit := "agent-demo-2.scope"
	path := ceiling.StatePath(home, unit)
	if err := ceiling.Write(path, ceiling.State{Unit: unit, Base: 1 << 30, By: "cl"}); err != nil {
		t.Fatal(err)
	}
	old := ceilingSetProperty
	ceilingSetProperty = func(string, int64) error { return nil }
	t.Cleanup(func() { ceilingSetProperty = old })
	a := reviewApp(home)
	a.ceilingJournalSource = func(context.Context, string, time.Time) ([]oomVictim, time.Time, error) { return nil, time.Time{}, nil }
	if err := a.ceilingRound(context.Background(), slice); err != nil {
		t.Fatal(err)
	}
	reviewScope(t, slice, unit, "oom_kill 0\n")
	if err := a.ceilingRound(context.Background(), slice); err != nil {
		t.Fatal(err)
	}
	st, err := ceiling.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if !st.MissingAt.IsZero() || st.MissingAttempts != 0 {
		t.Errorf("MissingAt survived: %+v", st)
	}
}

// прежняя находка 2: попытки ограничены, файл удаляется
func TestReviewGoneSendFailureBounded(t *testing.T) {
	home := t.TempDir()
	unit := "agent-demo-3.scope"
	path := ceiling.StatePath(home, unit)
	if err := ceiling.Write(path, ceiling.State{Unit: unit, Base: 1 << 30, By: "cl", OOMSeen: true, OOM: 2, OOMNotified: 1}); err != nil {
		t.Fatal(err)
	}
	queries, sends := 0, 0
	a := reviewApp(home)
	a.ceilingJournalSource = func(context.Context, string, time.Time) ([]oomVictim, time.Time, error) {
		queries++
		return []oomVictim{{PID: 5, Name: "grow", RSS: 1 << 30}}, time.Now(), nil
	}
	a.ceilingNoticeSend = func(_ ceiling.State, _ []oomVictim, count int64, _, _ int64, _ map[string]session.Info, _ error) error {
		sends++
		return errors.New("сессии demo нет")
	}
	for i := range 20 {
		cur, err := ceiling.Read(path)
		if err != nil {
			t.Logf("state removed after %d passes, journal queries=%d sends=%d", i, queries, sends)
			return
		}
		// сдвиг времени: последний опрос «раньше секунды назад»
		cur.MissingCheckAt = cur.MissingCheckAt.Add(-2 * time.Second)
		a.ceilingGone(context.Background(), path, cur)
	}
	t.Errorf("state never removed: queries=%d", queries)
}

// новое: письмо в живом scope ушло без имён (журнал ещё не дописан), имена пришли позже — при исчезновении scope второе письмо о том же
func TestReviewGoneResendsAlreadyCountedVictims(t *testing.T) {
	home := t.TempDir()
	unit := "agent-demo-4.scope"
	path := ceiling.StatePath(home, unit)
	notifiedAt := time.Now().Add(-10 * time.Second)
	if err := ceiling.Write(path, ceiling.State{Unit: unit, Base: 1 << 30, By: "cl", OOMSeen: true, OOM: 1, OOMNotified: 1}); err != nil {
		t.Fatal(err)
	}
	var counts []int64
	a := reviewApp(home)
	a.ceilingJournalSource = func(context.Context, string, time.Time) ([]oomVictim, time.Time, error) {
		// строка ядра получена journald после fetchedAt первого письма
		return []oomVictim{{PID: 5, Name: "grow", RSS: 1 << 30}}, notifiedAt.Add(50 * time.Millisecond), nil
	}
	a.ceilingNoticeSend = func(_ ceiling.State, _ []oomVictim, count int64, _, _ int64, _ map[string]session.Info, _ error) error {
		counts = append(counts, count)
		return nil
	}
	cur, _ := ceiling.Read(path)
	a.ceilingGone(context.Background(), path, cur)
	t.Logf("letters after scope gone for an already-notified OOM: %v", counts)
	if len(counts) != 0 {
		t.Errorf("second letter for the same OOM, count=%v", counts)
	}
}

// новое: SaveObserved затирает ClaudePID, записанный cl между Read и записью наблюдателя
func TestReviewClaudePIDLostUpdate(t *testing.T) {
	home := t.TempDir()
	unit := "agent-demo-5.scope"
	path := ceiling.StatePath(home, unit)
	if err := ceiling.Write(path, ceiling.State{Unit: unit, Base: 1 << 30, By: "cl", ClaudePID: 100}); err != nil {
		t.Fatal(err)
	}
	observed, _ := ceiling.Read(path)                                                                               // наблюдатель прочёл в начале прохода
	if _, err := ceiling.Update(path, func(s *ceiling.State) error { s.ClaudePID = 777; return nil }); err != nil { // cl: updateScopeClaudePID
		t.Fatal(err)
	}
	observed.OOMCursor = "observer cursor"
	if _, err := ceiling.SaveObserved(path, observed); err != nil {
		t.Fatal(err)
	}
	got, _ := ceiling.Read(path)
	t.Logf("ClaudePID after race: %d", got.ClaudePID)
	if got.ClaudePID != 777 {
		t.Errorf("cl's ClaudePID lost: %d", got.ClaudePID)
	}
}

// новое: запись состояния на каждом проходе, когда memory.current меняется (так почти всегда)
func TestReviewStateRewrittenWhenCurrentMoves(t *testing.T) {
	home := t.TempDir()
	slice := filepath.Join(home, "cg")
	unit := "agent-demo-6.scope"
	reviewScope(t, slice, unit, "oom_kill 0\n")
	path := ceiling.StatePath(home, unit)
	if err := ceiling.Write(path, ceiling.State{Unit: unit, Base: 1 << 30, By: "cl"}); err != nil {
		t.Fatal(err)
	}
	a := reviewApp(home)
	writes := 0
	var lastIno uint64
	for i := range 5 {
		if err := os.WriteFile(filepath.Join(slice, unit, "memory.current"), []byte(strings.Repeat("1", i+1)+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := a.ceilingRound(context.Background(), slice); err != nil {
			t.Fatal(err)
		}
		fi, _ := os.Stat(path)
		ino := fi.Sys().(*syscall.Stat_t).Ino
		if ino != lastIno {
			writes++
			lastIno = ino
		}
	}
	t.Logf("state rewrites over 5 passes with moving memory.current: %d", writes)
}

// прежняя находка 3 на реальном журнале: жертвы по pid без заголовка
func TestReviewRealKernelLines(t *testing.T) {
	path := os.Getenv("REVIEW_KERN_JSON")
	if path == "" {
		t.Skip("REVIEW_KERN_JSON не задан")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	units := map[string]bool{}
	killed := 0
	for l := range strings.SplitSeq(string(raw), "\n") {
		if strings.Contains(l, "Killed process") {
			killed++
		}
		if m := oomMemcgRE.FindStringSubmatch(l); m != nil {
			units[filepath.Base(strings.TrimSuffix(m[1], `"`))] = true
		}
	}
	total := 0
	for u := range units {
		v, _, err := parseCeilingOOMJSON(string(raw), u, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		total += len(v)
	}
	t.Logf("Killed process lines=%d, victims attributed by nearest header=%d (units %d)", killed, total, len(units))
}
