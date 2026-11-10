package night

import (
	"slices"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/cllog"
	"github.com/aiseeq/aguard/internal/policy"
)

func testConfig() Config {
	c := Default("/home/u")
	c.TrustWait = policy.Duration(5 * time.Minute)
	return c
}

func local(h, m int) time.Time {
	return time.Date(2026, 10, 28, h, m, 0, 0, time.FixedZone("CET", 3600))
}

func TestValidateExpandsDreamPausedHome(t *testing.T) {
	cfg := testConfig()
	cfg.DreamPaused = "~/.claude/improve/state/night-paused"
	if err := cfg.Validate("/home/u"); err != nil {
		t.Fatal(err)
	}
	if cfg.DreamPaused != "/home/u/.claude/improve/state/night-paused" {
		t.Fatalf("путь паузы не раскрыт: %q", cfg.DreamPaused)
	}
	if got := PauseCommand(cfg, local(3, 0)); !slices.Equal(got, []string{"/home/u/.claude/improve/bin/dreamstats", "-improve",
		"/home/u/.claude/improve", "-night", "2026-10-28", "-pause-check"}) {
		t.Fatalf("команда статуса паузы: %q", got)
	}
	for _, bad := range [][]string{nil, {"dreamstats", "-pause-check"}} {
		c := testConfig()
		c.DreamPauseCheck = bad
		if err := c.Validate("/home/u"); err == nil {
			t.Fatalf("dream_pause_check %q принят", bad)
		}
	}
}

func TestCheckMarkers(t *testing.T) {
	cases := []struct {
		name  string
		now   time.Time
		facts Facts
		key   string
	}{
		{"до Сна", local(2, 49), Facts{}, ""},
		{"Сон без маркера", local(2, 51), Facts{}, "dream:2026-10-28"},
		{"Сон с маркером", local(2, 51), Facts{DreamMarker: true}, ""},
		{"Сон снят", local(2, 51), Facts{DreamPaused: true}, ""},
		{"Дайджест с датой вчера", local(7, 1), Facts{DreamMarker: true, DigestText: "2026-10-27"}, "digest:2026-10-28"},
		{"Дайджест с сегодняшней датой", local(7, 1), Facts{DreamMarker: true, DigestText: "done 2026-10-28"}, ""},
		{"Дайджест без файла", local(7, 1), Facts{DreamMarker: true}, "digest:2026-10-28"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Check(tc.now, testConfig(), tc.facts)
			if tc.key == "" && len(got) != 0 {
				t.Fatalf("лишнее: %+v", got)
			}
			if tc.key != "" && (len(got) != 1 || got[0].Key != tc.key) {
				t.Fatalf("получено %+v, ключ %s", got, tc.key)
			}
		})
	}
}

func TestDreamSignalWindowAndFallbackPause(t *testing.T) {
	cfg := testConfig()
	cfg.DreamBy = "02:50"
	cfg.DreamFallbackBy = "03:10"
	if got := Check(local(2, 51), cfg, Facts{}); len(got) != 1 || got[0].Key != "dream:2026-10-28" {
		t.Fatalf("сигнал сразу после dream_by: %+v", got)
	}
	if got := Check(local(6, 11), cfg, Facts{}); len(got) != 0 {
		t.Fatalf("сигнал после окна запасного Сна: %+v", got)
	}
	if got := Check(local(6, 9), cfg, Facts{}); len(got) != 1 {
		t.Fatalf("нет сигнала за минуту до конца окна: %+v", got)
	}
	if got := Check(local(6, 10), cfg, Facts{}); len(got) != 0 {
		t.Fatalf("сигнал ровно на конце окна: %+v", got)
	}
	if !FallbackDue(local(3, 11), cfg, Facts{}) {
		t.Fatal("запасной Сон не наступил без паузы")
	}
	if !FallbackDue(local(6, 9), cfg, Facts{}) || FallbackDue(local(6, 10), cfg, Facts{}) {
		t.Fatal("конец окна запасного Сна: 06:09 — да, 06:10 — нет")
	}
	facts := Facts{DreamPaused: true}
	if FallbackDue(local(3, 11), cfg, facts) {
		t.Fatal("запасной Сон наступил при паузе")
	}
	if got := Check(local(3, 11), cfg, facts); len(got) != 0 {
		t.Fatalf("сигнал при паузе: %+v", got)
	}
}

func TestCheckTrust(t *testing.T) {
	e := cllog.TrustEvent{At: local(0, 38), Name: "lead", Dir: "/home/u"}
	cases := []struct {
		name  string
		now   time.Time
		event cllog.TrustEvent
		want  bool
	}{
		{"рано", local(0, 42), e, false}, {"пора", local(0, 44), e, true},
		{"поднялся", local(0, 44), cllog.TrustEvent{At: e.At, Name: e.Name, Dir: e.Dir, Recovered: local(0, 39)}, false},
		{"старше суток", local(6, 38), cllog.TrustEvent{At: e.At.Add(-30 * time.Hour), Name: e.Name, Dir: e.Dir}, false},
		{"подтверждено, не поднялся", local(0, 44), cllog.TrustEvent{At: e.At, Name: e.Name, Dir: e.Dir, Confirmed: true}, true},
		{"подтверждено и поднялся", local(0, 44), cllog.TrustEvent{At: e.At, Name: e.Name, Dir: e.Dir, Confirmed: true, Recovered: local(0, 39)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Check(tc.now, testConfig(), Facts{DreamMarker: true, DigestText: "2026-10-28", TrustEvents: []cllog.TrustEvent{tc.event}})
			if (len(got) == 1) != tc.want {
				t.Fatalf("сигналы %+v, ждали %v", got, tc.want)
			}
		})
	}
}
