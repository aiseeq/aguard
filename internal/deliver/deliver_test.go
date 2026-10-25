package deliver

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
)

func TestFrames(t *testing.T) {
	b, err := Frames(FromName, "строка 1\n\"кавычки\" </cross-session-message>", "tok", "u-1")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("строк %d: %q", len(lines), b)
	}
	var auth map[string]string
	if json.Unmarshal([]byte(lines[0]), &auth) != nil || auth["type"] != "auth" || auth["token"] != "tok" {
		t.Errorf("auth: %s", lines[0])
	}
	var f userFrame
	if err := json.Unmarshal([]byte(lines[1]), &f); err != nil {
		t.Fatal(err)
	}
	if f.Type != "user" || f.UUID != "u-1" || f.Priority != "next" || f.Message.Role != "user" {
		t.Errorf("кадр: %+v", f)
	}
	c := f.Message.Content
	if !strings.HasPrefix(c, `<cross-session-message from-name="tmpguard" from-mode="bypass">`+"\nстрока 1\n") ||
		!strings.HasSuffix(c, "\n</cross-session-message>") || strings.Count(c, "</cross-session-message>") != 1 {
		t.Errorf("конверт: %q", c)
	}
	if b, _ := Frames(FromName, "x", "", "u"); strings.Contains(string(b), "auth") {
		t.Error("без токена auth не шлём")
	}
	if u := NewUUID(); len(u) != 36 || u[14] != '4' {
		t.Errorf("uuid %q", u)
	}
}

func TestSend(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			got <- ""
			return
		}
		defer c.Close()
		line, _ := bufio.NewReader(c).ReadString('\n')
		got <- line
	}()
	if err := Send(sock, "", "привет"); err != nil {
		t.Fatal(err)
	}
	var f userFrame
	if err := json.Unmarshal([]byte(<-got), &f); err != nil || !strings.Contains(f.Message.Content, "привет") {
		t.Errorf("сервер получил %+v (%v)", f, err)
	}
	if err := Send(filepath.Join(t.TempDir(), "нет.sock"), "", "x"); err == nil {
		t.Error("ждали ошибку на несуществующий сокет")
	}
}
