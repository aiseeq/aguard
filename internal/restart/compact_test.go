package restart

import (
	"slices"
	"strings"
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
		"wakeup": func(f *facts) { f.Transcript.Wakeups = []string{"ScheduleWakeup до 19:00"} },
	}
	for code, spoil := range cases {
		f := compactFacts(now)
		spoil(&f)
		if v := decideCompact(f, &c, allowance{}, now); v.Compact || !slices.Contains(codes(Verdict{Against: v.Against}), code) {
			t.Errorf("%s: %+v", code, v)
		}
	}
	// сжатие — через перезапуск: невосстановимые аргументы держат, таймеры — нет (после -p --resume /compact и
	// --resume они на месте, замер 27.10 10:07 CET)
	if v := decideCompact(compactFacts(now), &c, allowance{argsProblem: "свой флаг claude --bare"}, now); v.Compact {
		t.Errorf("аргументы пропали бы: %+v", v)
	}
	f := compactFacts(now)
	f.Transcript.Crons = []cronJob{{ID: "j1", Prompt: "замер"}}
	if v := decideCompact(f, &c, allowance{}, now); !v.Compact {
		t.Errorf("таймеры сжатие не держат: %+v", v)
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

// Сброс: с порога и простоя сжатия, без верхней границы, после сжатия тоже, при чистом дереве; любой довод держит.
func TestDecideReset(t *testing.T) {
	now := time.Date(2026, 10, 25, 19, 0, 0, 0, time.UTC)
	c := DefaultCompact()
	resetFacts := func() facts {
		f := idleFacts()
		f.Transcript.Context = contextFacts{Request: now.Add(-3 * time.Hour), Tokens: 235_000}
		return f
	}
	if v := decideReset(resetFacts(), &c, allowance{}, now); !v.Reset || !v.Candidate {
		t.Fatalf("закрытую большую беседу сбросить: %+v", v)
	}
	f := compactFacts(now) // окно сжатия: 54 мин, 150 тыс.
	if v := decideReset(f, &c, allowance{}, now); !v.Reset {
		t.Errorf("сброс с того же порога и простоя, что сжатие: %+v", v)
	}
	f = resetFacts()
	f.Transcript.Context.Compacted, f.Transcript.Context.Trigger = now.Add(-time.Hour), "manual"
	if v := decideReset(f, &c, allowance{}, now); !v.Reset {
		t.Errorf("сжатую беседу тоже сбросить: %+v", v)
	}
	cases := map[string]func(*facts){
		"tokens": func(f *facts) { f.Transcript.Context.Tokens = 99_000 },
		"early":  func(f *facts) { f.Transcript.Context.Request = now.Add(-52 * time.Minute) },
		"screen": func(f *facts) { f.Prompt = screen.Draft },
		"crons":  func(f *facts) { f.Transcript.Crons = []cronJob{{ID: "j1", Prompt: "Сон (скилл sleep)"}} },
		"tasks":  func(f *facts) { f.Transcript.Tasks = []string{"Agent «ревью» a1"} },
	}
	for code, spoil := range cases {
		f := resetFacts()
		spoil(&f)
		if v := decideReset(f, &c, allowance{}, now); v.Reset || !slices.Contains(codes(Verdict{Against: v.Against}), code) {
			t.Errorf("%s: %+v", code, v)
		}
	}
	f = resetFacts()
	f.Transcript.Crons = []cronJob{{ID: "j1", Prompt: "Сон (скилл sleep)"}}
	if v := decideReset(f, &c, allowance{typed: true}, now); !v.Reset {
		t.Errorf("crons под pty: /clear набором таймеры не теряет: %+v", v)
	}
	// Под pty процесс не перезапускают: фоновая команда и дети живут и после /clear, суб-агенты держат только сброс.
	busy := func(agent bool) facts {
		f := resetFacts()
		f.Children = []string{"bash 42"}
		f.Transcript.Tasks = []string{"Bash «замер» b1"}
		f.HookState, f.HookTasks, f.HookAgents, f.HookWaits = "paused", true, agent, false
		if agent {
			f.Transcript.Tasks = append(f.Transcript.Tasks, "Agent «ревью» a1")
			f.Transcript.Agents = []string{"Agent «ревью» a1"}
		}
		return f
	}
	if v := decideReset(busy(false), &c, allowance{typed: true}, now); !v.Reset {
		t.Errorf("фоновая команда под pty: /clear её не убивает: %+v", v)
	}
	if v := decideReset(busy(false), &c, allowance{}, now); v.Reset || !slices.Contains(codes(Verdict{Against: v.Against}), "children") ||
		!slices.Contains(codes(Verdict{Against: v.Against}), "tasks") || !slices.Contains(codes(Verdict{Against: v.Against}), "hook-tasks") {
		t.Errorf("без pty сброс — перезапуск, задачи умерли бы: %+v", v)
	}
	agents := decideReset(busy(true), &c, allowance{typed: true}, now)
	if got := codes(Verdict{Against: agents.Against}); agents.Reset || !slices.Equal(got, []string{"hook-agents", "agents"}) {
		t.Errorf("суб-агенты под pty держат сброс: %v", got)
	}
	if against, in := idleChecks(busy(true), &c, allowance{typed: true}, idleWindow{c.MinTokens, c.After.D(), 0, "окно", false}, now); len(against) != 0 || !in {
		t.Errorf("/compact набором под pty суб-агенты не держат: %+v", against)
	}
	if v := decideReset(resetFacts(), &c, allowance{argsProblem: "свой флаг claude --foo"}, now); v.Reset || !slices.Contains(codes(Verdict{Against: v.Against}), "args") {
		t.Errorf("args: новая беседа потеряла бы аргументы запуска: %+v", v)
	}
	v := decideReset(resetFacts(), &c, allowance{}, now)
	treeCheck(&v, func(string) ([]string, error) { return []string{"M docs/design.md", "?? new.go"}, nil }, "/w")
	if v.Reset || len(v.Against) != 1 || v.Against[0].Code != "tree" || !strings.Contains(v.Against[0].Text, "незакоммиченное (2)") {
		t.Errorf("грязное дерево: %+v", v)
	}
	v = decideReset(resetFacts(), &c, allowance{}, now)
	if treeCheck(&v, func(string) ([]string, error) { return nil, nil }, "/w"); !v.Reset {
		t.Errorf("чистое дерево: %+v", v)
	}
}

// Можно сбросить — не сжимать; нельзя — сжатие как было.
func TestPreferReset(t *testing.T) {
	v := CompactVerdict{Compact: true, InWindow: true}
	if preferReset(&v, ResetVerdict{Reset: true}); v.Compact || len(v.Against) != 1 || v.Against[0].Code != "reset" {
		t.Errorf("сброс вместо сжатия: %+v", v)
	}
	v = CompactVerdict{Compact: true}
	if preferReset(&v, ResetVerdict{Against: []Reason{{Code: "crons"}}}); !v.Compact {
		t.Errorf("сброс нельзя — сжатие: %+v", v)
	}
}
