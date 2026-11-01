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
	raw316, err := os.ReadFile("testdata/trust-dialog-2.1.316.raw")
	if err != nil {
		t.Fatal(err)
	}
	for _, chunk := range []int{len(raw316), 7} {
		found := make(chan struct{}, 1)
		w := newTrustWatch("/home/alice", func() { found <- struct{}{} })
		for i := 0; i < len(raw316); i += chunk {
			if _, err := w.Write(raw316[i:min(i+chunk, len(raw316))]); err != nil {
				t.Fatal(err)
			}
		}
		select {
		case <-found:
		case <-time.After(5 * time.Second):
			t.Fatalf("окно 2.1.316 по %d байт не найдено", chunk)
		}
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

func TestTrustWatchStopsSelecting(t *testing.T) {
	w := newTrustWatch("/home/alice", nil)
	w.selectMu.Lock()
	w.selecting = true
	w.selectBuf = []byte("прежний вывод")
	w.selectMu.Unlock()

	w.stopSelecting()
	if _, err := w.Write([]byte(strings.Repeat("x", trustScanMax*2))); err != nil {
		t.Fatal(err)
	}
	if w.selecting || w.selectBuf != nil {
		t.Fatalf("после отключения копится вывод: selecting=%v, selectBuf=%d байт", w.selecting, len(w.selectBuf))
	}
}

// TestConfirmTrustSelects проверяет, что ответы Konsole на запросы Claude Code (случай 28.10) проходят через cl,
// а Enter нажимается только после перерисовки с выбранным «Yes».
func TestConfirmTrustSelects(t *testing.T) {
	typer, tty, shown := outerTerminal(t)
	var mu sync.Mutex
	var notes []string
	c := openConsole(tty, tty, func(s string) { mu.Lock(); notes = append(notes, s); mu.Unlock() })
	if c == nil {
		t.Fatal("терминал не распознан")
	}
	cmd := exec.Command("sh", "-c",
		`stty raw -echo; cat "$0"; dd bs=1 count=32 of=/dev/null 2>/dev/null; dd bs=1 count=3 of=/dev/null 2>/dev/null; cat "$1"; y=$(dd bs=1 count=1 2>/dev/null | od -An -tu1 | tr -d ' '); if [ "$y" = 13 ]; then echo ENTER-OK; else echo OTHER; fi`,
		"testdata/trust-dialog-2.1.316.raw", "testdata/trust-down-2.1.316.raw")
	finish, err := c.start(cmd, "/home/alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := typer.WriteString("\x1b_Gi=31;OK\x1b\\\x1b[6;28;13t\x1b[?62;1;4c"); err != nil {
		t.Fatal(err)
	}
	shown.wait(t, "ENTER-OK")
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	finish()
	waitTrustNote(t, &mu, &notes, "подтвердил доверие к /home/alice")
}

// TestConfirmTrustDoesNotSelect проверяет, что без перерисовки выбора cl не отправляет Enter.
func TestConfirmTrustDoesNotSelect(t *testing.T) {
	oldWait := trustSelectWait
	trustSelectWait = 100 * time.Millisecond
	t.Cleanup(func() { trustSelectWait = oldWait })

	_, tty, shown := outerTerminal(t)
	var mu sync.Mutex
	var notes []string
	c := openConsole(tty, tty, func(s string) { mu.Lock(); notes = append(notes, s); mu.Unlock() })
	if c == nil {
		t.Fatal("терминал не распознан")
	}
	cmd := exec.Command("sh", "-c",
		`stty raw -echo; cat "$0"; dd bs=1 count=3 of=/dev/null 2>/dev/null; if timeout --foreground 1 dd bs=1 count=1 2>/dev/null | od -An -tu1 | tr -d ' ' | grep -q '^13$'; then echo ENTER-OK; else echo NO-ENTER; fi`,
		"testdata/trust-dialog-2.1.316.raw")
	finish, err := c.start(cmd, "/home/alice")
	if err != nil {
		t.Fatal(err)
	}
	shown.wait(t, "NO-ENTER")
	if shown.has("ENTER-OK") {
		t.Fatal("claude получил Enter без подтверждения выбора «Yes»")
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	finish()
	waitTrustNote(t, &mu, &notes, "окно доверия к /home/alice: выбор не встал на «Yes» за 100ms, Enter не нажат")
}

func waitTrustNote(t *testing.T, mu *sync.Mutex, notes *[]string, want string) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		mu.Lock()
		found := slices.ContainsFunc(*notes, func(s string) bool { return strings.Contains(s, want) })
		mu.Unlock()
		if found {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Errorf("в журнале нет %q: %q", want, *notes)
}
