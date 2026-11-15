package queue

import (
	"strings"
	"testing"
	"time"
)

func cancelTicket() Ticket {
	t := tick("MED", "в работе")
	t.Executor = "executor-a"
	return t
}

func TestCancelNotifiesOtherAuthorOnceAndKeepsBackNotice(t *testing.T) {
	w := newWorld(ModeOn)
	w.live["executor-a"], w.free["executor-a"] = true, true
	w.free["git-admin"] = true
	s := seeded(w)
	tk := cancelTicket()
	step(s, []Ticket{tk}, w)
	tk.Status, tk.CancelAuthor, tk.CancelReason, tk.CancelJournalRead = "отклонён", "executor-b", "пользователь отменил задачу", true
	tk.Back = "git-admin"
	ns := step(s, []Ticket{tk}, w)
	var cancel, back bool
	for _, n := range ns {
		if n.Kind == KindCancel && n.To == "executor-a" {
			cancel = n.Line == "aguard: отменён m/x — пользователь отменил задачу; бойцов по нему остановить"
		}
		if n.Kind == KindBack && n.To == "git-admin" && strings.HasPrefix(n.Line, "aguard: закрыт m/x (отклонён)") {
			back = true
		}
	}
	if !cancel || !back {
		t.Fatalf("отмена исполнителю и закрытие заказчику: %+v", ns)
	}
	if ns := step(s, []Ticket{tk}, w); len(ns) != 0 {
		t.Fatalf("повтор после подтверждения: %+v", ns)
	}
}

func TestCancelByExecutorIsSilent(t *testing.T) {
	w := newWorld(ModeOn)
	w.live["executor-a"], w.free["executor-a"] = true, true
	s := seeded(w)
	tk := cancelTicket()
	step(s, []Ticket{tk}, w)
	tk.Status, tk.CancelAuthor, tk.CancelReason, tk.CancelJournalRead = "отклонён", "executor-a", "сам отказался", true
	if ns := step(s, []Ticket{tk}, w); has(ns, KindCancel, "executor-a") {
		t.Fatalf("исполнитель отменил сам: %+v", ns)
	}
}

func TestUnreadableCancelJournalSendsWithUnknownAuthor(t *testing.T) {
	w := newWorld(ModeOn)
	w.live["executor-a"], w.free["executor-a"] = true, true
	s := seeded(w)
	tk := cancelTicket()
	step(s, []Ticket{tk}, w)
	tk.Status, tk.CancelJournalRead = "отклонён", true // парсер сообщил WARN и оставил автора неизвестным
	ns := step(s, []Ticket{tk}, w)
	if len(ns) != 1 || ns[0].Kind != KindCancel || ns[0].To != "executor-a" || !strings.Contains(ns[0].Line, "причина не указана") {
		t.Fatalf("неизвестный автор и причина не должны блокировать письмо: %+v", ns)
	}
}

func TestCancelWaitsUntilJournalRead(t *testing.T) {
	w := newWorld(ModeOn)
	w.live["executor-a"], w.free["executor-a"] = true, true
	s := seeded(w)
	tk := cancelTicket()
	step(s, []Ticket{tk}, w)
	tk.Status = "отклонён"
	if ns := step(s, []Ticket{tk}, w); has(ns, KindCancel, "executor-a") {
		t.Fatalf("отмена до чтения журнала: %+v", ns)
	}
	tk.CancelJournalRead, tk.CancelAuthor = true, "executor-b"
	if ns := step(s, []Ticket{tk}, w); !has(ns, KindCancel, "executor-a") {
		t.Fatalf("отмена после чтения журнала: %+v", ns)
	}
}

func TestUnknownAuthorIsNotSameAsMissingRecipient(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	tk := cancelTicket()
	tk.Master = ""
	step(s, []Ticket{tk}, w)
	tk.Status, tk.CancelJournalRead = "отклонён", true
	step(s, []Ticket{tk}, w)
	w.now = w.now.Add(7 * 24 * time.Hour)
	if ns := step(s, []Ticket{tk}, w); !has(ns, KindCancelLost, "") {
		t.Fatalf("неизвестный автор при пустом получателе должен дойти до срока: %+v", ns)
	}
}

func TestTransferredByOtherAuthorNotifiesExecutor(t *testing.T) {
	w := newWorld(ModeOn)
	w.live["executor-a"], w.free["executor-a"] = true, true
	s := seeded(w)
	tk := cancelTicket()
	step(s, []Ticket{tk}, w)
	tk.Status, tk.CancelAuthor, tk.CancelReason, tk.CancelJournalRead = StatusTransferred, "executor-b", "перенос отменён пользователем", true
	if ns := step(s, []Ticket{tk}, w); !has(ns, KindCancel, "executor-a") {
		t.Fatalf("перенос чужой рукой: %+v", ns)
	}
}

func TestTransferredWithoutAuthorLogsOnceAndDoesNotEmail(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	tk := cancelTicket()
	step(s, []Ticket{tk}, w)
	tk.Status, tk.CancelJournalRead = StatusTransferred, true
	ns := step(s, []Ticket{tk}, w)
	if len(ns) != 1 || ns[0].Kind != KindCancelUnknown || ns[0].To != "" {
		t.Fatalf("автор переноса неизвестен: %+v", ns)
	}
	if ns := step(s, []Ticket{tk}, w); len(ns) != 0 {
		t.Fatalf("строка об неизвестном авторе не повторяется: %+v", ns)
	}
}

func TestWorkingTicketFallsBackToProjectMaster(t *testing.T) {
	w := newWorld(ModeOn)
	w.live["executor-a"], w.free["executor-a"] = true, true
	s := seeded(w)
	tk := tick("LOW", "в работе")
	tk.Executor = ""
	tk.Master = "executor-a"
	step(s, []Ticket{tk}, w)
	tk.Status, tk.CancelAuthor, tk.CancelJournalRead = "отклонён", "executor-b", true
	if ns := step(s, []Ticket{tk}, w); !has(ns, KindCancel, "executor-a") {
		t.Fatalf("LOW без поля исполнителя запоминает мастера проекта: %+v", ns)
	}
}

func TestWaitingTicketTracksExecutor(t *testing.T) {
	w := newWorld(ModeOn)
	w.live["executor-a"], w.free["executor-a"] = true, true
	s := seeded(w)
	tk := cancelTicket()
	tk.Status = "ждёт"
	step(s, []Ticket{tk}, w)
	tk.Status, tk.CancelAuthor, tk.CancelJournalRead = "отклонён", "executor-b", true
	if ns := step(s, []Ticket{tk}, w); !has(ns, KindCancel, "executor-a") {
		t.Fatalf("отмена после «ждёт»: %+v", ns)
	}
}

func TestWaitingTicketWithoutExecutorDoesNotFallbackOrNotify(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	tk := tick("MED", "ждёт")
	tk.Master = "m-master"
	step(s, []Ticket{tk}, w)
	if st := s.Tickets[tk.Key]; st != nil {
		t.Fatalf("отложенный без исполнителя не должен создавать состояние: %+v", st)
	}
	tk.Status, tk.CancelAuthor, tk.CancelJournalRead = "отклонён", "lead", true
	if ns := step(s, []Ticket{tk}, w); has(ns, KindCancel, "m-master") {
		t.Fatalf("отмена тикета, который никто не взял: %+v", ns)
	}
}

func TestWaitingTicketWithoutExecutorIsSkippedInFrozenDeadProject(t *testing.T) {
	w := newWorld(ModeOn)
	w.live["m-master"] = false
	s := seeded(w)
	v := w.view()
	v.Frozen = func(string) (bool, error) { return true, nil }
	tk := tick("MED", "ждёт")
	tk.Dir = "/m"
	s.ProjectWakes[tk.Dir] = ProjectWake{Ticket: "m/old"}
	s.Wakes[tk.Master] = Wake{Ticket: "m/old"}
	active := map[string]bool{}
	st, eligible := s.prepareTicket(tk, v, true, active)
	if eligible || st != nil || s.Tickets[tk.Key] != nil || len(s.ProjectWakes) != 0 || len(s.Wakes) != 0 {
		t.Fatalf("отложенный без исполнителя в замороженном проекте не создаёт состояние: state=%+v eligible=%v", st, eligible)
	}
}

func TestDeadExecutorFallsBackToProjectMaster(t *testing.T) {
	w := newWorld(ModeOn)
	w.live["project-master"], w.free["project-master"] = true, true
	s := seeded(w)
	tk := cancelTicket()
	tk.Master, tk.Executor = "project-master", "dead-executor"
	step(s, []Ticket{tk}, w)
	w.live["dead-executor"] = false
	tk.Status, tk.CancelAuthor, tk.CancelJournalRead = "отклонён", "other-author", true
	if ns := step(s, []Ticket{tk}, w); !has(ns, KindCancel, "project-master") {
		t.Fatalf("мёртвый исполнитель: %+v", ns)
	}
}

func TestDeadExecutorDoesNotNotifyMasterWhoCancelled(t *testing.T) {
	w := newWorld(ModeOn)
	w.live["project-master"], w.free["project-master"] = true, true
	s := seeded(w)
	tk := cancelTicket()
	tk.Master, tk.Executor = "project-master", "dead-executor"
	step(s, []Ticket{tk}, w)
	w.live["dead-executor"] = false
	tk.Status, tk.CancelAuthor, tk.CancelJournalRead = "отклонён", "project-master", true
	if ns := step(s, []Ticket{tk}, w); has(ns, KindCancel, "project-master") {
		t.Fatalf("мастер сам отменил: %+v", ns)
	}
}

func TestCancelWaitsThroughProjectRest(t *testing.T) {
	w := newWorld(ModeOn)
	w.now = at("2026-11-07T10:00:00+01:00")
	w.rest = cyRest("/h/work")
	w.live["executor-a"], w.free["executor-a"] = true, true
	s := seeded(w)
	tk := cancelTicket()
	tk.Dir = "/h/work/project"
	step(s, []Ticket{tk}, w)
	tk.Status, tk.CancelAuthor, tk.CancelJournalRead = "отклонён", "executor-b", true
	if ns := step(s, []Ticket{tk}, w); has(ns, KindCancel, "executor-a") {
		t.Fatalf("в день отдыха письмо отложено: %+v", ns)
	}
	w.now = at("2026-11-09T10:00:00+01:00")
	if ns := step(s, []Ticket{tk}, w); !has(ns, KindCancel, "executor-a") {
		t.Fatalf("после дня отдыха письмо идёт: %+v", ns)
	}
}

func TestCancelWaitsForExecutorAndReportsExpiredDelivery(t *testing.T) {
	w := newWorld(ModeOn)
	w.live["executor-a"] = true
	s := seeded(w)
	tk := cancelTicket()
	step(s, []Ticket{tk}, w)
	tk.Status, tk.CancelAuthor, tk.CancelJournalRead = "отклонён", "executor-b", true
	if ns := step(s, []Ticket{tk}, w); has(ns, KindCancel, "executor-a") {
		t.Fatalf("занятому не отправлять: %+v", ns)
	}
	w.live["executor-a"], w.free["executor-a"] = true, true
	if ns := step(s, []Ticket{tk}, w); !has(ns, KindCancel, "executor-a") {
		t.Fatalf("дождаться свободного исполнителя: %+v", ns)
	}

	s2 := seeded(w)
	tk2 := cancelTicket()
	step(s2, []Ticket{tk2}, w)
	tk2.Status, tk2.CancelAuthor, tk2.CancelJournalRead = "отклонён", "executor-b", true
	w.live["executor-a"], w.free["executor-a"] = false, false
	step(s2, []Ticket{tk2}, w)
	w.now = w.now.Add(7 * 24 * time.Hour)
	if ns := step(s2, []Ticket{tk2}, w); !has(ns, KindCancelLost, "") {
		t.Fatalf("просроченная доставка должна попасть в журнал: %+v", ns)
	}
	if ns := step(s2, []Ticket{tk2}, w); has(ns, KindCancelLost, "") {
		t.Fatalf("просроченную доставку не повторять: %+v", ns)
	}
}

func TestCancelSeedDoesNotNotifyAlreadyClosedTicket(t *testing.T) {
	w := newWorld(ModeOn)
	w.free["git-admin"] = true
	s := &State{}
	tk := cancelTicket()
	tk.Status, tk.CancelAuthor, tk.CancelReason, tk.CancelJournalRead, tk.Back = "отклонён", "executor-b", "до запуска", true, "git-admin"
	if ns := step(s, []Ticket{tk}, w); len(ns) != 0 {
		t.Fatalf("закрытый до первого прохода засевается молча: %+v", ns)
	}
	if ns := step(s, []Ticket{tk}, w); len(ns) != 0 {
		t.Fatalf("посев закрытия сохраняется: %+v", ns)
	}
}

func TestCancelNeedsRememberedExecutor(t *testing.T) {
	w := newWorld(ModeOn)
	s := &State{Seeded: true}
	tk := cancelTicket()
	tk.Status, tk.CancelAuthor, tk.CancelJournalRead = "отклонён", "executor-b", true
	if ns := step(s, []Ticket{tk}, w); has(ns, KindCancel, "executor-a") {
		t.Fatalf("без сохранённого исполнителя письма нет: %+v", ns)
	}
}

func TestCancelAfterReopenCanNotifyAgain(t *testing.T) {
	w := newWorld(ModeOn)
	w.live["executor-a"], w.free["executor-a"] = true, true
	s := seeded(w)
	tk := tick("HIGH", "в работе")
	tk.Executor = "executor-a"
	step(s, []Ticket{tk}, w)
	tk.Status, tk.CancelAuthor, tk.CancelJournalRead = "отклонён", "executor-b", true
	if ns := step(s, []Ticket{tk}, w); !has(ns, KindCancel, "executor-a") {
		t.Fatalf("первая отмена: %+v", ns)
	}
	w.now = w.now.Add(8 * 24 * time.Hour)
	tk.Status, tk.CancelJournalRead = "открыт", false
	w.free["m-master"] = false
	step(s, []Ticket{tk}, w)
	tk.Status = "в работе"
	step(s, []Ticket{tk}, w)
	tk.Status, tk.CancelAuthor, tk.CancelJournalRead = "отклонён", "executor-c", true
	if ns := step(s, []Ticket{tk}, w); !has(ns, KindCancel, "executor-a") {
		t.Fatalf("повторная отмена после переоткрытия: %+v", ns)
	}
}

func TestOpenAfterWorkClearsWorkingHistory(t *testing.T) {
	w := newWorld(ModeOn)
	w.live["executor-a"], w.free["executor-a"] = true, true
	s := seeded(w)
	tk := tick("HIGH", "в работе")
	tk.Executor = "executor-a"
	step(s, []Ticket{tk}, w)
	tk.Status = "открыт"
	w.free["m-master"] = false
	step(s, []Ticket{tk}, w)
	tk.Status, tk.CancelAuthor, tk.CancelJournalRead = "отклонён", "executor-b", true
	if ns := step(s, []Ticket{tk}, w); has(ns, KindCancel, "executor-a") {
		t.Fatalf("отмена после возврата в открыто не считается переходом из работы: %+v", ns)
	}
}

func TestRejectedWithoutWorkDoesNotCreateState(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	tk := tick("MED", "отклонён")
	if ns := step(s, []Ticket{tk}, w); len(ns) != 0 {
		t.Fatalf("отклонён без истории работы: %+v", ns)
	}
	if _, exists := s.Tickets[tk.Key]; exists {
		t.Fatalf("лишнее состояние создано: %+v", s.Tickets[tk.Key])
	}
}

func TestCancelJournalIsReadOnlyForTrackedPendingRejection(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	tk := cancelTicket()
	if s.NeedsCancelJournal(Ticket{Key: tk.Key, Status: "отклонён"}) {
		t.Fatal("отклонён без состояния не требует чтения файла")
	}
	step(s, []Ticket{tk}, w)
	tk.Status = "отклонён"
	if !s.NeedsCancelJournal(tk) {
		t.Fatal("для перехода из работы журнал нужно прочитать")
	}
	s.RememberCancelJournal(tk.Key, "other-author", "отменить")
	if s.NeedsCancelJournal(tk) {
		t.Fatal("после чтения журнал не перечитывается на каждом проходе")
	}
	author, reason, ok := s.CancelJournal(tk.Key)
	if !ok || author != "other-author" || reason != "отменить" {
		t.Fatalf("сохранённые сведения журнала: %q %q %v", author, reason, ok)
	}
	tk.Status = "в работе"
	step(s, []Ticket{tk}, w)
	if _, _, ok := s.CancelJournal(tk.Key); ok || !s.NeedsCancelJournal(Ticket{Key: tk.Key, Status: "отклонён"}) {
		t.Fatal("возврат в работу сбрасывает кеш автора и причины журнала")
	}
}

func TestNeedsCancelJournalRequiresWorkingAndUnsentState(t *testing.T) {
	s := seeded(newWorld(ModeOn))
	tk := Ticket{Key: "p/x", Status: "отклонён"}
	s.Tickets[tk.Key] = &ticketState{Executor: "exec", Working: false}
	if s.NeedsCancelJournal(tk) {
		t.Fatal("без истории работы файл тикета читать не нужно")
	}
	s.Tickets[tk.Key].Working, s.Tickets[tk.Key].CancelSent = true, true
	if s.NeedsCancelJournal(tk) {
		t.Fatal("после отправки файл тикета перечитывать не нужно")
	}
}

func TestReviewStatusDoesNotTrackExecutor(t *testing.T) {
	w := newWorld(ModeOn)
	s := seeded(w)
	tk := tick("MED", "на проверке")
	if ns := step(s, []Ticket{tk}, w); len(ns) != 0 {
		t.Fatalf("на проверке без истории: %+v", ns)
	}
	if _, exists := s.Tickets[tk.Key]; exists {
		t.Fatalf("для «на проверке» состояние создано: %+v", s.Tickets[tk.Key])
	}
}

func TestCompletedTicketIsNotCancellation(t *testing.T) {
	w := newWorld(ModeOn)
	w.live["executor-a"], w.free["executor-a"] = true, true
	s := seeded(w)
	tk := tick("MED", "готово")
	tk.Back = "git-admin"
	w.free[tk.Back] = true
	s.Tickets[tk.Key] = &ticketState{Executor: "executor-a", Working: true}
	if ns := step(s, []Ticket{tk}, w); has(ns, KindCancel, "executor-a") {
		t.Fatalf("готово не является отменой: %+v", ns)
	}
}

func TestSeededClosedTicketReturnsIneligible(t *testing.T) {
	w := newWorld(ModeOn)
	s := &State{}
	s.init()
	tk := tick("MED", "отклонён")
	tk.Back = "git-admin"
	active := map[string]bool{}
	st, eligible := s.prepareTicket(tk, w.view(), false, active)
	if eligible || st == nil || !st.BackSent {
		t.Fatalf("старое закрытие засеяно без обработки: state=%+v eligible=%v", st, eligible)
	}
}
