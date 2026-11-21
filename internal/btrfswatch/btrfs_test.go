package btrfswatch

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
)

func put(t *testing.T, root, rel, value string) {
	t.Helper()
	p := filepath.Join(root, "01234567-89ab-cdef-0123-456789abcdef", rel)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(value), 0644); err != nil {
		t.Fatal(err)
	}
}
func tree(t *testing.T, root string, device, data, metadata, system, used, total int64) {
	t.Helper()
	put(t, root, "devices/dm-0/size", fmtInt(device/512))
	put(t, root, "allocation/data/disk_total", fmtInt(data))
	put(t, root, "allocation/metadata/disk_total", fmtInt(metadata))
	put(t, root, "allocation/system/disk_total", fmtInt(system))
	put(t, root, "allocation/metadata/bytes_used", fmtInt(used))
	put(t, root, "allocation/metadata/total_bytes", fmtInt(total))
}
func fmtInt(n int64) string { return strconv.FormatInt(n, 10) }

func TestReadThresholdsDUPAndQuiet(t *testing.T) {
	root := t.TempDir()
	c := Default()
	device := int64(100 * GiB)
	tree(t, root, device, 20*GiB, 20*GiB, 1*GiB, 9*GiB, 10*GiB)
	put(t, root, "devices/dm-0/size", fmtInt((50*GiB)/512))
	put(t, root, "devices/dm-1/size", fmtInt((50*GiB)/512))
	// metadata disk_total accounts for DUP; using logical total_bytes would overstate unallocated by 10 GiB.
	fs, err := Read(root)
	if err != nil || len(fs) != 1 {
		t.Fatalf("Read = %v, %v", fs, err)
	}
	if fs[0].Unallocated != 59*GiB {
		t.Fatalf("unallocated=%d, want 59 GiB", fs[0].Unallocated)
	}
	if got := fs[0].Alerts(c); len(got) != 0 {
		t.Fatalf("below thresholds: %+v", got)
	}
	fs[0].Unallocated = 20 * GiB
	fs[0].MetadataUsed, fs[0].MetadataTotal = 17*GiB, 20*GiB
	if got := fs[0].Alerts(c); len(got) != 0 {
		t.Fatalf("threshold equality must stay quiet: %+v", got)
	}
	// Each threshold independently emits a signal.
	fs[0].MetadataUsed = 0
	fs[0].Unallocated = 19 * GiB
	if got := fs[0].Alerts(c); len(got) != 1 || got[0].Key != "unallocated" {
		t.Fatalf("unallocated alert: %+v", got)
	}
	fs[0].Unallocated = 39 * GiB
	fs[0].MetadataUsed = 18 * GiB
	fs[0].MetadataTotal = 20 * GiB
	if got := fs[0].Alerts(c); len(got) != 1 || got[0].Key != "metadata" {
		t.Fatalf("metadata alert: %+v", got)
	}
	fs[0].Unallocated = 40 * GiB
	if got := fs[0].Alerts(c); len(got) != 0 {
		t.Fatalf("metadata condition requires unallocated <40 GiB: %+v", got)
	}
	fs[0].Unallocated = 39 * GiB
	fs[0].Qgroups = true
	if got := fs[0].Alerts(c); len(got) != 2 || got[1].Key != "qgroups" {
		t.Fatalf("qgroups alert: %+v", got)
	}
}

func TestReadRejectsInvalidDeviceSpace(t *testing.T) {
	t.Run("empty devices", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "01234567-89ab-cdef-0123-456789abcdef", "devices"), 0o755); err != nil {
			t.Fatal(err)
		}
		if got, err := Read(root); err == nil || len(got) != 0 {
			t.Fatalf("empty devices: files=%v err=%v", got, err)
		}
	})
	t.Run("zero size", func(t *testing.T) {
		root := t.TempDir()
		put(t, root, "devices/dm-0/size", "0")
		if got, err := Read(root); err == nil || len(got) != 0 {
			t.Fatalf("zero device: files=%v err=%v", got, err)
		}
	})
	t.Run("negative unallocated", func(t *testing.T) {
		root := t.TempDir()
		tree(t, root, 40*GiB, 20*GiB, 20*GiB, 1*GiB, 9*GiB, 10*GiB)
		if got, err := Read(root); err == nil || len(got) != 0 {
			t.Fatalf("negative unallocated: files=%v err=%v", got, err)
		}
	})
}

func TestToldRepeatAndReset(t *testing.T) {
	m := Told{}
	now := time.Now()
	key := "uuid/metadata/lead"
	if !m.Due(key, true, now) {
		t.Fatal("first active threshold must alert")
	}
	m.Mark(key, now, true)
	if m.Due(key, true, now.Add(time.Hour)) {
		t.Fatal("sent threshold repeated too early")
	}
	if !m.Due(key, true, now.Add(24*time.Hour)) {
		t.Fatal("daily repeat was not due")
	}
	m.Mark(key, now.Add(24*time.Hour), true)
	if m.Due(key, false, now.Add(25*time.Hour)) {
		t.Fatal("recovered threshold alerted")
	}
	if !m.Due(key, true, now.Add(26*time.Hour)) {
		t.Fatal("threshold after recovery must alert immediately")
	}
}

func TestMetadataUsesIndependentUnallocatedThreshold(t *testing.T) {
	c := Default()
	c.UnallocatedMin = policy.Size(10 * GiB)
	c.MetadataUnallocatedMax = policy.Size(35 * GiB)
	f := FS{Unallocated: 30 * GiB, MetadataUsed: 18 * GiB, MetadataTotal: 20 * GiB}
	if got := f.Alerts(c); len(got) != 1 || got[0].Key != "metadata" {
		t.Fatalf("metadata uses its independent 35 GiB condition: %+v", got)
	}
}

func TestNoFilesystemAndPersistence(t *testing.T) {
	if got, err := Read(filepath.Join(t.TempDir(), "missing")); err != nil || len(got) != 0 {
		t.Fatalf("missing root: %v %v", got, err)
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "features"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := Read(root); err != nil || len(got) != 0 {
		t.Fatalf("sysfs features is not a mounted Btrfs: %v %v", got, err)
	}
	path := filepath.Join(t.TempDir(), "sent.json")
	now := time.Now()
	m := Told{"k": now.Add(time.Hour)}
	if err := m.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil || len(got) != 1 || !got["k"].Equal(m["k"]) {
		t.Fatalf("persistence: %#v %v", got, err)
	}
}

func TestLiveSysfsNumbers(t *testing.T) {
	if os.Getenv("AGUARD_BTRFS_LIVE") != "1" {
		t.Skip("AGUARD_BTRFS_LIVE=1 enables the read-only live sysfs check")
	}
	c := Default()
	files, err := Read(c.SysfsRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no mounted Btrfs in live sysfs")
	}
	for _, f := range files {
		pct := 0
		if f.MetadataTotal > 0 {
			pct = int(f.MetadataUsed * 100 / f.MetadataTotal)
		}
		t.Logf("%s: device=%s allocated=%s unallocated=%s metadata=%s/%s (%d%%) qgroups=%t", f.UUID, policy.HumanBytes(f.DeviceBytes), policy.HumanBytes(f.Allocated), policy.HumanBytes(f.Unallocated), policy.HumanBytes(f.MetadataUsed), policy.HumanBytes(f.MetadataTotal), pct, f.Qgroups)
		if alerts := f.Alerts(c); len(alerts) > 0 {
			t.Errorf("unexpected live Btrfs threshold: %s", fmt.Sprint(alerts))
		}
	}
}
