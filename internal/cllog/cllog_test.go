package cllog

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Строки — теми же функциями, что пишет cl: разбор и запись не расходятся.
func TestParse(t *testing.T) {
	at := time.Date(2026, 10, 27, 11, 19, 29, 0, time.FixedZone("CET", 3600))
	var b strings.Builder
	line := func(text string) {
		b.WriteString(Line(at, text))
		at = at.Add(time.Second)
	}
	line("sc2go-master pid=5 2.1.309→2.1.310 log: не стал бы: Claude Code: busy")
	line("ledger-master pid=10 act: " + Reset(238_400, "s-reset"))
	line("shop-master pid=11 act: " + Compact(150_000, "s-ok") + "; под pty набрал бы /compact")
	line("graft-master pid=12 act: " + Compact(120_000, "s-fail"))
	line(CompactResult("shop-master", "s-ok", 41*time.Second, nil, "", ""))
	line(CompactResult("graft-master", "s-fail", 3*time.Second, errors.New("exit status 1"), "", "API Error"))
	line("x-master pid=13 act: " + Compact(130_000, "s-lost")) // cl умер посреди сжатия
	line("aguard-master pid=14 act: " + Typing(110_000, TypedCompact))
	line("aguard-master pid=14: " + Typed(TypedCompact, "s-typed"))
	line("lead pid=15 act: " + Typing(200_000, TypedClear))
	line("lead pid=15: " + HumanTyping(10*time.Second))
	line("git-admin pid=16 act: " + Reset(300_000, "s-cancel"))
	line("git-admin pid=16: " + Cancelled(errors.New("no such process")))
	b.WriteString("обрывок без времени\n")
	got, bad, err := Parse(strings.NewReader(b.String()))
	if err != nil || bad != 1 {
		t.Fatalf("bad %d, err %v", bad, err)
	}
	type short struct{ name, sid, kind, result string }
	var list []short
	for _, a := range got {
		list = append(list, short{a.Name, a.SessionID, a.Kind, a.Result})
	}
	want := []short{
		{"ledger-master", "s-reset", KindReset, ResultDone},
		{"shop-master", "s-ok", KindCompact, ResultDone},
		{"graft-master", "s-fail", KindCompact, ResultFailed},
		{"x-master", "s-lost", KindCompact, ResultNone},
		{"aguard-master", "s-typed", KindTypeCompact, ResultDone},
		{"lead", "", KindTypeClear, ResultSkipped},
		{"git-admin", "s-cancel", KindReset, ResultFailed},
	}
	if !slices.Equal(list, want) {
		t.Errorf("\n got %v\nwant %v", list, want)
	}
	if got[0].Tokens != 238_000 || !got[0].At.Equal(time.Date(2026, 10, 27, 10, 19, 30, 0, time.UTC)) {
		t.Errorf("сброс: %+v", got[0])
	}
}

// Окно [from, to); журнала нет — пусто без ошибки.
func TestRead(t *testing.T) {
	file := filepath.Join(t.TempDir(), "restart.log")
	if got, _, err := Read(file, time.Time{}, time.Now()); err != nil || got != nil {
		t.Fatalf("нет журнала: %v %v", got, err)
	}
	at := time.Date(2026, 10, 27, 11, 0, 0, 0, time.UTC)
	text := Line(at, "a pid=1 act: "+Reset(200_000, "s1")) + Line(at.Add(time.Hour), "b pid=2 act: "+Reset(200_000, "s2"))
	if err := os.WriteFile(file, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	got, _, err := Read(file, at.Add(time.Minute), at.Add(2*time.Hour))
	if err != nil || len(got) != 1 || got[0].Name != "b" {
		t.Errorf("окно: %+v %v", got, err)
	}
}
