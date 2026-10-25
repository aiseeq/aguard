package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/restart"
)

// aguardTimeout — сколько ждать ответа: тик cl не должен висеть на мёртвом сокете.
const aguardTimeout = 2 * time.Second

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
func askAguard(socket string, req restart.Request) (restart.Answer, error) {
	conn, err := net.DialTimeout("unix", socket, aguardTimeout)
	if err != nil {
		return restart.Answer{}, fmt.Errorf("aguard: %w", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(aguardTimeout)); err != nil {
		return restart.Answer{}, fmt.Errorf("aguard: срок: %w", err)
	}
	line, err := json.Marshal(req)
	if err != nil {
		return restart.Answer{}, fmt.Errorf("aguard: вопрос: %w", err)
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return restart.Answer{}, fmt.Errorf("aguard: запись: %w", err)
	}
	b, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return restart.Answer{}, fmt.Errorf("aguard: ответ: %w", err)
	}
	var a restart.Answer
	if err := json.Unmarshal(b, &a); err != nil {
		return restart.Answer{}, fmt.Errorf("aguard: ответ не JSON: %w", err)
	}
	switch {
	case a.V != restart.Version:
		return restart.Answer{}, fmt.Errorf("aguard: версия протокола %d, cl знает %d", a.V, restart.Version)
	case a.Error != "":
		return restart.Answer{}, errors.New("aguard: " + a.Error)
	}
	return a, nil
}

// compactNote — вердикт aguard о сжатии для --cl-check: « сжатие: да» или « сжатие: нет (коды доводов)».
func compactNote(a restart.Answer, err error) string {
	switch {
	case err != nil:
		return ""
	case a.Compact:
		return fmt.Sprintf("; сжатие: да, контекст %d тыс.", a.Tokens/1000)
	}
	codes := make([]string, len(a.CompactAgainst))
	for i, r := range a.CompactAgainst {
		codes[i] = r.Code
	}
	return "; сжатие: нет (" + strings.Join(codes, ", ") + ")"
}
