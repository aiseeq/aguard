package queue

import (
	"slices"
	"strings"
	"time"
)

// ExternalTask — задача внешнего трекера, которую мастеру показывает tk next. Список проекта живёт в state.json:
// сбой трекера и рестарт демона его не стирают, иначе отметки о письмах пропали бы и письма пошли заново.
type ExternalTask struct {
	Key       string    `json:"key"`
	Title     string    `json:"title"`
	Status    string    `json:"status"`
	Prio      string    `json:"prio"`
	URL       string    `json:"url"`
	FirstSeen time.Time `json:"first_seen"` // даты создания источник не даёт: первая встреча вместо неё, для утренней строки
}

// SetExternal заменяет список проекта ответом трекера; первая встреча известной задачи сохраняется.
func (s *State) SetExternal(project string, fresh []ExternalTask, now time.Time) {
	if len(fresh) == 0 {
		delete(s.External, project)
		return
	}
	if s.External == nil {
		s.External = map[string][]ExternalTask{}
	}
	old := s.External[project]
	out := make([]ExternalTask, 0, len(fresh))
	for _, task := range fresh {
		task.FirstSeen = now
		if i := slices.IndexFunc(old, func(o ExternalTask) bool { return o.Key == task.Key }); i >= 0 {
			task.FirstSeen = old[i].FirstSeen
		}
		out = append(out, task)
	}
	s.External[project] = out
}

// KeepExternal убирает списки проектов, ушедших из настройки: по замороженному проекту писем быть не должно.
func (s *State) KeepExternal(projects []string) {
	for project := range s.External {
		if !slices.Contains(projects, project) {
			delete(s.External, project)
		}
	}
}

// Ticket — задача как тикет для Step. Открыта всегда: письмо нужно и о задаче в работе, её мог начать человек, а мастер не знает.
func (t ExternalTask) Ticket(project, dir, master, admin string) Ticket {
	return Ticket{Key: project + "/" + t.Key, Project: project, Dir: dir, Slug: t.Key, Title: t.Title, Prio: t.Prio, Status: "открыт",
		Master: master, Admin: admin, External: true, Started: strings.EqualFold(t.Status, "In Progress"), Created: t.FirstSeen}
}
