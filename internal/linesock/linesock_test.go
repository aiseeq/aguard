package linesock

import (
	"context"
	"errors"
	"log/slog"
	"net"
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

// Сервер принял вопрос и молчит — Ask возвращает ошибку к сроку, а не висит (тик cl на мёртвом aguard).
func TestAskSilentServerDeadline(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "silent.sock")
	ln, err := Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		if conn, err := ln.Accept(); err == nil {
			accepted <- conn
		}
	}()
	const timeout = 100 * time.Millisecond
	start := time.Now()
	var out struct{ N int }
	if err := Ask(socket, timeout, 1, &out); err == nil || errors.Is(err, ErrNoServer) {
		t.Fatalf("молчит: %v", err)
	}
	if d := time.Since(start); d > timeout+time.Second {
		t.Errorf("ждал %v, срок %v", d, timeout)
	}
	if conn := <-accepted; conn != nil {
		_ = conn.Close()
	}
}
