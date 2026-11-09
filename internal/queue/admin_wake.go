package queue

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

func frozen(v View, dir string) bool {
	if v.Frozen == nil {
		return false
	}
	yes, err := v.Frozen(dir)
	return err == nil && yes
}

// adminWakeNotices предлагает работу администратору только при исправном списке замороженных проектов.
func (s *State) adminWakeNotices(ts []Ticket, v View, notices []Notice) []Notice {
	if v.Frozen == nil || v.AdminNext == nil || v.Free == nil || v.WakeFree == nil {
		return nil
	}
	if v.Projects != nil {
		ts = append(slices.Clone(ts), v.Projects()...)
	}
	projects := map[string]Ticket{}
	for i := range ts {
		t := ts[i]
		if t.Dir != "" {
			projects[t.Dir] = t
		}
	}
	busy := wakeBusy(notices) // одно предложение за проход: следующий проект — когда админ снова свободен
	var out []Notice
	for _, dir := range slices.Sorted(maps.Keys(projects)) {
		p := projects[dir]
		if v.AdminFor != nil {
			p.Admin = v.AdminFor(dir)
		}
		if alive(v, p.Master) || !s.adminMayOffer(p, v, busy) {
			continue
		}
		if n, ok := s.adminOffer(p, v); ok {
			out = append(out, n)
			busy[p.Admin] = true
		}
	}
	return out
}

// adminMayOffer — проект не заморожен, а его админ жив, свободен и в этом проходе ещё без письма.
func (s *State) adminMayOffer(p Ticket, v View, busy map[string]bool) bool {
	isFrozen, err := v.Frozen(p.Dir)
	if err != nil { // причину как предупреждение уже записал источник: без списка предлагать нельзя
		v.Log.Debug("aguard queue: проект без предложения", "dir", p.Dir, "err", err)
		return false
	}
	if isFrozen {
		delete(s.ProjectWakes, p.Dir)
		return false
	}
	return !busy[p.Admin] && alive(v, p.Admin) && v.Free(p.Admin) && v.WakeFree(p.Admin)
}

// adminOffer решает по первому тикету проекта: предложить его админу или промолчать.
func (s *State) adminOffer(p Ticket, v View) (Notice, bool) {
	first, found, err := v.AdminNext(p.Admin, p.Dir)
	if err != nil { // причину как предупреждение уже записал источник; отметку не трогаем
		v.Log.Debug("aguard queue: проект без предложения", "dir", p.Dir, "err", err)
		return Notice{}, false
	}
	if !found {
		delete(s.ProjectWakes, p.Dir)
		return Notice{}, false
	}
	if first.Status == "в работе" && !strings.EqualFold(first.Executor, p.Admin) {
		return Notice{}, false
	}
	if restingDir(p.Dir, v) && NormPrio(first.Prio) != "CRIT" {
		return Notice{}, false
	}
	serious := v.Serious != nil && v.Serious(p.Project)
	if w, ok := s.ProjectWakes[p.Dir]; ok && w.Ticket == first.Key && !(serious && w.Mode == ModeLog) {
		return Notice{}, false
	}
	line := fmt.Sprintf("проект %s: живого мастера нет, есть работа: cd %s && tk next --by %s. Бери, если не занят, бойцом в отдельном дереве", p.Project, p.Dir, p.Admin)
	return Notice{To: p.Admin, Kind: KindWakeAdmin, Key: first.Key, Dir: p.Dir, Line: line, Serious: serious}, true
}
