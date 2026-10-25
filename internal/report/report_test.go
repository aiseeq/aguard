package report

import (
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
