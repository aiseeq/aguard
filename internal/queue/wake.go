package queue

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// wakeBusy — кому в этом проходе уже идёт письмо, которое и так его разбудит.
func wakeBusy(notices []Notice) map[string]bool {
	busy := map[string]bool{}
	for i := range notices {
		switch notices[i].Kind {
		case KindCrit, KindCritCopy, KindHigh, KindHighAdmin, KindWake, KindWakeStuck:
			busy[notices[i].To] = true
		}
	}
	return busy
}

// wakeNotices будит свободных живых мастеров, у которых tk next показывает работу.
func (s *State) wakeNotices(ts []Ticket, v View, notices []Notice) []Notice {
	if v.Next == nil || v.Session == nil || v.WakeFree == nil || v.Free == nil {
		return nil
	}
	if v.Projects != nil {
		ts = append(slices.Clone(ts), v.Projects()...)
	}
	masters := map[string][]Ticket{}
	for i := range ts {
		if ts[i].Master != "" && ts[i].Dir != "" {
			masters[ts[i].Master] = append(masters[ts[i].Master], ts[i])
		}
	}
	busy := wakeBusy(notices)
	var out []Notice
	for _, master := range slices.Sorted(maps.Keys(masters)) {
		if busy[master] || !alive(v, master) || !v.Free(master) || !v.WakeFree(master) {
			continue
		}
		session := v.Session(master)
		if session == "" {
			continue
		}
		cwd := ""
		if v.Cwd != nil {
			cwd = v.Cwd(master)
		}
		project := wakeProject(masters[master], cwd)
		first, ok := s.wakeFirst(master, project, ts, v)
		if !ok {
			continue
		}
		if n, ok := s.wakeDecide(master, session, first, project, v); ok {
			out = append(out, n)
		}
	}
	return out
}

// wakeBlocked проверяет, держит ли первый тикет найденная незакрытая задача этого прохода.
func wakeBlocked(ticket Ticket, ts []Ticket, v View) bool {
	for _, ref := range ticket.BlockedBy {
		dir, slug, ok := blockerLocation(ref, ticket, v.Home)
		if !ok {
			continue
		}
		key := dir + "\x00" + slug
		if _, found := v.blockers[key]; !found {
			continue
		}
		for i := range ts {
			if ts[i].Dir == dir && ts[i].Slug == slug && !closed(ts[i].Status) {
				return true
			}
		}
	}
	return false
}

// wakeFirst — тикет, по которому будить мастера, или ничего. Нет работы — запись о письме снята; сбой источника —
// причину как предупреждение уже записал источник, запись о письме не трогаем.
func (s *State) wakeFirst(master string, project Ticket, ts []Ticket, v View) (Ticket, bool) {
	first, found, err := v.Next(master, project.Dir)
	if err != nil {
		v.Log.Debug("aguard queue: мастер пропущен", "master", master, "err", err)
		return Ticket{}, false
	}
	if !found {
		delete(s.Wakes, master)
		return Ticket{}, false
	}
	return s.wakeFree(master, first, project, ts, v)
}

// wakeFree — тикет, по которому будить мастера: первый, а если он заблокирован незакрытым тикетом — первый свободный
// локальный кандидат ниже, за ним — первая задача внешнего источника из tk next (also: tk ставит локальную группу выше).
// Свободного нет — запись о письме снята; сбой списка кандидатов — предупреждение уже записал источник
// (cmd/aguard), запись не тронута.
func (s *State) wakeFree(master string, first, project Ticket, ts []Ticket, v View) (Ticket, bool) {
	if !wakeBlocked(first, ts, v) {
		return first, true
	}
	var candidates []Ticket
	if v.WakeCandidates != nil {
		var err error
		if candidates, err = v.WakeCandidates(master, project.Dir); err != nil {
			v.Log.Debug("aguard queue: кандидаты мастера недоступны", "master", master, "err", err)
			return first, false
		}
	}
	free, found := wakeCandidate(candidates, ts, v)
	if !found && first.WakeAlso != nil {
		free, found = *first.WakeAlso, true
	}
	if !found {
		delete(s.Wakes, master)
	}
	return free, found
}

// wakeCandidate выбирает первый доступный тикет из tk ls без незакрытых блокеров. Внешних задач Jira в tk ls нет —
// их берёт wakeFree из also ответа tk next.
func wakeCandidate(candidates, ts []Ticket, v View) (Ticket, bool) {
	for i := range candidates {
		if candidates[i].CanTake && !wakeBlocked(candidates[i], ts, v) {
			return candidates[i], true
		}
	}
	return Ticket{}, false
}

// wakeProject — папка, где мастер запустит tk next: иначе он увидит не ту очередь, о которой письмо.
func wakeProject(projects []Ticket, cwd string) Ticket {
	best := projects[0]
	for i := range projects[1:] {
		p := &projects[i+1]
		bestHere, here := cwd != "" && underRoot(cwd, best.Dir), cwd != "" && underRoot(cwd, p.Dir)
		if here && !bestHere || here == bestHere && p.Dir < best.Dir {
			best = *p
		}
	}
	return best
}

// wakeDecide решает по первому тикету свободного мастера: письмо, сигнал админу или ничего.
func (s *State) wakeDecide(master, session string, first, project Ticket, v View) (Notice, bool) {
	if first.Project == "" {
		first.Project = project.Project
	}
	if first.Dir == "" {
		first.Dir = project.Dir
	}
	if first.Admin == "" {
		first.Admin = project.Admin
	}
	if first.Status == "в работе" && !strings.EqualFold(first.Executor, master) {
		return Notice{}, false
	}
	if restingDir(first.Dir, v) && NormPrio(first.Prio) != "CRIT" {
		return Notice{}, false
	}
	serious := v.Serious != nil && v.Serious(first.Project)
	w, exists := s.Wakes[master]
	if !exists || w.Ticket != first.Key || w.Session != session || serious && w.Mode == ModeLog {
		return Notice{To: master, Kind: KindWake, Key: first.Key, Session: session, Serious: serious, Line: "есть работа: tk next. Первый тикет не взять: ждёт внешнего (срок, человек, чужой тикет) — tk wait <X> --until <когда и чего>; ждёт своего тикета ID — tk set <ID> --blocks <X>, X — первый; просто не сейчас — оставь открытым, но админу уйдёт «встал»"}, true
	}
	if w.Stuck || v.TurnAfter == nil || !v.TurnAfter(master, w.At) {
		return Notice{}, false
	}
	to := liveRecipient(v, first.Admin, v.Lead, master)
	line := fmt.Sprintf("мастер %s получил «есть работа» и встал: первым остался %s", master, first.Key)
	if to == "" { // доставлять некому: строка в журнал — один раз, а сигнал уйдёт, когда получатель вернётся
		if w.NoOne {
			return Notice{}, false
		}
		w.NoOne = true
		s.Wakes[master] = w
		return Notice{Kind: KindUnreachable, Key: first.Key, Serious: serious, Line: "некому сообщить: " + line}, true
	}
	return Notice{To: to, Kind: KindWakeStuck, Master: master, Session: session, Key: first.Key, Serious: serious, Line: line}, true
}

// WakeTicket создаёт сведения о проекте или первом тикете для будильника.
func WakeTicket(project, dir, key string) Ticket { return Ticket{Project: project, Dir: dir, Key: key} }
