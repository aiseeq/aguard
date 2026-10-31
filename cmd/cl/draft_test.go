package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDraftAfter(t *testing.T) {
	cases := []struct {
		name string
		was  bool
		in   string
		want bool
	}{
		{"знаки", false, "abc", true},
		{"отправка", true, "abc\r", false},
		{"знаки после отправки", false, "ok\rещё", true},
		{"Ctrl+C очищает", true, "abc\x03", false},
		{"фокус окна", false, "\x1b[I\x1b[O", false},
		{"фокус не чистит", true, "\x1b[O", true},
		{"ответ о курсоре", false, "\x1b[12;40R", false},
		{"мышь", false, "\x1b[<0;10;5M", false},
		{"цвет фона от терминала", false, "\x1b]11;rgb:0000/0000/0000\x07", false},
		{"OSC с ST", false, "\x1b]10;rgb:ffff/ffff/ffff\x1b\\", false},
		{"история вверх", false, "\x1b[A", true},
		{"история в режиме приложения", false, "\x1bOB", true},
		{"стрелка влево", false, "\x1b[D", false},
		{"вставка с переводом строки", false, "\x1b[200~a\rb\x1b[201~", true},
		{"вставка без конца", false, "\x1b[200~abc", true},
		{"вставка кончается переводом строки", false, "\x1b[200~ab\r\x1b[201~", true},
		{"\\ перед Enter — перевод строки", false, "a\\\r", true},
		{"Esc перед Enter — перевод строки", false, "\x1b\r", true},
		{"одинокий Esc", false, "\x1b", false},
		{"разрезанная последовательность", false, "\x1b[", true},
		{"Backspace на пустом", false, "\x7f", false},
		{"Tab — дополнение", false, "\t", true},
		{"Alt+знак", false, "\x1bf", true},
	}
	for _, tc := range cases {
		if got := draftAfter(tc.was, []byte(tc.in)); got != tc.want {
			t.Errorf("%s: draftAfter(%t, %q) = %t, want %t", tc.name, tc.was, tc.in, got, tc.want)
		}
	}
}

func TestEmptySeen(t *testing.T) {
	c := &console{}
	c.drafted.Store(true)
	asked := time.Now()
	c.typed.Store(asked.Add(-time.Minute).UnixNano())
	c.emptySeen(asked)
	if !c.clean() {
		t.Error("поле пусто по экрану, ввода с вопроса не было — чисто")
	}
	c.drafted.Store(true)
	c.typed.Store(asked.Add(time.Second).UnixNano())
	c.emptySeen(asked)
	if c.clean() {
		t.Error("ввод после вопроса — экран мог его не застать")
	}
}

// Набор cl кончается Enter — поле снова чисто (c794bfa).
func TestTypeLineCleans(t *testing.T) {
	master, err := os.Create(filepath.Join(t.TempDir(), "pty"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = master.Close() })
	c := &console{master: master}
	c.drafted.Store(true)
	if err := c.typeLine("/compact"); err != nil {
		t.Fatal(err)
	}
	if !c.clean() {
		t.Error("cl отправил набранное — поле чисто")
	}
}
