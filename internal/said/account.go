package said

import (
	"fmt"
	"regexp"
	"time"
)

// Account — учётка OpenAI для разметки и что о ней знает замер aguard (codexwatch).
type Account struct {
	Name string
	Home string // CODEX_HOME вызова; пусто — окружение демона (модель не на codex)
	// Known — замер есть. Нет — учётка считается доступной: отказ виден сразу и даст метку, а смерть замера иначе молча
	// остановила бы разметку (так указатель простоял 03–06.11).
	Known    bool
	Used     float64
	ResetsAt time.Time
	Denied   bool // сервер запретил обычную работу и ниже 100%
}

// AccountRetry — срок метки после отказа. Не до сброса окна: текст отказа бывает и при живом лимите (короткое окно), а
// настоящие 100% держит замер.
const AccountRetry = time.Hour

// Шаблоны отказа по кредитам — как у бойцов (llmark tool/cmd/worker/codexaccounts.go): темп запросов важнее слов о
// квоте рядом и учётку не метит.
var (
	accountOutRe = regexp.MustCompile(`(?i)out of credits|usage limit|quota`)
	rateRe       = regexp.MustCompile(`(?i)rate limit|\b429\b|too many requests`)
)

// AccountOut — текст отказа говорит о кредитах учётки, а не о сети, темпе или модели.
func AccountOut(msg string) bool { return accountOutRe.MatchString(msg) && !rateRe.MatchString(msg) }

// Usable — учётки, которым можно отдать вызов, по порядку accounts; skipped — пропущенные с причиной, для журнала.
// Запас ceiling оставляет остаток окна бойцам: разметка не должна доводить подписку до 100%. Сброс окна в прошлом снимает
// замер: следующий замер или отказ скажут правду.
func Usable(accounts []Account, out map[string]time.Time, ceiling float64, now time.Time) (ok []Account, skipped []string) {
	for _, acc := range accounts {
		switch until, marked := out[acc.Name]; {
		case marked && until.After(now):
			skipped = append(skipped, fmt.Sprintf("%s: отказ, метка до %s", acc.Name, until.Format(time.RFC3339)))
		case acc.Known && acc.ResetsAt.After(now) && acc.Denied:
			skipped = append(skipped, acc.Name+": работа запрещена сервером")
		case acc.Known && acc.ResetsAt.After(now) && acc.Used >= ceiling:
			skipped = append(skipped, fmt.Sprintf("%s: used %g ≥ %g", acc.Name, acc.Used, ceiling))
		default:
			ok = append(ok, acc)
		}
	}
	return ok, skipped
}
