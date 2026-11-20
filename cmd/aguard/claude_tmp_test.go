package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClaudeTmpFailureBackoffAndNightlyMailDedupe(t *testing.T) {
	now := time.Date(2026, 11, 20, 3, 0, 0, 0, time.Local)
	state := claudeTmpFailureState{Window: "night-1", LastAttempt: now, Mailed: map[string]time.Time{}}
	if claudeTmpRetryAllowed(state, "night-1", now.Add(59*time.Minute)) {
		t.Fatal("failure retried before one hour")
	}
	if !claudeTmpRetryAllowed(state, "night-1", now.Add(time.Hour)) {
		t.Fatal("failure retry remained blocked after one hour")
	}
	if !claudeTmpShouldMail(&state, "night-1", "cause-a", now) {
		t.Fatal("first cause should mail")
	}
	if claudeTmpShouldMail(&state, "night-1", "cause-a", now.Add(time.Hour)) {
		t.Fatal("same cause mailed twice in one night")
	}
	if !claudeTmpShouldMail(&state, "night-1", "cause-b", now.Add(time.Hour)) {
		t.Fatal("distinct cause should mail once")
	}
	if !claudeTmpShouldMail(&state, "night-2", "cause-a", now.Add(24*time.Hour)) {
		t.Fatal("new night should permit a fresh cause alert")
	}
}

func TestCorruptClaudeTmpFailureStateResetsAndWarns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "failure-state.json")
	if err := os.WriteFile(path, []byte("{bad json"), 0o600); err != nil {
		t.Fatal(err)
	}
	warnings := 0
	state, err := loadClaudeTmpFailureState(path, func(error) { warnings++ })
	if err != nil {
		t.Fatal(err)
	}
	if warnings != 1 || state.Window != "" || !state.LastAttempt.IsZero() || state.Mailed == nil {
		t.Fatalf("corrupt state reset: state=%+v warnings=%d", state, warnings)
	}
}
