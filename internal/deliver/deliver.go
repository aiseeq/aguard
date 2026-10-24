// Package deliver кладёт сообщение в сокет сессии Claude Code
// (/run/user/<uid>/cc-socks/<pid>.sock) тем же кадром, что шлёт SendMessage.
package deliver

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"strings"
	"time"
)

// FromName — имя отправителя в конверте по умолчанию.
const FromName = "tmpguard"

// Envelope — конверт межсессионного сообщения от отправителя from. from-mode="bypass" совпадает
// с классом наших сессий (--dangerously-skip-permissions), поэтому сообщение
// не задерживается на диалоге подтверждения.
func Envelope(from, body string) string {
	return `<cross-session-message from-name="` + html.EscapeString(from) + `" from-mode="bypass">` +
		"\n" + body + "\n</cross-session-message>"
}

type userFrame struct {
	Type     string  `json:"type"`
	UUID     string  `json:"uuid"`
	Message  message `json:"message"`
	Priority string  `json:"priority"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Frames — строки для сокета: необязательная auth и сам кадр от from.
func Frames(from, body, token, uuid string) ([]byte, error) {
	var out []byte
	if token != "" {
		a, err := json.Marshal(map[string]string{"type": "auth", "token": token})
		if err != nil {
			return nil, err
		}
		out = append(append(out, a...), '\n')
	}
	// закрывающий тег внутри текста сломал бы разбор конверта
	body = strings.ReplaceAll(body, "</cross-session-message>", "</cross-session-message >")
	f, err := json.Marshal(userFrame{
		Type: "user", UUID: uuid, Priority: "next",
		Message: message{Role: "user", Content: Envelope(from, body)},
	})
	if err != nil {
		return nil, err
	}
	return append(append(out, f...), '\n'), nil
}

// NewUUID — случайный UUID v4.
func NewUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Send пишет сообщение от FromName в сокет и закрывает запись.
func Send(socket, token, body string) error { return SendFrom(FromName, socket, token, body) }

// SendFrom пишет сообщение от from в сокет и закрывает запись.
func SendFrom(from, socket, token, body string) error {
	data, err := Frames(from, body, token, NewUUID())
	if err != nil {
		return err
	}
	conn, err := net.DialTimeout("unix", socket, 3*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	if _, err := conn.Write(data); err != nil {
		return fmt.Errorf("запись в %s: %w", socket, err)
	}
	if uc, ok := conn.(*net.UnixConn); ok {
		if err := uc.CloseWrite(); err != nil {
			return err
		}
	}
	// как в проверенном send_test.py: дождаться, пока сторона сессии закроет
	// соединение (ответа протокол не шлёт); таймаут здесь не ошибка
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _ = io.Copy(io.Discard, conn)
	return nil
}
