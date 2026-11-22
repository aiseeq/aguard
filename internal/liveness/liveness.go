// Package liveness checks heartbeat state files of long-running processes.
package liveness

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"
)

// State is the producer's atomic JSON status file.
type State struct {
	Heartbeat time.Time `json:"heartbeat"`
	LastRowAt time.Time `json:"last_row_at"`
	LagBytes  int64     `json:"lag_bytes"`
	Files     int64     `json:"files"`
	Error     string    `json:"error"`
	PID       int       `json:"pid"`
}

// PIDAlive is replaceable by tests and platform-specific callers.
var PIDAlive = func(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.Stat("/proc/" + strconv.Itoa(pid))
	return err == nil
}

// Check returns whether a decoded state describes a stuck process. Invalid JSON is stuck.
func Check(state []byte, now time.Time, maxAge time.Duration) (bool, string) {
	var s State
	if err := json.Unmarshal(state, &s); err != nil {
		return true, fmt.Sprintf("битый JSON состояния: %v", err)
	}
	if s.Heartbeat.IsZero() || now.Sub(s.Heartbeat) > maxAge {
		return true, "heartbeat старше max_age"
	}
	if s.Heartbeat.After(now.Add(maxAge)) {
		return true, "heartbeat в будущем"
	}
	if s.LagBytes > 0 && (s.LastRowAt.IsZero() || now.Sub(s.LastRowAt) > maxAge) {
		return true, "есть lag, last_row_at старше max_age"
	}
	if s.PID <= 0 || !PIDAlive(s.PID) {
		return true, fmt.Sprintf("процесс pid %d отсутствует", s.PID)
	}
	return false, ""
}
