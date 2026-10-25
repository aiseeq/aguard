package restart

import (
	"slices"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/screen"
)

// compactFacts — сессия, которую пора сжать: 150 тыс. контекста, последний запрос 54 мин назад, всё тихо.
func compactFacts(now time.Time) facts {
	f := idleFacts()
	f.Transcript.Context = contextFacts{Request: now.Add(-54 * time.Minute), Tokens: 150_000, CacheRead: 149_000}
	return f
}

func TestDecideCompact(t *testing.T) {
	now := time.Date(2026, 10, 25, 17, 0, 0, 0, time.UTC)
	c := DefaultCompact()
	if v := decideCompact(compactFacts(now), &c, allowance{}, now); !v.Compact || !v.InWindow || len(v.Against) != 0 {
		t.Fatalf("простаивающую большую беседу сжимать: %+v", v)
	}
	cases := map[string]func(*facts){
		"error":   func(f *facts) { f.Errors = []string{"нет реестра"} },
		"context": func(f *facts) { f.Transcript.Context = contextFacts{} },
		"tokens":  func(f *facts) { f.Transcript.Context.Tokens = 99_000 },
		"early":   func(f *facts) { f.Transcript.Context.Request = now.Add(-52 * time.Minute) },
		"late":    func(f *facts) { f.Transcript.Context.Request = now.Add(-57 * time.Minute) },
		"compacted": func(f *facts) {
			f.Transcript.Context.Compacted, f.Transcript.Context.Trigger = now.Add(-time.Minute), "auto"
		},
		"status": func(f *facts) { f.Status = "busy" },
		"tty":    func(f *facts) { f.TTYQuiet = time.Minute },
		"screen": func(f *facts) { f.Prompt = screen.Draft },
		"tasks":  func(f *facts) { f.Transcript.Tasks = []string{"Agent «ревью» a1"} },
		"crons":  func(f *facts) { f.Transcript.Crons = []cronJob{{ID: "j1", Prompt: "Сон (скилл sleep)"}} },
		"wakeup": func(f *facts) { f.Transcript.Wakeups = []string{"ScheduleWakeup до 19:00"} },
	}
	for code, spoil := range cases {
		f := compactFacts(now)
		spoil(&f)
		if v := decideCompact(f, &c, allowance{}, now); v.Compact || !slices.Contains(codes(Verdict{Against: v.Against}), code) {
			t.Errorf("%s: %+v", code, v)
		}
	}
	// сжатие — через перезапуск: невосстановимые аргументы держат, таймеры роли — нет
	if v := decideCompact(compactFacts(now), &c, allowance{argsProblem: "свой флаг claude --bare"}, now); v.Compact {
		t.Errorf("аргументы пропали бы: %+v", v)
	}
	f := compactFacts(now)
	f.Transcript.Crons = []cronJob{{ID: "j1", Prompt: "Сон (скилл sleep)"}}
	if v := decideCompact(f, &c, allowance{role: leadRole}, now); !v.Compact {
		t.Errorf("таймеры лида восстановимы: %+v", v)
	}
	// версия и порог простоя перезапуска сжатию не мешают
	f = compactFacts(now)
	f.Running, f.Quiet = f.Installed, 3*time.Minute
	f.TTYQuiet, f.TurnQuiet = 55*time.Minute, 54*time.Minute
	if v := decideCompact(f, &c, allowance{}, now); !v.Compact {
		t.Errorf("свежая версия: %+v", v)
	}
	// сжатие до последнего запроса — не повод
	f = compactFacts(now)
	f.Transcript.Context.Compacted = now.Add(-2 * time.Hour)
	if v := decideCompact(f, &c, allowance{}, now); !v.Compact {
		t.Errorf("давнее сжатие: %+v", v)
	}
}

func TestCompactConfigValidate(t *testing.T) {
	ok := DefaultCompact()
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, spoil := range map[string]func(*CompactConfig){
		"act":      func(c *CompactConfig) { c.Mode = "act" }, // act — в cl, не в aguard
		"режим":    func(c *CompactConfig) { c.Mode = "on" },
		"порог":    func(c *CompactConfig) { c.MinTokens = 0 },
		"окно":     func(c *CompactConfig) { c.After = c.Before },
		"за часом": func(c *CompactConfig) { c.Before = 61 * 60 * 1e9 },
		"интервал": func(c *CompactConfig) { c.Interval = 10 * 60 * 1e9 },
		"хранение": func(c *CompactConfig) { c.Keep = 0 },
	} {
		c := DefaultCompact()
		spoil(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: прошло", name)
		}
	}
}
