package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// aguardVersion — версия протокола вопроса к aguard (~/git/aguard, internal/restart). Другая версия в ответе — то же,
// что нет ответа: не перезапускать.
const aguardVersion = 1

// aguardTimeout — сколько ждать ответа: тик cl не должен висеть на мёртвом сокете.
const aguardTimeout = 2 * time.Second

// aguardRequest — вопрос cl о своей сессии.
type aguardRequest struct {
	V           int    `json:"v"`
	PID         int    `json:"pid"`
	Name        string `json:"name"`
	ArgsProblem string `json:"args_problem"`
}

// aguardReason — довод против перезапуска в ответе aguard.
type aguardReason struct {
	Code string `json:"code"`
	Text string `json:"text"`
}

// aguardAnswer — вердикт aguard и то, что нужно cl для журнала и перезапуска.
type aguardAnswer struct {
	V         int            `json:"v"`
	Restart   bool           `json:"restart"`
	Fresh     bool           `json:"fresh"`
	Against   []aguardReason `json:"against"`
	Key       string         `json:"key"`
	Mode      string         `json:"mode"`
	SessionID string         `json:"session_id"`
	Name      string         `json:"name"`
	Running   string         `json:"running"`
	Installed string         `json:"installed"`
	Error     string         `json:"error"`
}

// aguardSocket — сокет aguard: $XDG_RUNTIME_DIR/aguard/aguard.sock.
func aguardSocket() string {
	runtime := os.Getenv("XDG_RUNTIME_DIR")
	if runtime == "" {
		runtime = os.TempDir()
	}
	return filepath.Join(runtime, "aguard", "aguard.sock")
}

// askAguard — вердикт aguard по сессии. Ошибка — ответа нет: сокета нет, срок вышел, другая версия, aguard не
// разобрал вопрос.
func askAguard(socket string, req aguardRequest) (aguardAnswer, error) {
	req.V = aguardVersion
	conn, err := net.DialTimeout("unix", socket, aguardTimeout)
	if err != nil {
		return aguardAnswer{}, fmt.Errorf("aguard: %w", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(aguardTimeout)); err != nil {
		return aguardAnswer{}, fmt.Errorf("aguard: срок: %w", err)
	}
	line, err := json.Marshal(req)
	if err != nil {
		return aguardAnswer{}, fmt.Errorf("aguard: вопрос: %w", err)
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return aguardAnswer{}, fmt.Errorf("aguard: запись: %w", err)
	}
	b, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return aguardAnswer{}, fmt.Errorf("aguard: ответ: %w", err)
	}
	var a aguardAnswer
	if err := json.Unmarshal(b, &a); err != nil {
		return aguardAnswer{}, fmt.Errorf("aguard: ответ не JSON: %w", err)
	}
	switch {
	case a.V != aguardVersion:
		return aguardAnswer{}, fmt.Errorf("aguard: версия протокола %d, cl знает %d", a.V, aguardVersion)
	case a.Error != "":
		return aguardAnswer{}, errors.New("aguard: " + a.Error)
	}
	return a, nil
}

// compareLine — сверка своего вердикта с ответом aguard на время перехода (строка журнала сверки).
func compareLine(name string, pid int, own verdict, a aguardAnswer, askErr error) string {
	head := fmt.Sprintf("%s pid=%d ", name, pid)
	switch {
	case askErr != nil:
		return head + "нет ответа: cl=" + own.key() + " " + askErr.Error()
	case a.Key == own.key():
		return head + "совпало: " + own.key()
	}
	return head + "расходится: cl=" + own.key() + " aguard=" + a.Key
}
