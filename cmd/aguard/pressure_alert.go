package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aiseeq/aguard/internal/record"
	"github.com/aiseeq/aguard/internal/yield"
)

const pressureAlertDelay = time.Hour
const pressureAlertRepeat = 24 * time.Hour

var pressureAlertInitMu sync.Mutex

type pressureFaultState struct {
	Since  time.Time       `json:"since"`
	Sent   time.Time       `json:"sent"`
	Active map[string]bool `json:"active,omitempty"`
}

type pressureAlertState struct {
	Faults map[string]pressureFaultState `json:"faults"`
	Sent   map[string]time.Time          `json:"sent,omitempty"`
}

type pressureAlerts struct {
	mu   sync.Mutex
	path string
	now  func() time.Time
	send func(string) error
	warn func(error)
}

func newPressureAlerts(path string, now func() time.Time, send func(string) error, warn func(error)) *pressureAlerts {
	return &pressureAlerts{path: path, now: now, send: send, warn: warn}
}

func pressureErrorKind(err error) string {
	message := err.Error()
	if _, detail, ok := strings.Cut(message, ": "); ok {
		message = detail
	}
	fields := strings.Fields(message)
	kind := message
	if len(fields) != 0 {
		kind = strings.ToLower(fields[0])
		switch kind {
		case "vmstat", "meminfo":
			if len(fields) > 1 {
				kind += " " + strings.Trim(fields[1], `:"'`)
			}
		}
	}
	h := sha256.Sum256([]byte(kind))
	return hex.EncodeToString(h[:8])
}

func (p *pressureAlerts) load() (pressureAlertState, error) {
	var state pressureAlertState
	b, err := os.ReadFile(p.path)
	if errors.Is(err, os.ErrNotExist) {
		return pressureAlertState{Faults: map[string]pressureFaultState{}, Sent: map[string]time.Time{}}, nil
	}
	if err != nil {
		return state, fmt.Errorf("прочитать состояние ошибок давления: %w", err)
	}
	if err := json.Unmarshal(b, &state); err != nil {
		return state, fmt.Errorf("разобрать состояние ошибок давления: %w", err)
	}
	if state.Faults == nil {
		state.Faults = map[string]pressureFaultState{}
	}
	if state.Sent == nil {
		state.Sent = map[string]time.Time{}
	}
	for key, value := range state.Faults {
		if !value.Sent.IsZero() && state.Sent[key].IsZero() {
			state.Sent[key] = value.Sent
		}
	}
	return state, nil
}

func (p *pressureAlerts) save(state pressureAlertState) error {
	if err := os.MkdirAll(filepath.Dir(p.path), 0o700); err != nil {
		return fmt.Errorf("создать каталог состояния ошибок давления: %w", err)
	}
	b, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("кодировать состояние ошибок давления: %w", err)
	}
	if err := record.WriteFileAtomic(p.path, b, 0o600); err != nil {
		return fmt.Errorf("сохранить состояние ошибок давления: %w", err)
	}
	return nil
}

func (p *pressureAlerts) report(err error) {
	if err != nil && p.warn != nil {
		p.warn(err)
	}
}

func (p *pressureAlerts) fault(scope string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	state, loadErr := p.load()
	if loadErr != nil {
		p.report(loadErr)
		return
	}
	for staleKey, stale := range state.Faults {
		if len(stale.Active) == 0 {
			delete(state.Faults, staleKey)
		}
	}
	now, key := p.now(), pressureErrorKind(err)
	item, ok := state.Faults[key]
	if !ok {
		item.Active = map[string]bool{}
	}
	if item.Active == nil {
		item.Active = map[string]bool{}
	}
	if item.Since.IsZero() {
		item.Since = now
	}
	item.Active[scope] = true
	if sent := state.Sent[key]; !sent.IsZero() && now.Sub(sent) < pressureAlertRepeat {
		state.Faults[key] = item
		p.report(p.save(state))
		return
	}
	if now.Sub(item.Since) < pressureAlertDelay {
		state.Faults[key] = item
		p.report(p.save(state))
		return
	}
	state.Sent[key] = now
	state.Faults[key] = item
	if err := p.save(state); err != nil {
		p.report(err)
		return
	}
	if p.send != nil {
		p.report(p.send(fmt.Sprintf("Замер занятости машины не удаётся больше часа (%s): %v", scope, err)))
	}
}

func (p *pressureAlerts) recovered(scope string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	state, err := p.load()
	if err != nil {
		p.report(err)
		return
	}
	changed := false
	for key, value := range state.Faults {
		wasActive := value.Active[scope]
		delete(value.Active, scope)
		if !wasActive && len(value.Active) != 0 {
			continue
		}
		changed = true
		if len(value.Active) == 0 {
			delete(state.Faults, key)
		} else {
			state.Faults[key] = value
		}
	}
	if !changed {
		return
	}
	if len(state.Faults) == 0 && len(state.Sent) == 0 {
		if err := os.Remove(p.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			p.report(fmt.Errorf("удалить пустое состояние pressure: %w", err))
		}
		return
	}
	p.report(p.save(state))
}

func (a *app) pressureYielder(scope string, warn func(error), lead string, auth bool) *yield.Yielder {
	pressureAlertInitMu.Lock()
	defer pressureAlertInitMu.Unlock()
	if a.pressureAlerts == nil {
		a.pressureAlerts = newPressureAlerts(filepath.Join(a.stateDir, "pressure-errors.json"), time.Now, func(msg string) error {
			alive, err := a.reg.Alive()
			if err != nil {
				a.warn("pressure", "реестр сессий для письма лиду", "err", err)
				return err
			}
			if err := a.sendAlive(alive, lead, auth, "pressure.failure", msg); err != nil {
				a.warn("pressure", "письмо лиду о долгой ошибке", "err", err)
				return err
			}
			return nil
		}, func(err error) { a.warn("pressure", "состояние/оповещение", "err", err) })
	}
	alerts := a.pressureAlerts
	return yield.NewObserved(procRoot, warn, func(err error) { alerts.fault(scope, err) }, func() { alerts.recovered(scope) })
}
