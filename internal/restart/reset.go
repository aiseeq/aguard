package restart

import (
	"fmt"
	"strings"
	"time"
)

// Сброс беседы (/clear): журнал «сбросил бы» со сверкой исхода (задача лида 25.10 по слову пользователя 18:14 CET о
// сессии с закрытой работой и подсказкой Claude Code «/clear to save 235.7k tokens»: «ну вот в таком случае, например,
// я бы сбрасывал сессию») и вердикт cl в ответе. Сбрасывает cl: останавливает claude и поднимает новую беседу со
// строкой роли и файлом возобновления (задание Сна 26.10; ввод в сессию — через cl: пользователь 25.10 17:49 CET
// «Пускай отправляет сообщение cl, а тот уже пишет»). Беседа, брошенная надолго с большим контекстом, следующим
// запросом пишет весь контекст в кэш заново; если работа закрыта, контекст не нужен вовсе.

// ResetVerdict — сбросил бы беседу и почему нет.
type ResetVerdict struct {
	Reset   bool     `json:"reset"`
	Against []Reason `json:"against"`
	// Candidate — большой контекст и долгий простой: решение стоит записать.
	Candidate bool `json:"candidate"`
}

// decideReset — сбросить беседу: с того же контекста и простоя, что сжатие (MinTokens, After), без верхней границы
// окна — кэш новой беседе не нужен; сжатие беседы сброс не держит; сессия свободна, ничего не идёт, таймеров нет
// (перезапуск их не восстановит; /clear под pty — p.typed — их не теряет), человек не печатает, аргументы запуска восстановимы (p.argsProblem: cl поднимает новую
// беседу с ними). Дерево git проверяет Judge последним: git — только когда остальное за. Где сброс можно, он вместо
// сжатия (Judge.Compact): пользователь 27.10 10:48 CET «мы должны использовать сброс везде где можем. Компакт это
// компромисс», вариант 3 лида — 11:12 CET «Да, давай 3»; подсказка Claude Code «/clear to save» больше не решает.
func decideReset(f facts, c *CompactConfig, p allowance, now time.Time) ResetVerdict {
	var v ResetVerdict
	v.Against, v.Candidate = idleChecks(f, c, allowance{argsProblem: p.argsProblem}, idleWindow{c.MinTokens, c.After.D(), 0, "сброс", true}, now)
	if len(f.Transcript.Crons) > 0 && !p.typed {
		v.Against = append(v.Against, Reason{Code: "crons", Text: "таймеры пропали бы: " + cronList(f.Transcript.Crons)})
	}
	v.Reset = len(v.Against) == 0
	return v
}

// Key — решение без чисел: cl пишет строку журнала, только когда оно меняется.
func (v ResetVerdict) Key() string {
	return verdictKey(v.Reset, v.Against)
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
