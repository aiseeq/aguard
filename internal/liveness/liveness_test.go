package liveness

import (
	"encoding/json"
	"testing"
	"time"
)

func TestCheck(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	oldPIDAlive := PIDAlive
	PIDAlive = func(pid int) bool { return pid == 123 }
	t.Cleanup(func() { PIDAlive = oldPIDAlive })
	state := State{Heartbeat: now.Add(-time.Minute), LastRowAt: now.Add(-time.Hour), LagBytes: 0, PID: 123}
	encoded := func(s State) []byte { b, _ := json.Marshal(s); return b }
	tests := []struct {
		name  string
		state State
		raw   []byte
		want  bool
	}{
		{"fresh and idle", state, nil, false},
		{"old heartbeat", State{Heartbeat: now.Add(-11 * time.Minute), PID: 123}, nil, true},
		{"old row with lag", State{Heartbeat: now, LastRowAt: now.Add(-11 * time.Minute), LagBytes: 1, PID: 123}, nil, true},
		{"idle despite old row", state, nil, false},
		{"dead pid", State{Heartbeat: now, PID: 456}, nil, true},
		{"broken JSON", State{}, []byte("{"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := tt.raw
			if raw == nil {
				raw = encoded(tt.state)
			}
			got, _ := Check(raw, now, 10*time.Minute)
			if got != tt.want {
				t.Fatalf("Check() = %t, want %t", got, tt.want)
			}
		})
	}
}
