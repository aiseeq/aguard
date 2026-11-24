package codexwatch

import (
	"fmt"
	"strings"
)

// Несколько учёток: worker идёт на первую с лимитом, поэтому исчерпание одной — ещё не исчерпание всех, а исчерпание
// всех переводит новые задачи на другого исполнителя.

// Fleet — отметка перехода «все учётки без лимита»; на диске между проходами.
type Fleet struct {
	AllOut bool `json:"all_out"` // событие записано; снимается, когда лимит появился
}

func LoadFleet(path string) (Fleet, error) { return load[Fleet](path) }

func (f *Fleet) Save(path string) error { return save(path, f) }

// noLimit — учётка без лимита: 100% или сервер запретил обычную работу.
func noLimit(r *Reading) bool { return r.Used >= 100 || !r.Allowed }

// Apply — строка журнала при переходе «все учётки без лимита»; пусто — писать нечего. Учётка без замера (failed) ничего не
// решает: неизвестно, есть ли у неё лимит.
func (f *Fleet) Apply(rs []Reading, failed []string) string {
	for i := range rs {
		if !noLimit(&rs[i]) {
			f.AllOut = false
			return ""
		}
	}
	if f.AllOut || len(failed) > 0 || len(rs) < 2 { // одна учётка — не «все»: о ней говорят её письма
		return ""
	}
	f.AllOut = true
	parts := make([]string, 0, len(rs))
	for i := range rs {
		r := &rs[i]
		parts = append(parts, fmt.Sprintf("%s — плановый сброс %s, досрочных %d", r.Account.Name, local(r.ResetsAt), r.Available))
	}
	return "aguard codex: все учётки Codex без лимита: " + strings.Join(parts, "; ") +
		". worker отказывает новым задачам моделей Codex (sol, terra, luna): задачу — бойцу Opus (Agent fighter)."
}

// Others — запас других учёток для письма об учётке self; других нет — пусто.
func Others(rs []Reading, failed []string, self string) string {
	var parts []string
	for i := range rs {
		if r := &rs[i]; r.Account.Name != self {
			parts = append(parts, fmt.Sprintf("%s %.0f%%, плановый сброс %s", r.Account.Name, r.Used, local(r.ResetsAt)))
		}
	}
	for _, name := range failed {
		if name != self {
			parts = append(parts, name+" — замера нет")
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " Другие учётки: " + strings.Join(parts, "; ") + "."
}
