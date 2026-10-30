// Ночной сторож: сигнализирует, если Сон, Дайджест или подъём после окна доверия не состоялись.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/cllog"
	"github.com/aiseeq/aguard/internal/night"
	"github.com/aiseeq/akit/session"
)

// nightSent — сигнал: когда и каким адресатам уже дошёл. Канал — письмо сессии: лиду и night.also_to; Telegram снят
// 28.10 (пользователь на сигналы в группе Дайджеста: «не надо так»).
type nightSent struct {
	At       time.Time            `json:"at"`
	Channels map[string]time.Time `json:"channels"`
}

// nightState — отправленное по ключу сигнала (night.Signal.Key).
type nightState map[string]nightSent

// nightFailure — канал, по которому сигнал не дошёл в этот проход.
type nightFailure struct {
	Key, Channel string
	Err          error
}

// dispatchNight шлёт каждый сигнал каждому адресату to и его собственным (Signal.To), которому он ещё не дошёл;
// дошедшему не повторяется, неудачный — в следующий проход.
func dispatchNight(state nightState, signals []night.Signal, to []string, now time.Time, send func(string, string) error) (nightState, []nightFailure) {
	var failed []nightFailure
	for _, s := range signals {
		rec := state[s.Key]
		if rec.Channels == nil {
			rec.Channels = map[string]time.Time{}
		}
		for _, channel := range nightAddressees(to, s.To) {
			if _, ok := rec.Channels[channel]; ok {
				continue
			}
			if err := send(channel, s.Text); err != nil {
				failed = append(failed, nightFailure{s.Key, channel, err})
				continue
			}
			rec.Channels[channel] = now
		}
		if len(rec.Channels) > 0 {
			rec.At = now
			state[s.Key] = rec
		}
	}
	return state, failed
}

// nightAddressees — общие адресаты и свои адресаты сигнала без повторов, в этом порядке.
func nightAddressees(to, own []string) []string {
	out := slices.Clone(to)
	for _, name := range own {
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// nightCmd — `aguard night tick [-dry-run]`.
func (a *app) nightCmd(ctx context.Context, args []string) error {
	cmd, rest := sub(args)
	cfg, err := a.config()
	if err != nil {
		return err
	}
	if cmd != "tick" {
		usage()
		return nil
	}
	fl := flag.NewFlagSet("night tick", flag.ContinueOnError)
	dry := fl.Bool("dry-run", false, "показать сигналы, не отправлять и не сохранять")
	parse(fl, rest)
	return a.nightRound(ctx, &cfg.Night, *dry)
}

// nightRound — один проход: факты, сигналы, доставка, состояние; dry и mode=log — только журнал «написал бы».
func (a *app) nightRound(_ context.Context, cfg *night.Config, dry bool) error {
	if cfg.Mode == night.ModeOff {
		return nil
	}
	now := time.Now().In(time.Local)
	facts := a.nightFacts(cfg, now)
	signals := night.Check(now, *cfg, facts)
	signals = append(signals, a.nightFallback(cfg, now, facts, dry)...)
	if dry {
		a.nightDry(cfg, now, facts, signals)
		return nil
	}
	if cfg.Mode == night.ModeLog { // учёт «написал бы» — только в памяти: переход на alert шлёт сегодняшнее по-настоящему
		if a.nightLogged == nil {
			a.nightLogged = map[string]bool{}
		}
		for _, s := range signals {
			if !a.nightLogged[s.Key] {
				a.nightLogged[s.Key] = true
				a.log.Info("aguard night: написал бы", "key", s.Key, "msg", s.Text)
				a.nightNote("would_send", s.Key, "", 0, "")
			}
		}
		return nil
	}
	statePath := filepath.Join(a.dir("night"), "state.json")
	state := a.nightSent
	if state == nil {
		var err error
		if state, err = loadNightState(statePath); err != nil {
			return err
		}
		a.nightSent = state
	}
	for k, sent := range state {
		if now.Sub(sent.At) > 7*24*time.Hour {
			delete(state, k)
		}
	}
	alive, regErr := a.reg.Alive()
	if regErr != nil {
		a.log.Warn("aguard night: реестр сессий", "err", regErr)
		a.nightNote("registry_failed", "", "", 0, "")
	}
	// состояние в памяти демона: сбой записи state.json не повторяет дошедшее каждую минуту
	state, failures := dispatchNight(state, signals, cfg.Recipients(), now, func(to, msg string) error {
		if regErr != nil {
			return fmt.Errorf("реестр сессий: %w", regErr)
		}
		err := a.nightEmail(cfg, alive, to, msg)
		if a.act != nil {
			a.note("night", map[bool]string{true: "email_sent", false: "email_failed"}[err == nil], "", to, 0, "")
		}
		return err
	})
	for _, f := range failures {
		a.log.Warn("aguard night: канал доставки", "channel", f.Channel, "key", f.Key, "err", f.Err)
	}
	if err := saveNightState(statePath, state); err != nil {
		return err
	}
	return nil
}

// nightDry — печать прохода без отправки.
func (a *app) nightDry(cfg *night.Config, now time.Time, facts night.Facts, signals []night.Signal) {
	fmt.Fprintf(a.out, "Сон %s: маркер %s; Дайджест %s: дата %s в %s; окон доверия за сутки %d\n", cfg.DreamBy,
		map[bool]string{true: "есть", false: "нет"}[facts.DreamMarker], cfg.DigestBy,
		map[bool]string{true: "есть", false: "нет"}[strings.Contains(facts.DigestText, now.Format("2006-01-02"))],
		cfg.DigestMarker, recentTrust(facts.TrustEvents, now))
	fmt.Fprintf(a.out, "адресаты: %s; Сон — ещё %s\n", strings.Join(cfg.Recipients(), ", "), strings.Join(cfg.DreamTo, ", "))
	for _, e := range facts.TrustEvents {
		if now.Sub(e.At) <= 24*time.Hour {
			fmt.Fprintf(a.out, "  %s %s %s подтверждено=%v поднялась=%s\n", e.At.Format("15:04:05"), e.Name, e.Dir, e.Confirmed,
				map[bool]string{true: "нет", false: e.Recovered.Format("15:04:05")}[e.Recovered.IsZero()])
		}
	}
	for _, s := range signals {
		fmt.Fprintf(a.out, "  → %s: %s\n", s.Key, s.Text)
	}
}

// nightFallback — запасной Сон (night.dream_fallback): запуск, если пора, наблюдение за unit и сигналы о нём. dry —
// только печать, mode=log — журнал «запустил бы» раз за дату. Сбой — в журнал aguard: сигналы о Сне идут и без него.
func (a *app) nightFallback(cfg *night.Config, now time.Time, facts night.Facts, dry bool) []night.Signal {
	signals, err := a.fallbackStep(cfg, now, facts, dry)
	if err != nil {
		a.log.Warn("aguard night: запасной Сон", "err", err)
		a.nightNote("dream_fallback_failed", err.Error(), "", 0, "")
	}
	return signals
}

func (a *app) fallbackStep(cfg *night.Config, now time.Time, facts night.Facts, dry bool) ([]night.Signal, error) {
	if cfg.DreamFallback == "" {
		return nil, nil
	}
	path := filepath.Join(a.dir("night"), "fallback.json")
	if a.nightFb == nil {
		state, err := loadFallbackState(path)
		if err != nil {
			return nil, err
		}
		a.nightFb = state
	}
	date := now.Format("2006-01-02")
	if dry {
		rec, launched := a.nightFb[date]
		fmt.Fprintf(a.out, "запасной Сон: `%s` в %s без маркера, unit %s; за %s %s\n", cfg.DreamFallback, cfg.DreamFallbackBy,
			night.FallbackUnit(now), date, map[bool]string{true: "запущен в " + rec.At.Format("15:04"), false: "не запускался"}[launched])
		if !launched && night.FallbackDue(now, *cfg, facts) {
			fmt.Fprintf(a.out, "  → запустил бы сейчас\n")
		}
		return nil, nil
	}
	if a.units == nil {
		return nil, errors.New("запасной Сон: нет запускателя unit")
	}
	for d, rec := range a.nightFb {
		if now.Sub(rec.At) > 7*24*time.Hour {
			delete(a.nightFb, d)
		}
	}
	save := func(s fallbackState) error { return saveFallbackState(path, s) }
	signals, notes, err := fallbackRound(cfg, now, facts, a.nightFb, a.units, a.home, save, cfg.Mode == night.ModeLog)
	for _, n := range notes {
		if cfg.Mode == night.ModeLog {
			if a.nightLogged == nil {
				a.nightLogged = map[string]bool{}
			}
			if a.nightLogged["dream-fallback-would:"+date] {
				continue
			}
			a.nightLogged["dream-fallback-would:"+date] = true
		}
		a.log.Info("aguard night: запасной Сон", "msg", n)
		a.nightNote("dream_fallback", n, "", 0, "")
	}
	if err != nil {
		return signals, err
	}
	if len(notes) > 0 && cfg.Mode != night.ModeLog {
		if err := save(a.nightFb); err != nil {
			return signals, err
		}
	}
	return signals, nil
}

// recentTrust — окон доверия за последние сутки.
func recentTrust(events []cllog.TrustEvent, now time.Time) int {
	n := 0
	for _, e := range events {
		if now.Sub(e.At) <= 24*time.Hour {
			n++
		}
	}
	return n
}

// nightFacts — маркеры Сна и Дайджеста и окна доверия из журнала cl; сбой чтения — в журнал aguard.
func (a *app) nightFacts(cfg *night.Config, now time.Time) night.Facts {
	facts := night.Facts{}
	marker := night.DreamPath(*cfg, now)
	_, err := os.Stat(marker)
	facts.DreamMarker = err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		a.log.Warn("aguard night: маркер Сна", "path", marker, "err", err)
	}
	if b, err := os.ReadFile(cfg.DigestMarker); err == nil {
		facts.DigestText = string(b)
	} else if !errors.Is(err, os.ErrNotExist) {
		a.log.Warn("aguard night: маркер Дайджеста", "path", cfg.DigestMarker, "err", err)
	}
	f, err := os.Open(cllog.Path(a.home))
	if err == nil {
		facts.TrustEvents, _, err = cllog.TrustEvents(f)
		if closeErr := f.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
		facts.TrustEvents = openTrust(facts.TrustEvents, clRunning)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		a.log.Warn("aguard night: журнал cl", "err", err)
	}
	return facts
}

// openTrust — окна, чей cl ещё жив: консоль закрыли — claude не стоит на окне (ревью 28.10: 3 закрытые пробы
// trustprobe 27.10 считались бы «не поднялась»). Строка без pid cl — проверить нечем, остаётся.
func openTrust(events []cllog.TrustEvent, running func(int) bool) []cllog.TrustEvent {
	var out []cllog.TrustEvent
	for _, e := range events {
		if e.PID == 0 || running(e.PID) {
			out = append(out, e)
		}
	}
	return out
}

// clRunning — процесс pid есть и это cl (pid не занят чужим процессом).
func clRunning(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	return err == nil && strings.TrimSpace(string(b)) == "cl"
}

// nightNote — учёт срабатывания; учёта нет (тест) — ничего: нулевой activity.Recorder ничего не делает.
func (a *app) nightNote(kind, what, target string, value int64, unit string) {
	a.note("night", kind, what, target, value, unit)
}

// nightEmail — письмо сессии to.
func (a *app) nightEmail(cfg *night.Config, alive map[string]session.Info, to, msg string) error {
	if err := a.sendAlive(alive, to, cfg.SendAuth, msg); err != nil {
		return err
	}
	a.log.Info("aguard night: написал", "session", to, "msg", msg)
	return nil
}

// loadNightState читает состояние; файла нет — пустое.
func loadNightState(path string) (nightState, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nightState{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("состояние night: чтение: %w", err)
	}
	var state nightState
	if err := json.Unmarshal(b, &state); err != nil {
		return nil, fmt.Errorf("состояние night: %w", err)
	}
	if state == nil {
		state = nightState{}
	}
	return state, nil
}

// saveNightState пишет состояние.
func saveNightState(path string, state nightState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("состояние night: папка: %w", err)
	}
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("состояние night: кодирование: %w", err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("состояние night: %w", err)
	}
	return nil
}
