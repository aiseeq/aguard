package queue

import (
	"strings"
	"testing"
	"time"
)

func countKind(ns []Notice, kind, to string) int {
	c := 0
	for _, n := range ns {
		if n.Kind == kind && n.To == to {
			c++
		}
	}
	return c
}

func copies(ns []Notice) []Notice {
	var out []Notice
	for _, n := range ns {
		if n.Kind == KindCritCopy {
			out = append(out, n)
		}
	}
	return out
}

func TestCritCopyWaitsUntilMasterDoesNotTake(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	ticket := tick("CRIT", "открыт")
	if ns := step(s, []Ticket{ticket}, w); !has(ns, KindCrit, "m-master") || has(ns, KindCritCopy, "lead") {
		t.Fatalf("первый проход: %+v", ns)
	}
	w.now = w.now.Add(14 * time.Minute)
	if ns := step(s, []Ticket{ticket}, w); has(ns, KindCritCopy, "lead") {
		t.Fatalf("до срока: %+v", ns)
	}
	w.now = w.now.Add(time.Minute)
	ns := step(s, []Ticket{ticket}, w)
	if !has(ns, KindCritCopy, "lead") || !strings.Contains(ns[0].Line, "m-master не взял за 15 мин") {
		t.Fatalf("копия по сроку с пояснением: %+v", ns)
	}
	w.now = w.now.Add(time.Minute)
	if ns := step(s, []Ticket{ticket}, w); len(ns) != 0 {
		t.Fatalf("повторная копия: %+v", ns)
	}
}

func TestCritCopySuppressedWhenMasterTakes(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	ticket := tick("CRIT", "открыт")
	step(s, []Ticket{ticket}, w)
	w.now = w.now.Add(5 * time.Minute)
	ticket.Status = "в работе"
	if ns := step(s, []Ticket{ticket}, w); len(ns) != 0 {
		t.Fatalf("мастер взял: %+v", ns)
	}
	w.now = w.now.Add(time.Hour)
	if ns := step(s, []Ticket{ticket}, w); len(ns) != 0 {
		t.Fatalf("после срока взятого тикета: %+v", ns)
	}
}

func TestCritCopyMasterNoteDoesNotCountAsTaking(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	ticket := tick("CRIT", "открыт")
	step(s, []Ticket{ticket}, w)
	w.now = w.now.Add(15 * time.Minute)
	ticket.MasterNote = w.now.Add(-time.Minute)
	if ns := step(s, []Ticket{ticket}, w); !has(ns, KindCritCopy, "lead") {
		t.Fatalf("note без Started не отменяет копию: %+v", ns)
	}
}

func TestCritCopyTakenAndNoteMinuteBoundaries(t *testing.T) {
	t.Run("status в работе", func(t *testing.T) {
		w := newWorld(ModeOn)
		s := seeded(w)
		ticket := tick("CRIT", "открыт")
		step(s, []Ticket{ticket}, w)
		w.now = w.now.Add(5 * time.Minute)
		ticket.Status = "в работе"
		if ns := step(s, []Ticket{ticket}, w); has(ns, KindCritCopy, "lead") {
			t.Fatalf("взятый статусом тикет: %+v", ns)
		}
		w.now = w.now.Add(30 * time.Minute)
		if ns := step(s, []Ticket{ticket}, w); has(ns, KindCritCopy, "lead") {
			t.Fatalf("в работе через 30 минут: %+v", ns)
		}
	})
	t.Run("Started", func(t *testing.T) {
		w := newWorld(ModeOn)
		s := seeded(w)
		ticket := tick("CRIT", "открыт")
		ticket.External, ticket.Started = true, true
		step(s, []Ticket{ticket}, w)
		w.now = w.now.Add(30 * time.Minute)
		if ns := step(s, []Ticket{ticket}, w); has(ns, KindCritCopy, "lead") {
			t.Fatalf("Started: %+v", ns)
		}
	})
	t.Run("note в ту же минуту", func(t *testing.T) {
		w := newWorld(ModeOn)
		w.now = w.now.Add(52 * time.Second)
		s := seeded(w)
		ticket := tick("CRIT", "открыт")
		step(s, []Ticket{ticket}, w)
		ticket.MasterNote = at("2026-11-04T10:00:00+01:00")
		w.now = w.now.Add(15 * time.Minute)
		if ns := step(s, []Ticket{ticket}, w); !has(ns, KindCritCopy, "lead") {
			t.Fatalf("note в минуту уведомления без Started не отменяет копию: %+v", ns)
		}
	})
	t.Run("старая note", func(t *testing.T) {
		w := newWorld(ModeOn)
		s := seeded(w)
		ticket := tick("CRIT", "открыт")
		ticket.MasterNote = w.now.Add(-time.Hour)
		step(s, []Ticket{ticket}, w)
		w.now = w.now.Add(15 * time.Minute)
		if ns := step(s, []Ticket{ticket}, w); !has(ns, KindCritCopy, "lead") {
			t.Fatalf("note до уведомления не мешает копии: %+v", ns)
		}
	})
}

func TestCritCopyToLeadImmediateWhenMasterUnavailable(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	w.live["m-master"] = false
	ns := step(s, []Ticket{tick("CRIT", "открыт")}, w)
	if !has(ns, KindCrit, "git-admin") || !has(ns, KindCritCopy, "lead") {
		t.Fatalf("резервная доставка сразу: %+v", ns)
	}
}

func TestCritCopyWhenMasterDiesAndLeadIsFallback(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	ticket := tick("CRIT", "открыт")
	step(s, []Ticket{ticket}, w)
	w.now = w.now.Add(5 * time.Minute)
	w.live["m-master"], w.live["git-admin"] = false, false
	ns := step(s, []Ticket{ticket}, w)
	if len(ns) != 1 || ns[0].Kind != KindCritCopy || ns[0].To != "lead" || !strings.Contains(ns[0].Line, "мастер m-master недоступен") {
		t.Fatalf("копия без дубля CRIT лиду: %+v", ns)
	}
	w.now = w.now.Add(time.Minute)
	if ns := step(s, []Ticket{ticket}, w); len(ns) != 0 {
		t.Fatalf("повтор после смерти мастера: %+v", ns)
	}
}

func TestCritCopyToAdminIsImmediateAndNotDelayedAfterRevival(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	w.live["m-master"] = false
	ticket := tick("CRIT", "открыт")
	ns := step(s, []Ticket{ticket}, w)
	if !has(ns, KindCrit, "git-admin") || !has(ns, KindCritCopy, "lead") {
		t.Fatalf("админ получил CRIT и лид копию сразу: %+v", ns)
	}
	for _, n := range ns {
		if n.Kind == KindCritCopy && strings.Contains(n.Line, "не взял") {
			t.Fatalf("админу ушло исходное CRIT: %+v", n)
		}
	}
	w.now = w.now.Add(20 * time.Minute)
	w.live["m-master"] = true
	if ns := step(s, []Ticket{ticket}, w); has(ns, KindCritCopy, "lead") {
		t.Fatalf("повторная копия после оживления мастера: %+v", ns)
	}
}

func TestCritCopyAfterUnconfirmedMasterNotice(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	ticket := tick("CRIT", "открыт")
	if ns := s.Step([]Ticket{ticket}, w.view()); !has(ns, KindCrit, "m-master") || has(ns, KindCritCopy, "lead") {
		t.Fatalf("первое письмо мастеру без Confirm: %+v", ns)
	}
	w.now = w.now.Add(14 * time.Minute)
	if ns := s.Step([]Ticket{ticket}, w.view()); has(ns, KindCritCopy, "lead") {
		t.Fatalf("до срока от Seen: %+v", ns)
	}
	w.now = w.now.Add(time.Minute)
	ns := s.Step([]Ticket{ticket}, w.view())
	if !has(ns, KindCritCopy, "lead") {
		t.Fatalf("копия по сроку от Seen: %+v", ns)
	}
	for _, n := range ns {
		if n.Kind == KindCritCopy && !strings.Contains(n.Line, "письмо мастеру m-master не дошло за 15 мин") {
			t.Fatalf("текст о недошедшем письме: %+v", n)
		}
	}
	for _, n := range ns {
		if n.Kind == KindCritCopy {
			s.Confirm(n, w.view())
		}
	}
	w.now = w.now.Add(time.Minute)
	if ns := s.Step([]Ticket{ticket}, w.view()); has(ns, KindCritCopy, "lead") {
		t.Fatalf("вторая копия: %+v", ns)
	}
}

func TestCritCopyLogToOnStartsAtRealNotice(t *testing.T) {
	w := newWorld(ModeLog)
	s := seeded(w)
	ticket := tick("CRIT", "открыт")
	step(s, []Ticket{ticket}, w)
	w.now = w.now.Add(15 * time.Minute)
	step(s, []Ticket{ticket}, w)
	w.mode = ModeOn
	w.now = w.now.Add(time.Minute)
	ns := step(s, []Ticket{ticket}, w)
	if !has(ns, KindCrit, "m-master") || has(ns, KindCritCopy, "lead") {
		t.Fatalf("боевое письмо мастеру без копии в том же проходе: %+v", ns)
	}
	w.now = w.now.Add(14 * time.Minute)
	if ns := step(s, []Ticket{ticket}, w); has(ns, KindCritCopy, "lead") {
		t.Fatalf("до срока боевого письма: %+v", ns)
	}
	w.now = w.now.Add(time.Minute)
	if ns := step(s, []Ticket{ticket}, w); !has(ns, KindCritCopy, "lead") {
		t.Fatalf("после срока боевого письма: %+v", ns)
	}
}

func TestReviewHighRaisedToCrit(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	ticket := tick("HIGH", "открыт")
	ticket.Created = w.now
	step(s, []Ticket{ticket}, w)
	w.now = w.now.Add(time.Hour)
	ticket.Prio = "CRIT"
	ns := step(s, []Ticket{ticket}, w)
	if has(ns, KindCrit, "m-master") && has(ns, KindCritCopy, "lead") {
		t.Fatalf("первый CRIT после повышения приоритета: %+v", ns)
	}
	if !has(ns, KindCrit, "m-master") || has(ns, KindCritCopy, "lead") {
		t.Fatalf("отсчёт должен начаться с первой попытки CRIT: %+v", ns)
	}
}

func TestReviewReopenedCrit(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	ticket := tick("CRIT", "в работе")
	step(s, []Ticket{ticket}, w)
	w.now = w.now.Add(time.Hour)
	ticket.Status = "открыт"
	ns := step(s, []Ticket{ticket}, w)
	if has(ns, KindCrit, "m-master") && has(ns, KindCritCopy, "lead") {
		t.Fatalf("переоткрытый тикет получил копию со старого Seen: %+v", ns)
	}
}

func TestCritCopyReopenedStartsNewEpisode(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	ticket := tick("CRIT", "открыт")
	step(s, []Ticket{ticket}, w)
	ticket.Status = "в работе"
	step(s, []Ticket{ticket}, w)
	w.now = w.now.Add(time.Hour)
	ticket.Status = "открыт"
	ns := step(s, []Ticket{ticket}, w)
	if !has(ns, KindCrit, "m-master") || has(ns, KindCritCopy, "lead") {
		t.Fatalf("повторное открытие начинает новый срок: %+v", ns)
	}
	w.now = w.now.Add(15 * time.Minute)
	if ns := step(s, []Ticket{ticket}, w); !has(ns, KindCritCopy, "lead") {
		t.Fatalf("копия по новому сроку: %+v", ns)
	}
}

func TestReviewLogThenOnSendFails(t *testing.T) {
	w := newWorld(ModeLog)
	w.live["git-admin"] = false
	s := seeded(w)
	ticket := tick("CRIT", "открыт")
	step(s, []Ticket{ticket}, w)
	w.mode = ModeOn
	var got []Notice
	for range 8 {
		w.now = w.now.Add(10 * time.Minute)
		ns := s.Step([]Ticket{ticket}, w.view())
		for _, n := range ns {
			if n.To == "lead" {
				s.Confirm(n, w.view())
			}
			if n.Kind == KindCritCopy {
				got = append(got, n)
			}
		}
	}
	if len(got) != 1 || !strings.Contains(got[0].Line, "письмо мастеру m-master не дошло") {
		t.Fatalf("копия после сбоя боевого письма: %+v", got)
	}
}

func TestReviewSendFailsMasterNoted(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	ticket := tick("CRIT", "открыт")
	s.Step([]Ticket{ticket}, w.view())
	ticket.MasterNote = w.now.Add(5 * time.Minute)
	w.now = w.now.Add(20 * time.Minute)
	if ns := s.Step([]Ticket{ticket}, w.view()); !has(ns, KindCritCopy, "lead") {
		t.Fatalf("note без Started не отменяет копию: %+v", ns)
	}
}

func TestReviewLogModeCopyLogged(t *testing.T) {
	w := newWorld(ModeLog)
	s := seeded(w)
	ticket := tick("CRIT", "открыт")
	step(s, []Ticket{ticket}, w)
	w.now = w.now.Add(15 * time.Minute)
	if ns := step(s, []Ticket{ticket}, w); !has(ns, KindCritCopy, "lead") {
		t.Fatalf("копия в log по сроку: %+v", ns)
	}
}

func TestReviewCritToLeadNoDup(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	ticket := tick("CRIT", "открыт")
	w.live["m-master"], w.live["git-admin"] = false, false
	for i := range 3 {
		ns := s.Step([]Ticket{ticket}, w.view())
		if has(ns, KindCritCopy, "lead") || !has(ns, KindCrit, "lead") {
			t.Fatalf("проход %d: %+v", i, ns)
		}
		w.now = w.now.Add(10 * time.Minute)
	}
}

func TestReviewAdminFirstThenMaster(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	ticket := tick("CRIT", "открыт")
	w.live["m-master"], w.live["lead"] = false, false
	step(s, []Ticket{ticket}, w)
	w.now = w.now.Add(20 * time.Minute)
	w.live["m-master"], w.live["lead"] = true, true
	ns := step(s, []Ticket{ticket}, w)
	copies := 0
	for _, n := range ns {
		if n.Kind == KindCritCopy {
			copies++
			if strings.Contains(n.Line, "не взял") {
				t.Fatalf("адресатом первого письма был админ: %+v", n)
			}
		}
	}
	if copies != 1 {
		t.Fatalf("немедленная копия после появления лида: %+v", ns)
	}
}

func TestCritCopyLegacyStateAndRestart(t *testing.T) {
	t.Run("без CritTo", func(t *testing.T) {
		w := newWorld(ModeOn)
		ticket := tick("CRIT", "открыт")
		legacy := State{Seeded: true, Tickets: map[string]*ticketState{
			ticket.Key: {Prio: "CRIT", Seen: w.now.Add(-time.Hour), Crit: mark{At: w.now.Add(-time.Hour), Mode: ModeOn}},
		}}
		path := t.TempDir() + "/legacy-state.json"
		if err := legacy.Save(path); err != nil {
			t.Fatal(err)
		}
		legacy, err := LoadState(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := legacy.Tickets[ticket.Key].CritTo; got != "" {
			t.Fatalf("старое состояние должно оставить CritTo пустым: %q", got)
		}
		ns := step(&legacy, []Ticket{ticket}, w)
		if !has(ns, KindCritCopy, "lead") {
			t.Fatalf("старое состояние без CritTo отправляется сразу: %+v", ns)
		}
		if strings.Contains(ns[0].Line, "не взял") || strings.Contains(ns[0].Line, "недоступен") {
			t.Fatalf("для старого состояния нет оснований утверждать маршрут/причину: %+v", ns)
		}
	})
	t.Run("после перезапуска", func(t *testing.T) {
		w := newWorld(ModeOn)
		s := seeded(w)
		ticket := tick("CRIT", "открыт")
		step(s, []Ticket{ticket}, w)
		path := t.TempDir() + "/state.json"
		if err := s.Save(path); err != nil {
			t.Fatal(err)
		}
		s2, err := LoadState(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := s2.Tickets[ticket.Key].CritTo; got != ticket.Master {
			t.Fatalf("CritTo из state: %q", got)
		}
		w.now = w.now.Add(10 * time.Minute)
		if ns := step(&s2, []Ticket{ticket}, w); has(ns, KindCritCopy, "lead") {
			t.Fatalf("до срока после загрузки: %+v", ns)
		}
		w.now = w.now.Add(5 * time.Minute)
		if ns := step(&s2, []Ticket{ticket}, w); !has(ns, KindCritCopy, "lead") {
			t.Fatalf("по сроку после загрузки: %+v", ns)
		}
	})
}

func TestCritCopyAfterConfig(t *testing.T) {
	if got := Default("/h").CritCopyAfter.D(); got != 15*time.Minute {
		t.Fatalf("умолчание crit_copy_after: %s", got)
	}
	c := Default("/h")
	c.CritCopyAfter = 0
	if err := c.Validate("/h"); err == nil || !strings.Contains(err.Error(), "crit_copy_after") {
		t.Fatalf("ошибка конфига должна назвать crit_copy_after: %v", err)
	}
}

func TestRv2CritHighCrit(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	ticket := tick("CRIT", "открыт")
	step(s, []Ticket{ticket}, w)
	w.now = w.now.Add(time.Minute)
	ticket.Prio = "HIGH"
	step(s, []Ticket{ticket}, w)
	w.now = w.now.Add(time.Hour)
	ticket.Prio = "CRIT"
	ns := step(s, []Ticket{ticket}, w)
	if !has(ns, KindCrit, "m-master") || has(ns, KindCritCopy, "lead") {
		t.Fatalf("первый проход второго CRIT: %+v", ns)
	}
	w.now = w.now.Add(15 * time.Minute)
	if ns = step(s, []Ticket{ticket}, w); !has(ns, KindCritCopy, "lead") {
		t.Fatalf("копия второго эпизода: %+v", ns)
	}
}

func TestRv2RaisedOnce(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	ticket := tick("HIGH", "открыт")
	ticket.Created = w.now
	step(s, []Ticket{ticket}, w)
	ticket.Prio = "CRIT"
	var all []Notice
	for range 5 {
		w.now = w.now.Add(time.Minute)
		all = append(all, step(s, []Ticket{ticket}, w)...)
	}
	if n := countKind(all, KindCrit, "m-master"); n != 1 {
		t.Fatalf("CRIT мастеру после повышения: %d, %+v", n, all)
	}
}

func TestRv2TriedAdminThenMaster(t *testing.T) {
	w := newWorld(ModeOn)
	w.live["m-master"], w.live["lead"] = false, false
	s := seeded(w)
	ticket := tick("CRIT", "открыт")
	s.Step([]Ticket{ticket}, w.view())
	w.now = w.now.Add(10 * time.Minute)
	w.live["m-master"], w.live["lead"] = true, true
	var got []Notice
	for range 4 {
		ns := s.Step([]Ticket{ticket}, w.view())
		for _, n := range ns {
			if n.To == "lead" {
				s.Confirm(n, w.view())
			}
		}
		got = append(got, copies(ns)...)
		w.now = w.now.Add(10 * time.Minute)
	}
	if len(got) != 1 || !strings.Contains(got[0].Line, "не дошло за 20 мин") {
		t.Fatalf("копия по сроку от первой попытки мастеру: %+v", got)
	}
}

func TestRv2MasterReleased(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	ticket := tick("CRIT", "открыт")
	step(s, []Ticket{ticket}, w)
	w.now = w.now.Add(2 * time.Minute)
	ticket.Status = "в работе"
	ticket.MasterNote = at("2026-11-04T10:02:00+01:00")
	step(s, []Ticket{ticket}, w)
	w.now = w.now.Add(40 * time.Minute)
	ticket.Status = "открыт"
	ticket.MasterNote = at("2026-11-04T10:42:00+01:00")
	w.now = w.now.Add(20 * time.Second)
	if ns := step(s, []Ticket{ticket}, w); !has(ns, KindCrit, "m-master") || has(ns, KindCritCopy, "lead") {
		t.Fatalf("повтор CRIT после release: %+v", ns)
	}
	w.now = w.now.Add(15 * time.Minute)
	if ns := step(s, []Ticket{ticket}, w); !has(ns, KindCritCopy, "lead") {
		t.Fatalf("копия через срок после повторного CRIT: %+v", ns)
	}
}

func TestCritCopyLogThenOnMasterFailsAdminDead(t *testing.T) {
	w := newWorld(ModeLog)
	w.live["git-admin"] = false
	s := seeded(w)
	ticket := tick("CRIT", "открыт")
	step(s, []Ticket{ticket}, w)
	w.mode = ModeOn
	var got []Notice
	for range 8 {
		w.now = w.now.Add(10 * time.Minute)
		ns := s.Step([]Ticket{ticket}, w.view())
		for _, n := range ns {
			if n.To == "lead" {
				s.Confirm(n, w.view())
			}
			if n.Kind == KindCritCopy {
				got = append(got, n)
			}
		}
	}
	if len(got) != 1 {
		t.Fatalf("копия после log → on и сбоя отправки мастеру: %+v", got)
	}
}

func TestCritCopyLogThenOnMasterDeadAdminFails(t *testing.T) {
	w := newWorld(ModeLog)
	s := seeded(w)
	ticket := tick("CRIT", "открыт")
	step(s, []Ticket{ticket}, w)
	w.mode = ModeOn
	w.live["m-master"] = false
	var got []Notice
	for range 6 {
		w.now = w.now.Add(10 * time.Minute)
		ns := s.Step([]Ticket{ticket}, w.view())
		for _, n := range ns {
			if n.To == "lead" {
				s.Confirm(n, w.view())
			}
		}
		got = append(got, copies(ns)...)
	}
	if len(got) == 0 {
		t.Errorf("log → on, CRIT админу не доходит 60 мин: копии лиду нет")
	}
}

func TestCritCopyStartedSendFails(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	ticket := tick("CRIT", "открыт")
	ticket.External, ticket.Started = true, true
	for range 4 {
		if ns := s.Step([]Ticket{ticket}, w.view()); has(ns, KindCritCopy, "lead") {
			t.Fatalf("Started, сбой письма мастеру: %+v", ns)
		}
		w.now = w.now.Add(10 * time.Minute)
	}
}
