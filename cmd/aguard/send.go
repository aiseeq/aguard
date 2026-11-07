package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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
func (a *app) sendSocket(pid int, name, socket string, auth bool, msg string) error {
	state, active, err := a.readQuiet()
	if err != nil {
		return err
	}
	if active {
		line := strings.SplitN(msg, "\n", 2)[0]
		line = string([]rune(line)[:min(len([]rune(line)), 200)])
		a.log.Info("aguard тихо: не отправил", "to", name, "msg", line, "until", state.Until, "why", state.Why)
		return fmt.Errorf("%w: %s", errQuiet, state.Until.Format(time.RFC3339))
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
	return nil
}

// sendAlive — письмо живой сессии to через её сокет; auth — с токеном peer первой строкой.
func (a *app) sendAlive(alive map[string]session.Info, to string, auth bool, msg string) error {
	in, ok := alive[to]
	if !ok || in.SocketPath == "" {
		return fmt.Errorf("сессии %s нет", to)
	}
	return a.sendSocket(in.Pid, to, in.SocketPath, auth, msg)
}
