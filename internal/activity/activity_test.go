package activity

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNilRecorder(t *testing.T) {
	var r *Recorder
	r.Add("tmp", KindPass, "", "", 0, "", time.Now())
	if err := r.Flush(time.Now(), time.Hour); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir string) []Record {
	t.Helper()
	var out []Record
	if _, err := NewStore(dir).Read(time.Time{}, func(r *Record) { out = append(out, *r) }); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFlush(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Now().Add(-time.Hour)
	r := New(dir, t0)
	r.Add("tmp", "sent", "", "lead", 3, "", t0.Add(time.Minute))
	r.Add("tmp", "sent", "", "lead", 7, "", t0.Add(2*time.Minute))
	r.Add("tmp", KindPass, "", "", 0, "", t0)
	if err := r.Flush(t0.Add(10*time.Minute), 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	got := read(t, dir)
	if len(got) != 2 || got[1].Kind != "sent" || got[1].Count != 2 || got[1].Max != 7 ||
		!got[1].From.Equal(t0) || !got[1].Last.Equal(t0.Add(2*time.Minute)) {
		t.Fatalf("после слива: %+v", got)
	}
	if err := r.Flush(t0.Add(20*time.Minute), 30*24*time.Hour); err != nil || len(read(t, dir)) != 2 {
		t.Errorf("пустой слив дописал записи: %v", err)
	}

	// запись не удалась — накопленное остаётся и сливается с новым
	r.Add("orphans", "would_reap", "node", "lead", 2, "", t0.Add(21*time.Minute))
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r.store.Dir = blocker // папка учёта — файл: запись не удастся
	if err := r.Flush(t0.Add(30*time.Minute), 30*24*time.Hour); err == nil {
		t.Fatal("слив в файл вместо папки прошёл")
	}
	r.store.Dir = dir
	r.Add("orphans", "would_reap", "node", "lead", 5, "", t0.Add(31*time.Minute))
	if err := r.Flush(t0.Add(40*time.Minute), 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	got = read(t, dir)
	last := got[len(got)-1]
	if len(got) != 3 || last.Count != 2 || last.Max != 5 || !last.From.Equal(t0.Add(20*time.Minute)) {
		t.Errorf("после сбоя записи: %+v", got)
	}
}

func TestBuild(t *testing.T) {
	at := time.Date(2026, 10, 25, 12, 30, 0, 0, time.UTC)
	h := func(d time.Duration) time.Time { return at.Add(-d) }
	recs := []Record{
		{From: h(50 * time.Hour), To: h(50 * time.Hour), Watcher: "aguard", Kind: KindStart, Count: 1},
		// за неделю, но не за сутки
		{From: h(50 * time.Hour), To: h(49 * time.Hour), Watcher: "tmp", Kind: KindPass, Count: 60},
		{From: h(50 * time.Hour), To: h(49 * time.Hour), Watcher: "tmp", Kind: "would_send", Target: "lead", Count: 1, Max: 4},
		// за сутки: проходы в последние 3 часа, дальше по сутки — пусто (демон стоял)
		{From: h(3 * time.Hour), To: h(3 * time.Hour), Watcher: "aguard", Kind: KindStart, Count: 1},
		{From: h(3 * time.Hour), To: at, Watcher: "tmp", Kind: KindPass, Count: 180},
		{From: h(3 * time.Hour), To: at, Watcher: "tmp", Kind: KindPanic, What: "boom", Count: 1},
		{From: h(3 * time.Hour), To: at, Watcher: "tmp", Kind: "would_send", Target: "lead", Count: 2, Max: 9, Unit: "entries"},
		// позже момента отчёта — не входит
		{From: at, To: at.Add(time.Minute), Watcher: "tmp", Kind: KindPass, Count: 1},
	}
	extra := func(from, to time.Time) ([]Line, error) {
		return []Line{{Kind: "hook_timeout", What: "PostToolBatch", Count: 1, Max: 23900, Unit: "ms"}}, nil
	}
	r, err := Build(at, recs, []Spec{{Name: "tmp", Mode: "observe", Looped: true}, {Name: "waits", Looped: true, Extra: extra}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Daemon.Starts["day"] != 1 || r.Daemon.Starts["week"] != 2 {
		t.Errorf("запуски: %v", r.Daemon.Starts)
	}
	day, week := r.Watchers[0].Windows["day"], r.Watchers[0].Windows["week"]
	if day.Passes != 180 || day.Panics != 1 || week.Passes != 240 {
		t.Errorf("проходы: сутки %+v, неделя %+v", day, week)
	}
	// сутки: с 12:30 вчера, первый целый час 13:00, часы до 09:00 сегодня без проходов: 20
	if day.SilentHours != 20 {
		t.Errorf("часов без проходов за сутки: %d", day.SilentHours)
	}
	if !week.Covered.Equal(h(50*time.Hour)) || week.SilentHours != 50-3-2 {
		t.Errorf("неделя: учёт с %v, часов без проходов %d", week.Covered, week.SilentHours)
	}
	var send *Line
	for i := range week.Lines {
		if week.Lines[i].Kind == "would_send" {
			send = &week.Lines[i]
		}
	}
	if send == nil || send.Count != 3 || send.Max != 9 || send.Target != "lead" {
		t.Errorf("сводки за неделю: %+v", week.Lines)
	}
	if w := r.Watchers[1].Windows["day"]; len(w.Lines) != 1 || w.Lines[0].Max != 23900 || w.Passes != 0 {
		t.Errorf("waits: %+v", w)
	}
	var out bytes.Buffer
	if err := r.Print(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "would_send → lead: 2, макс 9 записей") {
		t.Errorf("печать:\n%s", out.String())
	}
}
