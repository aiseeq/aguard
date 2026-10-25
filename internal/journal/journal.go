// Package journal — логгер для служб под systemd: slog в stderr без времени.
package journal

import (
	"log/slog"
	"os"
)

// New — текстовый slog в stderr; время в строки не пишется, его ставит journald.
func New() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{ReplaceAttr: dropTime}))
}

func dropTime(_ []string, at slog.Attr) slog.Attr {
	if at.Key == slog.TimeKey {
		return slog.Attr{}
	}
	return at
}
