package queue

import (
	"testing"
	"time"
)

func TestBootGraceForMissingMasterNotice(t *testing.T) {
	w := newWorld(ModeOn)
	w.live["m-master"] = false
	w.BootAt = w.now.Add(-30 * time.Minute)
	w.BootGrace = time.Hour
	s := &State{}
	ts := []Ticket{freshHigh(w, time.Hour)}

	if ns := s.Step(ts, w.view()); len(ns) != 0 {
		t.Fatalf("письмо об отсутствии мастера в льготный срок: %+v", ns)
	}
	w.now = w.BootAt.Add(w.BootGrace + time.Minute)
	if ns := s.Step(ts, w.view()); !has(ns, KindHighAdmin, "git-admin") {
		t.Fatalf("письмо после льготного срока: %+v", ns)
	}
}

func TestBootGraceDoesNotDelayOtherReason(t *testing.T) {
	w := newWorld(ModeOn)
	w.live["m-master"] = false
	w.BootAt = w.now.Add(-30 * time.Minute)
	w.BootGrace = time.Hour
	crit := tick("CRIT", "открыт")
	if ns := (&State{}).Step([]Ticket{crit}, w.view()); !has(ns, KindCrit, "git-admin") {
		t.Fatalf("CRIT отложен льготным сроком загрузки: %+v", ns)
	}
}
