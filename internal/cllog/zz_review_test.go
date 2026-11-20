package cllog

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

var reviewVocab = []string{
	"lead: окно доверия к /a: человек у терминала",
	"lead pid=7: окно доверия к /b: x",
	"lead: подтвердил доверие к /a: ok",
	"lead pid=1 2.1→2.2 act: версия свежая",
	"lead pid=1: SIGTERM, поднимаю новую беседу",
	"other: окно доверия к /a: x",
	"other pid=2 2.1→2.2 act: y",
	"other pid=3: SIGTERM, поднимаю x",
	"lead pid=q: окно доверия к /a: b",
	"lead pid=1 2.1→2.2 x pid=z: окно доверия к /a: b",
	"lead: окно доверия к /a: повтор Down 1/3 по подсказке",
	"мусор",
	"",
}

func TestReviewTrustStateDifferential(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	diffs := 0
	for range 20000 {
		var b strings.Builder
		n := 1 + r.Intn(8)
		for i := range n {
			v := reviewVocab[r.Intn(len(reviewVocab))]
			if v == "мусор" || v == "" {
				b.WriteString(v + "\n")
				continue
			}
			b.WriteString("2026-11-20T10:0" + string(rune('0'+i)) + ":00+01:00 " + v + "\n")
		}
		full := b.String()
		if r.Intn(2) == 0 {
			full = strings.TrimSuffix(full, "\n")
		}
		want, wbad, _ := TrustEvents(strings.NewReader(full))
		var s TrustState
		data := []byte(full)
		for len(data) > 0 {
			k := 1 + r.Intn(len(data))
			if err := s.Append(data[:k]); err != nil {
				t.Fatal(err)
			}
			data = data[k:]
		}
		got, gbad, _ := s.Snapshot()
		if len(want) == 0 {
			want = nil
		}
		if len(got) == 0 {
			got = nil
		}
		if !reflect.DeepEqual(got, want) || gbad != wbad {
			diffs++
			if diffs <= 3 {
				t.Errorf("diff\nlog=%q\nfull=%+v bad=%d\ninc=%+v bad=%d", full, want, wbad, got, gbad)
			}
		}
	}
	if diffs > 0 {
		t.Errorf("diffs=%d", diffs)
	}
}
