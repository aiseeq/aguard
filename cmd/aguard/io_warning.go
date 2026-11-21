package main

import "time"

// ioWarningLimiter shares per-kind warning throttling between the service and io record.
type ioWarningLimiter struct {
	at map[string]time.Time
}

func (l *ioWarningLimiter) allow(now time.Time, key string, every time.Duration) bool {
	if now.Before(l.at[key]) {
		return false
	}
	if l.at == nil {
		l.at = make(map[string]time.Time)
	}
	l.at[key] = now.Add(every)
	return true
}
