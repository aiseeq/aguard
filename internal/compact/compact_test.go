package compact

import (
	"bytes"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/restart"
)

var t0 = time.Date(2026, 10, 25, 16, 0, 0, 0, time.UTC)

// check — проверка сессии s1: последний запрос в req, контекст 150 тыс.
func check(req time.Time, inWindow, compact bool, against ...string) restart.CompactCheck {
	c := restart.CompactCheck{SessionID: "s1", Name: "x-master", SessionKind: "interactive", Request: req, Tokens: 150_000}
	c.InWindow, c.Compact = inWindow, compact
	for _, a := range against {
		c.Against = append(c.Against, restart.Reason{Code: a})
	}
	return c
}

func kinds(recs []Record) []string {
	var out []string
	for _, r := range recs {
		out = append(out, r.Kind+"/"+r.Outcome)
	}
	return out
}

func TestObserve(t *testing.T) {
	tr, err := Load(filepath.Join(t.TempDir(), "нет.json"))
	if err != nil {
		t.Fatal(err)
	}
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	if recs := tr.Observe(check(t0, false, false, "early"), at(30)); len(recs) != 0 {
		t.Errorf("вне окна записей нет: %+v", recs)
	}
	recs := tr.Observe(check(t0, true, true), at(53))
	if len(recs) != 1 || recs[0].Kind != KindWould || recs[0].Tokens != 150_000 {
		t.Fatalf("сжал бы: %+v", recs)
	}
	if recs := tr.Observe(check(t0, true, true), at(54)); len(recs) != 0 {
		t.Errorf("по тому же запросу — одно решение: %+v", recs)
	}
	// человек вернулся через полтора часа, кэш умер: прочитано только общее
	back := check(at(90), false, false, "early")
	back.CacheRead = 18_000
	recs = tr.Observe(back, at(91))
	if !slices.Equal(kinds(recs), []string{"outcome/" + OutcomeCacheExpired}) || recs[0].Decision != KindWould ||
		recs[0].AfterS != 90*60 || recs[0].CacheRead != 18_000 {
		t.Fatalf("исход — кэш умер: %+v", recs)
	}

	// не стал бы из-за черновика, а потом беседу сжали
	recs = tr.Observe(check(at(90), true, false, "screen", "draft"), at(143))
	if len(recs) != 1 || recs[0].Kind != KindWouldNot || !slices.Equal(recs[0].Against, []string{"screen", "draft"}) {
		t.Fatalf("не стал бы: %+v", recs)
	}
	sq := check(at(90), false, false, "compacted")
	sq.Compacted, sq.Trigger = at(144), "auto"
	recs = tr.Observe(sq, at(145))
	if !slices.Equal(kinds(recs), []string{"outcome/" + OutcomeCompacted}) || recs[0].Trigger != "auto" || recs[0].Decision != KindWouldNot {
		t.Fatalf("исход — сжали: %+v", recs)
	}

	// вернулся при живом кэше; состояние переживает перезапуск демона
	tr.Observe(check(at(200), true, true), at(253))
	file := filepath.Join(t.TempDir(), "state.json")
	if err := tr.Save(file); err != nil {
		t.Fatal(err)
	}
	if tr, err = Load(file); err != nil {
		t.Fatal(err)
	}
	if recs := tr.Observe(check(at(200), true, true), at(254)); len(recs) != 0 {
		t.Errorf("после перезапуска демона — то же решение снова: %+v", recs)
	}
	alive := check(at(255), false, false, "early")
	alive.CacheRead = 150_000
	if recs := tr.Observe(alive, at(256)); !slices.Equal(kinds(recs), []string{"outcome/" + OutcomeCacheAlive}) {
		t.Errorf("исход — кэш жив: %+v", recs)
	}

	// сессия пропала
	tr.Observe(check(at(400), true, true), at(453))
	if recs := tr.Expire(at(453+24*60), 7*24*time.Hour); len(recs) != 0 {
		t.Errorf("рано терять: %+v", recs)
	}
	if recs := tr.Expire(at(453+8*24*60), 7*24*time.Hour); !slices.Equal(kinds(recs), []string{"outcome/" + OutcomeLost}) {
		t.Errorf("пропавшая сессия: %+v", recs)
	}
	if len(tr.Decided) != 0 || len(tr.Pending) != 0 {
		t.Errorf("после Expire осталось: %+v", tr)
	}
	if recs := tr.Observe(restart.CompactCheck{CompactVerdict: restart.CompactVerdict{InWindow: true}}, at(500)); len(recs) != 0 {
		t.Errorf("без сессии записей нет: %+v", recs)
	}
}

func TestReport(t *testing.T) {
	recs := []Record{
		{Kind: KindWould}, {Kind: KindWouldNot, Against: []string{"screen", "draft"}}, {Kind: KindWouldNot, Against: []string{"draft"}},
		{Kind: KindOutcome, Decision: KindWould, Outcome: OutcomeCacheExpired, Tokens: 150_000},
		{Kind: KindOutcome, Decision: KindWould, Outcome: OutcomeCacheExpired, Tokens: 250_000},
	}
	r, err := Collect(func(fn func(*Record)) (int, error) {
		for i := range recs {
			fn(&recs[i])
		}
		return 1, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Decisions[KindWouldNot] != 2 || r.Against["draft"] != 2 || r.Outcomes["would/cache_expired"] != (Tally{2, 400_000}) || r.Bad != 1 {
		t.Errorf("сводка: %+v", r)
	}
	var b bytes.Buffer
	if err := r.Print(&b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "исход would/cache_expired: 2, контекст 400 тыс. токенов") {
		t.Errorf("печать:\n%s", b.String())
	}
}
