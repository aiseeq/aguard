package queue

import (
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type wakeWorld struct {
	view    View
	state   State
	ts      []Ticket
	first   Ticket
	found   bool
	err     error
	calls   int
	turn    bool
	session string
}

func newWakeWorld() *wakeWorld {
	w := &wakeWorld{state: State{Seeded: true}, found: true, session: "session-1"}
	w.ts = []Ticket{{Key: "p/base", Project: "p", Dir: "/p", Master: "master", Admin: "admin", Prio: "MED", Status: "открыт"}}
	w.first = w.ts[0]
	w.first.Key = "p/first"
	w.view = View{Now: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC), Lead: "lead", Serious: func(string) bool { return true }, Alive: func(string) bool { return true }, Free: func(string) bool { return true }, WakeFree: func(string) bool { return true }}
	w.view.Log = slog.New(slog.DiscardHandler)
	w.view.Next = func(string, string) (Ticket, bool, error) { w.calls++; return w.first, w.found, w.err }
	w.view.Session = func(string) string { return w.session }
	w.view.TurnAfter = func(string, time.Time) bool { return w.turn }
	return w
}
func (w *wakeWorld) step() []Notice { return w.state.Step(w.ts, w.view) }
func (w *wakeWorld) confirm(ns []Notice) {
	for _, n := range ns {
		w.state.Confirm(n, w.view)
	}
}
func TestWakeFreeMasterWithWork(t *testing.T) {
	w := newWakeWorld()
	ns := w.step()
	if !has(ns, KindWake, "master") || len(w.state.Wakes) != 0 {
		t.Fatalf("письмо и состояние: %+v %+v", ns, w.state.Wakes)
	}
	w.confirm(ns)
	if w.state.Wakes["master"].Ticket != "p/first" {
		t.Fatal(w.state.Wakes)
	}
}
func TestWakeBusyMasterSkipped(t *testing.T) {
	w := newWakeWorld()
	w.view.Free = func(string) bool { return false }
	if ns := w.step(); len(ns) != 0 || w.calls != 0 {
		t.Fatalf("%+v вызовов %d", ns, w.calls)
	}
}
func TestWakeNotWakeFreeSkipped(t *testing.T) {
	w := newWakeWorld()
	w.view.WakeFree = func(string) bool { return false }
	if ns := w.step(); len(ns) != 0 || w.calls != 0 {
		t.Fatalf("%+v вызовов %d", ns, w.calls)
	}
}

// Первым стоит тикет, взятый другим исполнителем (бойцом): будить не о чем.
func TestWakeFirstTakenByOtherSkipped(t *testing.T) {
	w := newWakeWorld()
	w.first.Status, w.first.Executor = "в работе", "worker-1"
	if ns := w.step(); len(ns) != 0 {
		t.Fatal(ns)
	}
	w.first.Executor = "master"
	if ns := w.step(); !has(ns, KindWake, "master") {
		t.Fatal("свой «в работе» будит", ns)
	}
}
func TestWakeEmptyOrErrorNoLetter(t *testing.T) {
	w := newWakeWorld()
	w.confirm(w.step())
	original := w.state.Wakes["master"]
	w.err, w.found = errors.New("ошибка"), false // как настоящий nextForQueue: found ложно, а отметку трогать нельзя
	if ns := w.step(); len(ns) != 0 || w.state.Wakes["master"] != original {
		t.Fatal(ns, w.state.Wakes)
	}
	w.err = nil
	if ns := w.step(); len(ns) != 0 || len(w.state.Wakes) != 0 {
		t.Fatal(ns, w.state.Wakes)
	}
}
func TestWakeSameFirstEscalatesOnce(t *testing.T) {
	w := newWakeWorld()
	w.confirm(w.step())
	w.turn = true
	ns := w.step()
	if len(ns) != 1 || !has(ns, KindWakeStuck, "admin") {
		t.Fatal(ns)
	}
	if !has(w.step(), KindWakeStuck, "admin") {
		t.Fatal("нет повтора при недоставке")
	}
	w.confirm(ns)
	if len(w.step()) != 0 || !w.state.Wakes["master"].Stuck {
		t.Fatal("сигнал повторился")
	}
}
func TestWakeNoTurnNoEscalation(t *testing.T) {
	w := newWakeWorld()
	w.confirm(w.step())
	if ns := w.step(); len(ns) != 0 {
		t.Fatal(ns)
	}
}
func TestWakeNewFirstLettersAgain(t *testing.T) {
	w := newWakeWorld()
	w.confirm(w.step())
	w.first.Key = "p/second"
	if ns := w.step(); !has(ns, KindWake, "master") {
		t.Fatal(ns)
	}
}
func TestWakeNewSessionLettersAgain(t *testing.T) {
	w := newWakeWorld()
	w.confirm(w.step())
	w.session = "session-2"
	w.turn = true
	if ns := w.step(); len(ns) != 1 || !has(ns, KindWake, "master") {
		t.Fatal(ns)
	}
}
func TestWakeDeadProjectNone(t *testing.T) {
	w := newWakeWorld()
	w.view.Alive = func(string) bool { return false }
	if ns := w.step(); len(ns) != 0 || len(w.state.Wakes) != 0 || w.calls != 0 {
		t.Fatal(ns)
	}
}
func TestWakeHighSameRoundOneLetter(t *testing.T) {
	w := newWakeWorld()
	w.ts[0].Prio = "HIGH"
	w.ts[0].Created = w.view.Now
	if ns := w.step(); !has(ns, KindHigh, "master") || has(ns, KindWake, "master") || w.calls != 0 {
		t.Fatal(ns)
	}
}
func TestWakeWeekendOnlyCrit(t *testing.T) {
	w := newWakeWorld()
	w.view.Now = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	w.view.WeekendCrit = []string{"/p"}
	if ns := w.step(); len(ns) != 0 {
		t.Fatal(ns)
	}
	w.first.Prio = "CRIT"
	if ns := w.step(); !has(ns, KindWake, "master") {
		t.Fatal(ns)
	}
}
func TestWakeLogModeThenLive(t *testing.T) {
	w := newWakeWorld()
	live := false
	w.view.Serious = func(string) bool { return live }
	ns := w.step()
	w.confirm(ns)
	if len(w.step()) != 0 {
		t.Fatal("повтор log")
	}
	live = true
	if ns = w.step(); !has(ns, KindWake, "master") || !ns[0].Serious {
		t.Fatal(ns)
	}
}
func TestWakeStatePersists(t *testing.T) {
	w := newWakeWorld()
	w.confirm(w.step())
	w.turn = true
	w.confirm(w.step())
	path := filepath.Join(t.TempDir(), "state.json")
	if err := w.state.Save(path); err != nil {
		t.Fatal(err)
	}
	s, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	w.state = s
	if !w.state.Wakes["master"].Stuck || len(w.step()) != 0 {
		t.Fatal(w.state.Wakes)
	}
}

// Письмо велит запустить tk next: спрашивать надо папку, где мастер его запустит.
func TestWakeAsksProjectOfMasterCwd(t *testing.T) {
	w := newWakeWorld()
	other := w.ts[0]
	other.Project, other.Dir, other.Key = "a", "/a", "a/base"
	w.ts = append([]Ticket{other}, w.ts...)
	var asked []string
	w.view.Next = func(_, dir string) (Ticket, bool, error) { asked = append(asked, dir); return w.first, true, nil }
	w.view.Cwd = func(string) string { return "/p/sub" }
	if ns := w.step(); !has(ns, KindWake, "master") || len(asked) != 1 || asked[0] != "/p" {
		t.Fatal(ns, asked)
	}
	asked = nil
	w.view.Cwd = func(string) string { return "/elsewhere" }
	if w.step(); len(asked) != 1 || asked[0] != "/a" {
		t.Fatal("вне проектов — устойчивый порядок", asked)
	}
}

func TestWakeBusyKinds(t *testing.T) {
	for _, kind := range []string{KindCrit, KindCritCopy, KindHigh, KindHighAdmin, KindWake, KindWakeStuck} {
		if !wakeBusy([]Notice{{To: "m", Kind: kind}})["m"] {
			t.Errorf("%s не будит", kind)
		}
	}
	if wakeBusy([]Notice{{To: "m", Kind: KindBack}})["m"] {
		t.Error("строка о закрытом — не повод молчать")
	}
}

// У настоящего ответа tk next поля Admin нет: сигнал идёт админу проекта, а не лиду.
func TestWakeStuckGoesToProjectAdmin(t *testing.T) {
	w := newWakeWorld()
	w.first.Admin = ""
	w.confirm(w.step())
	w.turn = true
	if ns := w.step(); len(ns) != 1 || !has(ns, KindWakeStuck, "admin") {
		t.Fatal(ns)
	}
}

// Доставлять некому (мастер и админ — один лид): строка в журнал один раз, а не каждый проход.
// Получатели вернулись — сигнал о вставшем мастере всё-таки уходит, один.
func TestWakeSignalAfterRecipientsReturn(t *testing.T) {
	w := newWakeWorld()
	back := false
	w.view.Alive = func(n string) bool { return n == "master" || back }
	w.confirm(w.step())
	w.turn = true
	ns := w.step()
	if len(ns) != 1 || ns[0].Kind != KindUnreachable {
		t.Fatal(ns)
	}
	path := filepath.Join(t.TempDir(), "state.json") // отметка переживает рестарт демона: строка не повторится
	if err := w.state.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	w.state = loaded
	if ns = w.step(); len(ns) != 0 {
		t.Fatal("строка после рестарта", ns)
	}
	back = true
	ns = w.step()
	if !has(ns, KindWakeStuck, "admin") {
		t.Fatal("вернувшемуся админу", ns)
	}
	w.confirm(ns)
	if ns = w.step(); len(ns) != 0 {
		t.Fatal("повтор", ns)
	}
}

// Проект известен только по папке: последний тикет исчез из tk ls, а будить мастера и админа всё равно надо.
func TestWakeProjectsWithoutListedTickets(t *testing.T) {
	w := newWakeWorld()
	projects := w.ts
	w.ts, w.view.Projects = nil, func() []Ticket { return projects }
	if ns := w.step(); !has(ns, KindWake, "master") {
		t.Fatal("мастеру", ns)
	}
	a := freezeWorld()
	projects = a.ts
	a.ts, a.view.Projects = nil, func() []Ticket { return projects }
	if ns := a.step(); !has(ns, KindWakeAdmin, "admin") {
		t.Fatal("админу", ns)
	}
}

func TestWakeNoOneToSignalOnce(t *testing.T) {
	w := newWakeWorld()
	w.ts[0].Master, w.ts[0].Admin, w.first.Admin = "lead", "lead", "lead"
	w.confirm(w.step())
	w.turn = true
	ns := w.step()
	if len(ns) != 1 || ns[0].Kind != KindUnreachable || !strings.Contains(ns[0].Line, "некому сообщить") {
		t.Fatal(ns)
	}
	if ns = w.step(); len(ns) != 0 {
		t.Fatal("повтор", ns)
	}
}

func TestWakeOwnInWorkAnyCase(t *testing.T) {
	w := newWakeWorld()
	w.first.Status, w.first.Executor = "в работе", "MASTER"
	if ns := w.step(); !has(ns, KindWake, "master") {
		t.Fatal(ns)
	}
}

func TestWakeMastersIndependent(t *testing.T) {
	w := newWakeWorld()
	other := w.ts[0]
	other.Project, other.Dir, other.Master, other.Key = "q", "/q", "other", "q/base"
	w.ts = append(w.ts, other)
	w.view.Next = func(name, dir string) (Ticket, bool, error) {
		return Ticket{Key: filepath.Base(dir) + "/first", Admin: "admin"}, name != "other" || w.found, nil
	}
	ns := w.step()
	if len(ns) != 2 || !has(ns, KindWake, "master") || !has(ns, KindWake, "other") {
		t.Fatal(ns)
	}
	w.confirm(ns)
	w.found = false
	w.step()
	if _, kept := w.state.Wakes["master"]; kept == false || len(w.state.Wakes) != 1 {
		t.Fatal("пустая очередь одного сняла отметку другого", w.state.Wakes)
	}
}
