package codexwatch

import (
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var (
	t0    = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	reset = t0.Add(6 * 24 * time.Hour) // плановый сброс через 6 суток
)

func cfg() *Config {
	c := Default("/home/u")
	return &c
}

// feed — замеры каждые step от from до to включительно; used(t) — непрерывный расход, в замер идёт целое (как у
// app-server). Возвращает письма с временем замера.
type got struct {
	at time.Time
	n  Notice
}

func feed(s *State, c *Config, from, to time.Time, step time.Duration, used func(time.Time) float64,
	mod func(*Reading)) []got {
	var out []got
	for at := from; !at.After(to); at = at.Add(step) {
		r := Reading{At: at, Used: math.Min(100, math.Floor(used(at))), ResetsAt: reset, Allowed: true, Available: 1}
		if mod != nil {
			mod(&r)
		}
		_, ns := s.Apply(r, c)
		for _, n := range ns {
			out = append(out, got{at, n})
		}
	}
	return out
}

func linear(start float64, at time.Time, perHour float64) func(time.Time) float64 {
	return func(t time.Time) float64 { return start + perHour*t.Sub(at).Hours() }
}

func kinds(g []got) string {
	var b []string
	for _, x := range g {
		b = append(b, string(x.n.Kind)+"@"+x.at.Format("15:04"))
	}
	return strings.Join(b, " ")
}

// Быстрый расход 4,5 %/ч (как 02.11): письмо 1 — когда до 100% не больше 2 ч, но не раньше 2 ч 10 мин до него;
// письмо «100%» — один раз; оба — по разу.
func TestFastBurnWarnThenFull(t *testing.T) {
	var s State
	c := cfg()
	g := feed(&s, c, t0, t0.Add(8*time.Hour), 10*time.Minute, linear(70, t0, 4.5), nil)
	if len(g) != 2 || g[0].n.Kind != KindWarn || g[1].n.Kind != KindFull {
		t.Fatalf("письма: %s", kinds(g))
	}
	hit := t0.Add(time.Duration(30 / 4.5 * float64(time.Hour)))
	if lead := hit.Sub(g[0].at); lead > 2*time.Hour+10*time.Minute || lead < 90*time.Minute {
		t.Fatalf("письмо 1 за %v до 100%% (%s)", lead, hit.Format("15:04"))
	}
	if g[1].at.Sub(hit) > 10*time.Minute {
		t.Fatalf("письмо 100%% в %s, 100%% в %s", g[1].at.Format("15:04"), hit.Format("15:04"))
	}
	if !strings.Contains(g[0].n.Message, "codex-reset-on-approval") || !strings.Contains(g[1].n.Message, "пора") {
		t.Fatalf("тексты:\n%s\n%s", g[0].n.Message, g[1].n.Message)
	}
}

// Медленный расход: 100% после планового сброса — писем нет.
func TestSlowBurnSilent(t *testing.T) {
	var s State
	g := feed(&s, cfg(), t0, t0.Add(12*time.Hour), 10*time.Minute, linear(50, t0, 0.3), nil)
	if len(g) != 0 {
		t.Fatalf("письма: %s", kinds(g))
	}
}

// 100% раньше сброса, но меньше чем за сутки до него — сброс не нужен, писем нет.
func TestShortIdleSilent(t *testing.T) {
	var s State
	c := cfg()
	near := t0.Add(20 * time.Hour)
	g := feed(&s, c, t0, t0.Add(8*time.Hour), 10*time.Minute, linear(70, t0, 4.5), func(r *Reading) { r.ResetsAt = near })
	if len(g) != 0 {
		t.Fatalf("письма: %s", kinds(g))
	}
}

// Сброса нет: одна строка журнала, писем нет.
func TestNoCreditLogOnly(t *testing.T) {
	var s State
	g := feed(&s, cfg(), t0, t0.Add(8*time.Hour), 10*time.Minute, linear(70, t0, 4.5), func(r *Reading) { r.Available = 0 })
	if len(g) != 1 || g[0].n.Kind != KindNoCredit {
		t.Fatalf("решения: %s", kinds(g))
	}
}

// Противник 2: темп 2 %/ч два часа, с 12:00 — 10 %/ч, 100% в 14:00: письмо 1 не позже 12:30 (наклон за 30 мин),
// а не когда его догонит двухчасовой.
func TestSpeedUpSeenByShortWindow(t *testing.T) {
	var s State
	used := func(at time.Time) float64 {
		if at.Before(t0) {
			return 80 + 2*at.Sub(t0).Hours()
		}
		return 80 + 10*at.Sub(t0).Hours()
	}
	g := feed(&s, cfg(), t0.Add(-2*time.Hour), t0.Add(3*time.Hour), 10*time.Minute, used, nil)
	if len(g) == 0 || g[0].n.Kind != KindWarn || g[0].at.After(t0.Add(30*time.Minute)) {
		t.Fatalf("письма: %s", kinds(g))
	}
}

// Противник 4: после письма 1 кредит пропал — письмо «100%» всё равно уходит, с тем, что сброса нет.
func TestFullAfterCreditGone(t *testing.T) {
	var s State
	c := cfg()
	g := feed(&s, c, t0, t0.Add(4*time.Hour), 10*time.Minute, linear(85, t0, 4.5), func(r *Reading) {
		if r.At.After(t0.Add(2 * time.Hour)) {
			r.Available = 0
		}
	})
	if len(g) != 2 || g[0].n.Kind != KindWarn || g[1].n.Kind != KindFull || !strings.Contains(g[1].n.Message, "больше нет") {
		t.Fatalf("письма: %s; %v", kinds(g), g)
	}
}

// Противник 5: ordinaryUsageAllowed=false при 70% — письмо «запрещена, это не 100%», не «пора»; один раз.
func TestBlockedBelowFullIsNotFull(t *testing.T) {
	var s State
	g := feed(&s, cfg(), t0, t0.Add(time.Hour), 10*time.Minute, func(time.Time) float64 { return 70 },
		func(r *Reading) { r.Allowed = false })
	if len(g) != 1 || g[0].n.Kind != KindBlocked || strings.Contains(g[0].n.Message, "пора") {
		t.Fatalf("письма: %s", kinds(g))
	}
}

// Противник 6: сразу после смены окна расход 2,5 %/мин, 100% через 40 мин — письмо 1 раньше 100%.
func TestBurstAfterWindowReset(t *testing.T) {
	s := State{ResetsAt: t0, Warned: true, Full: true, Samples: []Sample{{t0.Add(-time.Hour), 100}}}
	g := feed(&s, cfg(), t0, t0.Add(40*time.Minute), 10*time.Minute, linear(0, t0, 150), nil)
	if len(g) != 2 || g[0].n.Kind != KindWarn || g[1].n.Kind != KindFull {
		t.Fatalf("письма: %s", kinds(g))
	}
}

// Противник 7: после письма 1 работа встала на 12 ч — быстрый шаг кончается, когда прогноз отходит от Near;
// за паузу — шаг Interval, а не 360 чтений по 2 мин.
func TestPauseAfterWarnBackToInterval(t *testing.T) {
	var s State
	c := cfg()
	used := func(at time.Time) float64 {
		if at.Before(t0.Add(4 * time.Hour)) {
			return 70 + 6*at.Sub(t0).Hours()
		}
		return 94
	}
	feed(&s, c, t0, t0.Add(4*time.Hour), 10*time.Minute, used, nil)
	if !s.Warned {
		t.Fatal("письма 1 нет")
	}
	fast := 0
	for at := t0.Add(4*time.Hour + 10*time.Minute); at.Before(t0.Add(16 * time.Hour)); at = at.Add(10 * time.Minute) {
		r := Reading{At: at, Used: 94, ResetsAt: reset, Allowed: true, Available: 1}
		f, _ := s.Apply(r, c)
		if Step(r, f, c) == c.Fast.D() {
			fast++
		}
	}
	if fast > 3 {
		t.Fatalf("быстрый шаг в паузе %d раз", fast)
	}
}

// Быстрый шаг: used ≥ 99 или прогноз ближе Near; после 100% — обычный.
func TestStep(t *testing.T) {
	c := cfg()
	r := Reading{At: t0, Used: 99}
	if Step(r, Forecast{}, c) != c.Fast.D() {
		t.Fatal("99% — не быстрый шаг")
	}
	r.Used = 90
	if Step(r, Forecast{At: t0.Add(20 * time.Minute)}, c) != c.Fast.D() || Step(r, Forecast{At: t0.Add(time.Hour)}, c) != c.Interval.D() {
		t.Fatal("шаг по прогнозу")
	}
	r.Used = 100
	if Step(r, Forecast{}, c) != c.Interval.D() {
		t.Fatal("100% — быстрый шаг")
	}
}

// Смена resetsAt — флаги окна заново: новое окно может снова кончиться досрочно.
func TestNewWindowResetsFlags(t *testing.T) {
	s := State{ResetsAt: reset.Add(-7 * 24 * time.Hour), Warned: true, Full: true, Samples: []Sample{{t0, 100}}}
	s.Apply(Reading{At: t0.Add(time.Minute), Used: 0, ResetsAt: reset, Allowed: true, Available: 1}, cfg())
	if s.Warned || s.Full || len(s.Samples) != 1 || !s.ResetsAt.Equal(reset) {
		t.Fatalf("состояние: %+v", s)
	}
}

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := State{ResetsAt: reset, Warned: true, Samples: []Sample{{t0, 91}}}
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	back, err := LoadState(path)
	if err != nil || !back.Warned || len(back.Samples) != 1 || !back.ResetsAt.Equal(reset) {
		t.Fatalf("%+v %v", back, err)
	}
}

func TestValidate(t *testing.T) {
	c := cfg()
	if err := c.Validate("/home/u"); err != nil {
		t.Fatal(err)
	}
	c.Mode = "send"
	if c.Validate("/home/u") == nil {
		t.Fatal("mode send принят")
	}
}
