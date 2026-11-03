package codexwatch

import (
	"strings"
	"testing"
	"time"
)

func named(name string, used float64, allowed bool) Reading {
	return Reading{Account: Account{Name: name}, Used: used, Allowed: allowed, ResetsAt: time.Date(2026, 11, 9, 18, 41, 0, 0, time.UTC)}
}

// Запрет обычной работы при расходе меньше 100% — тоже «без лимита»: бойцы встают и так. Одна учётка — не «все».
func TestFleetApply(t *testing.T) {
	blocked := []Reading{named("work", 100, false), named("personal", 50, false)}
	for _, c := range []struct {
		name   string
		rs     []Reading
		failed []string
		was    bool
		want   bool // письмо
		after  bool
	}{
		{"обе без лимита", blocked, nil, false, true, true},
		{"уже писали", blocked, nil, true, false, true},
		{"лимит есть — флаг снят", []Reading{named("work", 100, false), named("personal", 5, true)}, nil, true, false, false},
		{"одной нет замера", blocked[:1], []string{"personal"}, false, false, false},
		{"нет замера, но вторая с лимитом — флаг снят", []Reading{named("personal", 5, true)}, []string{"work"}, true, false, false},
		{"две без лимита, третья без замера — письма нет", blocked, []string{"third"}, false, false, false},
		{"одна учётка", blocked[:1], nil, false, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := Fleet{AllOut: c.was}
			msg := f.Apply(c.rs, c.failed)
			if (msg != "") != c.want || f.AllOut != c.after {
				t.Fatalf("письмо %q, флаг %v", msg, f.AllOut)
			}
			if c.want && (!strings.Contains(msg, "work") || !strings.Contains(msg, "personal")) {
				t.Fatalf("письмо без учёток: %q", msg)
			}
		})
	}
}

func TestOthers(t *testing.T) {
	rs := []Reading{named("work", 100, false), named("personal", 5, true)}
	got := Others(rs, []string{"third"}, "work")
	if !strings.Contains(got, "personal 5%") || !strings.Contains(got, "third — замера нет") || strings.Contains(got, "work") {
		t.Fatalf("%q", got)
	}
	if got := Others(rs[:1], nil, "work"); got != "" {
		t.Fatalf("других нет: %q", got)
	}
}
