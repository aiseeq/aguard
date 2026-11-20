package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/config"
	"github.com/aiseeq/aguard/internal/orphans"
	"github.com/aiseeq/aguard/internal/prune"
	"github.com/aiseeq/aguard/internal/record"
	"github.com/aiseeq/aguard/internal/yield"
	"github.com/aiseeq/akit/proc"
)

var errClaudeTmpFailureState = errors.New("invalid claude-tmp failure state")

func (a *app) claudeTmpRound(ctx context.Context, cfg *config.Config) error {
	c := cfg.ClaudeTmp
	now := time.Now()
	if !prune.InWindow(c.Window, now) {
		return nil
	}
	last := prune.WindowKey(c.Window, now)
	state := filepath.Join(a.dir("claude-tmp"), "last-window")
	if done, err := claudeTmpWindowDone(state, last); err != nil || done {
		return err
	}
	failureStatePath := filepath.Join(a.dir("claude-tmp"), "failure-state.json")
	failureState, stateErr := loadClaudeTmpFailureState(failureStatePath, func(err error) {
		a.warn("claude-tmp", "состояние повтора повреждено; начинаю с пустого", "err", err)
	})
	if stateErr != nil {
		a.warn("claude-tmp", "состояние повтора", "err", stateErr)
		return stateErr
	}
	if !claudeTmpRetryAllowed(failureState, last, now) {
		return nil
	}
	r := prune.Run(ctx, c, a.claudeTmpHooks(cfg))
	a.logClaudeTmpReport(r)
	if r.Err != nil {
		return a.failClaudeTmpRound(ctx, cfg, last, failureStatePath, &failureState, r.Err)
	}
	return a.completeClaudeTmpRound(state, failureStatePath, last, &failureState)
}

func claudeTmpWindowDone(path, last string) (bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("состояние окна: %w", err)
	}
	return string(b) == last, nil
}

func (a *app) claudeTmpHooks(cfg *config.Config) prune.Hooks {
	c := cfg.ClaudeTmp
	yielder := yield.New(procRoot, func(err error) { a.warn("claude-tmp", "замер занятости", "err", err) })
	return prune.Hooks{
		Now: time.Now, SetIdle: prune.SetThreadIdle,
		Yield: func(ctx context.Context) error { return yielder.WaitSafe(ctx) },
		Live:  func(context.Context) (map[string]bool, error) { return a.claudeTmpLive(cfg) },
		References: func(ctx context.Context, since time.Time, pause func(context.Context) error) (*prune.References, error) {
			return prune.NewReferences(ctx, c.Home, c.Projects, since, pause)
		},
	}
}

func (a *app) claudeTmpLive(cfg *config.Config) (map[string]bool, error) {
	snap, err := proc.ReadSnapshot(procRoot, a.uid, time.Now())
	if snap == nil || err != nil {
		return nil, err
	}
	dead := map[string]bool{}
	trees := orphans.Plan(snap, &cfg.Orphans, map[string]time.Time{})
	for i := range trees {
		dead[strings.ToLower(trees[i].Session.ID)] = true
	}
	ids := map[string]bool{}
	for _, p := range snap.Procs {
		if p.SessionID != "" && !dead[strings.ToLower(p.SessionID)] {
			ids[strings.ToLower(p.SessionID)] = true
		}
	}
	return ids, nil
}

func (a *app) logClaudeTmpReport(r prune.Report) {
	var deleted, bytes, skipped int64
	for _, it := range r.Items {
		switch {
		case it.Removed:
			deleted++
			bytes += it.Bytes
			a.log.Info("aguard claude-tmp: удалил", "path", it.Path, "bytes", it.Bytes)
		case it.Reason == "удалил бы":
			skipped++
			a.log.Info("aguard claude-tmp: удалил бы", "path", it.Path, "bytes", it.Bytes)
		default:
			skipped++
			a.log.Info("aguard claude-tmp: пропущен", "path", it.Path, "bytes", it.Bytes, "reason", it.Reason)
		}
	}
	for _, it := range r.Outside {
		a.log.Info("aguard claude-tmp: вне claude-1000", "path", it.Path, "bytes", it.Bytes, "age", it.Age.Round(time.Hour).String(), "reason", it.Reason)
	}
	summary := fmt.Sprintf("aguard claude-tmp: проход удалено %d, освобождено %d байт, пропущено %d, по ссылкам %d", deleted, bytes, skipped, r.Linked)
	if r.Err != nil {
		a.log.Error(summary, "err", r.Err)
	} else {
		a.log.Info(summary)
	}
}

func (a *app) failClaudeTmpRound(ctx context.Context, cfg *config.Config, window, statePath string, state *claudeTmpFailureState, cause error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	now := time.Now()
	kind := claudeTmpErrorKind(cause)
	state.Window, state.LastAttempt = window, now
	shouldMail := claudeTmpShouldMail(state, window, kind, now)
	if err := writeClaudeTmpFailureState(statePath, *state); err != nil {
		a.warn("claude-tmp", "состояние повтора", "err", err)
		return cause
	}
	if shouldMail {
		a.sendClaudeTmpFailure(cfg, cause)
	}
	return cause
}

func (a *app) sendClaudeTmpFailure(cfg *config.Config, cause error) {
	alive, err := a.reg.Alive()
	if err != nil {
		a.warn("claude-tmp", "реестр сессий для письма о сбое", "err", err)
		return
	}
	if err := a.sendAlive(alive, cfg.Lead, cfg.SendAuth, claudeTmpGuideKind(), "Ночная уборка claude-tmp остановлена: "+cause.Error()); err != nil {
		a.warn("claude-tmp", "письмо лиду о сбое", "err", err)
	}
}

func (a *app) completeClaudeTmpRound(windowPath, failurePath, window string, state *claudeTmpFailureState) error {
	state.Window, state.LastAttempt = window, time.Time{}
	if err := writeClaudeTmpFailureState(failurePath, *state); err != nil {
		a.warn("claude-tmp", "состояние повтора", "err", err)
	}
	if err := record.WriteFileAtomic(windowPath, []byte(window), 0o600); err != nil {
		return fmt.Errorf("состояние окна: %w", err)
	}
	return nil
}

type claudeTmpFailureState struct {
	Window      string               `json:"window"`
	LastAttempt time.Time            `json:"last_attempt"`
	Mailed      map[string]time.Time `json:"mailed"`
}

func readClaudeTmpFailureState(path string) (claudeTmpFailureState, error) {
	var state claudeTmpFailureState
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		state.Mailed = map[string]time.Time{}
		return state, nil
	}
	if err != nil {
		return state, fmt.Errorf("прочитать состояние повтора: %w", err)
	}
	if err := json.Unmarshal(b, &state); err != nil {
		return state, fmt.Errorf("%w: разобрать состояние повтора: %w", errClaudeTmpFailureState, err)
	}
	if state.Mailed == nil {
		state.Mailed = map[string]time.Time{}
	}
	return state, nil
}

func loadClaudeTmpFailureState(path string, warn func(error)) (claudeTmpFailureState, error) {
	state, err := readClaudeTmpFailureState(path)
	if errors.Is(err, errClaudeTmpFailureState) {
		if warn != nil {
			warn(err)
		}
		return claudeTmpFailureState{Mailed: map[string]time.Time{}}, nil
	}
	return state, err
}

func writeClaudeTmpFailureState(path string, state claudeTmpFailureState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("создать каталог состояния повтора: %w", err)
	}
	b, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("кодировать состояние повтора: %w", err)
	}
	if err := record.WriteFileAtomic(path, b, 0o600); err != nil {
		return fmt.Errorf("сохранить состояние повтора: %w", err)
	}
	return nil
}

func claudeTmpErrorKind(err error) string {
	kind, _, ok := strings.Cut(err.Error(), ":")
	if !ok {
		kind = err.Error()
	}
	h := sha256.Sum256([]byte(strings.TrimSpace(kind)))
	return hex.EncodeToString(h[:8])
}

func claudeTmpRetryAllowed(state claudeTmpFailureState, window string, now time.Time) bool {
	return state.Window != window || state.LastAttempt.IsZero() || now.Sub(state.LastAttempt) >= time.Hour
}

func claudeTmpShouldMail(state *claudeTmpFailureState, window, kind string, now time.Time) bool {
	if state.Mailed == nil {
		state.Mailed = map[string]time.Time{}
	}
	key := window + ":" + kind
	if !state.Mailed[key].IsZero() {
		return false
	}
	state.Mailed[key] = now
	return true
}
