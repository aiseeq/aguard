package evictors

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTrackerSeedsAndCountsSessionCandidatesAgainstOtherSessions(t *testing.T) {
	tr := NewTracker(1<<20, 1<<20)
	at := time.Date(2026, 11, 15, 1, 0, 0, 0, time.UTC)
	base := []Group{{Path: "/agents.slice/agent-a-1.scope", Session: true, Swap: 10 << 20, Anon: 2 << 20}, {Path: "/agents.slice/agent-b-2.scope", Session: true, Swap: 20 << 20, Anon: 2 << 20}, {Path: "/app.slice/adream.service", Anon: 4 << 20}}
	if got := tr.Sample(at, base); got != nil {
		t.Fatalf("первый проход должен засеять состояние: %+v", got)
	}
	next := []Group{{Path: "/agents.slice/agent-a-1.scope", Session: true, Swap: 12 << 20, Anon: 4 << 20}, {Path: "/agents.slice/agent-b-2.scope", Session: true, Swap: 22 << 20, Anon: 4 << 20}, {Path: "/app.slice/adream.service", Anon: 6 << 20}}
	got := tr.Sample(at.Add(time.Minute), next)
	if got == nil || len(got.Changes) != 3 {
		t.Fatalf("три кандидата сессии/app: %+v", got)
	}
	byPath := map[string]Change{}
	for _, c := range got.Changes {
		byPath[c.Path] = c
	}
	if c := byPath["/agents.slice/agent-a-1.scope"]; !c.Candidate || !c.Session || c.Anon != 2<<20 || c.OtherSwap != 2<<20 {
		t.Errorf("сессия A должна видеть swap только B: %+v", c)
	}
	if c := byPath["/agents.slice/agent-b-2.scope"]; !c.Candidate || !c.Session || c.Anon != 2<<20 || c.OtherSwap != 2<<20 {
		t.Errorf("сессия B должна видеть swap только A: %+v", c)
	}
	if c := byPath["/app.slice/adream.service"]; !c.Candidate || c.Session || c.Anon != 2<<20 || c.OtherSwap != 4<<20 {
		t.Errorf("кандидат службы: %+v", c)
	}
}

func TestTrackerInclusiveThresholdsAndNewSessionBaseline(t *testing.T) {
	at := time.Date(2026, 11, 15, 1, 0, 0, 0, time.UTC)
	tr := NewTracker(1<<20, 1<<20)
	tr.Sample(at, []Group{{Path: "/agents.slice/old-1.scope", Session: true}, {Path: "/agents.slice/stable-3.scope", Session: true, Swap: 5 << 20}, {Path: "/app.slice/existing.service", Anon: 8 << 20}})
	// Both thresholds are inclusive; equal swap on another session does not count as a session.
	got := tr.Sample(at.Add(time.Second), []Group{{Path: "/agents.slice/old-1.scope", Session: true, Swap: 1 << 20}, {Path: "/agents.slice/stable-3.scope", Session: true, Swap: 5 << 20}, {Path: "/agents.slice/new-2.scope", Session: true, Swap: 90 << 20, Anon: 8 << 20}, {Path: "/app.slice/existing.service", Anon: 9 << 20}})
	if got == nil {
		t.Fatal("swap и anon ровно на пороге должны засчитываться")
	}
	for _, c := range got.Changes {
		if c.Path == "/app.slice/existing.service" && (!c.Candidate || c.OtherSwap != 1<<20 || c.Sessions != 1) {
			t.Errorf("порог или число изменивших swap сессий: %+v", c)
		}
	}
}

func TestTrackerNewCgroupStartsAtZeroAndNoSwapAdvancesBaseline(t *testing.T) {
	at := time.Date(2026, 11, 15, 1, 0, 0, 0, time.UTC)
	tr := NewTracker(1<<20, 1<<20)
	tr.Sample(at, []Group{{Path: "/agents.slice/agent-1.scope", Session: true}, {Path: "/app.slice/chrome.service", Anon: 3 << 20}})
	if got := tr.Sample(at.Add(time.Second), []Group{{Path: "/agents.slice/agent-1.scope", Session: true}, {Path: "/app.slice/chrome.service", Anon: 5 << 20}}); got != nil {
		t.Fatalf("без прироста swap кандидат создан: %+v", got)
	}
	got := tr.Sample(at.Add(2*time.Second), []Group{{Path: "/agents.slice/agent-1.scope", Session: true, Swap: 2 << 20}, {Path: "/app.slice/chrome.service", Anon: 5 << 20}, {Path: "/app.slice/new.service", Anon: 1 << 20}})
	if got == nil || len(got.Changes) != 1 || got.Changes[0].Path != "/app.slice/new.service" || got.Changes[0].Anon != 1<<20 {
		t.Fatalf("новая cgroup должна считаться от нуля, старая база — от предыдущего тика: %+v", got)
	}
}

func TestAggregateNetAnonOrderingProcessIdentityTopAndLastSessions(t *testing.T) {
	a := NewAggregate()
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	a.Add(&Interval{At: at, Changes: []Change{{Path: "/app.slice/chrome.service", Process: "chrome", Anon: 2 << 20, OtherSwap: 6 << 20, Sessions: 3, Candidate: true}, {Path: "/app.slice/z.service", Process: "z", Anon: 4 << 20, OtherSwap: 10 << 20, Sessions: 2, Candidate: true}}})
	a.Add(&Interval{At: at.Add(time.Minute), Changes: []Change{{Path: "/app.slice/chrome.service", Process: "chrome", Anon: -1 << 20, OtherSwap: 8 << 20, Sessions: 4}}})
	a.Add(&Interval{At: at.Add(2 * time.Minute), Changes: []Change{{Path: "/app.slice/chrome.service", Process: "renderer", Anon: 3 << 20, OtherSwap: 3 << 20, Sessions: 1, Candidate: true}}})
	a.Add(&Interval{At: at.Add(time.Hour), Changes: []Change{{Path: "/app.slice/session-history.service", Process: "worker", Anon: 2 << 20, OtherSwap: 4 << 20, Sessions: 5, Candidate: true}}})
	a.Add(&Interval{At: at.Add(2 * time.Hour), Changes: []Change{{Path: "/app.slice/session-history.service", Process: "worker", Anon: 2 << 20, OtherSwap: 1 << 20, Sessions: 1, Candidate: true}}})
	r := a.Report(at.Add(-time.Hour), 15)
	if len(r.Rows) != 3 {
		t.Fatalf("процессы одной cgroup должны попадать в одну строку: %+v", r.Rows)
	}
	if r.Rows[0].Cgroup != "/app.slice/chrome.service" || r.Rows[0].Matched != 5<<20 {
		t.Fatalf("сортировка по совпавшему swap: %+v", r.Rows)
	}
	chrome := r.Rows[0]
	if chrome.Process != "renderer" || chrome.Anon != 4<<20 || chrome.Swap != 9<<20 || chrome.Intervals != 2 || chrome.Sessions != 1 {
		t.Fatalf("чистый anon, последнее имя процесса, swap, интервалы, число сессий: %+v", chrome)
	}
	if r.Rows[1].Cgroup != "/app.slice/z.service" || r.Rows[1].Swap != 10<<20 || r.Rows[2].Cgroup != "/app.slice/session-history.service" {
		t.Fatalf("строки по совпавшему swap: %+v", r.Rows)
	}
	if r.Rows[2].Sessions != 1 {
		t.Fatalf("sessions должен быть значением последнего часа, а не максимумом: %+v", r.Rows[2])
	}
	if top := a.Report(at, 1); len(top.Rows) != 1 || top.Rows[0].Cgroup != "/app.slice/chrome.service" {
		t.Fatalf("top: %+v", top.Rows)
	}
	var out bytes.Buffer
	if err := r.Print(&out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "Период агрегатов: 2026-11-15 12:00 — 2026-11-15 15:00") || strings.Index(got, "/app.slice/chrome.service") > strings.Index(got, "/app.slice/z.service") {
		t.Fatalf("нет фактического окна или неверный порядок:\n%s", got)
	}
}

func TestAggregateRanksBySwapMatchedToAnonGrowth(t *testing.T) {
	a := NewAggregate()
	at := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	for i := range 7 {
		a.Add(&Interval{At: at.Add(time.Duration(i) * time.Minute), Changes: []Change{{Path: "/app.slice/frequent.service", Anon: 1 << 20, OtherSwap: 100 << 20, Candidate: true}}})
	}
	a.Add(&Interval{At: at.Add(time.Hour), Changes: []Change{{Path: "/app.slice/large.service", Anon: 400 << 20, OtherSwap: 100 << 20, Candidate: true}}})
	r := a.Report(at, 15)
	if len(r.Rows) != 2 || r.Rows[0].Cgroup != "/app.slice/large.service" || r.Rows[0].Matched != 100<<20 || r.Rows[1].Matched != 7<<20 {
		t.Fatalf("ранжирование должно учитывать совпавшую величину: %+v", r.Rows)
	}
}

func TestAggregateKeepsNegativeAnonInLaterHour(t *testing.T) {
	a := NewAggregate()
	at := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	a.Add(&Interval{At: at, Changes: []Change{{Path: "/app.slice/chrome.service", Process: "chrome", Anon: 4 << 20, OtherSwap: 4 << 20, Candidate: true}}})
	a.Add(&Interval{At: at.Add(time.Hour), Changes: []Change{{Path: "/app.slice/chrome.service", Anon: -4 << 20, OtherSwap: 4 << 20}}})
	r := a.Report(at, 15)
	if len(r.Rows) != 1 || r.Rows[0].Anon != 0 || r.Rows[0].Process != "chrome" || r.Rows[0].Swap != 4<<20 {
		t.Fatalf("позднее падение anon должно вычесться, не затирая имя: %+v", r.Rows)
	}
}

func TestPruneForgetsCandidatesRemovedFromAllHours(t *testing.T) {
	a := NewAggregate()
	at := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	a.Add(&Interval{At: at, Changes: []Change{{Path: "/app.slice/gone.service", Anon: 2 << 20, OtherSwap: 2 << 20, Candidate: true}}})
	a.Prune(at.Add(Keep + time.Hour))
	if _, known := a.known[candidateKey{cgroup: "/app.slice/gone.service"}]; known {
		t.Fatal("prune оставил в индексе удалённую cgroup")
	}
	a.Add(&Interval{At: at.Add(Keep + 2*time.Hour), Changes: []Change{{Path: "/app.slice/gone.service", Anon: -1 << 20}}})
	if len(a.Hours) != 0 {
		t.Fatalf("после prune cgroup снова должна быть неизвестна: %+v", a.Hours)
	}
}

func TestReportSinceExcludesOlderAggregateHours(t *testing.T) {
	a := NewAggregate()
	base := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	a.Add(&Interval{At: base, Changes: []Change{{Path: "/app.slice/old.service", Anon: 2 << 20, OtherSwap: 2 << 20, Candidate: true}}})
	a.Add(&Interval{At: base.Add(24 * time.Hour), Changes: []Change{{Path: "/app.slice/new.service", Anon: 3 << 20, OtherSwap: 3 << 20, Candidate: true}}})
	r := a.Report(base.Add(24*time.Hour), 15)
	if len(r.Rows) != 1 || r.Rows[0].Cgroup != "/app.slice/new.service" || !r.From.Equal(base.Add(24*time.Hour)) {
		t.Fatalf("since должен исключить старый час и показать реальный диапазон: %+v", r)
	}
}

func TestReportUsesLatestNonemptyProcessFromWindow(t *testing.T) {
	a := NewAggregate()
	at := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	a.Add(&Interval{At: at, Changes: []Change{{Path: "/app.slice/chrome.service", Process: "chrome-old", Anon: 2 << 20, OtherSwap: 2 << 20, Candidate: true}}})
	a.Add(&Interval{At: at.Add(time.Hour), Changes: []Change{{Path: "/app.slice/chrome.service", Process: "chrome-new", Anon: 2 << 20, OtherSwap: 2 << 20, Candidate: true}}})
	r := a.Report(at, 15)
	if len(r.Rows) != 1 || r.Rows[0].Process != "chrome-new" {
		t.Fatalf("имя должно обновляться из последнего часа окна: %+v", r.Rows)
	}
}

func TestReportOmitsRowsWithoutQualifyingIntervalsInWindow(t *testing.T) {
	a := NewAggregate()
	at := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	a.Add(&Interval{At: at, Changes: []Change{{Path: "/app.slice/chrome.service", Process: "chrome", Anon: 2 << 20, OtherSwap: 2 << 20, Candidate: true}}})
	a.Add(&Interval{At: at.Add(time.Hour), Changes: []Change{{Path: "/app.slice/chrome.service", Anon: -1 << 20, OtherSwap: 2 << 20}}})
	r := a.Report(at.Add(time.Hour), 15)
	if len(r.Rows) != 0 || !r.From.IsZero() {
		t.Fatalf("строка без qualifying-интервалов не должна быть в отчёте: %+v", r)
	}
}

func TestLoadLeavesOrphanTemporaryFilesForDaemonStartup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "evictors.json")
	a := NewAggregate()
	now := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	a.Add(&Interval{At: now, Changes: []Change{{Path: "/app.slice/kept.service", Anon: 2 << 20, OtherSwap: 2 << 20, Candidate: true}}})
	if err := Save(path, a); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(dir, "evictors.json.abandoned.tmp")
	other := filepath.Join(dir, "unrelated.tmp")
	for _, file := range []string{orphan, other} {
		if err := os.WriteFile(file, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Load(path, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Fatalf("Load не должен трогать временный файл: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("удалён посторонний temp: %v", err)
	}
}

func TestLoadRebuildsKnownCandidateIndex(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "evictors.json")
	now := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	a := NewAggregate()
	a.Add(&Interval{At: now, Changes: []Change{{Path: "/app.slice/restored.service", Anon: 2 << 20, OtherSwap: 2 << 20, Candidate: true}}})
	if err := Save(path, a); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path, now)
	if err != nil {
		t.Fatal(err)
	}
	key := candidateKey{cgroup: "/app.slice/restored.service"}
	if _, ok := loaded.known[key]; !ok {
		t.Fatal("Load не восстановил индекс известной cgroup")
	}
	loaded.Add(&Interval{At: now.Add(time.Hour), Changes: []Change{{Path: key.cgroup, Anon: -1 << 20}}})
	if len(loaded.Hours) != 2 || len(loaded.Hours[1].Rows) != 1 || loaded.Hours[1].Rows[0].Anon != -1<<20 {
		t.Fatalf("после Load отрицательная поправка должна найти прежнего кандидата: %+v", loaded.Hours)
	}
}

func TestRebuildKnownIgnoresRowsWithoutIntervals(t *testing.T) {
	a := NewAggregate()
	a.Hours = []Hour{{At: time.Now(), Rows: []Row{{Cgroup: "/app.slice/zero.service", Anon: -1 << 20}}}}
	a.rebuildKnown()
	if _, ok := a.known[candidateKey{cgroup: "/app.slice/zero.service"}]; ok {
		t.Fatal("строка без засчитанных интервалов попала в индекс")
	}
}

func TestReportSingleBadRecordIsPrinted(t *testing.T) {
	var out bytes.Buffer
	if err := (Report{Bad: 1}).Print(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "битых записей: 1") {
		t.Fatalf("битая запись не отражена:\n%s", out.String())
	}
}

func TestAggregateAtomicSaveLoadAndRetention(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "evictors.json")
	a := NewAggregate()
	now := time.Now().Truncate(time.Hour)
	a.Add(&Interval{At: now, Changes: []Change{{Path: "/app.slice/a.service", Process: "worker", Anon: 2 << 20, OtherSwap: 3 << 20, Sessions: 2, Candidate: true}}})
	if !a.Dirty() {
		t.Fatal("Add должен пометить агрегат изменённым")
	}
	if err := Save(path, a); err != nil {
		t.Fatal(err)
	}
	if a.Dirty() {
		t.Fatal("успешная атомарная запись должна очистить dirty")
	}
	secondAt := now.Add(time.Hour)
	a.Add(&Interval{At: secondAt, Changes: []Change{{Path: "/app.slice/b.service", Process: "new", Anon: 3 << 20, OtherSwap: 4 << 20, Sessions: 1, Candidate: true}}})
	if err := Save(path, a); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path, now)
	if err != nil {
		t.Fatal(err)
	}
	r := loaded.Report(now.Add(-time.Hour), 15)
	if len(r.Rows) != 2 || r.Rows[0].Process != "new" || r.Rows[0].Swap != 4<<20 {
		t.Fatalf("прочитанная копия: %+v", r.Rows)
	}
	for _, h := range loaded.Hours {
		if h.At.Before(now.Add(-Keep)) {
			t.Fatalf("устаревший час остался: %v", h.At)
		}
	}
}

func TestLoadPrunesHoursOlderThanRetention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evictors.json")
	now := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	hours := []Hour{{At: now.Add(-Keep - time.Hour), Rows: []Row{{Cgroup: "/old"}}}, {At: now.Add(-Keep + time.Hour), Rows: []Row{{Cgroup: "/kept"}}}}
	b, err := json.Marshal(hours)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Hours) != 1 || loaded.Hours[0].Rows[0].Cgroup != "/kept" || !loaded.Dirty() {
		t.Fatalf("Load должен отсечь старое и отметить сводку для перезаписи: %+v", loaded)
	}
}

func BenchmarkAggregateAddFullHistory(b *testing.B) {
	const historyHours, rowsPerHour, changes = 1439, 20, 180
	base := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	a := NewAggregate()
	a.Hours = make([]Hour, historyHours)
	for hour := range historyHours {
		rows := make([]Row, rowsPerHour)
		for row := range rowsPerHour {
			rows[row].Cgroup = fmt.Sprintf("/bench/%04d/%02d", hour, row)
		}
		a.Hours[hour] = Hour{At: base.Add(time.Duration(hour) * time.Hour), Rows: rows}
	}
	interval := &Interval{At: base.Add(historyHours * time.Hour), Changes: make([]Change, changes)}
	for i := range changes {
		hour := historyHours - changes/rowsPerHour + i/rowsPerHour
		row := i % rowsPerHour
		interval.Changes[i] = Change{Path: fmt.Sprintf("/bench/%04d/%02d", hour, row), Anon: -(1 << 20)}
	}
	a.rebuildKnown()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		a.Add(interval)
	}
}
