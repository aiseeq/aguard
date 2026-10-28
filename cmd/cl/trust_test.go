package main

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// trustDialog — вывод Claude Code 2.1.312 с окном доверия к /home/alice, снят tmux pipe-pane 27.10 21:5x CET.
const trustDialog = "testdata/trust-dialog.raw"

func TestTrustWatch(t *testing.T) {
	raw, err := os.ReadFile(trustDialog)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, dir string
		chunk     int
		want      int
	}{
		{"окно целиком", "/home/alice", len(raw), 1},
		{"окно по 7 байт: последовательности рвутся", "/home/alice", 7, 1},
		{"окно другой папки", "/home/other", len(raw), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			found := make(chan struct{}, 4)
			w := newTrustWatch(tc.dir, func() { found <- struct{}{} })
			for i := 0; i < len(raw); i += tc.chunk {
				if n, err := w.Write(raw[i:min(i+tc.chunk, len(raw))]); err != nil || n != min(tc.chunk, len(raw)-i) {
					t.Fatalf("Write: %d, %v", n, err)
				}
			}
			if _, err := w.Write(raw); err != nil { // окно ещё раз — второй раз не зовёт
				t.Fatal(err)
			}
			time.Sleep(50 * time.Millisecond)
			if got := len(found); got != tc.want {
				t.Errorf("found %d раз, хотим %d", got, tc.want)
			}
		})
	}
}

// Окна нет в начале вывода — дальше trustScanMax не копит.
func TestTrustWatchGivesUp(t *testing.T) {
	w := newTrustWatch("/home/alice", func() { t.Error("окна не было") })
	chunk := []byte(strings.Repeat("x", 4096))
	for range trustScanMax/len(chunk) + 2 {
		if _, err := w.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if !w.done || w.buf != nil {
		t.Errorf("после %d КБ без окна смотреть дальше незачем", trustScanMax>>10)
	}
}

func TestTerminalReplies(t *testing.T) {
	for in, want := range map[string]bool{
		"\x1bP>|tmux 3.7c\x1b\\":           true, // проба 27.10 22:01 CET под tmux
		"\x1b[?1;2;4c":                     true,
		"\x1b[?2026;2$y\x1b[?1;2;4c":       true,
		"\x1b[?5u":                         true, // флаги клавиатуры kitty
		"\x1b]11;rgb:0000/0000/0000\x1b\\": true,
		"\x1b[12;40R":                      true,
		"\x1b[I":                           true, // фокус
		// замер 28.10 09:37 CET: Konsole 26.08.1 на подъёме claude 2.1.312
		"\x1bP>|Konsole 26.08.1\x1b\\\x1b[?0u\x1b[?62;1;4c": true,
		"\x1b_Gi=31;OK\x1b\\\x1b[6;28;13t\x1b[?62;1;4c":     true,
		"\x1b[B":        false, // стрелка — человек
		"\x1b[97u":      false, // клавиша в протоколе kitty
		"x":             false,
		"\x1b[?1;2;4cx": false,
	} {
		if got := terminalReplies([]byte(in)); got != want {
			t.Errorf("%q: %v, хотим %v", in, got, want)
		}
	}
}

func TestInputShape(t *testing.T) {
	for in, want := range map[string]string{
		"\x1b[?1;2;4c\x1b[99;1z": `"\x1b[99;1z"`,
		"\x1b[?1;2;4cx":          "клавиши",
		"пароль\r":               "клавиши",
		"":                       "клавиши",
	} {
		if got := inputShape([]byte(in)); got != want {
			t.Errorf("%q: %s, хотим %s", in, got, want)
		}
	}
}

// Консоль целиком: «claude» выводит окно доверия и ждёт выбор. Терминал ответил на запросы, человек молчит — cl
// выбирает «да» (стрелка вниз и Enter) и пишет строку в журнал; человек нажал клавишу после подъёма — окно его.
func TestConfirmTrust(t *testing.T) {
	for _, human := range []bool{false, true} {
		t.Run(map[bool]string{false: "человек молчит", true: "человек у терминала"}[human], func(t *testing.T) {
			typer, tty, shown := outerTerminal(t)
			var mu sync.Mutex
			var notes []string
			c := openConsole(tty, tty, func(s string) { mu.Lock(); notes = append(notes, s); mu.Unlock() })
			if c == nil {
				t.Fatal("терминал не распознан")
			}
			cmd := exec.Command("sh", "-c",
				`stty -echo; cat "$0"; IFS= read -r x; case "$x" in *"$(printf '\033[B')") echo DOWN-OK;; *) echo "OTHER:$x";; esac`,
				trustDialog)
			finish, err := c.start(cmd, "/home/alice")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := typer.WriteString("\x1b[?1;2;4c"); err != nil { // ответ терминала — не человек
				t.Fatal(err)
			}
			if human {
				if _, err := typer.WriteString("x"); err != nil {
					t.Fatal(err)
				}
				time.Sleep(2 * trustSettle)
				if _, err := typer.WriteString("\r"); err != nil { // человек сам закончил выбор
					t.Fatal(err)
				}
				shown.wait(t, "OTHER:")
			} else {
				shown.wait(t, "DOWN-OK")
			}
			if err := cmd.Wait(); err != nil {
				t.Fatal(err)
			}
			finish()
			want := "подтвердил доверие к /home/alice"
			if human {
				want = "человек у терминала (ввод клавиши) — не подтверждаю"
			}
			noted := func() bool {
				mu.Lock()
				defer mu.Unlock()
				return slices.ContainsFunc(notes, func(s string) bool { return strings.Contains(s, want) })
			}
			for deadline := time.Now().Add(2 * time.Second); !noted() && time.Now().Before(deadline); {
				time.Sleep(20 * time.Millisecond)
			}
			if !noted() {
				mu.Lock()
				t.Errorf("в журнале нет %q: %q", want, notes)
				mu.Unlock()
			}
		})
	}
}
