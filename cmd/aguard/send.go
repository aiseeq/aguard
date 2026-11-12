package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/guide"
	"github.com/aiseeq/akit/deliver"
	"github.com/aiseeq/akit/session"
)

var errQuiet = errors.New("aguard тихий режим")

type quietState struct {
	Until time.Time `json:"until"`
	Why   string    `json:"why"`
	By    string    `json:"by"`
	At    time.Time `json:"at"`
}

func (a *app) quietPath() string { return filepath.Join(a.dir("quiet"), "state.json") }

func (a *app) readQuiet() (quietState, bool, error) {
	path := a.quietPath()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return quietState{}, false, nil
	}
	if err != nil {
		return quietState{}, false, fmt.Errorf("чтение %s: %w", path, err)
	}
	var state quietState
	if err := json.Unmarshal(b, &state); err != nil {
		return quietState{}, false, fmt.Errorf("чтение %s: %w", path, err)
	}
	if state.Until.IsZero() || strings.TrimSpace(state.Why) == "" {
		return quietState{}, false, fmt.Errorf("чтение %s: неполное состояние", path)
	}
	return state, time.Now().Before(state.Until), nil
}

// quietFailure учитывает отказ тихого режима один раз у вызывающего наблюдателя.
func (a *app) quietFailure(observer string, err error) bool {
	if !errors.Is(err, errQuiet) {
		return false
	}
	a.note(observer, "quiet", "", "", 0, "")
	return true
}

// sendSocket — письмо сессии name (pid, сокет) от aguard; auth — с токеном peer первой строкой.
func (a *app) sendSocket(pid int, name, socket string, auth bool, kind guide.Kind, msg string) error {
	a.warnOtherGuideKind(kind)
	state, active, err := a.readQuiet()
	if err != nil {
		return err
	}
	if active {
		line, _, _ := strings.Cut(msg, "\n")
		a.log.Info("aguard тихо: не отправил", "kind", kind, "to", name, "msg", line, "until", state.Until, "why", state.Why)
		return fmt.Errorf("%w: %s", errQuiet, state.Until.Format(time.RFC3339))
	}
	return a.deliverSocket(pid, name, socket, auth, kind, msg)
}

// sendUrgent — аварийное письмо живой сессии to (sigguard, снятие потолком памяти): идёт сквозь тишину, иначе
// повод теряется насовсем; файл тишины читается только ради строки журнала, его поломка письма не держит.
func (a *app) sendUrgent(alive map[string]session.Info, to string, auth bool, kind guide.Kind, msg string) error {
	a.warnOtherGuideKind(kind)
	in, err := aliveSocket(alive, to)
	if err != nil {
		return err
	}
	if state, active, err := a.readQuiet(); err != nil {
		a.log.Warn("aguard тихо: состояние не прочитано, аварийное письмо отправляю", "to", to, "err", err)
	} else if active {
		line, _, _ := strings.Cut(msg, "\n")
		a.log.Info("aguard тихо: аварийное письмо сквозь тишину", "kind", kind, "to", to, "msg", line, "until", state.Until, "why", state.Why)
	}
	return a.deliverSocket(in.Pid, to, in.SocketPath, auth, kind, msg)
}

// deliverSocket — отправка без проверки тишины: её решают sendSocket и sendUrgent.
func (a *app) deliverSocket(pid int, name, socket string, auth bool, kind guide.Kind, msg string) error {
	_, ok := guide.Lookup(kind)
	if !ok {
		return fmt.Errorf("неизвестный вид письма %q", kind)
	}
	token := ""
	if auth {
		var err error
		if token, err = a.reg.Token(pid); err != nil {
			return fmt.Errorf("токен %s: %w", name, err)
		}
	}
	if err := deliver.Send("aguard", socket, token, msg); err != nil {
		return fmt.Errorf("сокет %s: %w", name, err)
	}
	a.log.Info("aguard: письмо доставлено", "kind", kind, "to", name)
	return nil
}

func (a *app) warnOtherGuideKind(kind guide.Kind) {
	if strings.HasSuffix(string(kind), ".other") {
		a.log.Warn("aguard: неизвестный вид уведомления, отправляю как прочее", "kind", kind)
	}
}

func (a *app) warnOtherGuideInput(kind guide.Kind, source string) {
	if strings.HasSuffix(string(kind), ".other") {
		a.log.Warn("aguard: неизвестное исходное значение уведомления, отправляю как прочее", "kind", kind, "source", source)
	}
}

// sendAlive — письмо живой сессии to через её сокет; auth — с токеном peer первой строкой.
func (a *app) sendAlive(alive map[string]session.Info, to string, auth bool, kind guide.Kind, msg string) error {
	in, err := aliveSocket(alive, to)
	if err != nil {
		return err
	}
	return a.sendSocket(in.Pid, to, in.SocketPath, auth, kind, msg)
}

// aliveSocket — живая сессия to с сокетом.
func aliveSocket(alive map[string]session.Info, to string) (session.Info, error) {
	in, ok := alive[to]
	if !ok || in.SocketPath == "" {
		return session.Info{}, fmt.Errorf("сессии %s нет", to)
	}
	return in, nil
}
