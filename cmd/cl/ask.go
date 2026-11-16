package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/linesock"
	"github.com/aiseeq/aguard/internal/restart"
)

// aguardTimeout — сколько ждать ответа: тик cl не должен висеть на мёртвом сокете.
const aguardTimeout = 2 * time.Second

// aguardSocket — сокет aguard: $XDG_RUNTIME_DIR/aguard/aguard.sock.
func aguardSocket() string {
	runtime := os.Getenv("XDG_RUNTIME_DIR")
	if runtime == "" {
		runtime = os.TempDir()
	}
	return filepath.Join(runtime, "aguard", "aguard.sock")
}

// askAguard — вердикт aguard по сессии. Ошибка — ответа нет: сокета нет, срок вышел, другая версия, aguard не
// разобрал вопрос.
func askAguard(socket string, req restart.Request) (restart.Answer, error) {
	var a restart.Answer
	if err := linesock.Ask(socket, aguardTimeout, req, &a); err != nil {
		return restart.Answer{}, fmt.Errorf("aguard: %w", err)
	}
	switch {
	case a.V != restart.Version:
		return restart.Answer{}, fmt.Errorf("aguard: версия протокола %d, cl знает %d", a.V, restart.Version)
	case a.Error != "":
		return restart.Answer{}, errors.New("aguard: " + a.Error)
	}
	return a, nil
}

// compactNote — вердикты aguard о сжатии и сбросе для строки журнала --cl-check.
func compactNote(a restart.Answer) string {
	return strings.Join(compactJournalNotes(a), "")
}

func compactJournalNotes(a restart.Answer) []string {
	var notes []string
	if a.Compact && a.Reset {
		notes = append(notes, fmt.Sprintf("; сжатие и сброс: да, контекст %d тыс.", a.Tokens/1000))
	} else if a.Compact {
		notes = append(notes, fmt.Sprintf("; сжатие: да, контекст %d тыс.", a.Tokens/1000))
	} else if a.Reset {
		notes = append(notes, fmt.Sprintf("; сброс: да, контекст %d тыс.", a.Tokens/1000))
	}
	groups := groupIdleReasons(a.CompactAgainst, a.ResetAgainst)
	if !a.Compact && len(a.CompactAgainst) == 0 {
		notes = append(notes, "; сжатие: нет")
	}
	if !a.Reset && len(a.ResetAgainst) == 0 {
		notes = append(notes, "; сброс: нет")
	}
	for i := 0; i < len(groups); {
		group := groups[i]
		if group.hasCompact && group.hasReset {
			notes = append(notes, "; "+formatIdleReason(group, true))
			i++
			continue
		}
		compact := group.hasCompact
		texts := []string{restart.Text(group.compact)}
		if !compact {
			texts[0] = restart.Text(group.reset)
		}
		i++
		for i < len(groups) && !(groups[i].hasCompact && groups[i].hasReset) && groups[i].hasCompact == compact {
			if compact {
				texts = append(texts, restart.Text(groups[i].compact))
			} else {
				texts = append(texts, restart.Text(groups[i].reset))
			}
			i++
		}
		label := "сброс"
		if compact {
			label = "сжатие"
		}
		notes = append(notes, "; "+label+": нет — "+strings.Join(texts, "; "))
	}
	return notes
}

// compactStatus — короткие решения сжатия и сброса для заголовка --cl-check.
func compactStatus(a restart.Answer) string {
	status := func(yes bool) string {
		if yes {
			return fmt.Sprintf("да, контекст %d тыс.", a.Tokens/1000)
		}
		return "нет"
	}
	return "; сжатие: " + status(a.Compact) + "; сброс: " + status(a.Reset)
}

type idleReasonGroup struct {
	compact, reset       restart.Reason
	hasCompact, hasReset bool
}

// groupIdleReasons объединяет одинаковые доводы, а также пары с общим кодом, но разными текстами.
func groupIdleReasons(compact, reset []restart.Reason) []idleReasonGroup {
	resetUsed := make([]bool, len(reset))
	var groups []idleReasonGroup
	for _, c := range compact {
		matched := false
		for i, r := range reset {
			if !resetUsed[i] && c.Code == r.Code {
				resetUsed[i] = true
				groups = append(groups, idleReasonGroup{compact: c, reset: r, hasCompact: true, hasReset: true})
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		groups = append(groups, idleReasonGroup{compact: c, hasCompact: true})
	}
	for i, r := range reset {
		if !resetUsed[i] {
			groups = append(groups, idleReasonGroup{reset: r, hasReset: true})
		}
	}
	return groups
}

// formatIdleReason — общий довод без метки; разные тексты одного кода объединяет по общей конечной фразе.
func formatIdleReason(g idleReasonGroup, journal bool) string {
	if g.hasCompact && g.hasReset {
		compactText, resetText := restart.Text(g.compact), restart.Text(g.reset)
		if compactText == resetText {
			if journal {
				return "сжатие и сброс: нет — " + compactText
			}
			return compactText
		}
		if prefix, suffix := sharedWindowSuffix(compactText, resetText); suffix != "" && !journal {
			return prefix + "; сжатие " + suffix + ", сброс " + suffix
		}
		if journal {
			return "сжатие: нет — " + compactText + "; сброс: нет — " + resetText
		}
		return "сжатие: " + compactText + "; сброс: " + resetText
	}
	if g.hasCompact {
		if journal {
			return "сжатие: нет — " + restart.Text(g.compact)
		}
		return "сжатие: " + restart.Text(g.compact)
	}
	if journal {
		return "сброс: нет — " + restart.Text(g.reset)
	}
	return "сброс: " + restart.Text(g.reset)
}

func sharedWindowSuffix(compactText, resetText string) (string, string) {
	compactPrefix, compactTail, compactOK := strings.Cut(compactText, ",")
	resetPrefix, resetTail, resetOK := strings.Cut(resetText, ",")
	if !compactOK || !resetOK {
		return "", ""
	}
	compactPrefix, resetPrefix = strings.TrimSpace(compactPrefix), strings.TrimSpace(resetPrefix)
	compactTail, resetTail = strings.TrimSpace(compactTail), strings.TrimSpace(resetTail)
	if !strings.HasPrefix(compactTail, "окно ") || !strings.HasPrefix(resetTail, "сброс ") {
		return "", ""
	}
	compactTail = strings.TrimPrefix(compactTail, "окно ")
	resetTail = strings.TrimPrefix(resetTail, "сброс ")
	if compactPrefix != resetPrefix || compactTail != resetTail {
		return "", ""
	}
	return compactPrefix, compactTail
}

// checkDetails — строки доводов для stdout; решение уже показано в заголовке.
func checkDetails(a restart.Answer) []string {
	return checkDetailsFiltered(a, nil)
}

func checkDetailsFiltered(a restart.Answer, omitted map[string]bool) []string {
	filter := func(reasons []restart.Reason) []restart.Reason {
		filtered := make([]restart.Reason, 0, len(reasons))
		for _, reason := range reasons {
			if !omitted[reasonKey(reason)] {
				filtered = append(filtered, reason)
			}
		}
		return filtered
	}
	var details []string
	for _, group := range groupIdleReasons(filter(a.CompactAgainst), filter(a.ResetAgainst)) {
		details = append(details, formatIdleReason(group, false))
	}
	return details
}
