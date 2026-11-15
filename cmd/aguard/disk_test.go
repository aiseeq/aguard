package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/config"
	"github.com/aiseeq/aguard/internal/disk"
	"github.com/aiseeq/akit/session"
)

// Сигнал о свободном месте: отметки по адресатам — в free.json сразу после писем; дошедшему следующее через сутки,
// недошедшему (сессии нет) — повтор через час (8e8571b).
func TestDiskFreeMarksEachRecipient(t *testing.T) {
	a, _ := testIOApp(t)
	if err := os.MkdirAll(a.dir("disk"), 0o700); err != nil { // папку заводит run демона
		t.Fatal(err)
	}
	got := liveLead(t, a)
	cfg := disk.Default()
	cfg.Lead, cfg.FreeTell, cfg.FreeMode, cfg.FreeMin = "lead", []string{"git-admin"}, disk.FreeAlert, 150<<30
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.Local)
	if err := a.diskFree(&cfg, &disk.State{}, now, 140<<30); err != nil {
		t.Fatal(err)
	}
	if msg := <-got; !strings.Contains(msg, "свободно 140") {
		t.Fatalf("письмо лиду: %q", msg)
	}
	told, err := disk.LoadFreeTold(filepath.Join(a.dir("disk"), "free.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !told["lead"].Equal(now.Add(24*time.Hour)) || !told["git-admin"].Equal(now.Add(time.Hour)) {
		t.Fatalf("отметки в free.json: %v", told)
	}
}

// Свежие точки и отметки сигналов переживают перезапуск демона.
func TestDiskRoundRestartPreservesFreeAndTold(t *testing.T) {
	a, _ := testIOApp(t)
	if err := os.MkdirAll(a.dir("disk"), 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	st := &disk.State{
		Sizes: map[string][]disk.Point{"/entry": {{At: now, Bytes: 123}}}, Told: map[string]time.Time{"grew /entry": now},
		LastWalk: now, Free: []disk.Point{{At: now.Add(-time.Minute), Bytes: 100}},
	}
	statePath := filepath.Join(a.dir("disk"), "state.json")
	if err := st.Save(statePath); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default(a.home)
	cfg.Disk.Roots, cfg.Disk.Containers, cfg.Disk.Track = nil, nil, nil
	cfg.Disk.FreeMode = disk.FreeOff
	if err := a.diskRound(t.Context(), &cfg); err != nil {
		t.Fatal(err)
	}

	restarted, _ := testIOApp(t)
	restarted.home, restarted.stateDir = a.home, a.stateDir
	if err := restarted.diskRound(t.Context(), &cfg); err != nil {
		t.Fatal(err)
	}
	got, err := disk.Load(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Free) != 3 || !got.Told["grew /entry"].Equal(now) || len(got.Sizes["/entry"]) != 1 || got.Sizes["/entry"][0].Bytes != 123 {
		t.Fatalf("после перезапуска free=%v told=%v sizes=%v", got.Free, got.Told, got.Sizes)
	}
}

func TestDiskRoundReloadsExternalRoundState(t *testing.T) {
	a, _ := testIOApp(t)
	if err := os.MkdirAll(a.dir("disk"), 0o700); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(a.dir("disk"), "state.json")
	cfg := config.Default(a.home)
	cfg.Disk.Roots, cfg.Disk.Containers, cfg.Disk.Track = nil, nil, nil
	cfg.Disk.FreeMode = disk.FreeOff
	now := time.Now()
	initial := &disk.State{LastWalk: now.Add(-48 * time.Hour), Told: map[string]time.Time{}}
	if err := initial.Save(statePath); err != nil {
		t.Fatal(err)
	}
	if err := a.diskRound(t.Context(), &cfg); err != nil {
		t.Fatal(err)
	}
	if !a.diskState.LastWalk.After(initial.LastWalk) {
		t.Fatalf("начальный просроченный обход не выполнен: last_walk=%v", a.diskState.LastWalk)
	}
	stateInfo, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}

	updatedAt := now.Add(24 * time.Hour)
	updated := &disk.State{LastWalk: updatedAt, Free: []disk.Point{{At: updatedAt, Bytes: 765}},
		Told: map[string]time.Time{"grew /external": updatedAt}}
	if err := updated.SaveFrequent(statePath); err != nil {
		t.Fatal(err)
	}
	roundPath := disk.RoundStatePath(statePath)
	setDiskMtime(t, roundPath, now.Add(time.Minute))
	if err := a.diskRound(t.Context(), &cfg); err != nil {
		t.Fatal(err)
	}
	if !a.diskState.LastWalk.Equal(updatedAt) || !a.diskState.Told["grew /external"].Equal(updatedAt) {
		t.Fatalf("внешний round не подхвачен: last_walk=%v told=%v", a.diskState.LastWalk, a.diskState.Told)
	}
	if len(a.diskState.Free) == 0 || a.diskState.Free[0].Bytes != 765 {
		t.Fatalf("внешний free не подхвачен: %v", a.diskState.Free)
	}
	gotInfo, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !gotInfo.ModTime().Equal(stateInfo.ModTime()) {
		t.Fatalf("проход записал state.json: до %v, после %v", stateInfo.ModTime(), gotInfo.ModTime())
	}
}

func TestDiskRoundReloadsExternalWalkState(t *testing.T) {
	a, _ := testIOApp(t)
	if err := os.MkdirAll(a.dir("disk"), 0o700); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(a.dir("disk"), "state.json")
	cfg := config.Default(a.home)
	cfg.Disk.Roots, cfg.Disk.Containers, cfg.Disk.Track = nil, nil, nil
	cfg.Disk.FreeMode = disk.FreeOff
	now := time.Now()
	initial := &disk.State{LastWalk: now.Add(-48 * time.Hour), Told: map[string]time.Time{}}
	if err := initial.Save(statePath); err != nil {
		t.Fatal(err)
	}
	if _, err := a.diskRoundState(statePath, &cfg, now, 100); err != nil {
		t.Fatal(err)
	}

	updatedAt := now.Add(24 * time.Hour)
	updated := &disk.State{Sizes: map[string][]disk.Point{"/external": {{At: updatedAt, Bytes: 321}}}, LastWalk: updatedAt}
	if err := updated.SaveWalk(statePath); err != nil {
		t.Fatal(err)
	}
	setDiskMtime(t, statePath, now.Add(time.Minute))
	if _, err := a.diskRoundState(statePath, &cfg, now, 100); err != nil {
		t.Fatal(err)
	}
	if got := a.diskState.Sizes["/external"]; len(got) != 1 || got[0].Bytes != 321 {
		t.Fatalf("новые sizes не подхвачены: %v", a.diskState.Sizes)
	}
}

func TestDiskRoundKeepsCachedRoundState(t *testing.T) {
	a, _ := testIOApp(t)
	if err := os.MkdirAll(a.dir("disk"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default(a.home)
	cfg.Disk.Roots, cfg.Disk.Containers, cfg.Disk.Track = nil, nil, nil
	cfg.Disk.FreeMode = disk.FreeOff
	statePath := filepath.Join(a.dir("disk"), "state.json")
	initial := &disk.State{LastWalk: time.Now().Add(24 * time.Hour), Told: map[string]time.Time{}}
	if err := initial.Save(statePath); err != nil {
		t.Fatal(err)
	}
	loads := 0
	a.diskLoadRound = func(path string) (*disk.State, bool, error) {
		loads++
		return disk.LoadRound(path)
	}
	for range 3 {
		if err := a.diskRound(t.Context(), &cfg); err != nil {
			t.Fatal(err)
		}
	}
	if loads != 1 {
		t.Fatalf("LoadRound вызван %d раз, нужен один вызов при живом кеше", loads)
	}
}

func TestDiskRoundKeepsCacheAfterOwnWalk(t *testing.T) {
	a, _ := testIOApp(t)
	if err := os.MkdirAll(a.dir("disk"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default(a.home)
	cfg.Disk.Roots, cfg.Disk.Containers, cfg.Disk.Track = nil, nil, nil
	cfg.Disk.FreeMode = disk.FreeOff
	loads := 0
	a.diskLoadRound = func(path string) (*disk.State, bool, error) {
		loads++
		return disk.LoadRound(path)
	}
	var walkAt time.Time
	for pass := range 3 {
		if err := a.diskRound(t.Context(), &cfg); err != nil {
			t.Fatalf("проход %d: %v", pass+1, err)
		}
		if pass == 0 {
			if a.diskState.LastWalk.IsZero() {
				t.Fatal("первый проход не выполнил обход")
			}
			walkAt = a.diskState.LastWalk
			if _, err := os.Stat(filepath.Join(a.dir("disk"), "state.json")); err != nil {
				t.Fatalf("state.json после собственного обхода: %v", err)
			}
		} else if !a.diskState.LastWalk.Equal(walkAt) {
			t.Fatalf("проход %d повторил обход: last_walk=%v, после первого обхода %v", pass+1, a.diskState.LastWalk, walkAt)
		}
	}
	if loads != 1 {
		t.Fatalf("LoadRound вызван %d раз после собственного обхода и двух проходов без обхода, нужен один", loads)
	}
}

func setDiskMtime(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func TestDiskRoundYieldsAndMarksInterrupted(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // нет pressure.yaml пользователя
	if err := os.MkdirAll(filepath.Join(state, "akit"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "akit", "quiet-until"), []byte("не время\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, _ := testIOApp(t)
	var logged bytes.Buffer
	a.log = slog.New(slog.NewTextHandler(&logged, nil))
	a.reg = session.Registry{Dir: filepath.Join(t.TempDir(), "sessions"), ProcRoot: "/proc"}
	if err := os.MkdirAll(a.dir("disk"), 0o700); err != nil { // папку заводит run демона
		t.Fatal(err)
	}
	cache := filepath.Join(a.home, ".cache", "big")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range 1100 {
		if err := os.WriteFile(filepath.Join(cache, fmt.Sprintf("f%d", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Default(a.home)
	cfg.Disk.Roots, cfg.Disk.Containers, cfg.Disk.Track = []string{filepath.Join(a.home, ".cache")}, nil, nil
	cfg.Disk.FreeMode = disk.FreeOff
	stopped, cancel := context.WithCancel(t.Context())
	cancel()
	if err := a.diskRound(stopped, &cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logged.String(), "interrupted=true") {
		t.Fatalf("оборванный обход не помечен:\n%s", logged.String())
	}
	logged.Reset()
	if err := a.diskRound(t.Context(), &cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logged.String(), "замер машины для паузы обхода") || !strings.Contains(logged.String(), "interrupted=false") {
		t.Fatalf("обход не спросил о машине:\n%s", logged.String())
	}
}
