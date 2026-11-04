package queue

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Приёмка, четвёртый заход: находки независимой проверки queue-order-guard. Написана мастером, боец её не правит.

func freshHigh(w *world, age time.Duration) Ticket {
	high := tick("HIGH", "открыт")
	high.Created = w.now.Add(-age)
	return high
}

// Состояние потеряно, HIGH моложе 4 ч: мастер получает строку, а не молчание.
func TestSeedFreshHighGoes(t *testing.T) {
	w := newWorld(ModeOn)
	w.free["m-master"] = true
	s := &State{}
	ts := []Ticket{freshHigh(w, time.Hour)}
	if ns := step(s, ts, w); !has(ns, KindHigh, "m-master") {
		t.Fatalf("свежий HIGH после потери состояния: %+v", ns)
	}
	if ns := step(s, ts, w); len(ns) != 0 {
		t.Fatalf("второй раз: %+v", ns)
	}
}

// Ровно 4 ч и старше — учтён: ни мастеру, ни админу, ни сразу, ни позже.
func TestSeedOldHighCounted(t *testing.T) {
	for _, age := range []time.Duration{highWait, 5 * time.Hour} {
		w := newWorld(ModeOn)
		w.free["m-master"] = true
		s := &State{}
		ts := []Ticket{freshHigh(w, age)}
		if ns := step(s, ts, w); len(ns) != 0 {
			t.Fatalf("возраст %s: %+v", age, ns)
		}
		w.now = w.now.Add(5 * time.Hour)
		if ns := step(s, ts, w); len(ns) != 0 {
			t.Fatalf("возраст %s, позже: %+v", age, ns)
		}
	}
}

// Срок 4 ч у засеянного HIGH считается от создания тикета: потеря состояния его не отодвигает.
func TestSeedFreshHighAdminFromCreated(t *testing.T) {
	w := newWorld(ModeOn)
	s := &State{}
	ts := []Ticket{freshHigh(w, time.Hour)}
	step(s, ts, w)
	w.now = w.now.Add(2*time.Hour + 59*time.Minute)
	if ns := step(s, ts, w); has(ns, KindHighAdmin, "git-admin") {
		t.Fatalf("3 ч 59 мин от создания: %+v", ns)
	}
	w.now = w.now.Add(2 * time.Minute)
	if ns := step(s, ts, w); !has(ns, KindHighAdmin, "git-admin") {
		t.Fatalf("4 ч 01 мин от создания: %+v", ns)
	}
}

// Утренняя отметка журнала не съедает боевую строку: проект вошёл в live в тот же день — мастер её получает, один раз.
func TestMorningLogThenLiveSameDay(t *testing.T) {
	w := newWorld(ModeLog)
	w.now = at("2026-11-04T07:01:00+01:00")
	w.free["m-master"] = true
	w.open = []string{"LOW-DO"}
	s := seeded(w)
	low := tick("LOW", "открыт")
	low.Diff = "DO"
	ts := []Ticket{low}
	ns := step(s, ts, w)
	if !has(ns, KindMorning, "m-master") || ns[0].Serious {
		t.Fatalf("журнал считает как боевой, без письма: %+v", ns)
	}
	if ns := step(s, ts, w); len(ns) != 0 {
		t.Fatalf("в журнале раз в день: %+v", ns)
	}
	w.mode = ModeOn
	ns = step(s, ts, w)
	if !has(ns, KindMorning, "m-master") || !ns[0].Serious {
		t.Fatalf("вошёл в live в тот же день: %+v", ns)
	}
	if ns := step(s, ts, w); len(ns) != 0 {
		t.Fatalf("боевая раз в день: %+v", ns)
	}
}

// Состояние цело, новый HIGH: 4 ч считаются от первого появления, посев тут ни при чём.
func TestSeededNewHighWaitsFromSeen(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	ts := []Ticket{freshHigh(w, 3*time.Hour)}
	step(s, ts, w)
	w.now = w.now.Add(2 * time.Hour)
	if ns := step(s, ts, w); has(ns, KindHighAdmin, "git-admin") {
		t.Fatalf("2 ч от появления: %+v", ns)
	}
	w.now = w.now.Add(2*time.Hour + time.Minute)
	if ns := step(s, ts, w); !has(ns, KindHighAdmin, "git-admin") {
		t.Fatalf("4 ч 01 мин от появления: %+v", ns)
	}
}

// Утро считает работу как tk next: чужой «в работе» ему не виден, свой и наступивший «ждёт» видны.
func TestMorningCountsLikeTkNext(t *testing.T) {
	cases := []struct {
		name, status, executor string
		due, want              bool
	}{
		{"открыт", "открыт", "", false, true},
		{"в работе у мастера", "в работе", "m-master", false, true},
		{"в работе у мастера, другой регистр", "в работе", "M-Master", false, true},
		{"в работе у другого", "в работе", "luna:1104-x", false, false},
		{"в работе без исполнителя", "в работе", "", false, false},
		{"ждёт, срок наступил", "ждёт", "", true, true},
		{"ждёт, срок не наступил", "ждёт", "", false, false},
	}
	for _, c := range cases {
		w := newWorld(ModeOn)
		w.now = at("2026-11-04T07:01:00+01:00")
		w.free["m-master"] = true
		w.open = []string{"LOW-DO"}
		s := seeded(w)
		low := tick("LOW", c.status)
		low.Diff, low.Executor, low.WaitDue = "DO", c.executor, c.due
		if got := has(step(s, []Ticket{low}, w), KindMorning, "m-master"); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}

// Проект вошёл в live в тот же день, мастер мёртв: строка админу из журнала не заменяет настоящую, пока HIGH моложе 4 ч.
func TestHighAdminLogThenLive(t *testing.T) {
	w := newWorld(ModeLog)
	w.live["m-master"] = false
	s := seeded(w)
	ts := []Ticket{tick("HIGH", "открыт")}
	if ns := step(s, ts, w); !has(ns, KindHighAdmin, "git-admin") {
		t.Fatalf("журнал: %+v", ns)
	}
	w.mode = ModeOn
	w.now = w.now.Add(time.Hour)
	if ns := step(s, ts, w); !has(ns, KindHighAdmin, "git-admin") {
		t.Fatalf("вошёл в live, HIGH 1 ч: %+v", ns)
	}
	if ns := step(s, ts, w); has(ns, KindHighAdmin, "git-admin") {
		t.Fatalf("боевая один раз: %+v", ns)
	}
}

// Старше 4 ч — учтён, вход в live строку админу не поднимает.
func TestHighAdminLogOldStaysCounted(t *testing.T) {
	w := newWorld(ModeLog)
	w.live["m-master"] = false
	s := seeded(w)
	ts := []Ticket{tick("HIGH", "открыт")}
	step(s, ts, w)
	w.mode = ModeOn
	w.now = w.now.Add(5 * time.Hour)
	if ns := step(s, ts, w); has(ns, KindHighAdmin, "git-admin") {
		t.Fatalf("%+v", ns)
	}
}

func historyFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "history.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Строка учётки без срока сброса или расхода — сломанный вход: иначе учётка считается сброшенной, свободна на 100 %, запас 99.
func TestReadHistoryIncompleteRow(t *testing.T) {
	for name, line := range map[string]string{
		"нет сброса":  `{"at":"2026-11-04T10:00:00+01:00","sevenDayUsedPercentage":95,"account":"a"}`,
		"сброс null":  `{"at":"2026-11-04T10:00:00+01:00","sevenDayResetsAt":null,"sevenDayUsedPercentage":95,"account":"a"}`,
		"расход null": `{"at":"2026-11-04T10:00:00+01:00","sevenDayResetsAt":"2026-11-07T10:00:00+01:00","sevenDayUsedPercentage":null,"account":"a"}`,
		"нет времени": `{"sevenDayResetsAt":"2026-11-07T10:00:00+01:00","sevenDayUsedPercentage":95,"account":"a"}`,
	} {
		if _, err := ReadHistory(historyFile(t, line+"\n")); err == nil {
			t.Errorf("%s: прочитано без ошибки", name)
		}
	}
}

// Строки без учётки в живом файле есть (33 на 04.11, account null): они не про запас, их пропускаем и разбор не падает.
func TestReadHistorySkipsNoAccount(t *testing.T) {
	body := `{"at":"2026-11-04T09:00:00+01:00","sevenDayUsedPercentage":10,"account":null}
{"at":"2026-11-04T09:30:00+01:00","sevenDayUsedPercentage":null,"sevenDayResetsAt":null,"account":null}
{"at":"2026-11-04T10:00:00+01:00","sevenDayResetsAt":"2026-11-07T10:00:00+01:00","sevenDayUsedPercentage":20,"account":"a"}
`
	rows, err := ReadHistory(historyFile(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Account != "a" {
		t.Fatalf("%+v", rows)
	}
}

// История меньше суток считается по учётке: файл старый, новая учётка часовая — данных нет, а не расход, размазанный на 72 ч.
func TestMeasureShortNewAccount(t *testing.T) {
	now := at("2026-11-04T10:00:00+01:00")
	resets := now.Add(72 * time.Hour)
	rows := []Row{
		{At: now.Add(-72 * time.Hour), ResetsAt: resets, Used: 0, Account: "old"},
		{At: now.Add(-time.Minute), ResetsAt: resets, Used: 20, Account: "old"},
		{At: now.Add(-time.Hour), ResetsAt: resets, Used: 0, Account: "new"},
		{At: now.Add(-time.Minute), ResetsAt: resets, Used: 30, Account: "new"},
	}
	if got := MeasureShort(rows, []string{"old", "new"}, now); got != nil {
		t.Fatalf("новая учётка: %+v", *got)
	}
	rows[2].At = now.Add(-25 * time.Hour)
	if got := MeasureShort(rows, []string{"old", "new"}, now); got == nil {
		t.Fatal("сутки истории есть, данных нет")
	}
}
