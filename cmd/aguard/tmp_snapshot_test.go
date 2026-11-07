package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
	"github.com/aiseeq/aguard/internal/record"
)

// Запись без born исключается с WARN, остальные записи снимка остаются (tmp-measure-after-cleanup).
func TestTmpSnapshotDropsEntryWithoutBornWithWarn(t *testing.T) {
	a, log := quietObservedApp(t)
	run, track := t.TempDir(), t.TempDir()
	now := time.Now()
	b, _ := json.Marshal(record.State{Updated: now, Entries: []record.Entry{
		{Path: "/tmp/noborn", Bytes: 10},
		{Path: "/tmp/ok", Bytes: 20, Born: now.Add(-time.Hour)},
	}})
	if err := os.WriteFile(filepath.Join(run, record.StateFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := a.snapshot(&policy.Config{RunDir: run, TrackDir: track})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Entries) != 1 || s.Entries[0].Path != "/tmp/ok" {
		t.Fatalf("записи снимка: %+v", s.Entries)
	}
	if out := log.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "/tmp/noborn") {
		t.Fatalf("ожидался WARN о записи без born: %s", out)
	}
}
