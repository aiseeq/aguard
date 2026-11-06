package said

import (
	"reflect"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
)

func TestAccountOut(t *testing.T) {
	for _, tc := range []struct {
		msg  string
		want bool
	}{
		{"oneshot: codex: exit status 1: Your workspace is out of credits. Ask your workspace owner to refill", true},
		{"You've reached your usage limit. Please try again later.", true},
		{"quota exceeded", true},
		{"rate limit reached, quota is fine", false},
		{"status 429 usage limit", false},
		{"too many requests", false},
		{"too many requests, quota is fine", false},
		{"oneshot: codex: exit status 1: ", false},
		{"context deadline exceeded", false},
	} {
		if got := AccountOut(tc.msg); got != tc.want {
			t.Errorf("AccountOut(%q) = %v, надо %v", tc.msg, got, tc.want)
		}
	}
}

func TestUsable(t *testing.T) {
	now := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	later, before := now.Add(72*time.Hour), now.Add(-time.Hour)
	for _, tc := range []struct {
		name    string
		account Account
		out     map[string]time.Time
		want    bool
	}{
		{"нет замера — берём", Account{Name: "a"}, nil, true},
		{"живой лимит", Account{Name: "a", Known: true, Used: 22, ResetsAt: later}, nil, true},
		{"ниже запаса", Account{Name: "a", Known: true, Used: 89.9, ResetsAt: later}, nil, true},
		{"запас", Account{Name: "a", Known: true, Used: 90, ResetsAt: later}, nil, false},
		{"100%", Account{Name: "a", Known: true, Used: 100, ResetsAt: later}, nil, false},
		{"100%, сброс прошёл — снова пробуем", Account{Name: "a", Known: true, Used: 100, ResetsAt: before}, nil, true},
		{"запрет ниже 100%", Account{Name: "a", Known: true, Used: 60, ResetsAt: later, Denied: true}, nil, false},
		{"запрет, сброс прошёл", Account{Name: "a", Known: true, Used: 60, ResetsAt: before, Denied: true}, nil, true},
		{"метка впереди", Account{Name: "a"}, map[string]time.Time{"a": now.Add(time.Minute)}, false},
		{"метка кончилась", Account{Name: "a"}, map[string]time.Time{"a": before}, true},
		{"метка чужой учётки", Account{Name: "a"}, map[string]time.Time{"b": later}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, skipped := Usable([]Account{tc.account}, tc.out, 90, now)
			if got := len(ok) == 1; got != tc.want {
				t.Fatalf("взята %v, надо %v; пропуски %v", got, tc.want, skipped)
			}
			if len(ok)+len(skipped) != 1 {
				t.Fatalf("учётка потеряна: взяты %v, пропуски %v", ok, skipped)
			}
		})
	}
}

func TestUsableKeepsOrder(t *testing.T) {
	now := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	ok, _ := Usable([]Account{{Name: "work"}, {Name: "personal"}}, nil, 90, now)
	names := []string{ok[0].Name, ok[1].Name}
	if !reflect.DeepEqual(names, []string{"work", "personal"}) {
		t.Fatalf("порядок %v", names)
	}
}

func TestValidateCeilingAndStall(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Config)
	}{
		{"запас 0", func(c *Config) { c.Ceiling = 0 }},
		{"запас выше 100", func(c *Config) { c.Ceiling = 101 }},
		{"простой короче паузы", func(c *Config) { c.Stall = policy.Duration(time.Minute) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			tc.edit(&c)
			if err := c.Validate(t.TempDir()); err == nil {
				t.Fatal("ошибки нет")
			}
		})
	}
	c := Default()
	if err := c.Validate(t.TempDir()); err != nil {
		t.Fatalf("умолчание: %v", err)
	}
}

func TestStateDown(t *testing.T) {
	t0 := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	st := &State{}
	st.Down(t0, "первая")
	st.Down(t0.Add(time.Hour), "вторая")
	if !st.DownSince.Equal(t0) || st.DownErr != "вторая" {
		t.Fatalf("начало простоя %v, ошибка %q", st.DownSince, st.DownErr)
	}
	st.Waiting(t0.Add(2*time.Hour), "нет учётки")
	at := t0.Add(2 * time.Hour).Format(time.RFC3339)
	if !st.DownSince.Equal(t0) || st.DownErr != "вторая" || st.Skip != at+": нет учётки" {
		t.Fatalf("ожидание задело простой: %+v", st)
	}
	st.Down(t0.Add(3*time.Hour), "третья")
	if st.Skip != "" || st.DownErr != "третья" {
		t.Fatalf("ответ модели не снял причину пропуска: %+v", st)
	}
	st.Waiting(t0.Add(4*time.Hour), "нет учётки")
	st.Signal = t0
	st.Answered()
	if !st.DownSince.IsZero() || st.DownErr != "" || st.Skip != "" || !st.Signal.IsZero() {
		t.Fatalf("ответ не обнулил простой: %+v", st)
	}
	st.Waiting(t0, "нет учётки")
	if !st.DownSince.Equal(t0) {
		t.Fatalf("ожидание не начало простой: %+v", st)
	}
}

func TestStateWhy(t *testing.T) {
	for _, tc := range []struct{ err, skip, want string }{
		{"сеть", "", "Последняя ошибка модели: сеть"},
		{"", "09:00: used 100", "Проверка учёток 09:00: used 100."},
		{"сеть", "09:00: used 100", "Проверка учёток 09:00: used 100. Последняя ошибка модели: сеть"},
	} {
		st := State{DownErr: tc.err, Skip: tc.skip}
		if got := st.Why(); got != tc.want {
			t.Errorf("Why(%q, %q) = %q, надо %q", tc.err, tc.skip, got, tc.want)
		}
	}
}
