package night

import (
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

func TestCheckMarkers(t *testing.T) {
	cases := []struct {
		name  string
		now   time.Time
		facts Facts
		key   string
	}{
		{"до Сна", local(2, 29), Facts{}, ""},
		{"Сон без маркера", local(2, 31), Facts{}, "dream:2026-10-28"},
		{"Сон с маркером", local(2, 31), Facts{DreamMarker: true}, ""},
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
