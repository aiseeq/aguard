package restart

import (
	"fmt"
	"strings"
	"time"
)

// Сброс беседы (/clear) — только журнал «сбросил бы» со сверкой исхода (задача лида 25.10 по слову пользователя
// 18:14 CET о сессии с закрытой работой и подсказкой Claude Code «/clear to save 235.7k tokens»: «ну вот в таком
// случае, например, я бы сбрасывал сессию»). Сам сброс ничего не делает. Беседа, брошенная надолго с большим
// контекстом, следующим запросом пишет весь контекст в кэш заново; если работа закрыта, контекст не нужен вовсе.

// ResetVerdict — сбросил бы беседу и почему нет.
type ResetVerdict struct {
	Reset   bool     `json:"reset"`
	Against []Reason `json:"against"`
	// Candidate — большой контекст и долгий простой: решение стоит записать.
	Candidate bool `json:"candidate"`
}

// decideReset — сбросил бы беседу: контекст от ResetMinTokens, последний запрос не ближе ResetIdle, после него не
// сжимали, сессия свободна, ничего не идёт, таймеров нет (сброс их не восстановит), человек не печатает, на экране
// подсказка сброса. Дерево git проверяет Judge последним: git — только когда остальное за.
func decideReset(f facts, c *CompactConfig, now time.Time) ResetVerdict {
	var v ResetVerdict
	v.Against, v.Candidate = idleChecks(f, c, allowance{}, idleWindow{c.ResetMinTokens, c.ResetIdle.D(), 0, "сброс"}, now)
	if len(f.Errors) == 0 && !f.ClearHint {
		v.Against = append(v.Against, Reason{Code: "hint", Text: "на экране нет подсказки Claude Code «/clear to save»"})
	}
	v.Reset = len(v.Against) == 0
	return v
}

// treeCheck — довод против сброса по git-дереву папки сессии: незакоммиченное — работа не закрыта.
func treeCheck(v *ResetVerdict, tree func(string) ([]string, error), dir string) {
	dirty, err := tree(dir)
	switch {
	case err != nil:
		v.Against = append(v.Against, Reason{Code: "tree", Text: err.Error()})
	case len(dirty) > 0:
		shown := dirty[:min(len(dirty), 3)]
		v.Against = append(v.Against, Reason{Code: "tree", Text: fmt.Sprintf("в %s незакоммиченное (%d): %s", dir, len(dirty), strings.Join(shown, "; "))})
	}
	v.Reset = len(v.Against) == 0
}
