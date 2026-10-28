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
	line("shop-master pid=11 act: " + Compact(150_000, "s-ok") + NoReset([]string{"рабочее дерево: 2 изменений"}) + "; под pty набрал бы /compact")
	line("graft-master pid=12 act: " + Compact(120_000, "s-fail"))
	line(CompactResult("shop-master", "s-ok", 41*time.Second, nil, "", ""))
	line(CompactResult("graft-master", "s-fail", 3*time.Second, errors.New("exit status 1"), "", "API Error"))
	line("x-master pid=13 act: " + Compact(130_000, "s-lost")) // cl умер посреди сжатия
	line("aguard-master pid=14 act: " + Typing(110_000, TypedCompact) + NoReset([]string{"идёт фоновая задача"}))
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

func TestTrustEvents(t *testing.T) {
	const log = `2026-10-28T00:38:37+01:00 lead pid=707112 2.1.311→2.1.312 act: перезапускаю: простаивает, ничего не идёт
2026-10-28T00:38:38+01:00 lead: окно доверия к /home/alice: человек у терминала — не подтверждаю
2026-10-28T00:39:00+01:00 other pid=1 act: версия свежая
2026-10-28T00:39:30+01:00 lead pid=3739580 нет записи→нет записи act: не перезапускаю: не прочитал: реестр сессий: нет записи о процессе 3739580
2026-10-28T00:39:40+01:00 lead pid=666971: cl обновлён — перехожу в новый бинарник /home/alice/bin/cl
2026-10-28T00:40:00+01:00 x: окно доверия к /home/x: человек у терминала (ввод клавиши) — не подтверждаю
2026-10-28T00:41:00+01:00 y: окно доверия к /home/y: человек у терминала (ввод клавиши) — выбрал «да», Enter не нажимаю
2026-10-28T00:41:30+01:00 w: окно доверия к /home/w: write: broken pipe
2026-10-28T00:42:00+01:00 z: подтвердил доверие к /home/z: Claude Code не сохраняет доверие к домашней папке
2026-10-28T03:22:18+01:00 glint-master pid=1291562 2.1.311→2.1.312 act: перезапускаю
2026-10-28T09:34:38+01:00 lead pid=2077626 2.1.312→2.1.312 act: версия свежая
2026-10-28T10:04:35+01:00 p: подтвердил доверие к /home/alice: Claude Code не сохраняет доверие к домашней папке
2026-10-28T10:36:10+01:00 p: подтвердил доверие к /home/alice: Claude Code не сохраняет доверие к домашней папке
2026-10-28T10:37:08+01:00 p pid=177304 2.1.311→2.1.312 act: не перезапускаю: тихо 0 мин из 10 мин
2026-10-28T11:30:00+01:00 p pid=4242: подтвердил доверие к /home/alice: Claude Code не сохраняет доверие к домашней папке
2026-10-28T11:31:00+01:00 p pid=177305 2.1.312→2.1.312 act: версия свежая
`
	events, bad, err := TrustEvents(strings.NewReader(log))
	if err != nil || bad != 0 || len(events) != 8 {
		t.Fatalf("события=%+v bad=%d err=%v", events, bad, err)
	}
	if events[0].Name != "lead" || events[0].Dir != "/home/alice" || events[0].At.Format("15:04:05") != "00:38:38" || events[0].Recovered.Format("15:04:05") != "09:34:38" {
		t.Errorf("событие лида: %+v", events[0])
	}
	if !events[1].Recovered.IsZero() || !events[2].Recovered.IsZero() || events[3].Name != "w" || events[3].Dir != "/home/w" || !events[3].Recovered.IsZero() {
		t.Errorf("восстановление: %+v", events)
	}
	if events[4].Name != "z" || events[4].Dir != "/home/z" || !events[4].Confirmed || events[0].Confirmed || !events[4].Recovered.IsZero() {
		t.Errorf("подтверждённое окно: %+v", events[4])
	}
	// строка решения 10:37:08 — о подъёме после окна 10:36:10, не 10:04:35
	if !events[5].Recovered.IsZero() || events[6].Recovered.Format("15:04:05") != "10:37:08" {
		t.Errorf("подъём между окнами одной сессии: %+v %+v", events[5], events[6])
	}
	if e := events[7]; e.Name != "p" || e.PID != 4242 || e.Recovered.Format("15:04:05") != "11:31:00" || events[6].PID != 0 {
		t.Errorf("окно с pid cl: %+v", e)
	}
}
