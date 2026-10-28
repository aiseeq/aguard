package main

import (
	"fmt"

	"github.com/aiseeq/akit/deliver"
	"github.com/aiseeq/akit/session"
)

// sendSocket — письмо сессии name (pid, сокет) от aguard; auth — с токеном peer первой строкой.
func (a *app) sendSocket(pid int, name, socket string, auth bool, msg string) error {
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
