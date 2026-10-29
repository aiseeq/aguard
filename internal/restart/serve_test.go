package restart

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/linesock"
)

func ask(t *testing.T, socket, line string) Answer {
	t.Helper()
	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, line+"\n"); err != nil {
		t.Fatal(err)
	}
	b, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var a Answer
	if err := json.Unmarshal(b, &a); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestServe(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "aguard", "aguard.sock")
	ln, err := linesock.Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, ln, func(r Request) Answer {
			return Answer{V: Version, Verdict: Verdict{Restart: r.Pid == 7}, Name: r.Name}
		}, slog.New(slog.DiscardHandler))
	}()
	if a := ask(t, socket, `{"v":1,"pid":7,"name":"x-master"}`); !a.Restart || a.Name != "x-master" {
		t.Errorf("ответ %+v", a)
	}
	if a := ask(t, socket, `не json`); a.Error == "" || a.Restart {
		t.Errorf("кривой вопрос: %+v", a)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestAnswerVersion(t *testing.T) {
	cfg, cc := Default(t.TempDir(), t.TempDir()), DefaultCompact()
	a := NewJudge(t.TempDir()).Answer(&cfg, &cc, Request{V: Version + 1, Pid: 1}, time.Now())
	if a.Error == "" || a.Restart || a.V != Version {
		t.Errorf("чужая версия: %+v", a)
	}
}

func TestAnswerCompactOff(t *testing.T) {
	cfg, cc := Default(t.TempDir(), t.TempDir()), DefaultCompact()
	cc.Mode = CompactOff
	a := NewJudge(t.TempDir()).Answer(&cfg, &cc, Request{V: Version, Pid: 1 << 22}, time.Now())
	if a.Compact || len(a.CompactAgainst) == 0 || a.CompactAgainst[0].Code != "off" {
		t.Errorf("сжатие выключено: %+v", a)
	}
}
