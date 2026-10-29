package linesock

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Сокет 0600, ответ строкой; нет сокета и сокет без сервера (остался от прошлого запуска) — ErrNoServer.
func TestServeAskAndNoServer(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "s", "test.sock")
	var out struct{ N int }
	if err := Ask(socket, time.Second, map[string]int{"n": 1}, &out); !errors.Is(err, ErrNoServer) {
		t.Fatalf("нет сокета: %v", err)
	}
	ln, err := Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(socket); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("права сокета: %v %v", info, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, ln, func(line []byte) any { return map[string]int{"N": len(line)} }, slog.New(slog.DiscardHandler), "test")
	}()
	if err := Ask(socket, time.Second, map[string]int{"n": 1}, &out); err != nil || out.N != len(`{"n":1}`) {
		t.Fatalf("ответ %+v: %v", out, err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := Ask(socket, time.Second, 1, &out); !errors.Is(err, ErrNoServer) {
		t.Fatalf("сокет без сервера: %v", err)
	}
}
