package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestReadForkguardTreeNestedUnlimitedUnit(t *testing.T) {
	root := t.TempDir()
	writeCG := func(dir, current, max, events string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		for name, value := range map[string]string{"pids.current": current, "pids.max": max, "pids.events": events, "pids.peak": current} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeCG(root, "50", "max", "max 3\n")
	nested := filepath.Join(root, "agents-graft.slice")
	writeCG(nested, "30", "max", "max 0\n")
	unit := filepath.Join(nested, "graft-heavy-1.scope")
	writeCG(unit, "12", "max", "max 3\n")
	service := filepath.Join(root, "worker-job-7.service")
	writeCG(service, "8", "4096", "max 1\n")
	writeCG(filepath.Join(service, "nested.scope"), "1", "10", "max 0\n")
	if err := os.WriteFile(filepath.Join(root, "not-a-unit.service"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	s, err := ReadForkguardTree(root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if s.SliceCurrent != 50 || s.SliceMax != 0 || s.SliceEventsMax != 3 || s.SlicePidsPeak != 50 || len(s.Units) != 2 {
		t.Fatalf("sample: %+v", s)
	}
	seen := map[string]bool{}
	for _, u := range s.Units {
		seen[u.Name] = true
		if u.Inode == 0 {
			t.Errorf("unit inode missing: %+v", u)
		}
		if u.Name == "graft-heavy-1.scope" && (u.Max != 0 || u.EventsMax != 3) {
			t.Fatalf("unlimited unit: %+v", u)
		}
	}
	if !seen["worker-job-7.service"] || !seen["graft-heavy-1.scope"] {
		t.Fatalf("nested traversal missed units: %+v", seen)
	}
}

func TestMissingSliceIsAZeroSample(t *testing.T) {
	s, err := ReadForkguardTree(filepath.Join(t.TempDir(), "agents.slice"), time.Now())
	if err != nil || !s.SliceMissing || len(s.Units) != 0 {
		t.Fatalf("missing slice sample=%+v err=%v", s, err)
	}
}

func TestUnitReadErrorIsSkippedAndInodeIsCaptured(t *testing.T) {
	root := t.TempDir()
	writeCG := func(dir string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		for name, value := range map[string]string{"pids.current": "10", "pids.max": "100", "pids.events": "max 1\n", "pids.peak": "10"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeCG(root)
	unit := filepath.Join(root, "worker-job-1.service")
	writeCG(unit)
	if err := os.Remove(filepath.Join(unit, "pids.events")); err != nil {
		t.Fatal(err)
	}
	s, err := ReadForkguardTree(root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Units) != 0 || len(s.Skipped) != 1 || s.Skipped[0].Inode == 0 {
		t.Fatalf("transient read not skipped with identity: %+v", s)
	}
}

func TestForkguardInstallWarningReportsSSHCode(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", ".graft.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, part := range []string{"aguard-forkguard на PC не обновлён (ssh: код $rc)", "else rc=$?;"} {
		if !strings.Contains(text, part) {
			t.Fatalf("install warning missing %q", part)
		}
	}
}

func TestParseForkProcAndSelectCommandTree(t *testing.T) {
	procRoot := t.TempDir()
	cgroup := t.TempDir()
	pids := []int{10, 11, 12, 13, 14, 15}
	parents := map[int]int{10: 1, 11: 10, 12: 11, 13: 10, 14: 13, 15: 12}
	starts := map[int]uint64{10: 5, 11: 10, 12: 11, 13: 12, 14: 13, 15: 14}
	names := map[int]string{10: "claude (main)", 11: "largest", 12: "child", 13: "other", 14: "leaf", 15: "grandchild"}
	for _, pid := range pids {
		dir := filepath.Join(procRoot, strconv.Itoa(pid))
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		raw := fakeForkStat(pid, names[pid], parents[pid], starts[pid])
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(raw), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cgroup, "cgroup.procs"), []byte("10\n11\n12\n13\n14\n15\n"), 0644); err != nil {
		t.Fatal(err)
	}
	pid, name, err := selectForkguardTree(cgroup, procRoot)
	if err != nil || pid != 11 || name != "largest" {
		t.Fatalf("tree root pid=%d name=%q err=%v", pid, name, err)
	}
	parsed, parseErr := parseForkProc(fakeForkStat(99, "has ) paren", 1, 33))
	if parseErr != nil || parsed.ppid != 1 || parsed.startTime != 33 || parsed.name != "has ) paren" {
		t.Fatalf("parsed=%+v err=%v", parsed, parseErr)
	}
	if _, parseErr := parseForkProc("broken"); parseErr == nil {
		t.Fatal("malformed stat must return an explicit error")
	}
}

func fakeForkStat(pid int, name string, ppid int, start uint64) string {
	fields := []string{"S", strconv.Itoa(ppid)}
	for len(fields) < 19 {
		fields = append(fields, "0")
	}
	fields = append(fields, strconv.FormatUint(start, 10))
	return strconv.Itoa(pid) + " (" + name + ") " + strings.Join(fields, " ")
}

// Ядро без pids.peak: замер идёт, пик 0.
func TestSliceWithoutPidsPeak(t *testing.T) {
	root := filepath.Join(t.TempDir(), "agents.slice")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"pids.current": "12", "pids.max": "max", "pids.events": "max 0\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s, err := ReadForkguardTree(root, time.Now())
	if err != nil || s.SliceCurrent != 12 || s.SlicePidsPeak != 0 {
		t.Fatalf("без pids.peak: %+v %v", s, err)
	}
}
