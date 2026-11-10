package queue

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Приёмка, второй заход: дефекты первого ревью queue-order-guard. Написана мастером, боец её не правит.

// Письмо ушло админу вместо мёртвого мастера: следующий проход молчит, иначе письмо каждые 30 с.
func TestCritNoSessionOnce(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	w.live["m-master"] = false
	ts := []Ticket{tick("CRIT", "открыт")}
	step(s, ts, w)
	if ns := step(s, ts, w); len(ns) != 0 {
		t.Fatalf("второй проход: %+v", ns)
	}
	w.live["m-master"] = true
	if ns := step(s, ts, w); len(ns) != 0 {
		t.Fatalf("мастер поднялся: CRIT один раз, %+v", ns)
	}
}

// Мастеру ушло, копия лиду не ушла: повторяется только копия.
func TestCritCopyRetriedAlone(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	ts := []Ticket{tick("CRIT", "открыт")}
	for _, n := range s.Step(ts, w.view()) {
		if n.Kind == KindCrit {
			s.Confirm(n, w.view())
		}
	}
	ns := s.Step(ts, w.view())
	if len(ns) != 1 || ns[0].Kind != KindCritCopy {
		t.Fatalf("%+v", ns)
	}
}

// Мастер он же лид: копии нет.
func TestCritLeadNoCopy(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	own := tick("CRIT", "открыт")
	own.Master, own.Admin = "lead", "lead"
	ns := step(s, []Ticket{own}, w)
	if len(ns) != 1 || !has(ns, KindCrit, "lead") {
		t.Fatalf("%+v", ns)
	}
}

func TestHighAdminDeadOnce(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	w.live["m-master"], w.live["git-admin"] = false, false
	ts := []Ticket{tick("HIGH", "открыт")}
	if ns := step(s, ts, w); !has(ns, KindHighAdmin, "lead") {
		t.Fatalf("%+v", ns)
	}
	if ns := step(s, ts, w); len(ns) != 0 {
		t.Fatalf("лиду второй раз: %+v", ns)
	}
}

// Живых нет совсем: мёртвому письмо не адресуется, журнал один раз, поднялся лид — идёт ему.
func TestHighNobodyAlive(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	w.live = map[string]bool{}
	ts := []Ticket{tick("HIGH", "открыт")}
	ns := step(s, ts, w)
	if !has(ns, KindUnreachable, "") || len(ns) != 1 {
		t.Fatalf("%+v", ns)
	}
	if ns := step(s, ts, w); len(ns) != 0 {
		t.Fatalf("журнал один раз: %+v", ns)
	}
	w.live["lead"] = true
	if ns := step(s, ts, w); !has(ns, KindHighAdmin, "lead") {
		t.Fatalf("лид поднялся: %+v", ns)
	}
}

func TestBackSeededStaysSilent(t *testing.T) {
	w := newWorld(ModeOn)
	w.live["c-master"], w.free["c-master"] = true, true
	tk := tick("MED", "готово")
	tk.Back = "c-master"
	s := &State{}
	step(s, []Ticket{tk}, w)
	w.now = w.now.Add(time.Minute)
	if ns := step(s, []Ticket{tk}, w); len(ns) != 0 {
		t.Fatalf("закрытый до наблюдателя, второй проход: %+v", ns)
	}
}

// Закрытый тикет живёт в tk ls --all месяцами: уборка состояния не должна повторять строку заказчику.
func TestPruneDoesNotRepeatBack(t *testing.T) {
	w := newWorld(ModeOn)
	w.live["c-master"], w.free["c-master"] = true, true
	s := seeded(w)
	tk := tick("MED", "готово")
	tk.Back = "c-master"
	if ns := step(s, []Ticket{tk}, w); !has(ns, KindBack, "c-master") {
		t.Fatalf("%+v", ns)
	}
	w.now = w.now.Add(40 * 24 * time.Hour)
	for range 3 {
		if ns := step(s, []Ticket{tk}, w); len(ns) != 0 {
			t.Fatalf("через 40 суток: %+v", ns)
		}
	}
}

// Состояние — только то, что нужно однократности: иначе файл на сотни КБ переписывается на каждый проход.
func TestStateSmall(t *testing.T) {
	w := newWorld(ModeOn)
	w.free["m-master"] = true
	s := &State{}
	var ts []Ticket
	for i := range 2000 {
		tk := tick("MED", "готово")
		if i%2 == 0 {
			tk.Prio, tk.Status = "LOW", "открыт"
		}
		tk.Key = fmt.Sprintf("m/20261104-0950-x%d", i)
		ts = append(ts, tk)
	}
	step(s, ts, w)
	w.now = w.now.Add(time.Minute)
	step(s, ts, w)
	p := filepath.Join(t.TempDir(), "state.json")
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > 4096 {
		t.Fatalf("состояние %d байт при 2000 MED и LOW", fi.Size())
	}
}

func TestFingerprintCompact(t *testing.T) {
	dir := t.TempDir()
	for i := range 2000 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("20261103-1200-HIGH-DO-ticket-with-long-name-%d.md", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f, err := Fingerprint([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(f) > 128 {
		t.Fatalf("отпечаток %d байт: нужен хеш", len(f))
	}
}

// Тикет папки лида: мастер и админ — сам лид. HIGH не взят за 4 ч — напоминание живому лиду, а не «никто не доступен»;
// лид не в сети — журнал один раз (queue-lead-unreachable).
func TestHighLeadFolderNotUnreachable(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	lt := tick("HIGH", "открыт")
	lt.Master, lt.Admin = "lead", "lead"
	ts := []Ticket{lt}
	step(s, ts, w)
	w.now = w.now.Add(5 * time.Hour)
	ns := step(s, ts, w)
	if has(ns, KindUnreachable, "") || !has(ns, KindHighAdmin, "lead") {
		t.Fatalf("живой лид: %+v", ns)
	}
	w2 := newWorld(ModeOn)
	s2 := seeded(w2)
	w2.live["lead"] = false
	if ns := step(s2, ts, w2); !has(ns, KindUnreachable, "") || has(ns, KindHighAdmin, "lead") {
		t.Fatalf("лид не в сети: %+v", ns)
	}
}
