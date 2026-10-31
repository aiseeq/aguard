package main

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/restart"
)

// fakeAguard — сокет, который на каждый вопрос отвечает строкой reply. Молчащий сервер — linesock.TestAskSilentServerDeadline.
func fakeAguard(t *testing.T, reply string) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "aguard.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			if _, err := bufio.NewReader(conn).ReadBytes('\n'); err == nil {
				_, _ = conn.Write([]byte(reply + "\n"))
			}
			_ = conn.Close()
		}
	}()
	return socket
}

func TestAskAguard(t *testing.T) {
	socket := fakeAguard(t, `{"v":1,"restart":true,"fresh":false,"key":"true false","mode":"log","name":"x-master"}`)
	a, err := askAguard(socket, restart.NewRequest(7, "x-master", ""))
	if err != nil || !a.Restart || a.Key != "true false" {
		t.Fatalf("ответ %+v, %v", a, err)
	}
	if note := compactNote(a); note != "; сжатие: нет (); сброс: нет ()" {
		t.Errorf("сжатие: %q", note)
	}
}

func TestAskAguardNoAnswer(t *testing.T) {
	for name, socket := range map[string]string{
		"нет сокета":   filepath.Join(t.TempDir(), "нет.sock"),
		"чужая версия": fakeAguard(t, `{"v":2,"restart":true}`),
		"ошибка":       fakeAguard(t, `{"v":1,"error":"конфиг aguard: битый"}`),
	} {
		start := time.Now()
		if a, err := askAguard(socket, restart.NewRequest(7, "", "")); err == nil || a.Restart {
			t.Errorf("%s: ждали «нет ответа», получили %+v", name, a)
		}
		if d := time.Since(start); d > aguardTimeout+time.Second {
			t.Errorf("%s: ждал %v, срок %v", name, d, aguardTimeout)
		}
	}
}

// cl говорит aguard, чисто ли поле (Request.Clean), а «поле пусто» в ответе чистит его, если человек с вопроса ничего
// не вводил (c794bfa).
func TestAskClean(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "aguard.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	reqs := make(chan restart.Request, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			var req restart.Request
			if b, err := bufio.NewReader(conn).ReadBytes('\n'); err == nil && json.Unmarshal(b, &req) == nil {
				reqs <- req
				_, _ = conn.Write([]byte(`{"v":1,"field_empty":true}` + "\n"))
			}
			_ = conn.Close()
		}
	}()
	c := &console{}
	l := launcher{aguard: socket, name: "x-master", console: c}
	if _, err := l.ask(7, "", true); err != nil {
		t.Fatal(err)
	}
	if req := <-reqs; !req.Clean || !req.Typed {
		t.Errorf("поле чисто — clean в вопросе: %+v", req)
	}
	c.drafted.Store(true)
	c.typed.Store(time.Now().Add(-time.Minute).UnixNano())
	if _, err := l.ask(7, "", true); err != nil {
		t.Fatal(err)
	}
	if req := <-reqs; req.Clean {
		t.Errorf("в поле мог быть текст — clean нет: %+v", req)
	}
	if !c.clean() {
		t.Error("aguard видел поле пустым, человек с вопроса не вводил — чисто")
	}
}
