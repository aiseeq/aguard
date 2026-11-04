package queue

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Приёмка тикета queue-order-guard: написана мастером до кода, боец её не правит.

var plus2 = time.FixedZone("+02", 2*3600)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestReplayElevenMornings(t *testing.T) {
	rows, err := ReadHistory("testdata/limits-history.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	accounts := Accounts(rows, time.Time{}, 0)
	if len(accounts) != 3 {
		t.Fatalf("учёток %d", len(accounts))
	}
	want := []float64{0.39, -0.18, -0.17, 0.46, 0.07, -0.25, -0.38, -0.08, 0.12, 0.18, 0.29}
	bands := map[Band]int{}
	for i, w := range want {
		now := time.Date(2026, 10, 24+i, 7, 0, 0, 0, plus2)
		s := Measure(rows, accounts, now)
		if math.Abs(s.Margin-w) > 0.006 {
			t.Errorf("%s запас %.3f, образец %.2f", now.Format("02.01"), s.Margin, w)
		}
		if s.Burning {
			t.Errorf("%s сгорающего на истории нет", now.Format("02.01"))
		}
		bands[BandOf(s.Margin)]++
	}
	if bands[BandFull] != 0 || bands[BandMid] != 6 || bands[BandLow] != 5 {
		t.Fatalf("утра %v, нужно 0, 6, 5", bands)
	}
}

func row(acc, when string, used float64, resets string) Row {
	return Row{At: at(when), ResetsAt: at(resets), Used: used, Account: acc}
}

func TestMeasureBurning(t *testing.T) {
	now := at("2026-11-03T12:00:00+01:00")
	rows := []Row{row("a", "2026-11-03T11:00:00+01:00", 69, "2026-11-04T10:00:00+01:00")}
	if s := Measure(rows, []string{"a"}, now); !s.Burning {
		t.Fatal("31 % остатка, до сброса 22 ч: сгорает")
	}
	rows[0].Used = 71
	if s := Measure(rows, []string{"a"}, now); s.Burning {
		t.Fatal("29 % остатка: не сгорает")
	}
	rows[0].Used, rows[0].ResetsAt = 50, at("2026-11-04T13:00:00+01:00")
	if s := Measure(rows, []string{"a"}, now); s.Burning {
		t.Fatal("до сброса больше суток: не сгорает")
	}
}

// Расход 0 при нулевом остатке: сессии упёрлись в потолок и встали, это не бесконечный запас.
func TestMeasureNoBurn(t *testing.T) {
	now := at("2026-11-03T12:00:00+01:00")
	empty := []Row{row("a", "2026-10-29T12:00:00+01:00", 100, "2026-11-05T12:00:00+01:00")}
	if s := Measure(empty, []string{"a"}, now); s.Margin >= 0 {
		t.Fatalf("остаток 0, сброс через 2 дня: запас %.2f, нужен < 0", s.Margin)
	}
	half := []Row{row("a", "2026-10-29T12:00:00+01:00", 50, "2026-11-05T12:00:00+01:00")}
	if s := Measure(half, []string{"a"}, now); s.Margin < 1 {
		t.Fatalf("есть остаток и нет расхода: запас %.2f, нужен >= 1", s.Margin)
	}
}

// Учётка без свежих строк выпадает: иначе брошенная подписка добавляет 100 % навсегда.
func TestAccountsWindow(t *testing.T) {
	now := at("2026-11-03T12:00:00+01:00")
	rows := []Row{
		row("old", "2026-10-01T12:00:00+01:00", 10, "2026-10-08T12:00:00+01:00"),
		row("new", "2026-11-01T12:00:00+01:00", 10, "2026-11-08T12:00:00+01:00"),
	}
	if got := Accounts(rows, now, 14*24*time.Hour); len(got) != 1 || got[0] != "new" {
		t.Fatalf("учётки %v", got)
	}
}

func TestMeasureShort(t *testing.T) {
	now := at("2026-11-03T12:00:00+01:00")
	young := []Row{row("w", "2026-11-03T02:00:00+01:00", 1, "2026-11-10T12:00:00+01:00"), row("w", "2026-11-03T11:50:00+01:00", 9, "2026-11-10T12:00:00+01:00")}
	if s := MeasureShort(young, []string{"w"}, now); s != nil {
		t.Fatal("меньше суток истории: запас неизвестен")
	}
	stale := []Row{row("w", "2026-11-01T02:00:00+01:00", 1, "2026-11-10T12:00:00+01:00"), row("w", "2026-11-03T10:00:00+01:00", 9, "2026-11-10T12:00:00+01:00")}
	if s := MeasureShort(stale, []string{"w"}, now); s != nil {
		t.Fatal("последний замер старше часа: запас неизвестен")
	}
	// 2 суток истории, расход 20 %: 10 %/сут, не 6,7 (делим на факт, а не на 3 суток). Остаток 80, сброс через 7 суток: 8−7=1.
	ok := []Row{row("w", "2026-11-01T12:00:00+01:00", 0, "2026-11-10T12:00:00+01:00"), row("w", "2026-11-03T11:50:00+01:00", 20, "2026-11-10T12:00:00+01:00")}
	s := MeasureShort(ok, []string{"w"}, now)
	if s == nil || math.Abs(s.Margin-1) > 0.02 {
		t.Fatalf("запас %+v, нужен 1", s)
	}
}

func table(t *testing.T) Table {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "deploy", "gate.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tb, err := ParseTable(f)
	if err != nil {
		t.Fatal(err)
	}
	return tb
}

func TestParseTableRejects(t *testing.T) {
	for _, bad := range []string{
		"MED-DO\tда\tда\n",                         // мало колонок
		"MED-DO\tда\tда\tможет\n",                  // чужое значение
		"HIGH-DO\tда\tда\tда\n",                    // HIGH в таблице нет
		"MED-XX\tда\tда\tда\n",                     // чужая пара
		"MED-DO\tда\tда\tда\nMED-DO\tда\tда\tда\n", // дубль
	} {
		if _, err := ParseTable(strings.NewReader(bad)); err == nil {
			t.Errorf("должно быть ошибкой: %q", bad)
		}
	}
}

func eq(a, b []string) bool {
	return strings.Join(a, ",") == strings.Join(b, ",")
}

func TestComputeBands(t *testing.T) {
	tb := table(t)
	now := at("2026-11-03T12:00:00+01:00")
	all := []string{"MED-DO", "MED-HUNT", "MED-ARCH", "LOW-DO", "LOW-HUNT", "LOW-ARCH"}
	mid := []string{"MED-DO", "MED-HUNT", "MED-ARCH", "LOW-DO"}
	do := []string{"MED-DO", "LOW-DO"}
	cases := []struct {
		name       string
		claude     Supply
		openai     *Supply
		open, lane []string
	}{
		{"от 1", Supply{Margin: 1}, nil, all, do},
		{"от 0 до 1", Supply{Margin: 0}, nil, mid, do},
		{"меньше 0", Supply{Margin: -0.1}, nil, nil, do},
		{"сгорает при минусе", Supply{Margin: -3, Burning: true}, nil, all, do},
		{"OpenAI от 0 до 1 при полном Claude: худшее", Supply{Margin: 5}, &Supply{Margin: 0.5}, mid, do},
		{"OpenAI минус при полном Claude", Supply{Margin: 5}, &Supply{Margin: -1}, nil, do},
		{"сгорает Claude, минус OpenAI: пара стоит", Supply{Margin: -3, Burning: true}, &Supply{Margin: -1}, nil, do},
	}
	for _, c := range cases {
		open, lane := Compute(tb, nil, c.claude, c.openai, now)
		if !eq(open, c.open) || !eq(lane, c.lane) {
			t.Errorf("%s: open %v lane %v", c.name, open, lane)
		}
	}
}

func TestOverride(t *testing.T) {
	tb := table(t)
	now := at("2026-11-03T12:00:00+01:00")
	ov, err := ParseOverrides(strings.NewReader(
		"# пара\tзначение\tдо\tкто\tпочему\n" +
			"LOW-HUNT\tда\t2026-11-06\tuser\tоткрой до пятницы\n" +
			"MED-DO\tнет\t2026-11-01\tlead\tистёкшая\n" +
			"MED-ARCH\tбез мастера\t2026-11-06T18:00:00+01:00\tlead\tтолько дорожка\n"))
	if err != nil {
		t.Fatal(err)
	}
	open, lane := Compute(tb, ov, Supply{Margin: -1}, nil, now)
	if !eq(open, []string{"LOW-HUNT"}) {
		t.Errorf("open %v", open)
	}
	// HUNT и ARCH дорожка не берёт, даже при «без мастера»: только DO.
	if !eq(lane, []string{"MED-DO", "LOW-DO"}) {
		t.Errorf("lane %v", lane)
	}
	for _, bad := range []string{
		"HIGH-DO\tнет\t2026-11-06\tuser\tx\n",  // HIGH открыт всегда
		"MED-DO\tможет\t2026-11-06\tuser\tx\n", // чужое значение
		"MED-DO\tнет\tзавтра\tuser\tx\n",       // чужая дата
		"MED-DO\tнет\t2026-11-06\tuser\n",      // нет причины
	} {
		if _, err := ParseOverrides(strings.NewReader(bad)); err == nil {
			t.Errorf("должно быть ошибкой: %q", bad)
		}
	}
}

// Мир прохода: кто жив, кто свободен.
type world struct {
	live, free map[string]bool
	asked      map[string]int
	mode       string
	now        time.Time
	open       []string

	weekendCrit []string
}

func newWorld(mode string) *world {
	return &world{
		live: map[string]bool{"m-master": true, "git-admin": true, "lead": true}, free: map[string]bool{}, asked: map[string]int{},
		mode: mode, now: at("2026-11-04T10:00:00+01:00"),
	}
}

func (w *world) view() View {
	return View{
		Now: w.now, Lead: "lead", Open: w.open, Morning: 7 * time.Hour, WeekendCrit: w.weekendCrit,
		Serious: func(project string) bool { return w.mode == ModeOn },
		Alive:   func(n string) bool { return w.live[n] },
		Free:    func(n string) bool { w.asked[n]++; return w.free[n] },
	}
}

func tick(prio, status string) Ticket {
	return Ticket{
		Key: "m/20261104-0950-x", Project: "m", Slug: "x", Title: "заголовок", Prio: prio, Status: status,
		Master: "m-master", Admin: "git-admin", Created: at("2026-11-03T09:50:00+01:00"),
	}
}

// step — проход с подтверждением всех отправок.
func step(s *State, ts []Ticket, w *world) []Notice {
	ns := s.Step(ts, w.view())
	for _, n := range ns {
		s.Confirm(n, w.view())
	}
	return ns
}

func has(ns []Notice, kind, to string) bool {
	for _, n := range ns {
		if n.Kind == kind && n.To == to {
			return true
		}
	}
	return false
}

func TestSeedKeepsCrit(t *testing.T) {
	w := newWorld(ModeOn)
	s := &State{}
	old := tick("HIGH", "открыт")
	crit := tick("CRIT", "открыт")
	crit.Key = "m/20261104-0900-c"
	w.free["m-master"] = true
	ns := step(s, []Ticket{old, crit}, w)
	if !has(ns, KindCrit, "m-master") || !has(ns, KindCritCopy, "lead") {
		t.Fatalf("CRIT на первом проходе: %+v", ns)
	}
	if has(ns, KindHigh, "m-master") {
		t.Fatalf("старый HIGH засеян, письма нет: %+v", ns)
	}
	w.now = w.now.Add(5 * time.Hour)
	if ns := step(s, []Ticket{old, crit}, w); has(ns, KindHighAdmin, "git-admin") || has(ns, KindCrit, "m-master") {
		t.Fatalf("засеянный HIGH и отправленный CRIT молчат: %+v", ns)
	}
}

func seeded(w *world) *State {
	s := &State{}
	step(s, nil, w)
	return s
}

func TestCritOnce(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	ts := []Ticket{tick("CRIT", "открыт")}
	ns := step(s, ts, w) // занятому тоже: CRIT не ждёт
	if !has(ns, KindCrit, "m-master") || !has(ns, KindCritCopy, "lead") {
		t.Fatalf("%+v", ns)
	}
	if ns := step(s, ts, w); len(ns) != 0 {
		t.Fatalf("второй раз: %+v", ns)
	}
}

func TestCritTakenSilent(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	if ns := step(s, []Ticket{tick("CRIT", "в работе")}, w); len(ns) != 0 {
		t.Fatalf("взят: %+v", ns)
	}
}

func TestCritNoSession(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	w.live["m-master"] = false
	ns := step(s, []Ticket{tick("CRIT", "открыт")}, w)
	if !has(ns, KindCrit, "git-admin") || !has(ns, KindCritCopy, "lead") {
		t.Fatalf("%+v", ns)
	}
}

// Отправка не удалась: Confirm не вызван, в следующий проход снова.
func TestUnconfirmedRepeats(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	ts := []Ticket{tick("CRIT", "открыт")}
	if ns := s.Step(ts, w.view()); !has(ns, KindCrit, "m-master") {
		t.Fatalf("%+v", ns)
	}
	if ns := s.Step(ts, w.view()); !has(ns, KindCrit, "m-master") {
		t.Fatalf("без Confirm отметки нет: %+v", ns)
	}
}

func TestHighWaitsFree(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	ts := []Ticket{tick("HIGH", "открыт")}
	if ns := step(s, ts, w); len(ns) != 0 {
		t.Fatalf("мастер занят: %+v", ns)
	}
	w.now = w.now.Add(time.Hour)
	w.free["m-master"] = true
	if ns := step(s, ts, w); !has(ns, KindHigh, "m-master") {
		t.Fatalf("свободен: %+v", ns)
	}
	w.now = w.now.Add(time.Minute)
	if ns := step(s, ts, w); len(ns) != 0 {
		t.Fatalf("один раз: %+v", ns)
	}
	// Взят: 4 часа прошли, админу молчок.
	ts[0].Status = "в работе"
	w.now = w.now.Add(5 * time.Hour)
	if ns := step(s, ts, w); len(ns) != 0 {
		t.Fatalf("взятый: %+v", ns)
	}
}

func TestHighAdminAfter4h(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	ts := []Ticket{tick("HIGH", "открыт")}
	step(s, ts, w)
	w.now = w.now.Add(3*time.Hour + 59*time.Minute)
	if ns := step(s, ts, w); len(ns) != 0 {
		t.Fatalf("рано: %+v", ns)
	}
	w.now = w.now.Add(2 * time.Minute)
	if ns := step(s, ts, w); !has(ns, KindHighAdmin, "git-admin") {
		t.Fatalf("4 ч: %+v", ns)
	}
	w.now = w.now.Add(time.Hour)
	if ns := step(s, ts, w); len(ns) != 0 {
		t.Fatalf("админу один раз: %+v", ns)
	}
	// Сессия потом освободилась: своя строка мастеру всё равно одна.
	w.free["m-master"] = true
	if ns := step(s, ts, w); !has(ns, KindHigh, "m-master") {
		t.Fatalf("мастеру: %+v", ns)
	}
}

func TestHighNoSessionOnce(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	w.live["m-master"] = false
	ts := []Ticket{tick("HIGH", "открыт")}
	if ns := step(s, ts, w); !has(ns, KindHighAdmin, "git-admin") {
		t.Fatalf("%+v", ns)
	}
	w.now = w.now.Add(5 * time.Hour)
	if ns := step(s, ts, w); len(ns) != 0 {
		t.Fatalf("второй раз админу: %+v", ns)
	}
}

// Админ не жив: выше — лид. Мастер он же админ: эскалировать некуда, строка себе не дублируется.
func TestAdminChain(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	w.live["m-master"], w.live["git-admin"] = false, false
	if ns := step(s, []Ticket{tick("HIGH", "открыт")}, w); !has(ns, KindHighAdmin, "lead") {
		t.Fatalf("%+v", ns)
	}
	w = newWorld(ModeOn)
	s = seeded(w)
	own := tick("HIGH", "открыт")
	own.Master, own.Admin = "git-admin", "git-admin"
	step(s, []Ticket{own}, w)
	w.now = w.now.Add(5 * time.Hour)
	if ns := step(s, []Ticket{own}, w); has(ns, KindHighAdmin, "git-admin") {
		t.Fatalf("самому себе: %+v", ns)
	}
}

// Никого живого: отметки нет, следующий проход снова пробует; в журнал одна строка на ключ.
func TestNobodyAlive(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	w.live = map[string]bool{}
	ts := []Ticket{tick("CRIT", "открыт")}
	if ns := step(s, ts, w); !has(ns, KindUnreachable, "") {
		t.Fatalf("%+v", ns)
	}
	if ns := step(s, ts, w); len(ns) != 0 {
		t.Fatalf("журнал раз на ключ: %+v", ns)
	}
	w.live["lead"] = true
	if ns := step(s, ts, w); !has(ns, KindCrit, "lead") {
		t.Fatalf("лид поднялся: %+v", ns)
	}
}

func TestPrioRaisedIsNewHigh(t *testing.T) {
	w := newWorld(ModeOn)
	w.free["m-master"] = true
	s := &State{}
	med := tick("MED", "открыт")
	if ns := step(s, []Ticket{med}, w); len(ns) != 0 {
		t.Fatalf("MED молчит: %+v", ns)
	}
	high := med
	high.Prio = "HIGH"
	if ns := step(s, []Ticket{high}, w); !has(ns, KindHigh, "m-master") {
		t.Fatalf("поднят до HIGH: %+v", ns)
	}
	crit := med
	crit.Prio = "CRIT"
	if ns := step(s, []Ticket{crit}, w); !has(ns, KindCrit, "m-master") {
		t.Fatalf("поднят до CRIT: %+v", ns)
	}
}

func TestMedLowWakeNobody(t *testing.T) {
	w := newWorld(ModeOn)
	w.free["m-master"] = true
	s := seeded(w)
	w.now = at("2026-11-04T06:30:00+01:00")
	for _, p := range []string{"MED", "MEDIUM", "LOW"} {
		tk := tick(p, "открыт")
		tk.Key += p
		if ns := step(s, []Ticket{tk}, w); len(ns) != 0 {
			t.Fatalf("%s: %+v", p, ns)
		}
	}
}

// Закрытые тикеты «свободен» не спрашивают: чтение транскриптов — только когда есть что слать.
func TestFreeAskedOnlyForWork(t *testing.T) {
	w := newWorld(ModeOn)
	w.open = []string{"MED-DO"}
	s := seeded(w)
	w.asked = map[string]int{}
	step(s, []Ticket{tick("LOW", "готово"), tick("CRIT", "готово")}, w)
	if len(w.asked) != 0 {
		t.Fatalf("спрашивал: %v", w.asked)
	}
}

func TestMorning(t *testing.T) {
	w := newWorld(ModeOn)
	w.now = at("2026-11-04T06:50:00+01:00")
	w.free["m-master"] = true
	w.open = []string{"LOW-DO"}
	s := seeded(w)
	low := tick("LOW", "открыт")
	low.Diff = "DO"
	ts := []Ticket{low}
	if ns := step(s, ts, w); len(ns) != 0 {
		t.Fatalf("до утра: %+v", ns)
	}
	w.now = at("2026-11-04T07:01:00+01:00")
	w.free["m-master"] = false
	if ns := step(s, ts, w); len(ns) != 0 {
		t.Fatalf("занят: %+v", ns)
	}
	w.free["m-master"] = true
	ns := step(s, ts, w)
	if !has(ns, KindMorning, "m-master") {
		t.Fatalf("утро: %+v", ns)
	}
	if strings.Contains(strings.ToLower(ns[0].Line), "лимит") {
		t.Fatalf("о лимитах мастерам не пишем: %q", ns[0].Line)
	}
	if ns := step(s, ts, w); len(ns) != 0 {
		t.Fatalf("раз в день: %+v", ns)
	}
	w.now = w.now.Add(24 * time.Hour)
	if ns := step(s, ts, w); !has(ns, KindMorning, "m-master") {
		t.Fatalf("следующее утро: %+v", ns)
	}
}

func TestMorningPairs(t *testing.T) {
	cases := []struct {
		name       string
		prio, diff string
		open       []string
		want       bool
	}{
		{"пара закрыта", "LOW", "HUNT", []string{"LOW-DO"}, false},
		{"без DIFF, открыта одна пара его PRIO", "LOW", "", []string{"LOW-DO"}, false},
		{"без DIFF, открыты все пары его PRIO", "LOW", "", []string{"LOW-DO", "LOW-HUNT", "LOW-ARCH"}, true},
		{"MEDIUM без DIFF, открыты все пары", "MEDIUM", "", []string{"MED-DO", "MED-HUNT", "MED-ARCH"}, true},
		{"без DIFF, чужой PRIO", "LOW", "", []string{"MED-DO"}, false},
		{"MEDIUM читается как MED", "MEDIUM", "DO", []string{"MED-DO"}, true},
		{"всё закрыто", "MED", "DO", nil, false},
	}
	for _, c := range cases {
		w := newWorld(ModeOn)
		w.now = at("2026-11-04T07:01:00+01:00")
		w.free["m-master"] = true
		w.open = c.open
		s := seeded(w)
		tk := tick(c.prio, "открыт")
		tk.Diff = c.diff
		if got := has(step(s, []Ticket{tk}, w), KindMorning, "m-master"); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
	// Ждущий и закрытый не считаются.
	for _, st := range []string{"ждёт", "готово", "отклонён", "на проверке"} {
		w := newWorld(ModeOn)
		w.now = at("2026-11-04T07:01:00+01:00")
		w.free["m-master"] = true
		w.open = []string{"LOW-DO"}
		s := seeded(w)
		tk := tick("LOW", st)
		tk.Diff = "DO"
		if ns := step(s, []Ticket{tk}, w); has(ns, KindMorning, "m-master") {
			t.Errorf("статус %s: %+v", st, ns)
		}
	}
	// Живой сессии нет: строки нет, MED и LOW не будят админа.
	w := newWorld(ModeOn)
	w.now = at("2026-11-04T07:01:00+01:00")
	w.live["m-master"] = false
	w.open = []string{"LOW-DO"}
	s := seeded(w)
	tk := tick("LOW", "открыт")
	tk.Diff = "DO"
	if ns := step(s, []Ticket{tk}, w); len(ns) != 0 {
		t.Fatalf("нет сессии: %+v", ns)
	}
}

func TestBack(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	w.live["c-master"] = true
	tk := tick("MED", "открыт")
	tk.Back = "c-master"
	if ns := step(s, []Ticket{tk}, w); len(ns) != 0 {
		t.Fatalf("открыт: %+v", ns)
	}
	tk.Status = "готово"
	if ns := step(s, []Ticket{tk}, w); len(ns) != 0 {
		t.Fatalf("заказчик занят: %+v", ns)
	}
	w.free["c-master"] = true
	if ns := step(s, []Ticket{tk}, w); !has(ns, KindBack, "c-master") {
		t.Fatalf("свободен: %+v", ns)
	}
	if ns := step(s, []Ticket{tk}, w); len(ns) != 0 {
		t.Fatalf("один раз: %+v", ns)
	}
}

func TestBackExpires(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	tk := tick("MED", "готово")
	tk.Back = "gone-master"
	step(s, []Ticket{tk}, w)
	w.now = w.now.Add(7*24*time.Hour + time.Minute)
	if ns := step(s, []Ticket{tk}, w); !has(ns, KindBackLost, "") {
		t.Fatalf("срок вышел, строка в журнал: %+v", ns)
	}
	w.live["gone-master"], w.free["gone-master"] = true, true
	w.now = w.now.Add(time.Minute)
	if ns := step(s, []Ticket{tk}, w); len(ns) != 0 {
		t.Fatalf("после срока молчит: %+v", ns)
	}
}

// Закрытые до наблюдателя заказчику не идут.
func TestBackSeeded(t *testing.T) {
	w := newWorld(ModeOn)
	w.live["c-master"], w.free["c-master"] = true, true
	tk := tick("MED", "готово")
	tk.Back = "c-master"
	if ns := step(&State{}, []Ticket{tk}, w); len(ns) != 0 {
		t.Fatalf("%+v", ns)
	}
}

// Неделя журнала: отметка в log не считается доставкой. Включение on: открытый CRIT идёт; HIGH, который виден меньше 4 ч,
// идёт; виден дольше — учтён, и строка админу из журнала не повторяется.
func TestLogToOn(t *testing.T) {
	w := newWorld(ModeLog)
	w.free["m-master"] = true
	s := seeded(w)
	crit := tick("CRIT", "открыт")
	crit.Key = "m/crit"
	old := tick("HIGH", "открыт")
	old.Key = "m/old"
	if ns := step(s, []Ticket{crit, old}, w); !has(ns, KindCrit, "m-master") || !has(ns, KindHigh, "m-master") {
		t.Fatalf("log считает как боевой: %+v", ns)
	}
	if ns := step(s, []Ticket{crit, old}, w); len(ns) != 0 {
		t.Fatalf("в log тоже один раз: %+v", ns)
	}
	w.now = w.now.Add(5 * time.Hour)
	young := tick("HIGH", "открыт")
	young.Key = "m/young"
	step(s, []Ticket{crit, old, young}, w)

	w.mode = ModeOn
	w.now = w.now.Add(time.Hour)
	ns := step(s, []Ticket{crit, old, young}, w)
	byKey := map[string]int{}
	for _, n := range ns {
		byKey[n.Key]++
	}
	if byKey["m/crit"] == 0 {
		t.Fatalf("открытый CRIT после журнала: %+v", ns)
	}
	if byKey["m/old"] != 0 {
		t.Fatalf("HIGH, видный 6 ч, учтён: %+v", ns)
	}
	if byKey["m/young"] == 0 {
		t.Fatalf("HIGH, видный 1 ч, идёт: %+v", ns)
	}
	if ns := step(s, []Ticket{crit, old, young}, w); len(ns) != 0 {
		t.Fatalf("после боевой отправки молчит: %+v", ns)
	}
}

// Проба: glint всерьёз, остальные журнал. Ход решений один, различается только Serious у строки.
func TestLiveProjectsOnly(t *testing.T) {
	w := newWorld(ModeLog)
	w.free["m-master"], w.free["glint-master"] = true, true
	w.live["glint-master"] = true
	s := seeded(w)
	serious := func(project string) bool { return project == "glint" }
	view := w.view()
	view.Serious = serious
	other := tick("HIGH", "открыт")
	glint := tick("HIGH", "открыт")
	glint.Key, glint.Project, glint.Master = "glint/20261104-0950-x", "glint", "glint-master"
	ns := s.Step([]Ticket{other, glint}, view)
	if len(ns) != 2 {
		t.Fatalf("%+v", ns)
	}
	for _, n := range ns {
		if want := n.To == "glint-master"; n.Serious != want {
			t.Errorf("%s: Serious %v", n.To, n.Serious)
		}
		s.Confirm(n, view)
	}
	if ns := s.Step([]Ticket{other, glint}, view); len(ns) != 0 {
		t.Fatalf("один раз в обоих режимах: %+v", ns)
	}
	// Проект добавили в live: его HIGH, видный меньше 4 ч, идёт всерьёз; чужой остаётся журналом и не повторяется.
	view.Serious = func(project string) bool { return project == "glint" || project == "m" }
	view.Now = view.Now.Add(time.Hour)
	ns = s.Step([]Ticket{other, glint}, view)
	if len(ns) != 1 || ns[0].To != "m-master" || !ns[0].Serious {
		t.Fatalf("после расширения live: %+v", ns)
	}
}

// Копия лиду у CRIT идёт в режиме проекта тикета, не лида.
func TestCritCopyFollowsProject(t *testing.T) {
	w := newWorld(ModeLog)
	s := seeded(w)
	for _, n := range s.Step([]Ticket{tick("CRIT", "открыт")}, w.view()) {
		if n.Serious {
			t.Fatalf("вне live письма нет: %+v", n)
		}
	}
}

func TestStateRoundTrip(t *testing.T) {
	w := newWorld(ModeLog)
	s := seeded(w)
	step(s, []Ticket{tick("CRIT", "открыт")}, w)
	p := filepath.Join(t.TempDir(), "state.json")
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	back, err := LoadState(p)
	if err != nil {
		t.Fatal(err)
	}
	if ns := step(&back, []Ticket{tick("CRIT", "открыт")}, w); len(ns) != 0 {
		t.Fatalf("после загрузки: %+v", ns)
	}
}

func TestWriteGate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "gate.json")
	g := Gate{At: at("2026-11-03T12:00:00+01:00"), Live: []string{"glint", "graft"}, Claude: 0.29, Open: []string{"MED-DO"}, Lane: []string{"MED-DO"}}
	if err := g.Write(p); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(string(b), " ", "")
	for _, want := range []string{`"live":["glint","graft"]`, `"claude":0.29`, `"openai":null`, `"open":["MED-DO"]`, `"lane":["MED-DO"]`, `"at":"2026-11-03T12:00:00+01:00"`} {
		if !strings.Contains(s, want) {
			t.Errorf("нет %s в %s", want, s)
		}
	}
	if strings.Contains(s, `"mode"`) {
		t.Errorf("поля mode в договоре нет: %s", s)
	}
	empty := Gate{At: g.At, Claude: -1}
	if err := empty.Write(p); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"open":[]`, `"lane":[]`, `"live":[]`} {
		if !strings.Contains(strings.ReplaceAll(string(b), " ", ""), want) {
			t.Errorf("пустой список — [], не null: %s", b)
		}
	}
}

// Отпечаток: смена PRIO в имени при том же размере и времени видна.
func TestFingerprintRename(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "20261103-1200-HIGH-DO-api.md")
	if err := os.WriteFile(a, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	mt := at("2026-11-03T12:00:00+01:00")
	if err := os.Chtimes(a, mt, mt); err != nil {
		t.Fatal(err)
	}
	f1, err := Fingerprint([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	b := filepath.Join(dir, "20261103-1200-CRIT-DO-api.md")
	if err := os.Rename(a, b); err != nil {
		t.Fatal(err)
	}
	f2, err := Fingerprint([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if f1 == f2 {
		t.Fatal("переименование не видно")
	}
}

func TestNormPrio(t *testing.T) {
	for in, want := range map[string]string{"MEDIUM": "MED", "MED": "MED", "HIGH": "HIGH", "CRIT": "CRIT", "LOW": "LOW"} {
		if got := NormPrio(in); got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
}
