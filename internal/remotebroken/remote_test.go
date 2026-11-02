package remotebroken

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const sample = `{"at":"2026-10-28T14:16:03+01:00","project":"/home/alice/git/graft","task":"exec","host":"pc","reason":"ssh: connect to host fedora-pc.local port 1: No route to host","pid":1772652,"ppid":1772642,"cwd":"/home/alice/git/graft","claude_pid":"1254137","worker":""}`

func TestParseExample(t *testing.T) {
	e, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if e.Host != "pc" || e.Task != "exec" || e.ClaudePID != "1254137" || e.At.Hour() != 14 {
		t.Fatalf("неверная запись: %+v", e)
	}
}

func TestReadNewSeedsAndReadsAppends(t *testing.T) {
	file := filepath.Join(t.TempDir(), "remote.jsonl")
	if err := os.WriteFile(file, []byte(sample+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var s State
	got, err := ReadNew(file, &s, nil)
	if err != nil || len(got) != 0 || !s.Seeded {
		t.Fatalf("первый проход: %v, %d, %+v", err, len(got), s)
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(sample + "\n")
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	got, err = ReadNew(file, &s, nil)
	if err != nil || len(got) != 1 {
		t.Fatalf("дополнение: %v, %d", err, len(got))
	}
}

func TestReadNewMissingFileThenFirstEvent(t *testing.T) {
	file := filepath.Join(t.TempDir(), "remote.jsonl")
	var s State
	if got, err := ReadNew(file, &s, nil); err != nil || len(got) != 0 || !s.Seeded {
		t.Fatalf("пустой проход: err=%v events=%d state=%+v", err, len(got), s)
	}
	if err := os.WriteFile(file, []byte(sample+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadNew(file, &s, nil)
	if err != nil || len(got) != 1 {
		t.Fatalf("первая запись после создания файла: err=%v events=%d", err, len(got))
	}
}

func TestReadNewTruncationAndBadLine(t *testing.T) {
	file := filepath.Join(t.TempDir(), "remote.jsonl")
	if err := os.WriteFile(file, []byte(sample+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := State{Seeded: true, Offset: 10000}
	if err := os.WriteFile(file, []byte("{bad}\n"+sample+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	warnings := 0
	got, err := ReadNew(file, &s, func(error) { warnings++ })
	if err != nil || len(got) != 1 || warnings != 1 {
		t.Fatalf("обрезка и битая строка: err=%v got=%d warn=%d", err, len(got), warnings)
	}
}

func TestOwnerWorkerSessionAndLead(t *testing.T) {
	e, _ := Parse([]byte(sample))
	e.Worker = "job-1"
	owner, err := Owner(e, func(string) (string, error) { return "worker-master", nil }, func(int) (string, error) {
		t.Fatal("сессия не должна запрашиваться")
		return "", nil
	}, "lead")
	if err != nil || owner != "worker-master" {
		t.Fatalf("worker: %q %v", owner, err)
	}
	e.Worker = ""
	owner, err = Owner(e, nil, func(pid int) (string, error) {
		if pid != 1254137 {
			t.Fatalf("pid=%d", pid)
		}
		return "claude-master", nil
	}, "lead")
	if err != nil || owner != "claude-master" {
		t.Fatalf("session: %q %v", owner, err)
	}
	owner, err = Owner(e, nil, func(int) (string, error) { return "", nil }, "lead")
	if err != nil || owner != "lead" {
		t.Fatalf("lead: %q %v", owner, err)
	}
}

func TestRepeatWindowAndSummary(t *testing.T) {
	e, _ := Parse([]byte(sample))
	now := time.Date(2026, 10, 28, 15, 0, 0, 0, time.Local)
	s := State{}
	first := s.Apply([]Event{e}, []string{"master-a"}, "lead", now, time.Hour)
	if len(first) != 2 || first[0].To != "master-a" || first[1].To != "lead" {
		t.Fatalf("первое письмо: %+v", first)
	}
	e.At = e.At.Add(time.Minute)
	if got := s.Apply([]Event{e}, []string{"master-a"}, "lead", now.Add(time.Minute), time.Hour); len(got) != 0 {
		t.Fatalf("ранний повтор: %+v", got)
	}
	e.At = e.At.Add(time.Minute)
	got := s.Apply([]Event{e}, []string{"master-a"}, "lead", now.Add(time.Hour), time.Hour)
	if len(got) != 1 || !strings.Contains(got[0].Message, "До этого ещё 1 раз") { // поломка та же — лиду не повторяется
		t.Fatalf("счётчик повторов: %+v", got)
	}

	e2 := e
	e2.Project = "/other/project"
	e2.At = e.At.Add(time.Minute)
	got = s.Apply([]Event{e, e2}, []string{"master-a", "master-b"}, "lead", now.Add(2*time.Hour), time.Hour)
	found := false
	for _, n := range got {
		if n.Summary && n.To == "lead" && strings.Contains(n.Message, "PC: путь сломан у 2 мастеров") {
			found = true
		}
	}
	if !found {
		t.Fatalf("нет сводки лиду: %+v", got)
	}
}

func TestFirstEventToLeadAndSummary(t *testing.T) {
	e, _ := Parse([]byte(sample))
	now := time.Date(2026, 10, 28, 15, 0, 0, 0, time.Local)
	s := State{}
	apply := func(event Event, owner string, at time.Time) []Notice {
		return s.Apply([]Event{event}, []string{owner}, "lead", at, time.Hour)
	}
	find := func(notices []Notice, to string, summary bool) (Notice, bool) {
		for _, n := range notices {
			if n.To == to && n.Summary == summary {
				return n, true
			}
		}
		return Notice{}, false
	}

	first := apply(e, "master-a", now)
	ownerNotice, ownerOK := find(first, "master-a", false)
	leadNotice, leadOK := find(first, "lead", false)
	if len(first) != 2 || !ownerOK || !leadOK || leadNotice.Key != "first:pc" ||
		leadNotice.Message != ownerNotice.Message || leadNotice.Count != 1 {
		t.Fatalf("первое событие мастеру и лиду: %+v", first)
	}

	e.At = e.At.Add(time.Minute)
	if got := apply(e, "master-a", now.Add(time.Minute)); len(got) != 0 {
		t.Fatalf("второе событие той же машины повторило письмо лиду: %+v", got)
	}
	e.Host = "laptop"
	otherHost := apply(e, "master-a", now.Add(2*time.Minute))
	if n, ok := find(otherHost, "lead", false); !ok || n.Key != "first:laptop" {
		t.Fatalf("другая машина не уведомила лида: %+v", otherHost)
	}

	e.Host = "pc"
	e.Project = "/other/project"
	e.At = e.At.Add(time.Minute)
	secondOwner := apply(e, "master-b", now.Add(3*time.Minute))
	if _, ok := find(secondOwner, "lead", false); ok {
		t.Fatalf("первое письмо лиду продублировано при сводке: %+v", secondOwner)
	}
	if n, ok := find(secondOwner, "lead", true); !ok || !strings.Contains(n.Message, "у 2 мастеров") {
		t.Fatalf("нет сводки двум мастерам: %+v", secondOwner)
	}

	leadState := State{}
	leadOwner := leadState.Apply([]Event{e}, []string{"lead"}, "lead", now, time.Hour)
	if len(leadOwner) != 1 || leadOwner[0].To != "lead" {
		t.Fatalf("владелец-лид получил не одно письмо: %+v", leadOwner)
	}
}

// Лиду — только переход в «сломана»: события подряд (чаще repeat) три часа — одно письмо first, хотя окна сводки
// сменились; после тишины не короче repeat — новая поломка, снова письмо.
func TestFirstLeadNoticeOncePerIncident(t *testing.T) {
	e, _ := Parse([]byte(sample))
	start := e.At
	s := State{}
	firsts := func(d time.Duration) int {
		x := e
		x.At = start.Add(d)
		count := 0
		for _, n := range s.Apply([]Event{x}, []string{"master-a"}, "lead", x.At, time.Hour) {
			if n.To == "lead" && n.Key == "first:pc" {
				count++
			}
		}
		return count
	}
	total := 0
	for d := time.Duration(0); d <= 3*time.Hour; d += 30 * time.Minute {
		total += firsts(d)
	}
	if total != 1 {
		t.Fatalf("поломка подряд три часа — писем лиду %d, want 1", total)
	}
	if got := firsts(3*time.Hour + 59*time.Minute); got != 0 {
		t.Fatalf("тишина 59 мин — письмо лиду %d, want 0", got)
	}
	if got := firsts(4*time.Hour + 59*time.Minute); got != 1 { // ровно repeat после события 3:59
		t.Fatalf("тишина час — новая поломка, писем лиду %d, want 1", got)
	}
}

func TestSummaryWindow(t *testing.T) {
	e, _ := Parse([]byte(sample))
	at := e.At
	ev := func(project string, d time.Duration) Event {
		x := e
		x.Project, x.At = project, at.Add(d)
		return x
	}
	s := State{}
	// мастера с разницей больше окна — не одна поломка, сводки нет
	s.Apply([]Event{ev("/a", 0)}, []string{"master-a"}, "lead", at, time.Hour)
	for _, n := range s.Apply([]Event{ev("/b", 2*time.Hour)}, []string{"master-b"}, "lead", at.Add(2*time.Hour), time.Hour) {
		if n.Summary {
			t.Fatalf("сводка по мастерам из разных окон: %+v", n)
		}
	}
	// второй мастер в окне — сводка с первым временем и причиной окна
	got := s.Apply([]Event{ev("/c", 2*time.Hour+time.Minute)}, []string{"master-c"}, "lead", at.Add(2*time.Hour+time.Minute), time.Hour)
	var sum []Notice
	for _, n := range got {
		if n.Summary {
			sum = append(sum, n)
		}
	}
	first := at.Add(2 * time.Hour).Local().Format("15:04")
	if len(sum) != 1 || !strings.Contains(sum[0].Message, "у 2 мастеров (master-b, master-c)") ||
		!strings.Contains(sum[0].Message, "первая в "+first) || !strings.Contains(sum[0].Message, "No route to host") {
		t.Fatalf("сводка окна: %+v", sum)
	}
	// после сводки и окна — новое окно, а не продолжение старого
	got = s.Apply([]Event{ev("/d", 4*time.Hour)}, []string{"master-d"}, "lead", at.Add(4*time.Hour), time.Hour)
	for _, n := range got {
		if n.Summary {
			t.Fatalf("сводка по одному мастеру нового окна: %+v", n)
		}
	}
}

func TestMessageCount(t *testing.T) {
	e, _ := Parse([]byte(sample))
	if m := Message(e); strings.Contains(m, "раз") {
		t.Fatalf("одиночное письмо со счётчиком: %s", m)
	}
}

func TestReadNewWaitsForLineEnd(t *testing.T) {
	file := filepath.Join(t.TempDir(), "remote.jsonl")
	s := State{Seeded: true}
	half := len(sample) / 2
	if err := os.WriteFile(file, []byte(sample[:half]), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadNew(file, &s, func(err error) { t.Fatalf("недописанная строка — ошибка: %v", err) }); err != nil || len(got) != 0 || s.Offset != 0 {
		t.Fatalf("недописанная строка: %v, %d, %d", err, len(got), s.Offset)
	}
	if err := os.WriteFile(file, []byte(sample+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadNew(file, &s, nil); err != nil || len(got) != 1 {
		t.Fatalf("дописанная строка: %v, %d", err, len(got))
	}
}

// Переход на ноут по очереди PC — не поломка: письма и сводки не будет; ssh и сбой отправки — поломка.
func TestSplitQueuedKeepsOnlyBroken(t *testing.T) {
	at := time.Date(2026, 11, 2, 3, 24, 45, 0, time.Local)
	ev := func(reason string) Event {
		return Event{At: at, Project: "/home/u/git/sc2demu", Task: "exec", Host: "pc", Reason: reason}
	}
	events := []Event{
		ev("run exited 125: pc: не впущен очередью graft за 176 с — шаг уходит на ноут (код 125)"),
		ev("ssh: Could not resolve hostname fedora-pc.local: Name or service not known"),
		ev("run exited 125: worker: очередь PC дольше 1ms"),
		ev("run exited 125: worker: отправка дерева на PC: exit status 24"),
		ev("run exited 125: worker: уборка не подтверждена: PC в карантине (текст: очередь PC дольше 1ms)"),
	}
	broken, queued := SplitQueued(events)
	if len(broken) != 3 || len(queued) != 2 || !strings.HasPrefix(broken[0].Reason, "ssh:") ||
		!strings.Contains(broken[1].Reason, "отправка дерева") || !strings.Contains(broken[2].Reason, "карантине") {
		t.Fatalf("broken %v, queued %v", broken, queued)
	}
}
