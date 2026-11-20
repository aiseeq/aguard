package track

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"time"
)

// SharedTombs owns the connector and its table while at least one tracker uses it.
type SharedTombs struct {
	mu    sync.Mutex
	table *Tombs
	users int
	keep  time.Duration
	log   *slog.Logger
	start func(*Tombs) (func(), error)
	stop  func()
}

func newSharedTombs(keep time.Duration, log *slog.Logger) *SharedTombs {
	return &SharedTombs{keep: keep, log: log}
}

// NewSharedTombs creates a shared connector. open must return a fresh subscribed socket.
func NewSharedTombs(keep time.Duration, log *slog.Logger, open func() (*os.File, error)) *SharedTombs {
	s := newSharedTombs(keep, log)
	s.start = func(t *Tombs) (func(), error) { return startSharedConnector(t, open) }
	return s
}

func (s *SharedTombs) acquire(keep time.Duration) (*Tombs, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.table == nil {
		s.table = newTombs(max(s.keep, keep), s.log)
		var err error
		s.stop, err = s.start(s.table)
		if err != nil {
			s.table = nil
			return nil, err
		}
	} else {
		s.table.mu.Lock()
		s.table.keep = max(s.table.keep, keep)
		s.table.mu.Unlock()
	}
	s.users++
	return s.table, nil
}

// Acquire retains the table; Release must be called exactly once per acquisition.
func (s *SharedTombs) Acquire(keep time.Duration) (*Tombs, error) { return s.acquire(keep) }

func (s *SharedTombs) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users--
	if s.users == 0 {
		s.stop()
		s.table = nil
		s.stop = nil
	}
}

func (s *SharedTombs) Release() { s.release() }

func startSharedConnector(t *Tombs, open func() (*os.File, error)) (func(), error) {
	return startSharedConnectorRetry(t, open, time.Second)
}

func startSharedConnectorRetry(t *Tombs, open func() (*os.File, error), retry time.Duration) (func(), error) {
	initial, err := open()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	expireDone := make(chan struct{})
	var mu sync.Mutex
	conn := initial
	go func() { defer close(expireDone); t.expire(ctx.Done()) }()
	go func() {
		defer close(done)
		f := initial
		lastFailure := ""
		defer func() {
			if f != nil {
				_ = f.Close()
			}
		}()
		for ctx.Err() == nil {
			var err error
			if f == nil {
				f, err = open()
			}
			if err == nil {
				mu.Lock()
				if ctx.Err() != nil {
					mu.Unlock()
					_ = f.Close()
					return
				}
				conn = f
				mu.Unlock()
				var received bool
				received, err = t.readConnector(f)
				if received && lastFailure != "" {
					t.log.Info("aguard слежение: proc connector восстановлен")
					lastFailure = ""
				}
				mu.Lock()
				conn = nil
				mu.Unlock()
				_ = f.Close()
				f = nil
			}
			if ctx.Err() != nil {
				return
			}
			t.connFailures.Add(1)
			if reason := err.Error(); reason != lastFailure {
				t.log.Warn("aguard слежение: proc connector недоступен, повтор через 1 с", "err", err)
				lastFailure = reason
			}
			timer := time.NewTimer(retry)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return func() {
		cancel()
		mu.Lock()
		if conn != nil {
			_ = conn.Close()
		}
		mu.Unlock()
		<-done
		<-expireDone
	}, nil
}
