package report

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
	"github.com/aiseeq/aguard/internal/record"
)

func TestCollectAndPrint(t *testing.T) {
	cfg := policy.Default("/home/u")
	now := time.Date(2026, 10, 19, 23, 0, 0, 0, time.UTC)
	lead := &record.Session{Pid: 10, Name: "lead", ID: "l"}
	owner := &record.Owner{Proc: record.Proc{Pid: 5, Comm: "go", UID: 1000}, Session: lead}
	ev := []record.Event{
		{T: now.Add(-2 * time.Hour), Ev: record.EvCreate, Path: "/tmp/a", Owner: owner},
		{T: now.Add(-time.Hour), Ev: record.EvDelete, Path: "/tmp/a", Owner: owner, LifeS: 3600, MaxBytes: 2 << 30, Measured: true, ByteSec: 2 * policy.GiBMin * 60},
		{T: now.Add(-time.Hour), Ev: record.EvDelete, Path: "/tmp/b", Owner: owner, LifeS: 0.5},
		{T: now.Add(-time.Hour), Ev: record.EvGone, Path: "/tmp/c", LifeS: 5 * 86400, MaxBytes: 5 << 20, Measured: true},
		{T: now.Add(-48 * time.Hour), Ev: record.EvDelete, Path: "/tmp/old", Owner: owner, LifeS: 1},
		{T: now.Add(-time.Hour), Ev: record.EvDelete, Path: "/tmp/.X11-unix", LifeS: 1},
	}
	st := &record.State{Updated: now, Entries: []record.Entry{
		{Path: "/tmp/live", Born: now.Add(-30 * time.Minute), Owner: &record.Owner{Proc: record.Proc{Comm: "chrome", UID: 1000}},
			UID: 1000, Bytes: 50 << 20, MaxBytes: 60 << 20, MeasuredAt: now, ByteSec: 1},
	}}
	d := Collect(&cfg, ev, st, now.Add(-24*time.Hour), 1000, now)
	if len(d.Closed) != 3 || len(d.Live) != 1 {
		t.Fatalf("закрытых %d, живых %d", len(d.Closed), len(d.Live))
	}
	if d.Closed[0].Class != policy.ClassSession || d.Closed[2].Class != policy.ClassUnknown || d.Live[0].Class != policy.ClassNonAgent {
		t.Errorf("классы: %+v / %+v", d.Closed, d.Live)
	}
	var sb strings.Builder
	Print(&sb, d)
	out := sb.String()
	for _, want := range []string{"закрытых записей 3", "≥1 ГБ", "не замерено", "сессия lead", "чужая учётка", "без создателя"} {
		if want == "чужая учётка" {
			if strings.Contains(out, want) {
				t.Errorf("пустой класс напечатан: %s", want)
			}
			continue
		}
		if !strings.Contains(out, want) {
			t.Errorf("нет %q в отчёте:\n%s", want, out)
		}
	}
	if lifeText(180*time.Millisecond) != "0.2 с" || lifeText(90*time.Second) != "1 мин" {
		t.Errorf("lifeText: %q %q", lifeText(180*time.Millisecond), lifeText(90*time.Second))
	}
	if sizeBucket(2<<30) != 4 || sizeBucket(0) != 0 || lifeBucket(5*86400*time.Second) != 6 || lifeBucket(time.Hour) != 4 {
		t.Error("корзины")
	}
}

func TestNameMask(t *testing.T) {
	tests := map[string]string{
		"com.google.Chrome.IeF6Cl":                            "com.google.Chrome.XXXXXX",
		"com.google.Chrome.chrome_chrome_url_fetcher_.EWtYC1": "com.google.Chrome.chrome_chrome_url_fetcher_.XXXXXX",
		"glint-cmd-test3493130021":                            "glint-cmd-testN",
		"glint-cmd-test10":                                    "glint-cmd-testN",
		"worker-tests-123":                                    "worker-tests-N",
		"worker-tests-123456":                                 "worker-tests-N",
		"TestBriefCheckRealSignalShapeExitsWithSignalCode123": "TestBriefCheckRealSignalShapeExitsWithSignalCodeN",
		"playwright_chromiumdev_profile-px227k":               "playwright_chromiumdev_profile-XXXXXX",
		"playwright-artifacts-bbxAYL":                         "playwright-artifacts-XXXXXX",
		"tmp.16dwd8ZG6q":                                      "tmp.XXXXXX",
		"scoped_dirIfVyVd":                                    "scoped_dirXXXXXX",
		"name.123456":                                         "name.N",
		".config":                                             ".config",
		"svc.socket":                                          "svc.socket",
		"prefixABcDEF":                                        "prefixABcDEF",
	}
	for name, want := range tests {
		if got := NameMask(name); got != want {
			t.Errorf("NameMask(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestPrintLitterSummary(t *testing.T) {
	recs := []Rec{
		{Path: "/tmp/claude-tmp/com.google.Chrome.IeF6Cl", Class: policy.ClassSession, Life: 48 * time.Hour, Bytes: 4},
		{Path: "/tmp/claude-tmp/com.google.Chrome.EWtYC1", Class: policy.ClassSession, Life: 72 * time.Hour, Bytes: 8},
		{Path: "/tmp/claude-tmp/com.google.Chrome.QwErT9", Class: policy.ClassNonAgent, Life: 12 * time.Hour, Bytes: 16},
		{Path: "/tmp/claude-tmp/com.google.Chrome.XyZ123", Class: policy.ClassSession, Life: 24 * time.Hour, Bytes: 1},
		{Path: "/tmp/prefix-Abc123", Class: policy.ClassSession, Life: time.Hour, Bytes: 1},
		{Path: "/tmp/claude-tmp/prefix-Abc123", Class: policy.ClassSession, Life: time.Hour, Bytes: 1},
		{Path: "/tmp/claude-tmp/large-file", Class: policy.ClassSession, Life: 10 * 24 * time.Hour, Bytes: 1 << 20},
	}
	groups := LitterSummary(recs)
	if len(groups) != 3 || groups[0].Count != 4 || groups[0].Bytes != 29 || groups[0].P50 != 24*time.Hour ||
		groups[0].OlderThanDay != 2 || groups[0].Classes[policy.ClassSession] != 3 || groups[0].Classes[policy.ClassNonAgent] != 1 {
		t.Fatalf("litter summary: %+v", groups)
	}
	if groups[1].Count != 1 || groups[1].Name != "/tmp/claude-tmp/prefix-XXXXXX" ||
		groups[2].Count != 1 || groups[2].Name != "/tmp/prefix-XXXXXX" {
		t.Fatalf("equal groups must sort by full name and keep roots distinct: %+v", groups)
	}
	var out strings.Builder
	if err := Print(&out, Data{Live: recs}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"3. Мелочь числом", "com.google.Chrome.XXXXXX", "возраст p50", "старше 1 сут", "сессия 3, не агент 1"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}
}

func TestLitterTopTenAndNameTieOrder(t *testing.T) {
	recs := make([]Rec, 11)
	for i, tail := range []string{"aa", "ab", "ac", "ad", "ae", "af", "ag", "ah", "ai", "aj", "ak"} {
		recs[i] = Rec{Path: "/tmp/group-" + tail}
	}
	var out strings.Builder
	if err := printLitter(&out, recs); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	rows := 0
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "/tmp/group-") {
			rows++
		}
	}
	if rows != 10 || !strings.Contains(lines[1], "/tmp/group-aa") || !strings.Contains(lines[10], "/tmp/group-aj") ||
		!strings.Contains(out.String(), "ещё на границе топ-10") || !strings.Contains(out.String(), "/tmp/group-ak") {
		t.Fatalf("top ten/tie order wrong (%d rows):\n%s", rows, out.String())
	}
}

func TestNoticeableLitterThreshold(t *testing.T) {
	recs := make([]Rec, NoticeablePatternCount)
	for i := range recs {
		recs[i] = Rec{Path: "/tmp/claude-tmp/glint-cmd-test" + strconv.Itoa(i), Bytes: 0}
	}
	var out strings.Builder
	if err := Print(&out, Data{Live: recs}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "glint-cmd-testN (заметно)") {
		t.Fatalf("порог %d не отмечен в отчёте", NoticeablePatternCount)
	}
}
