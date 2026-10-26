package waits

import (
	"time"

	"github.com/aiseeq/aguard/internal/tail"
)

// Состояние и проход — общие с другими наблюдателями транскриптов (internal/tail).
type (
	State = tail.State
	Tail  = tail.Tail[Event]
)

// LoadState — смещения из файла; нет файла — пустое (первый проход).
func LoadState(path string) (*State, error) { return tail.LoadState(path) }

// NewTail — проход waits: транскрипты сессий и суб-агентов, без номеров строк.
func NewTail(projects string, backfill time.Duration, backfillBytes int64) *Tail {
	return &Tail{Projects: projects, Backfill: backfill, BackfillBytes: backfillBytes, Parse: parse, Subagents: true,
		Stamp: func(ev *Event, src tail.Source) {
			ev.Session, ev.Agent, ev.Project, ev.Name = src.Session, src.Agent, src.Project, src.Name
		}}
}
