// Package codexwatch — сторож недельного лимита Codex: письмо лиду за ~2 ч до 100%, когда 100% наступит раньше
// планового сброса с простоем больше суток и есть досрочный сброс, и второе — при 100%. Сам сброс не делает:
// исполнитель consume — лид (решение пользователя codex-reset-on-approval в ~/.claude/decisions.md, 02.11,
// `tl e7192203:147`, `:264`).
//
// Замер — чтение account/rateLimits/read у codex app-server раз в шаг; темп — больший из наклонов за 30 мин и
// за 2 ч: раннее письмо дешевле позднего (противник замысла 02.11: разгон с 2 до 10 %/ч и разгон сразу после
// сброса окна по одному двухчасовому наклону видны поздно).
package codexwatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/aiseeq/aguard/internal/bgtask"
	"github.com/aiseeq/aguard/internal/policy"
	"github.com/aiseeq/aguard/internal/record"
)

const (
	ModeOff   = bgtask.ModeOff
	ModeLog   = bgtask.ModeLog
	ModeAlert = bgtask.ModeAlert
)

// Окна темпа: наклон берётся по самому старому замеру окна, если он старше половины окна. usedPercent — целое,
// разница двух замеров врёт до 1 п.п.; на коротком окне это ±30 % темпа (4,5 %/ч за 30 мин — 2 или 3 п.п.), поэтому
// в нём берётся только несомненный рост: разница минус 1 п.п.
var rateWindows = []struct {
	span  time.Duration
	slack float64
}{{30 * time.Minute, 1}, {2 * time.Hour, 0}}

// keep — сколько замеров окна хранить: самое длинное окно темпа.
const keep = 2 * time.Hour

// Config — раздел [codex] конфига aguard.
type Config struct {
	Mode     string          `toml:"mode"`
	Interval policy.Duration `toml:"interval"` // шаг замера
	Fast     policy.Duration `toml:"fast"`     // шаг у самых 100%: прогноз ближе Near или used ≥ 99
	Near     policy.Duration `toml:"near"`     // прогноз 100% ближе этого — шаг Fast
	Warn     policy.Duration `toml:"warn"`     // письмо 1: прогноз 100% ближе этого
	MinIdle  policy.Duration `toml:"min_idle"` // простой до планового сброса, с которого сброс стоит предлагать
	Codex    string          `toml:"codex"`    // путь к codex (app-server)
	Lead     string          `toml:"-"`
	SendAuth bool            `toml:"-"`
}

// Default — шаг 10 мин: одно чтение ~1,5 с CPU и 230 МБ RSS на 8 с (замер 02.11), 144 в сутки; при темпе 02.11
// (~4,5 %/ч) за шаг уходит меньше 1 %, письмо за 2 ч приходит с точностью ±10 мин. У самых 100% — шаг 2 мин:
// письмо «100%» не позже 2 мин после него.
func Default(home string) Config {
	return Config{Mode: ModeAlert, Interval: policy.Duration(10 * time.Minute), Fast: policy.Duration(2 * time.Minute),
		Near: policy.Duration(30 * time.Minute), Warn: policy.Duration(2 * time.Hour), MinIdle: policy.Duration(24 * time.Hour),
		Codex: home + "/.npm/bin/codex"}
}

// Validate проверяет режим и шаги и раскрывает путь codex.
func (c *Config) Validate(home string) error {
	if c.Mode != ModeOff && c.Mode != ModeLog && c.Mode != ModeAlert {
		return fmt.Errorf("codex.mode = %q: off, log или alert", c.Mode)
	}
	if c.Fast.D() < 30*time.Second || c.Interval.D() < c.Fast.D() {
		return errors.New("codex.fast >= 30s, codex.interval >= codex.fast")
	}
	if c.Near.D() <= 0 || c.Warn.D() < c.Near.D() || c.MinIdle.D() <= 0 {
		return errors.New("codex.near > 0, codex.warn >= codex.near, codex.min_idle > 0")
	}
	c.Codex = policy.ExpandHome(c.Codex, home)
	if c.Codex == "" {
		return errors.New("codex.codex пуст")
	}
	return nil
}

// Reading — один замер лимита.
type Reading struct {
	At        time.Time
	Used      float64   // usedPercent недельного окна
	ResetsAt  time.Time // плановый сброс окна
	Allowed   bool      // ordinaryUsageAllowed
	Available int       // доступных досрочных сбросов
}

// Sample — замер в истории окна.
type Sample struct {
	At   time.Time `json:"at"`
	Used float64   `json:"used"`
}

// State — окно лимита и отправленное в нём; на диске между проходами.
type State struct {
	ResetsAt time.Time `json:"resets_at"`
	Samples  []Sample  `json:"samples"`
	Warned   bool      `json:"warned"`    // письмо 1 ушло
	Full     bool      `json:"full"`      // письмо «100%» ушло
	Blocked  bool      `json:"blocked"`   // письмо «работа запрещена до 100%» ушло
	NoCredit bool      `json:"no_credit"` // строка «сброса нет» записана
}

func LoadState(path string) (State, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("состояние codex: %w", err)
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("состояние codex %s: %w", path, err)
	}
	return s, nil
}

func (s *State) Save(path string) error {
	b, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("сериализация состояния codex: %w", err)
	}
	if err := record.WriteFileAtomic(path, b, 0o600); err != nil {
		return fmt.Errorf("состояние codex: %w", err)
	}
	return nil
}

// Kind — вид решения прохода.
type Kind string

const (
	KindWarn     Kind = "warn"      // письмо 1: прогноз 100% близко
	KindFull     Kind = "full"      // письмо: 100%
	KindBlocked  Kind = "blocked"   // письмо: обычная работа запрещена, хотя не 100%
	KindNoCredit Kind = "no_credit" // строка в журнал: досрочно кончится, но сброса нет
)

// Notice — решение прохода: письмо лиду или строка журнала (KindNoCredit).
type Notice struct {
	Kind    Kind
	Message string
}

// Unsent снимает флаг письма, которое не ушло: следующий проход отправит его снова.
func (s *State) Unsent(k Kind) {
	switch k {
	case KindWarn:
		s.Warned = false
	case KindFull:
		s.Full = false
	case KindBlocked:
		s.Blocked = false
	case KindNoCredit:
		s.NoCredit = false
	}
}

// Forecast — темп и прогноз 100% по истории окна.
type Forecast struct {
	Rate float64   // %/ч; 0 — темпа нет (мало истории или расхода)
	At   time.Time // когда будет 100%; нулевое — прогноза нет
}

// Apply добавляет замер в историю и решает, что написать. Смена resetsAt (сброс окна по плану или consume) —
// история и флаги окна заново.
func (s *State) Apply(r Reading, c *Config) (Forecast, []Notice) {
	if !r.ResetsAt.Equal(s.ResetsAt) {
		*s = State{ResetsAt: r.ResetsAt}
	}
	s.Samples = append(s.Samples, Sample{At: r.At, Used: r.Used})
	cut := 0
	for cut < len(s.Samples) && r.At.Sub(s.Samples[cut].At) > keep {
		cut++
	}
	s.Samples = s.Samples[cut:]
	f := s.forecast(r)
	idle := r.ResetsAt.Sub(r.At)
	var out []Notice
	switch {
	case r.Used >= 100:
		if !s.Full && idle > c.MinIdle.D() && (r.Available > 0 || s.Warned) {
			s.Full = true
			out = append(out, Notice{KindFull, fullText(r, idle)})
		}
	case !r.Allowed:
		if !s.Blocked {
			s.Blocked = true
			out = append(out, Notice{KindBlocked, blockedText(r)})
		}
	case !f.At.IsZero() && f.At.Sub(r.At) <= c.Warn.D() && r.ResetsAt.Sub(f.At) > c.MinIdle.D():
		if r.Available == 0 {
			if !s.NoCredit && !s.Warned { // после письма 1 о пропаже кредита скажет письмо «100%»
				s.NoCredit = true
				out = append(out, Notice{KindNoCredit, noCreditText(r, f)})
			}
		} else if !s.Warned {
			s.Warned = true
			out = append(out, Notice{KindWarn, warnText(r, f)})
		}
	}
	return f, out
}

// forecast — больший из наклонов окон rateWindows; окно без замера старше его половины не считается.
func (s *State) forecast(r Reading) Forecast {
	var rate float64
	for _, w := range rateWindows {
		for _, p := range s.Samples {
			age := r.At.Sub(p.At)
			if age > w.span {
				continue
			}
			if age >= w.span/2 {
				rate = max(rate, (r.Used-p.Used-w.slack)/age.Hours())
			}
			break
		}
	}
	if rate <= 0 || r.Used >= 100 {
		return Forecast{Rate: rate}
	}
	return Forecast{Rate: rate, At: r.At.Add(time.Duration((100 - r.Used) / rate * float64(time.Hour)))}
}

// Step — шаг до следующего замера: у самых 100% — Fast, иначе Interval. После 100% быстрый шаг не нужен: письмо
// ушло, дальше ждём сброса.
func Step(r Reading, f Forecast, c *Config) time.Duration {
	if r.Used >= 100 {
		return c.Interval.D()
	}
	if r.Used >= 99 || (!f.At.IsZero() && f.At.Sub(r.At) <= c.Near.D()) {
		return c.Fast.D()
	}
	return c.Interval.D()
}

const ref = "решение пользователя — ~/.claude/decisions.md codex-reset-on-approval (`tl e7192203:147`, `:264`)"

func local(t time.Time) string { return t.Local().Format("02.01 15:04 MST") }

func days(d time.Duration) string { return fmt.Sprintf("%.1f сут", d.Hours()/24) }

func warnText(r Reading, f Forecast) string {
	return fmt.Sprintf("aguard codex: недельный лимит Codex — %.0f%%, при темпе %.1f %%/ч 100%% около %s; плановый сброс %s, "+
		"простой %s; досрочных сбросов доступно: %d. Вопрос пользователю и consume при 100%% — твои; %s. "+
		"Письмо «100%%» пришлю, когда дойдёт.", r.Used, f.Rate, local(f.At), local(r.ResetsAt), days(r.ResetsAt.Sub(f.At)),
		r.Available, ref)
}

func fullText(r Reading, idle time.Duration) string {
	if r.Available == 0 {
		return fmt.Sprintf("aguard codex: недельный лимит Codex — %.0f%%, но досрочных сбросов больше нет (после прошлого "+
			"письма кредит пропал); плановый сброс %s, простой %s. consume не поможет.", r.Used, local(r.ResetsAt), days(idle))
	}
	return fmt.Sprintf("aguard codex: недельный лимит Codex — %.0f%%, пора: плановый сброс %s, простой %s; досрочных "+
		"сбросов доступно: %d. consume — твой, после «да» пользователя, один раз; %s.", r.Used, local(r.ResetsAt), days(idle),
		r.Available, ref)
}

func blockedText(r Reading) string {
	return fmt.Sprintf("aguard codex: обычная работа Codex запрещена (ordinaryUsageAllowed=false), хотя недельный лимит — "+
		"%.0f%%, не 100%%; плановый сброс %s. Это не случай досрочного сброса: consume по решению — только при 100%%.",
		r.Used, local(r.ResetsAt))
}

func noCreditText(r Reading, f Forecast) string {
	return fmt.Sprintf("недельный лимит Codex кончится около %s (%.0f%%, %.1f %%/ч), раньше планового сброса %s, но досрочных "+
		"сбросов нет", local(f.At), r.Used, f.Rate, local(r.ResetsAt))
}
