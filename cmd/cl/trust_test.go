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
			// found зовётся своей горутиной: ждём нужные вызовы со сроком (под нагрузкой горутина опаздывает), лишний
			// успел бы за 50 мс — его горутина запущена ещё внутри Write.
			for i := range tc.want {
				select {
				case <-found:
				case <-time.After(5 * time.Second):
					t.Fatalf("found %d раз, хотим %d", i, tc.want)
				}
			}
			time.Sleep(50 * time.Millisecond)
			if extra := len(found); extra != 0 {
				t.Errorf("found %d раз, хотим %d", tc.want+extra, tc.want)
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

// Консоль целиком: «claude» выводит окно доверия и ждёт выбор. Терминал на подъёме шлёт ответы на запросы claude
// (замер 28.10 09:37 CET под Konsole 26.08.1 — их cl до 28.10 принимал за клавиши человека), cl всё равно выбирает
// «да» (стрелка вниз и Enter) и пишет строку в журнал.
func TestConfirmTrust(t *testing.T) {
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
	if _, err := typer.WriteString("\x1b_Gi=31;OK\x1b\\\x1b[6;28;13t\x1b[?62;1;4c"); err != nil {
		t.Fatal(err)
	}
	shown.wait(t, "DOWN-OK")
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	finish()
	const want = "подтвердил доверие к /home/alice"
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
}
