// Package waits — отказы и ожидания агентов по их транскриптам (~/.claude/projects): хуки, снятые по таймауту,
// ошибки API (лимит, 5xx), сжатия контекста. По ним видно, где агент простаивает или машина его душит
// (docs/design.md, «Ресурсы, нехватку которых пользователь не заметит»).
package waits

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// Виды событий.
const (
	KindHookTimeout = "hook_timeout" // Claude Code снял хук по таймауту: признак, что машина душит процессы
	KindAPIError    = "api_error"    // ответ API — ошибка: лимит учётки, 5xx, перегрузка
	KindCompact     = "compact"      // сжатие контекста: детали беседы теряются
)

// Event — одно событие из транскрипта.
type Event struct {
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Session string    `json:"session"`         // id сессии: имя файла транскрипта
	Agent   string    `json:"agent,omitempty"` // суб-агент: id из subagents/agent-<id>.jsonl
	Project string    `json:"project"`         // папка проекта в ~/.claude/projects
	Name    string    `json:"name,omitempty"`  // имя сессии по последней записи agent-name до события
	// Detail — что именно: событие хука (Stop, PostToolBatch), вид ошибки API и код (rate_limit 429), повод сжатия.
	Detail  string `json:"detail"`
	Ms      int64  `json:"ms,omitempty"`       // длительность хука или сжатия
	LimitMs int64  `json:"limit_ms,omitempty"` // таймаут хука
	Tokens  int64  `json:"tokens,omitempty"`   // токенов контекста до сжатия
}

// markers — подстроки нужных записей: строку без них не разбираем (транскрипты — гигабайты в неделю).
var markers = [][]byte{
	[]byte(`"hook_cancelled"`), []byte(`"isApiErrorMessage":true`), []byte(`"compact_boundary"`), []byte(`"agent-name"`),
}

func interesting(line []byte) bool {
	for _, m := range markers {
		if bytes.Contains(line, m) {
			return true
		}
	}
	return false
}

// line — нужные поля строки транскрипта.
type line struct {
	Type      string    `json:"type"`
	Subtype   string    `json:"subtype"`
	Timestamp time.Time `json:"timestamp"`
	AgentName string    `json:"agentName"`
	// ответ API с ошибкой
	IsAPIError bool   `json:"isApiErrorMessage"`
	Error      string `json:"error"`
	Status     int    `json:"apiErrorStatus"`
	Attachment struct {
		Type      string `json:"type"`
		HookEvent string `json:"hookEvent"`
		TimedOut  bool   `json:"timedOut"`
		Duration  int64  `json:"durationMs"`
		Timeout   int64  `json:"timeoutMs"`
	} `json:"attachment"`
	Compact struct {
		Trigger   string `json:"trigger"`
		PreTokens int64  `json:"preTokens"`
		Duration  int64  `json:"durationMs"`
	} `json:"compactMetadata"`
}

// parse разбирает строку: событие (ok) и/или новое имя сессии. Строка без маркеров — пусто без ошибки.
func parse(b []byte) (ev Event, ok bool, name string, err error) {
	if !interesting(b) {
		return Event{}, false, "", nil
	}
	var r line
	if err := json.Unmarshal(b, &r); err != nil {
		return Event{}, false, "", fmt.Errorf("строка транскрипта: %w", err)
	}
	ev = Event{At: r.Timestamp}
	switch {
	case r.Type == "agent-name":
		return Event{}, false, r.AgentName, nil
	case r.Type == "attachment" && r.Attachment.Type == "hook_cancelled" && r.Attachment.TimedOut:
		ev.Kind, ev.Detail = KindHookTimeout, r.Attachment.HookEvent
		ev.Ms, ev.LimitMs = r.Attachment.Duration, r.Attachment.Timeout
	case r.IsAPIError:
		ev.Kind, ev.Detail = KindAPIError, r.Error
		if r.Status != 0 {
			ev.Detail += " " + strconv.Itoa(r.Status)
		}
	case r.Type == "system" && r.Subtype == "compact_boundary":
		ev.Kind, ev.Detail = KindCompact, r.Compact.Trigger
		ev.Ms, ev.Tokens = r.Compact.Duration, r.Compact.PreTokens
	default:
		return Event{}, false, "", nil // маркер попался внутри текста, а не в поле
	}
	return ev, true, "", nil
}
