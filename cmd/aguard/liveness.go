package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/guide"
	"github.com/aiseeq/aguard/internal/liveness"
	"github.com/aiseeq/aguard/internal/statefile"
	"github.com/aiseeq/akit/session"
)

type livenessRecord struct {
	Seen           bool                 `json:"seen"`
	Stuck          bool                 `json:"stuck"`
	InstantReason  string               `json:"instant_reason,omitempty"`
	InstantSince   time.Time            `json:"instant_since,omitzero"`
	Notice         map[string]time.Time `json:"notice,omitempty"`
	Recovery       map[string]time.Time `json:"recovery,omitempty"`
	LoggedStuck    bool                 `json:"logged_stuck,omitempty"`
	LoggedRecovery bool                 `json:"logged_recovery,omitempty"`
}
type livenessDiskState struct {
	Watches map[string]livenessRecord `json:"watches"`
}

func (a *app) livenessCmd(args []string) error {
	cmd, rest := sub(args)
	cfg, err := a.config()
	if err != nil {
		return err
	}
	if cmd != "tick" {
		usage()
		return nil
	}
	fl := flag.NewFlagSet("liveness tick", flag.ContinueOnError)
	dry := fl.Bool("dry-run", false, "показать проверку без отправки и сохранения")
	parse(fl, rest)
	return a.livenessRound(&cfg.Liveness, *dry)
}

func (a *app) livenessRound(cfg *liveness.Config, dry bool) error {
	if cfg.Mode == liveness.ModeOff {
		return nil
	}
	now := time.Now()
	if a.now != nil {
		now = a.now()
	}
	path := filepath.Join(a.dir("liveness"), "state.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("liveness: папка состояния: %w", err)
	}
	data, err := statefile.Load(path)
	if err != nil {
		return fmt.Errorf("liveness: состояние: %w", err)
	}
	var st livenessDiskState
	if err := json.Unmarshal(data, &st); err != nil {
		return fmt.Errorf("liveness: состояние JSON: %w", err)
	}
	st.Watches = nonNilLivenessWatches(st.Watches)
	alive, registryErr := a.livenessAlive(cfg, dry)
	changed := false
	for _, w := range cfg.Watch {
		rec := st.Watches[w.Name]
		var changedOne bool
		st.Watches[w.Name], changedOne = a.livenessWatch(cfg, w, rec, now, dry, alive, registryErr)
		changed = changed || changedOne
	}
	if !changed {
		return nil
	}
	b, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("liveness: кодирование состояния: %w", err)
	}
	if err := statefile.Save(path, b); err != nil {
		return fmt.Errorf("liveness: сохранить состояние: %w", err)
	}
	return nil
}

type livenessStatus struct {
	seen, checked, stuck bool
	instant, pending     bool
	reason               string
	state                liveness.State
}

func nonNilLivenessWatches(watches map[string]livenessRecord) map[string]livenessRecord {
	if watches == nil {
		return map[string]livenessRecord{}
	}
	return watches
}

func (a *app) livenessAlive(cfg *liveness.Config, dry bool) (map[string]session.Info, error) {
	if dry || cfg.Mode != liveness.ModeAlert || a.livenessSend != nil {
		return map[string]session.Info{}, nil
	}
	return a.reg.Alive()
}

func (a *app) livenessWatch(cfg *liveness.Config, w liveness.Watch, rec livenessRecord, now time.Time, dry bool, alive map[string]session.Info, registryErr error) (livenessRecord, bool) {
	wasSeen := rec.Seen
	status := readLivenessStatus(w, rec, now)
	if !status.checked {
		return rec, false
	}
	if status.reason == "файла нет, ещё не появлялся" {
		if dry {
			fmt.Fprintf(a.out, "%s: %s\n", w.Name, status.reason)
		}
		return rec, false
	}
	rec.Seen = status.seen
	changed := rec.Seen != wasSeen
	if status.instant {
		// Сравнивается вид признака, не текст: в тексте pid и ошибка, а перезапуск по кругу не должен заново начинать отсрочку.
		if rec.InstantSince.IsZero() || livenessInstantKind(rec.InstantReason) != livenessInstantKind(status.reason) {
			rec.InstantReason, rec.InstantSince = status.reason, now
			changed = true
		}
		if now.Sub(rec.InstantSince) < w.Grace.D() {
			status.stuck, status.pending = false, true
		}
	} else if !rec.InstantSince.IsZero() || rec.InstantReason != "" {
		rec.InstantReason, rec.InstantSince = "", time.Time{}
		changed = true
	}
	if dry {
		fmt.Fprintf(a.out, "%s: stuck=%t reason=%s\n", w.Name, status.stuck, status.reason)
		return rec, false
	}
	if status.pending {
		return rec, changed
	} else if status.stuck {
		a.livenessStuck(cfg, w, &rec, status, now, alive, registryErr)
		changed = true
	} else if rec.Stuck {
		changed = a.livenessRecovered(cfg, w, &rec, now, alive, registryErr) || changed
	}
	return rec, changed
}

// livenessInstantKind — вид мгновенного признака: текст до первого числа или двоеточия («процесс pid», «битый JSON состояния»).
func livenessInstantKind(reason string) string {
	if i := strings.IndexAny(reason, "0123456789:"); i >= 0 {
		return strings.TrimSpace(reason[:i])
	}
	return reason
}

func readLivenessStatus(w liveness.Watch, rec livenessRecord, now time.Time) livenessStatus {
	b, err := os.ReadFile(w.Path)
	if errors.Is(err, os.ErrNotExist) && !rec.Seen {
		return livenessStatus{checked: true, reason: "файла нет, ещё не появлялся"}
	}
	if errors.Is(err, os.ErrNotExist) {
		return livenessStatus{seen: true, checked: true, stuck: true, instant: true, reason: "файл состояния пропал"}
	}
	if err != nil {
		return livenessStatus{seen: true, checked: true, stuck: true, instant: true, reason: fmt.Sprintf("файл состояния нечитаем: %v", err)}
	}
	var state liveness.State
	if err := json.Unmarshal(b, &state); err != nil {
		return livenessStatus{seen: true, checked: true, stuck: true, instant: true, reason: fmt.Sprintf("битый JSON состояния: %v", err)}
	}
	stuck, reason := liveness.Check(b, now, w.MaxAge.D())
	instant := strings.HasPrefix(reason, "процесс pid ")
	return livenessStatus{seen: true, checked: true, stuck: stuck, instant: instant, reason: reason, state: state}
}

func (a *app) livenessStuck(cfg *liveness.Config, w liveness.Watch, rec *livenessRecord, status livenessStatus, now time.Time, alive map[string]session.Info, registryErr error) {
	if !rec.Stuck {
		rec.Notice = nil
	}
	msg := livenessMessage(w.Name, status.reason, status.state)
	if cfg.Mode == liveness.ModeLog {
		if !rec.Stuck || !rec.LoggedStuck {
			a.log.Info("aguard liveness: написал бы", "name", w.Name, "msg", msg)
			rec.LoggedStuck = true
		}
	} else {
		sendLivenessStuck(a, cfg, w, rec, now, alive, registryErr, msg)
	}
	rec.Stuck, rec.Recovery, rec.LoggedRecovery = true, nil, false
}

func sendLivenessStuck(a *app, cfg *liveness.Config, w liveness.Watch, rec *livenessRecord, now time.Time, alive map[string]session.Info, registryErr error, msg string) bool {
	if rec.Notice == nil {
		rec.Notice = map[string]time.Time{}
	}
	changed := false
	for _, to := range w.To {
		last := rec.Notice[to]
		if !last.IsZero() && now.Sub(last) < time.Hour {
			continue
		}
		if registryErr != nil {
			a.log.Warn("aguard liveness: реестр сессий", "err", registryErr)
			continue
		}
		err := a.livenessMail(alive, cfg, to, "liveness.stuck", msg)
		if err != nil {
			a.livenessEmailResult(w.Name, to, err)
			continue
		}
		a.livenessEmailResult(w.Name, to, nil)
		rec.Notice[to] = now
		changed = true
	}
	return changed
}

func (a *app) livenessRecovered(cfg *liveness.Config, w liveness.Watch, rec *livenessRecord, now time.Time, alive map[string]session.Info, registryErr error) bool {
	if cfg.Mode == liveness.ModeLog {
		if !rec.LoggedRecovery {
			a.log.Info("aguard liveness: написал бы", "name", w.Name, "msg", fmt.Sprintf("Процесс %s восстановился.", w.Name))
		}
		rec.Stuck, rec.Notice, rec.LoggedRecovery = false, nil, true
		return true
	}
	if registryErr != nil {
		a.log.Warn("aguard liveness: реестр сессий при восстановлении", "err", registryErr)
		return false
	}
	if rec.Recovery == nil {
		rec.Recovery = map[string]time.Time{}
	}
	for _, to := range w.To {
		if !rec.Recovery[to].IsZero() {
			continue
		}
		err := a.livenessMail(alive, cfg, to, "liveness.recovered", fmt.Sprintf("Процесс %s восстановился.", w.Name))
		if err != nil {
			a.livenessEmailResult(w.Name, to, err)
			continue
		}
		a.livenessEmailResult(w.Name, to, nil)
		rec.Recovery[to] = now
	}
	if len(rec.Recovery) != len(w.To) {
		return true
	}
	rec.Stuck, rec.Notice, rec.Recovery = false, nil, nil
	return true
}

func (a *app) livenessEmailResult(name, to string, err error) {
	if a.act != nil && !errors.Is(err, errQuiet) {
		a.note("liveness", map[bool]string{true: "email_sent", false: "email_failed"}[err == nil], name, to, 0, "")
	}
	if err == nil || a.quietFailure("liveness", err) {
		return
	}
	a.log.Warn("aguard liveness: доставка письма", "name", name, "session", to, "err", err)
}

func (a *app) livenessMail(alive map[string]session.Info, cfg *liveness.Config, to, kind, msg string) error {
	if a.livenessSend != nil {
		return a.livenessSend(to, msg, kind)
	}
	return a.sendAlive(alive, to, cfg.SendAuth, livenessGuideKind(kind), msg)
}

func livenessGuideKind(kind string) guide.Kind {
	if kind == "liveness.recovered" {
		return "liveness.recovered"
	}
	return "liveness.stuck"
}

func livenessMessage(name, reason string, s liveness.State) string {
	parts := []string{fmt.Sprintf("%s стоит: %s", name, reason)}
	heartbeat, lastRow := "", ""
	if !s.Heartbeat.IsZero() {
		heartbeat = s.Heartbeat.Format(time.RFC3339)
	}
	if !s.LastRowAt.IsZero() {
		lastRow = s.LastRowAt.Format(time.RFC3339)
	}
	parts = append(parts, "heartbeat="+heartbeat, "last_row_at="+lastRow)
	parts = append(parts, fmt.Sprintf("lag_bytes=%d", s.LagBytes))
	if s.Error != "" {
		parts = append(parts, "error="+strings.TrimSpace(s.Error))
	}
	return strings.Join(parts, "; ")
}
