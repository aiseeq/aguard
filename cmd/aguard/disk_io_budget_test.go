package main

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/config"
	"github.com/aiseeq/aguard/internal/disk"
	"github.com/aiseeq/akit/session"
)

// selfIOBytes — rchar+wchar процесса теста (байты read/write системных вызовов).
func selfIOBytes(t *testing.T) int64 {
	t.Helper()
	f, err := os.Open("/proc/self/io")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var sum int64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ": ")
		if ok && (k == "rchar" || k == "wchar") {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			sum += n
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return sum
}

// TestDiskRoundWithoutWalkKeepsStateIOSmall — мера тикетов self-io и state-json-rewrite: проход disk без обхода
// (раз в минуту) не перечитывает и не переписывает большой state.json. Состояние — ~6 МБ размеров и следов (как на
// машине 01.11: 9 МБ), обход был только что; 10 проходов — бюджет 64 КБ чтения и записи на проход (было ~18 МБ).
// После — размеры записей на месте, а свежие точки свободного места переживают перезапуск: новое состояние,
// загруженное заново, видит их все.
func TestDiskRoundWithoutWalkKeepsStateIOSmall(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	a, _ := testIOApp(t)
	a.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	a.reg = session.Registry{Dir: filepath.Join(t.TempDir(), "sessions"), ProcRoot: "/proc"}
	if err := os.MkdirAll(a.dir("disk"), 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	st := &disk.State{Sizes: map[string][]disk.Point{}, Told: map[string]time.Time{}, LastWalk: now}
	for i := range 4000 {
		pts := make([]disk.Point, 20)
		for j := range pts {
			pts[j] = disk.Point{At: now.Add(-time.Duration(j) * time.Hour), Bytes: int64(i*1000 + j)}
		}
		st.Sizes[filepath.Join(a.home, ".cache", fmt.Sprintf("entry-%05d", i))] = pts
	}
	statePath := filepath.Join(a.dir("disk"), "state.json")
	if err := st.Save(statePath); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default(a.home)
	cfg.Disk.Roots, cfg.Disk.Containers, cfg.Disk.Track = []string{filepath.Join(a.home, ".cache")}, nil, nil
	cfg.Disk.FreeMode = disk.FreeOff
	const rounds = 10
	before := selfIOBytes(t)
	for range rounds {
		if err := a.diskRound(t.Context(), &cfg); err != nil {
			t.Fatal(err)
		}
	}
	spent := selfIOBytes(t) - before
	t.Logf("state %d КБ, %d rounds without walk: IO %d КБ", info.Size()>>10, rounds, spent>>10)
	if budget := int64(rounds * 64 << 10); spent > budget {
		t.Fatalf("IO on %d rounds without walk = %d КБ, budget %d КБ (state %d КБ)", rounds, spent>>10, budget>>10, info.Size()>>10)
	}
	got, err := disk.Load(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Sizes) != 4000 {
		t.Fatalf("sizes after rounds: %d entries, want 4000", len(got.Sizes))
	}
	if len(got.Free) < rounds {
		t.Fatalf("free points after reload: %d, want >= %d", len(got.Free), rounds)
	}
}
