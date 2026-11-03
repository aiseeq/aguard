package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/track"
	"github.com/aiseeq/aguard/internal/trackfd"
)

// Помощника нет часами: одна строка журнала на причину, а не по строке раз в 30 с; отмена выходит сразу.
func TestTrackLoopLogsOncePerCause(t *testing.T) {
	logs := &syncBuilder{}
	a := &app{home: t.TempDir(), log: slog.New(slog.NewTextHandler(logs, nil))}
	calls := make(chan struct{}, 8)
	a.trackFetch = func() (trackfd.Pair, error) {
		calls <- struct{}{}
		return trackfd.Pair{}, errors.New("помощника нет")
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { a.trackLoop(ctx, &track.Config{Name: "track"}) })
	<-calls
	for end := time.Now().Add(5 * time.Second); !strings.Contains(logs.String(), "помощника нет"); {
		if time.Now().After(end) {
			t.Fatal("причина не записана")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("отмена не завершила цикл")
	}
	if n := strings.Count(logs.String(), "помощника нет"); n != 1 {
		t.Fatalf("строк %d:\n%s", n, logs.String())
	}
}

// Паника экземпляра не роняет демон: она становится причиной простоя.
func TestTrackOnceRecoversPanic(t *testing.T) {
	logs := &syncBuilder{}
	a := &app{home: t.TempDir(), log: slog.New(slog.NewTextHandler(logs, nil))}
	a.trackFetch = func() (trackfd.Pair, error) { panic("сбой") }
	err := a.trackOnce(context.Background(), &track.Config{Name: "track"})
	if err == nil || !strings.Contains(err.Error(), "паника") {
		t.Fatalf("%v", err)
	}
}

type syncBuilder struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuilder) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuilder) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
