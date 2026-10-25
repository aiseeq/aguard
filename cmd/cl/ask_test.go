package main

import (
	"bufio"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/restart"
)

// fakeAguard — сокет, который на каждый вопрос отвечает строкой reply (пусто — молчит, пока клиент не уйдёт).
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
			if _, err := bufio.NewReader(conn).ReadBytes('\n'); err == nil && reply != "" {
				_, _ = conn.Write([]byte(reply + "\n"))
			}
			if reply == "" {
				time.Sleep(3 * time.Second) // дольше срока клиента
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
	if note := compactNote(a, nil); note != "; сжатие: нет ()" {
		t.Errorf("сжатие: %q", note)
	}
}

func TestAskAguardNoAnswer(t *testing.T) {
	for name, socket := range map[string]string{
		"нет сокета":   filepath.Join(t.TempDir(), "нет.sock"),
		"чужая версия": fakeAguard(t, `{"v":2,"restart":true}`),
		"ошибка":       fakeAguard(t, `{"v":1,"error":"конфиг aguard: битый"}`),
		"молчит":       fakeAguard(t, ""),
	} {
		start := time.Now()
		if a, err := askAguard(socket, restart.NewRequest(7, "", "")); err == nil || a.Restart {
			t.Errorf("%s: ждали «нет ответа», получили %+v", name, a)
		}
		if d := time.Since(start); d > aguardTimeout+time.Second {
			t.Errorf("%s: ждал %v, срок %v", name, d, aguardTimeout)
		}
	}
	if note := compactNote(restart.Answer{}, net.ErrClosed); note != "" {
		t.Errorf("сжатие без ответа: %q", note)
	}
}
