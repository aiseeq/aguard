package screen

import (
	"fmt"

	"github.com/aiseeq/akit/proc"
	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
)

// live — настоящие Konsole и tmux. Чтение экрана Konsole по D-Bus открыто без настроек; печать в вкладку
// (sendText) закрыта настройкой Konsole EnableSecuritySensitiveDBusAPI.
type live struct {
	proc string
}

func (l *live) Tmux(socket, tty string) ([]string, error) {
	list, err := tmuxRun(socket, "list-panes", "-a", "-F", "#{pane_tty} #{pane_id}")
	if err != nil {
		return nil, err
	}
	pane, ok := tmuxPane(list, tty)
	if !ok {
		return nil, fmt.Errorf("tmux %s: нет панели с %s", socket, tty)
	}
	out, err := tmuxRun(socket, "capture-pane", "-p", "-t", pane)
	if err != nil {
		return nil, err
	}
	return splitLines(out), nil
}

const konsoleSession = "org.kde.konsole.Session"

func (l *live) Konsole(konsolePID int, tty string) ([]string, error) {
	bus, err := dbus.SessionBus()
	if err != nil {
		return nil, fmt.Errorf("D-Bus: %w", err)
	}
	service := fmt.Sprintf("org.kde.konsole-%d", konsolePID)
	node, err := introspect.Call(bus.Object(service, "/Sessions"))
	if err != nil {
		return nil, fmt.Errorf("%s: вкладки: %w", service, err)
	}
	for _, s := range node.Children {
		obj := bus.Object(service, dbus.ObjectPath("/Sessions/"+s.Name))
		var pid int32
		if err := obj.Call(konsoleSession+".processId", 0).Store(&pid); err != nil {
			return nil, fmt.Errorf("%s вкладка %s: %w", service, s.Name, err)
		}
		t, err := ttyOf(l.proc, int(pid))
		switch {
		case proc.Gone(err):
			continue // процесс вкладки вышел
		case err != nil:
			return nil, fmt.Errorf("%s вкладка %s: терминал: %w", service, s.Name, err)
		case t != tty:
			continue
		}
		var lines []string
		if err := obj.Call(konsoleSession+".getAllDisplayedTextList", 0, true).Store(&lines); err != nil {
			return nil, fmt.Errorf("%s вкладка %s: экран: %w", service, s.Name, err)
		}
		return lines, nil
	}
	return nil, fmt.Errorf("%s: нет вкладки с %s", service, tty)
}
