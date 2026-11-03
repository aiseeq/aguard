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

// Учётки по умолчанию не делят ни имя (файл состояния), ни CODEX_HOME (лимит и кредит), ни файл «да» (взвод).
func TestDefaultAccountsDistinct(t *testing.T) {
	seen := map[string]bool{}
	accounts := Default("/home/u").Accounts
	if len(accounts) != 2 {
		t.Fatalf("учётки: %+v", accounts)
	}
	for _, a := range accounts {
		for _, v := range []string{a.Name, a.Home, a.Approvals} {
			if v == "" || seen[v] {
				t.Fatalf("пусто или повтор %q: %+v", v, accounts)
			}
			seen[v] = true
		}
	}
}

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
		_, ns, consume := s.Apply(r, c)
		for _, n := range ns {
			out = append(out, got{at, n})
		}
		if consume { // подставной consume: сразу reset
			out = append(out, got{at, Notice{Kind: "consume"}})
			s.Calling()
			s.Done("reset", r)
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
		f, _, _ := s.Apply(r, c)
		if s.Step(r, f, c) == c.Fast.D() {
			fast++
		}
	}
	if fast > 3 {
		t.Fatalf("быстрый шаг в паузе %d раз", fast)
	}
}

// Быстрый шаг — только при прогнозе ближе Near и когда при 100% что-то будет: письмо (есть кредит или было письмо
// 1) или consume по взводу; без прогноза, после 100%, без кредита, при простое меньше суток — обычный (проверка 02.11).
func TestStep(t *testing.T) {
	c := cfg()
	near := Forecast{At: t0.Add(20 * time.Minute)}
	r := Reading{At: t0, Used: 99, ResetsAt: reset, Available: 1}
	var s State
	if s.Step(r, Forecast{}, c) != c.Interval.D() {
		t.Fatal("99% без прогноза — быстрый шаг")
	}
	if s.Step(r, near, c) != c.Fast.D() || s.Step(r, Forecast{At: t0.Add(time.Hour)}, c) != c.Interval.D() {
		t.Fatal("шаг по прогнозу")
	}
	if s.Step(Reading{At: t0, Used: 99, ResetsAt: reset}, near, c) != c.Interval.D() {
		t.Fatal("без кредита и письма 1 — быстрый шаг")
	}
	if (&State{Warned: true}).Step(Reading{At: t0, Used: 99, ResetsAt: reset}, near, c) != c.Fast.D() {
		t.Fatal("было письмо 1, кредит пропал — письмо «100%» уйдёт, а шаг не быстрый")
	}
	if s.Step(Reading{At: t0, Used: 99, ResetsAt: t0.Add(10 * time.Hour), Available: 1}, near, c) != c.Interval.D() {
		t.Fatal("простой меньше суток — быстрый шаг")
	}
	if (&State{Full: true}).Step(Reading{At: t0, Used: 99, ResetsAt: reset, Available: 1, Armed: "x"}, near, c) != c.Fast.D() {
		t.Fatal("взвод без исхода — не быстрый шаг")
	}
	r.Used = 100
	if s.Step(r, Forecast{}, c) != c.Interval.D() {
		t.Fatal("100% — быстрый шаг")
	}
}

// Проверка 02.11, дефект 1: письмо 1 ушло, работа встала на 12 ч на 99% — быстрый шаг держится, пока наклон за 2 ч
// ещё помнит рост (~40 мин, 20 чтений), а не все 12 ч (было 355).
func TestPauseAt99BackToInterval(t *testing.T) {
	var s State
	c := cfg()
	used := func(at time.Time) float64 { return min(99, 70+4.5*at.Sub(t0).Hours()) }
	feed(&s, c, t0, t0.Add(7*time.Hour), 10*time.Minute, used, nil)
	if !s.Warned {
		t.Fatal("письма 1 нет")
	}
	fast := 0
	for at := t0.Add(7*time.Hour + 2*time.Minute); at.Before(t0.Add(19 * time.Hour)); {
		r := Reading{At: at, Used: 99, ResetsAt: reset, Allowed: true, Available: 1}
		f, _, _ := s.Apply(r, c)
		step := s.Step(r, f, c)
		if step == c.Fast.D() {
			fast++
		}
		at = at.Add(step)
	}
	if fast > 25 {
		t.Fatalf("быстрых шагов за 12 ч паузы: %d", fast)
	}
}

// Проверка 02.11, дефект 2: расход упал при том же resetsAt (сброс без смены планового) — окно новое, флаги заново.
func TestUsageDropIsNewWindow(t *testing.T) {
	s := State{ResetsAt: reset, Warned: true, Full: true, Samples: []Sample{{t0, 100}}}
	s.Apply(Reading{At: t0.Add(10 * time.Minute), Used: 0, ResetsAt: reset, Allowed: true, Available: 1}, cfg())
	if s.Warned || s.Full || len(s.Samples) != 1 {
		t.Fatalf("состояние: %+v", s)
	}
	s.Apply(Reading{At: t0.Add(20 * time.Minute), Used: 0, ResetsAt: reset, Allowed: true, Available: 1}, cfg())
	if len(s.Samples) != 2 {
		t.Fatal("ровный расход завёл окно заново")
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
	s := State{ResetsAt: reset, Warned: true, Samples: []Sample{{t0, 91}},
		Reset:   Reset{Key: "k", Called: true, Outcome: "reset", At: t0, Spent: true},
		Pending: []Delivery{{Message: "m", Note: "n", At: t0, Lead: true}}}
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	back, err := LoadState(path)
	// проверка 3, D2: Reset (и Spent) проходит через диск — демон читает состояние заново каждый проход
	if err != nil || !back.Warned || len(back.Samples) != 1 || !back.ResetsAt.Equal(reset) || back.Reset != s.Reset ||
		len(back.Pending) != 1 || back.Pending[0] != s.Pending[0] {
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

func armed(r *Reading) { r.Armed = "e7192203:278" }

// Взвод: при 100% — consume вместо письма «пора», один раз в окне; письмо 1 говорит, что сброшу сам.
func TestArmedConsumesOnce(t *testing.T) {
	var s State
	g := feed(&s, cfg(), t0, t0.Add(8*time.Hour), 10*time.Minute, linear(70, t0, 4.5), armed)
	if kinds(g) == "" || len(g) != 2 || g[0].n.Kind != KindWarn || g[1].n.Kind != "consume" {
		t.Fatalf("решения: %s", kinds(g))
	}
	if !strings.Contains(g[0].n.Message, "Сброс взведён") || s.Reset.Key == "" || len(s.Pending) != 1 ||
		!strings.Contains(s.Pending[0].Message, "outcome=reset") || !strings.Contains(s.Pending[0].Message, s.Reset.Key) {
		t.Fatalf("состояние: %+v; письмо 1: %s", s, g[0].n.Message)
	}
}

// Без взвода consume нет — только письма.
func TestUnarmedNoConsume(t *testing.T) {
	var s State
	g := feed(&s, cfg(), t0, t0.Add(8*time.Hour), 10*time.Minute, linear(70, t0, 4.5), nil)
	if strings.Contains(kinds(g), "consume") || s.Reset.Key != "" {
		t.Fatalf("решения: %s", kinds(g))
	}
}

// Противник 1: взвод, но до планового сброса меньше суток — consume нет.
func TestArmedShortIdleNoConsume(t *testing.T) {
	var s State
	near := t0.Add(20 * time.Hour)
	g := feed(&s, cfg(), t0, t0.Add(8*time.Hour), 10*time.Minute, linear(70, t0, 4.5), func(r *Reading) {
		armed(r)
		r.ResetsAt = near
	})
	if len(g) != 0 {
		t.Fatalf("решения: %s", kinds(g))
	}
}

// Противник 2: взвод без кредита — письмо «100%, кредита нет», consume нет.
func TestArmedNoCreditLetter(t *testing.T) {
	var s State
	g := feed(&s, cfg(), t0, t0.Add(8*time.Hour), 10*time.Minute, linear(85, t0, 4.5), func(r *Reading) {
		armed(r)
		if r.Used >= 100 {
			r.Available = 0
		}
	})
	last := g[len(g)-1]
	if strings.Contains(kinds(g), "consume") || last.n.Kind != KindFull || !strings.Contains(last.n.Message, "не вызываю") {
		t.Fatalf("решения: %s", kinds(g))
	}
}

// Сбой вызова: повтор тем же ключом; после вызова кредит пропал (ушёл на наш же сброс) — всё равно повтор, ответ
// alreadyRedeemed скажет, что сброс был.
func TestArmedRetrySameKey(t *testing.T) {
	var s State
	c := cfg()
	r := Reading{At: t0, Used: 100, ResetsAt: reset, Allowed: false, Available: 1, Armed: "x"}
	_, _, consume := s.Apply(r, c)
	key := s.Reset.Key
	if !consume || key == "" {
		t.Fatal("нет consume")
	}
	s.Calling() // ответа нет
	r.At, r.Available = t0.Add(2*time.Minute), 0
	_, ns, consume := s.Apply(r, c)
	if !consume || s.Reset.Key != key || len(ns) != 0 {
		t.Fatalf("повтор: consume=%v ключ %q→%q письма %v", consume, key, s.Reset.Key, ns)
	}
	s.Done("alreadyRedeemed", r)
	if _, _, consume = s.Apply(r, c); consume {
		t.Fatal("consume после исхода")
	}
}

// Противник 3: вызов без ответа, затем окно сменилось — исход «видимо, прошёл» к доставке; ключ нового окна — новый.
func TestLostAnswerWindowChanged(t *testing.T) {
	var s State
	c := cfg()
	r := Reading{At: t0, Used: 100, ResetsAt: reset, Available: 1, Armed: "x"}
	s.Apply(r, c)
	s.Calling()
	key := s.Reset.Key
	s.Apply(Reading{At: t0.Add(2 * time.Minute), Used: 0, ResetsAt: t0.Add(7 * 24 * time.Hour), Allowed: true}, c)
	if len(s.Pending) != 1 || !strings.Contains(s.Pending[0].Message, key) || s.Reset.Key != "" {
		t.Fatalf("состояние: %+v", s)
	}
}

// Противник 4: недоставленный исход переживает проходы и смену окна; уходит, когда доставлен и лиду, и в тикет.
func TestPendingKeptUntilDelivered(t *testing.T) {
	s := State{ResetsAt: reset, Pending: []Delivery{{Message: "m", Lead: true}}}
	s.Apply(Reading{At: t0, Used: 0, ResetsAt: t0.Add(7 * 24 * time.Hour), Allowed: true}, cfg())
	s.Delivered()
	if len(s.Pending) != 1 {
		t.Fatal("исход пропал до записи в тикет")
	}
	s.Pending[0].Ticket = true
	s.Delivered()
	if len(s.Pending) != 0 {
		t.Fatal("доставленный исход остался")
	}
}

// Противник 6: секунды resetsAt дрожат в той же минуте — окно и ключ те же.
func TestSameWindowByMinute(t *testing.T) {
	var s State
	c := cfg()
	base := time.Date(2026, 11, 8, 14, 39, 23, 0, time.UTC)
	s.Apply(Reading{At: t0, Used: 100, ResetsAt: base, Available: 1, Armed: "x"}, c)
	key := s.Reset.Key
	s.Apply(Reading{At: t0.Add(2 * time.Minute), Used: 100, ResetsAt: base.Add(24 * time.Second), Available: 1, Armed: "x"}, c)
	if s.Reset.Key != key {
		t.Fatal("ключ сменился при дрожании секунд")
	}
	if !SameWindow(base, time.Date(2026, 11, 8, 15, 39, 0, 0, time.FixedZone("CET", 3600))) {
		t.Fatal("15:39:00 CET и 14:39:23 UTC — одно окно")
	}
}

func TestNewKeyUUID(t *testing.T) {
	k := newKey()
	if len(k) != 36 || k[14] != '4' || k == newKey() {
		t.Fatalf("ключ %q", k)
	}
}

// Проверка взвода 02.11 (проба): после reset расход упал при том же resetsAt — флаги окна заново, а взвод с исходом
// остаётся: при новых 100% второго consume по тому же «да» нет.
func TestNoSecondConsumeSameResetsAt(t *testing.T) {
	var s State
	c := cfg()
	r := Reading{At: t0, Used: 100, ResetsAt: reset, Available: 2, Armed: "x"}
	if _, _, consume := s.Apply(r, c); !consume {
		t.Fatal("нет consume при взводе")
	}
	key := s.Reset.Key
	s.Calling()
	s.Done("reset", r)
	s.Apply(Reading{At: t0.Add(10 * time.Minute), Used: 0, ResetsAt: reset, Allowed: true, Available: 1, Armed: "x"}, c)
	if s.Warned || s.Full || s.Reset.Key != key {
		t.Fatalf("после падения расхода: %+v", s)
	}
	_, ns, consume := s.Apply(Reading{At: t0.Add(time.Hour), Used: 100, ResetsAt: reset, Available: 1, Armed: "x"}, c)
	if consume {
		t.Fatal("второй consume по тому же «да»")
	}
	// повторная проверка: бойцы снова стоят — лиду письмо «100%, взвод израсходован», один раз
	if len(ns) != 1 || ns[0].Kind != KindFull || !strings.Contains(ns[0].Message, "израсходован: outcome=reset") ||
		!strings.Contains(ns[0].Message, "consume — твой") {
		t.Fatalf("письма при новых 100%%: %+v", ns)
	}
	if _, ns, _ := s.Apply(Reading{At: t0.Add(70 * time.Minute), Used: 100, ResetsAt: reset, Available: 1, Armed: "x"}, c); len(ns) != 0 {
		t.Fatalf("повтор письма: %+v", ns)
	}
}

// Сразу после исхода (расход ещё 100%) — письма «израсходован» нет: исход уже ушёл письмом.
func TestNoSpentLetterRightAfterOutcome(t *testing.T) {
	var s State
	c := cfg()
	r := Reading{At: t0, Used: 100, ResetsAt: reset, Available: 1, Armed: "x"}
	s.Apply(r, c)
	s.Calling()
	s.Done("nothingToReset", r)
	if _, ns, consume := s.Apply(Reading{At: t0.Add(10 * time.Minute), Used: 100, ResetsAt: reset, Available: 1, Armed: "x"}, c); consume || len(ns) != 0 {
		t.Fatalf("после исхода: consume %v, письма %+v", consume, ns)
	}
}

// Проверка 3: вызов без ответа, затем «да» снято (или строка не совпала) — consume без «да» не зовётся; лиду письмо
// «100%, consume — твой» (в том же окне прошлый вызов кредит не потратил: кредит уходит только со сбросом).
func TestNoConsumeAfterApprovalLost(t *testing.T) {
	var s State
	c := cfg()
	base := time.Date(2026, 11, 8, 14, 39, 59, 0, time.UTC)
	s.Apply(Reading{At: t0, Used: 100, ResetsAt: base, Available: 1, Armed: "x"}, c)
	s.Calling()
	key := s.Reset.Key
	_, ns, consume := s.Apply(Reading{At: t0.Add(2 * time.Minute), Used: 100, ResetsAt: base.Add(time.Second), Available: 1}, c)
	if consume || s.Reset.Key != key || len(ns) != 1 || !strings.Contains(ns[0].Message, "consume — твой") {
		t.Fatalf("consume %v, ключ %q→%q, письма %+v", consume, key, s.Reset.Key, ns)
	}
}

// Проверка 3, D1: вызов без ответа, расход упал при том же resetsAt, снова 100% — consume нет, лиду письмо «израсходован».
func TestSpentAfterLostAnswer(t *testing.T) {
	var s State
	c := cfg()
	s.Apply(Reading{At: t0, Used: 100, ResetsAt: reset, Available: 1, Armed: "x"}, c)
	s.Calling()
	s.Apply(Reading{At: t0.Add(10 * time.Minute), Used: 1, ResetsAt: reset, Allowed: true, Available: 1, Armed: "x"}, c)
	_, ns, consume := s.Apply(Reading{At: t0.Add(time.Hour), Used: 100, ResetsAt: reset, Available: 1, Armed: "x"}, c)
	if consume || len(ns) != 1 || !strings.Contains(ns[0].Message, "израсходован: outcome=ответа не было") {
		t.Fatalf("consume %v, письма %+v", consume, ns)
	}
}

// Проверка 3, D3: взвод израсходован, кредитов 0 и письма 1 не было — молчание, как без взвода (текст «кредит пропал
// после прошлого письма» был бы ложью).
func TestSpentNoCreditNoWarnSilent(t *testing.T) {
	s := State{ResetsAt: reset, Reset: Reset{Key: "k", Called: true, Outcome: "reset", Spent: true}}
	if _, ns, consume := s.Apply(Reading{At: t0, Used: 100, ResetsAt: reset, Armed: "x"}, cfg()); consume || len(ns) != 0 {
		t.Fatalf("consume %v, письма %+v", consume, ns)
	}
}

// Вызов без ответа, затем расход упал при том же resetsAt — письмо «видимо, прошёл», ключ тот же: повтор даст alreadyRedeemed.
func TestLostAnswerUsageDropped(t *testing.T) {
	var s State
	c := cfg()
	s.Apply(Reading{At: t0, Used: 100, ResetsAt: reset, Available: 1, Armed: "x"}, c)
	s.Calling()
	key := s.Reset.Key
	s.Apply(Reading{At: t0.Add(2 * time.Minute), Used: 1, ResetsAt: reset, Allowed: true, Armed: "x"}, c)
	if len(s.Pending) != 1 || !strings.Contains(s.Pending[0].Message, "расход был 100%, теперь 1%") || s.Reset.Key != key {
		t.Fatalf("состояние: %+v", s)
	}
}

// Риск проверки: дрожание секунд через границу минуты (:59 → :01, журнал 02.11: 18:41:51 и :52) — окно и ключ те же.
func TestSameWindowMinuteBoundary(t *testing.T) {
	var s State
	c := cfg()
	base := time.Date(2026, 11, 8, 14, 39, 59, 0, time.UTC)
	s.Apply(Reading{At: t0, Used: 100, ResetsAt: base, Available: 1, Armed: "x"}, c)
	key := s.Reset.Key
	s.Calling()
	s.Apply(Reading{At: t0.Add(2 * time.Minute), Used: 100, ResetsAt: base.Add(2 * time.Second), Available: 1, Armed: "x"}, c)
	if s.Reset.Key != key || len(s.Pending) != 0 {
		t.Fatalf("граница минуты: %+v", s)
	}
	if SameWindow(base, base.Add(time.Minute)) || SameWindow(base, base.Add(7*24*time.Hour)) {
		t.Fatal("разные окна совпали")
	}
}

// m5: каждый из четырёх исходов — своё пояснение в письме и в строке тикета.
func TestDoneEachOutcome(t *testing.T) {
	want := map[string]string{"reset": "окно сброшено", "nothingToReset": "сбрасывать нечего", "noCredit": "кредитов сброса нет",
		"alreadyRedeemed": "уже сбросил"}
	for outcome, what := range want {
		var s State
		s.Done(outcome, Reading{At: t0, Used: 100, ResetsAt: reset, Armed: "x"})
		if d := s.Pending[0]; !strings.Contains(d.Message, what) || !strings.Contains(d.Note, what) ||
			!strings.Contains(d.Message, "outcome="+outcome) {
			t.Fatalf("%s: %+v", outcome, d)
		}
	}
}

// Проверка 4: окно сбросил не aguard (расход упал при том же resetsAt без нашего вызова) — по тому же «да» consume нет,
// лиду письмо «израсходован… не через aguard»; у близких 100% без кредита — шаг interval (при 100% писать нечего).
func TestSpentByOtherNoConsume(t *testing.T) {
	var s State
	c := cfg()
	s.Apply(Reading{At: t0, Used: 99, ResetsAt: reset, Allowed: true, Available: 1, Armed: "x"}, c)
	s.Apply(Reading{At: t0.Add(10 * time.Minute), Used: 2, ResetsAt: reset, Allowed: true, Available: 1, Armed: "x"}, c)
	near := Reading{At: t0.Add(30 * time.Minute), Used: 99, ResetsAt: reset, Allowed: true, Armed: "x"} // кредитов 0: при 100% писать нечего
	if st := s.Step(near, Forecast{At: near.At.Add(10 * time.Minute)}, c); st != c.Interval.D() {
		t.Fatalf("шаг у близких 100%% после израсходованного взвода: %v", st)
	}
	_, ns, consume := s.Apply(Reading{At: t0.Add(time.Hour), Used: 100, ResetsAt: reset, Available: 1, Armed: "x"}, c)
	if consume || len(ns) != 1 || !strings.Contains(ns[0].Message, "не через aguard") {
		t.Fatalf("consume %v, письма %+v", consume, ns)
	}
}

// Проверка 4: вызов без ответа, «да» не совпало — в письме «consume — твой» ключ прошлого вызова.
func TestFullLetterNamesPendingKey(t *testing.T) {
	var s State
	c := cfg()
	s.Apply(Reading{At: t0, Used: 100, ResetsAt: reset, Available: 1, Armed: "x"}, c)
	s.Calling()
	_, ns, _ := s.Apply(Reading{At: t0.Add(2 * time.Minute), Used: 100, ResetsAt: reset, Available: 1}, c)
	if len(ns) != 1 || !strings.Contains(ns[0].Message, "тем же idempotencyKey "+s.Reset.Key) {
		t.Fatalf("письма %+v", ns)
	}
}
