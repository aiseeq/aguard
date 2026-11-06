package codexwatch

import (
	"testing"
	"time"
)

// Запрет ниже 100% виден в состоянии: said читает его из файла (пропуск учётки), Allowed снова true снимает его.
func TestRecordKeepsDenied(t *testing.T) {
	at := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	reset := at.Add(72 * time.Hour)
	s := &State{}
	s.record(Reading{At: at, Used: 60, ResetsAt: reset, Allowed: false})
	if !s.Denied {
		t.Fatal("запрет не записан")
	}
	s.record(Reading{At: at.Add(time.Minute), Used: 60, ResetsAt: reset, Allowed: true})
	if s.Denied {
		t.Fatal("запрет не снят")
	}
	s.record(Reading{At: at.Add(2 * time.Minute), Used: 60, ResetsAt: reset.Add(7 * 24 * time.Hour), Allowed: false})
	if !s.Denied {
		t.Fatal("запрет в новом окне не записан")
	}
}
