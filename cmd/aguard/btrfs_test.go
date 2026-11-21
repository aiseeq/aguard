package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/btrfswatch"
	"github.com/aiseeq/aguard/internal/config"
	"github.com/aiseeq/akit/session"
)

func sysfsFile(t *testing.T, root, rel, value string) {
	t.Helper()
	p := filepath.Join(root, "01234567-89ab-cdef-0123-456789abcdef", rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(value), 0o644); err != nil {
		t.Fatal(err)
	}
}

func btrfsFixture(t *testing.T, root string, freeGiB int64, usedPct int64, qgroups bool) {
	t.Helper()
	// disk_total is physical allocation; metadata is duplicated, so its disk_total is twice total_bytes.
	values := map[string]string{
		"devices/dm-0/size":               "209715200",   // 100 GiB in 512-byte sectors
		"allocation/data/disk_total":      "42949672960", // 40 GiB
		"allocation/metadata/disk_total":  "21474836480", // 20 GiB DUP
		"allocation/system/disk_total":    "1073741824",  // 1 GiB
		"allocation/metadata/bytes_used":  fmt.Sprintf("%d", 10*btrfswatch.GiB*usedPct/100),
		"allocation/metadata/total_bytes": fmt.Sprintf("%d", 10*btrfswatch.GiB),
	}
	allocated := 100 - freeGiB
	values["allocation/data/disk_total"] = fmt.Sprintf("%d", (allocated-21)*btrfswatch.GiB)
	for rel, value := range values {
		sysfsFile(t, root, rel, value)
	}
	if qgroups {
		if err := os.Mkdir(filepath.Join(root, "01234567-89ab-cdef-0123-456789abcdef", "qgroups"), 0o755); err != nil {
			t.Fatal(err)
		}
	} else if err := os.RemoveAll(filepath.Join(root, "01234567-89ab-cdef-0123-456789abcdef", "qgroups")); err != nil {
		t.Fatal(err)
	}
}

func TestBtrfsRoundSendsThresholdLettersAndResets(t *testing.T) {
	a, _ := testIOApp(t)
	got := liveLead(t, a)
	root := t.TempDir()
	cfg := config.Default(a.home)
	cfg.Btrfs.SysfsRoot = root
	cfg.Btrfs.Lead = "lead"
	cfg.Btrfs.Tell = nil
	reference, err := btrfswatch.Reference()
	if err != nil {
		t.Fatal(err)
	}
	// The initial state is quiet; below-threshold values produce no message.
	btrfsFixture(t, root, 50, 50, false)
	if err := a.btrfsRound(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-got:
		t.Fatalf("unexpected letter below thresholds: %s", msg)
	default:
	}
	// Trigger each signal independently, receiving a real registered session letter.
	cases := []struct {
		name       string
		free, used int64
		qgroups    bool
		want       string
	}{
		{"unallocated", 19, 50, false, "порог меньше 20 ГБ"},
		{"metadata", 39, 90, false, "порог больше 85%"},
		{"qgroups", 50, 50, true, "включены квоты"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			btrfsFixture(t, root, tc.free, tc.used, tc.qgroups)
			if err := a.btrfsRound(context.Background(), &cfg); err != nil {
				t.Fatal(err)
			}
			select {
			case msg := <-got:
				if !strings.Contains(msg, tc.want) || !strings.Contains(msg, reference) {
					t.Fatalf("letter lacks threshold/reference: %s", msg)
				}
			case <-time.After(time.Second):
				t.Fatal("threshold letter not sent")
			}
			if err := a.btrfsRound(context.Background(), &cfg); err != nil {
				t.Fatal(err)
			}
			select {
			case msg := <-got:
				t.Fatalf("repeat before due: %s", msg)
			default:
			}
		})
	}
	// Recovery removes the mark; a new unallocated-space episode sends immediately again.
	btrfsFixture(t, root, 50, 50, false)
	if err := a.btrfsRound(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	btrfsFixture(t, root, 19, 50, false)
	if err := a.btrfsRound(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-got:
		if !strings.Contains(msg, "порог меньше 20 ГБ") {
			t.Fatalf("new episode letter: %s", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("recovered threshold did not alert again")
	}
}

func TestBtrfsRoundContinuesAfterOneFilesystemReadError(t *testing.T) {
	a, _ := testIOApp(t)
	got := liveLead(t, a)
	root := t.TempDir()
	btrfsFixture(t, root, 19, 50, false)
	bad := filepath.Join(root, "11111111-1111-4111-8111-111111111111", "devices")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default(a.home)
	cfg.Btrfs.SysfsRoot, cfg.Btrfs.Lead, cfg.Btrfs.Tell = root, "lead", nil
	err := a.btrfsRound(context.Background(), &cfg)
	if err == nil || !strings.Contains(err.Error(), "11111111-1111-4111-8111-111111111111") {
		t.Fatalf("read failure must be returned after other FS was checked: %v", err)
	}
	select {
	case msg := <-got:
		if !strings.Contains(msg, "порог меньше 20 ГБ") {
			t.Fatalf("valid FS letter missing: %s", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("valid filesystem was skipped because another filesystem was broken")
	}
}

func TestBtrfsFailedDeliveryAndQuietRetryAfterOneHour(t *testing.T) {
	t.Run("recipient unavailable", func(t *testing.T) {
		a, _ := testIOApp(t)
		sessions := filepath.Join(t.TempDir(), "sessions")
		if err := os.MkdirAll(sessions, 0o700); err != nil {
			t.Fatal(err)
		}
		a.reg = session.Registry{Dir: sessions, ProcRoot: "/proc"}
		root := t.TempDir()
		btrfsFixture(t, root, 19, 50, false)
		cfg := config.Default(a.home)
		cfg.Btrfs.SysfsRoot, cfg.Btrfs.Lead, cfg.Btrfs.Tell = root, "lead", nil
		assertBtrfsRetryAfterHour(t, a, &cfg)
	})
	t.Run("quiet hours", func(t *testing.T) {
		a, _ := testIOApp(t)
		_ = liveLead(t, a)
		writeQuietState(t, a, time.Now().Add(time.Hour))
		root := t.TempDir()
		btrfsFixture(t, root, 19, 50, false)
		cfg := config.Default(a.home)
		cfg.Btrfs.SysfsRoot, cfg.Btrfs.Lead, cfg.Btrfs.Tell = root, "lead", nil
		assertBtrfsRetryAfterHour(t, a, &cfg)
	})
}

func assertBtrfsRetryAfterHour(t *testing.T, a *app, cfg *config.Config) {
	t.Helper()
	before := time.Now()
	if err := a.btrfsRound(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	told, err := btrfswatch.Load(filepath.Join(a.dir("btrfs"), "sent.json"))
	if err != nil {
		t.Fatal(err)
	}
	key := "01234567-89ab-cdef-0123-456789abcdef/unallocated/lead"
	next, ok := told[key]
	if !ok || next.Before(before.Add(time.Hour)) || next.After(after.Add(time.Hour)) {
		t.Fatalf("failed delivery/quiet should schedule retry in one hour: next=%v ok=%t window=[%v,%v]", next, ok, before.Add(time.Hour), after.Add(time.Hour))
	}
}
