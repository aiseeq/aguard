// Package linesock — локальный сокет «строка JSON вопроса → строка JSON ответа»: сокет restart для cl
// и сокет моментального состояния диска (io). Сервер, клиент и срок соединения — здесь, один раз.
package linesock

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Timeout — срок на соединение: чтение вопроса, ответ и его запись. Медленный клиент не держит сервер. С запасом на
// большой ответ (сводки io за месяц — 14 МБ JSON) после 1,5 с ожидания цикла наблюдателя под нагрузкой.
const Timeout = 5 * time.Second

// maxConns — сколько вопросов разбирается сразу; остальные ждут в очереди accept.
const maxConns = 8

// Listen открывает сокет: папка только для владельца, старый сокет от прошлого запуска удаляется, сам
// сокет — 0600.
func Listen(socket string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		return nil, fmt.Errorf("папка сокета: %w", err)
	}
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("старый сокет: %w", err)
	}
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return nil, fmt.Errorf("сокет %s: %w", socket, err)
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		return nil, errors.Join(fmt.Errorf("права сокета %s: %w", socket, err), ln.Close())
	}
	return ln, nil
}

// Serve отвечает до отмены ctx: answer получает строку вопроса (без перевода строки) и отдаёт ответ
// для json.Marshal. Ошибки соединений — в журнал с пометкой who.
func Serve(ctx context.Context, ln net.Listener, answer func(line []byte) any, lg *slog.Logger, who string) error {
	stop := context.AfterFunc(ctx, func() {
		if err := ln.Close(); err != nil {
			lg.Warn(who+": закрытие сокета", "err", err)
		}
	})
	defer stop()
	slots := make(chan struct{}, maxConns)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := ln.Accept()
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return fmt.Errorf("приём вопроса: %w", err)
		}
		slots <- struct{}{}
		wg.Go(func() { // не больше maxConns сразу: слот берётся до запуска
			defer func() { <-slots }()
			if err := handle(conn, answer); err != nil {
				lg.Warn(who+": вопрос", "err", err)
			}
		})
	}
}

// handle — один вопрос и один ответ строкой JSON.
func handle(conn net.Conn, answer func([]byte) any) (err error) {
	defer func() { err = errors.Join(err, conn.Close()) }()
	if err := conn.SetDeadline(time.Now().Add(Timeout)); err != nil {
		return fmt.Errorf("срок соединения: %w", err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("чтение вопроса: %w", err)
	}
	b, err := json.Marshal(answer(bytes.TrimSuffix(line, []byte("\n"))))
	if err != nil {
		return fmt.Errorf("ответ: %w", err)
	}
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("запись ответа: %w", err)
	}
	return nil
}

// ErrNoServer — сокета нет или его никто не слушает: сервер не запущен.
var ErrNoServer = errors.New("сервер не слушает сокет")

// Ask — вопрос req строкой JSON и ответ в answer; весь обмен — за timeout. Сокета нет или отказ в
// соединении — ошибка с ErrNoServer.
func Ask(socket string, timeout time.Duration, req, answer any) error {
	conn, err := net.DialTimeout("unix", socket, timeout)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("%w: %w", ErrNoServer, err)
	}
	if err != nil {
		return fmt.Errorf("сокет %s: %w", socket, err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("срок: %w", err)
	}
	line, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("вопрос: %w", err)
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("запись: %w", err)
	}
	b, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("ответ: %w", err)
	}
	if err := json.Unmarshal(b, answer); err != nil {
		return fmt.Errorf("ответ не JSON: %w", err)
	}
	return nil
}
