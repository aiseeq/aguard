package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPressureAlertAfterHourAndDailyDedupe(t *testing.T) {
	now := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	sent := 0
	path := t.TempDir() + "/pressure.json"
	alerts := newPressureAlerts(path, func() time.Time { return now }, func(string) error { sent++; return nil }, nil)
	err := errors.New("замер pressure: unavailable")
	alerts.fault("disk", err)
	now = now.Add(30 * time.Minute)
	alerts.fault("track", err)
	now = now.Add(30 * time.Minute)
	alerts.fault("track", err)
	if sent != 1 {
		t.Fatalf("after hour sent=%d, want 1", sent)
	}
	alerts.recovered("disk")
	alerts.recovered("track")
	now = now.Add(time.Hour)
	alerts.fault("track", err)
	if sent != 1 {
		t.Fatalf("daily cooldown sent=%d, want 1", sent)
	}
	now = now.Add(time.Hour)
	alerts.fault("track", err)
	if sent != 1 {
		t.Fatalf("recovered fault resent before daily cooldown: %d", sent)
	}
	now = now.Add(22 * time.Hour)
	alerts.fault("disk", err)
	alerts.fault("disk", err)
	if sent != 2 {
		t.Fatalf("after daily cooldown sent=%d, want 2", sent)
	}
}

func TestPressureRecoveryWithoutFaultDoesNotWriteState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pressure-errors.json")
	alerts := newPressureAlerts(path, time.Now, func(string) error { return nil }, nil)
	for range 50 {
		alerts.recovered("disk")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("healthy recovery created state file: %v", err)
	}
	alerts.fault("disk", errors.New("замер pressure: loadavg invalid"))
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("pressure fault did not persist active state: %v", err)
	}
	alerts.recovered("disk")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("resolved fault state was not cleaned: %v", err)
	}
}

func TestPressureErrorKindNormalizesChangingSamples(t *testing.T) {
	first := errors.New(`замер pressure: loadavg "0.12 0.10 0.08": invalid`)
	second := errors.New(`замер pressure: loadavg "8.12 7.10 6.08": invalid`)
	if pressureErrorKind(first) != pressureErrorKind(second) {
		t.Fatal("loadavg values changed the pressure error kind")
	}
	first = errors.New(`замер pressure: vmstat pswpin "123": invalid`)
	second = errors.New(`замер pressure: vmstat pswpin "456": invalid`)
	if pressureErrorKind(first) != pressureErrorKind(second) {
		t.Fatal("vmstat values changed the pressure error kind")
	}
}
