package main

import (
	"os"
	"path/filepath"
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
		for name, value := range map[string]string{"pids.current": current, "pids.max": max, "pids.events": events} {
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
	s, err := ReadForkguardTree(root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if s.SliceCurrent != 50 || s.SliceMax != 0 || len(s.Units) != 2 {
		t.Fatalf("sample: %+v", s)
	}
	seen := map[string]bool{}
	for _, u := range s.Units {
		seen[u.Name] = true
		if u.Name == "graft-heavy-1.scope" && (u.Max != 0 || u.EventsMax != 3) {
			t.Fatalf("unlimited unit: %+v", u)
		}
	}
	if !seen["worker-job-7.service"] || !seen["graft-heavy-1.scope"] {
		t.Fatalf("nested traversal missed units: %+v", seen)
	}
}
