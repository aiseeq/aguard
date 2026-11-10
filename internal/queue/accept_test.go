package queue

import (
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

func row(acc, when string, used float64, resets string) Row {
	return Row{At: at(when), ResetsAt: at(resets), Used: used, Account: acc}
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

// Мир прохода: кто жив, кто свободен.
type world struct {
	live, free map[string]bool
	asked      map[string]int
	mode       string
	now        time.Time

	rest Rest
}

func newWorld(mode string) *world {
	return &world{
		live: map[string]bool{"m-master": true, "git-admin": true, "lead": true}, free: map[string]bool{}, asked: map[string]int{},
		mode: mode, now: at("2026-11-04T10:00:00+01:00"),
	}
}

func (w *world) view() View {
	return View{
		Now: w.now, Lead: "lead", Rest: w.rest,
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
	s := seeded(w)
	w.asked = map[string]int{}
	step(s, []Ticket{tick("LOW", "готово"), tick("CRIT", "готово")}, w)
	if len(w.asked) != 0 {
		t.Fatalf("спрашивал: %v", w.asked)
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
	g := Gate{At: at("2026-11-03T12:00:00+01:00"), Live: []string{"glint", "graft"}, Open: []string{"MED-DO"}, Lane: []string{"MED-DO"}}
	if err := g.Write(p); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(string(b), " ", "")
	for _, want := range []string{`"live":["glint","graft"]`, `"open":["MED-DO"]`, `"lane":["MED-DO"]`, `"at":"2026-11-03T12:00:00+01:00"`} {
		if !strings.Contains(s, want) {
			t.Errorf("нет %s в %s", want, s)
		}
	}
	for _, gone := range []string{`"mode"`, `"claude"`, `"openai"`} { // запаса в договоре нет: ворота от него не зависят
		if strings.Contains(s, gone) {
			t.Errorf("поля %s в договоре нет: %s", gone, s)
		}
	}
	empty := Gate{At: g.At}
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
