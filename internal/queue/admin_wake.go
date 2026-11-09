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
		if alive(v, p.Master) {
			continue
		}
		isFrozen, err := v.Frozen(dir)
		if err != nil {
			continue
		}
		if isFrozen {
			delete(s.ProjectWakes, dir)
			continue
		}
		if busy[p.Admin] || !alive(v, p.Admin) || !v.Free(p.Admin) || !v.WakeFree(p.Admin) {
			continue
		}
		first, found, err := v.AdminNext(p.Admin, dir)
		if err != nil {
			continue
		}
		if !found {
			delete(s.ProjectWakes, dir)
			continue
		}
		if first.Status == "в работе" && !strings.EqualFold(first.Executor, p.Admin) {
			continue
		}
		if restingDir(dir, v) && NormPrio(first.Prio) != "CRIT" {
			continue
		}
		serious := v.Serious != nil && v.Serious(p.Project)
		w, ok := s.ProjectWakes[dir]
		if ok && w.Ticket == first.Key && !(serious && w.Mode == ModeLog) {
			continue
		}
		line := fmt.Sprintf("проект %s: живого мастера нет, есть работа: cd %s && tk next --by %s. Бери, если не занят, бойцом в отдельном дереве", p.Project, dir, p.Admin)
		out = append(out, Notice{To: p.Admin, Kind: KindWakeAdmin, Key: first.Key, Dir: dir, Line: line, Serious: serious})
		busy[p.Admin] = true
	}
	return out
}
