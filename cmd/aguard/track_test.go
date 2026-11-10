package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/track"
	"github.com/aiseeq/aguard/internal/trackfd"
)

// Помощника нет часами: одна строка журнала на причину, а не по строке раз в 30 с; отмена выходит сразу.
func TestTrackLoopLogsOncePerCause(t *testing.T) {
	logs := &syncBuilder{}
	a := &app{home: t.TempDir(), log: slog.New(slog.NewTextHandler(logs, nil))}
	var calls atomic.Int32
	a.trackFetch = func() (trackfd.Pair, error) {
		if calls.Add(1) <= 3 {
			return trackfd.Pair{}, errors.New("помощника нет")
		}
		return trackfd.Pair{}, errors.New("другая причина")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Go(func() { a.trackLoop(ctx, &track.Config{Name: "track"}, time.Millisecond) })
	for end := time.Now().Add(5 * time.Second); calls.Load() < 6; {
		if time.Now().After(end) {
			t.Fatalf("заходов %d, ждали 6", calls.Load())
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("отмена не завершила цикл")
	}
	for _, cause := range []string{"помощника нет", "другая причина"} {
		if n := strings.Count(logs.String(), cause); n != 1 {
			t.Fatalf("%q: строк %d:\n%s", cause, n, logs.String())
		}
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

// Замер каждого экземпляра слежения демона уступает занятой машине (тикет io-freeze-aguard).
func TestTrackInstancesYield(t *testing.T) {
	a := &app{home: t.TempDir()}
	insts := a.trackInstances()
	if len(insts) == 0 {
		t.Fatal("нет экземпляров")
	}
	for _, c := range insts {
		if c.Yield == nil {
			t.Fatalf("экземпляр %s без уступки", c.Name)
		}
	}
}
