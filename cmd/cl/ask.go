package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/linesock"
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
	var a restart.Answer
	if err := linesock.Ask(socket, aguardTimeout, req, &a); err != nil {
		return restart.Answer{}, fmt.Errorf("aguard: %w", err)
	}
	switch {
	case a.V != restart.Version:
		return restart.Answer{}, fmt.Errorf("aguard: версия протокола %d, cl знает %d", a.V, restart.Version)
	case a.Error != "":
		return restart.Answer{}, errors.New("aguard: " + a.Error)
	}
	return a, nil
}

// compactNote — вердикты aguard о сжатии и сбросе для --cl-check: «; сжатие: да…» или «; сжатие: нет (коды доводов)»,
// так же «; сброс: …».
func compactNote(a restart.Answer) string {
	return idleNote("сжатие", a.Compact, a.Tokens, a.CompactAgainst) + idleNote("сброс", a.Reset, a.Tokens, a.ResetAgainst)
}

func idleNote(what string, yes bool, tokens int64, against []restart.Reason) string {
	if yes {
		return fmt.Sprintf("; %s: да, контекст %d тыс.", what, tokens/1000)
	}
	codes := make([]string, len(against))
	for i, r := range against {
		codes[i] = r.Code
	}
	return "; " + what + ": нет (" + strings.Join(codes, ", ") + ")"
}
