package codexwatch

import (
	"fmt"
	"strings"
)

// Несколько учёток: бойцы worker идут на первую с лимитом, поэтому простой одной — ещё не простой работы, а простой всех
// — да (противник замысла 03.11, сценарий 2: у обеих учёток нет кредитов, по одной учётке письма в этом случае нет,
// бойцы codex встают молча).

// Fleet — письмо «все учётки без лимита»; на диске между проходами.
type Fleet struct {
	AllOut bool `json:"all_out"` // письмо ушло; снимается, когда лимит появился
}

func LoadFleet(path string) (Fleet, error) { return load[Fleet](path) }

func (f *Fleet) Save(path string) error { return save(path, f) }

// noLimit — учётка без лимита: 100% или сервер запретил обычную работу.
func noLimit(r Reading) bool { return r.Used >= 100 || !r.Allowed }

// Apply — письмо лиду при переходе «все учётки без лимита»; пусто — писать нечего. Учётка без замера (failed) ничего не
// решает: неизвестно, есть ли у неё лимит.
func (f *Fleet) Apply(rs []Reading, failed []string) string {
	for _, r := range rs {
		if !noLimit(r) {
			f.AllOut = false
			return ""
		}
	}
	if f.AllOut || len(failed) > 0 || len(rs) < 2 { // одна учётка — не «все»: о ней говорят её письма
		return ""
	}
	f.AllOut = true
	parts := make([]string, 0, len(rs))
	for _, r := range rs {
		parts = append(parts, fmt.Sprintf("%s — плановый сброс %s, досрочных %d", r.Account.Name, local(r.ResetsAt), r.Available))
	}
	return "aguard codex: все учётки Codex без лимита: " + strings.Join(parts, "; ") +
		". worker отказывает новым задачам Luna и Terra: задачу — --model deepseek или Opus."
}

// Others — запас других учёток для письма об учётке self; других нет — пусто.
func Others(rs []Reading, failed []string, self string) string {
	var parts []string
	for _, r := range rs {
		if r.Account.Name != self {
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
