package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/iowatch"
)

// Review probe: the sudo lookup done by io report must reach the case lines of the report.
func TestIOReportCaseLineKeepsSudoLookup(t *testing.T) {
	a, out := testIOApp(t)
	rows := []journalRecord{
		{Message: "aisee : TTY=pts/17 ; PWD=/home/aisee/Downloads ; USER=root ; COMMAND=dnf upgrade --refresh", PID: "492", Timestamp: "1000000", BootID: "boot"},
		{Message: "pam_unix(sudo:session): session opened for user root(uid=0) by aisee(uid=1000)", PID: "492", Timestamp: "1100000", Session: "c492", BootID: "boot"},
	}
	logind := []journalRecord{{Message: "New session 'c492' of user root.", Timestamp: "1200000", BootID: "boot"}}
	a.sudoJournalSource = appWithSudoFixture(rows, logind).sudoJournalSource
	start := time.UnixMicro(1500000)
	path := filepath.Join(t.TempDir(), "cases.jsonl")
	if err := iowatch.SaveCases(path, []iowatch.Case{{Start: start, DurMS: 3000, BootID: "boot", Culprits: &iowatch.Culprit{Main: "session-c492.scope"}}}); err != nil {
		t.Fatal(err)
	}
	if err := a.ioReport(context.Background(), path, false, start.Add(-time.Second), start.Add(time.Hour), 0, "", false); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "сопоставлено 1 из 1") {
		t.Fatalf("lookup did not match:\n%s", got)
	}
	if !strings.Contains(got, "dnf upgrade --refresh") {
		t.Fatalf("case line lost sudo found by report:\n%s", got)
	}
}
