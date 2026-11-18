package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Записи Claude Code 2.1.327 (cl-trust-after-live-upgrade, проба 18.11 02:0x CET в pty 200×50): окно «Adding a new
// directory» рисуется без фокуса, «❯ No, exit» встаёт отдельным кадром через ~0,45–0,5 с. Down до фокуса — подсказка
// «Esc to exit · To add it, press ↓ to Yes, then Enter», выбор остаётся на No, окно висит (chooks-master 01:44).
const (
	trustFrame327    = "testdata/trust-frame-2.1.327.raw"    // вывод до окна и само окно, ещё без «❯»
	trustFocus327    = "testdata/trust-focus-2.1.327.raw"    // кадр фокуса: «❯ No, exit»
	trustSelected327 = "testdata/trust-selected-2.1.327.raw" // кадр после Down: «❯ Yes, add it»
	trustHint327     = "testdata/trust-hint-2.1.327.raw"     // подсказка на Down до фокуса
)

// trustFiles пишет записи во временную папку: окно — с папкой dir вместо записанной.
func trustFiles(t *testing.T, dir string) (frame, focus, selected, hint string) {
	t.Helper()
	tmp := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(tmp, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	return write("frame.raw", replaceAddDir(t, readTrust(t, trustFrame327), dir)),
		write("focus.raw", readTrust(t, trustFocus327)),
		write("selected.raw", readTrust(t, trustSelected327)),
		write("hint.raw", readTrust(t, trustHint327))
}

// readKey — sh: прочесть n байт ввода за срок s; пусто — ничего не пришло.
const readKey = `key() { timeout --foreground "$2" dd bs=1 count="$1" 2>/dev/null | od -An -tx1 | tr -d ' \n'; }; `

func runTrustScript(t *testing.T, dir, script string, args ...string) (*screen, []string) {
	t.Helper()
	typer, tty, shown := outerTerminal(t)
	var mu sync.Mutex
	var notes []string
	c := openConsole(tty, tty, func(s string) { mu.Lock(); notes = append(notes, s); mu.Unlock() })
	cmd := exec.Command("sh", append([]string{"-c", "stty raw -echo; " + readKey + script}, args...)...)
	finish, err := c.start(cmd, "", false, dir)
	if err != nil {
		t.Fatal(err)
	}
	finish = trackTrustFinish(t, finish)
	if _, err := typer.WriteString("\x1b_Gi=31;OK\x1b\\\x1b[6;28;13t\x1b[?62;1;4c"); err != nil {
		t.Fatal(err)
	}
	shown.wait(t, "DONE-")
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	finish()
	mu.Lock()
	defer mu.Unlock()
	return shown, append([]string(nil), notes...)
}

// Down — только после кадра фокуса: окно без «❯» ещё не принимает клавиши.
func TestTrustDownWaitsForFocus(t *testing.T) {
	oldEnterWait := trustEnterWait
	trustEnterWait = 20 * time.Millisecond
	t.Cleanup(func() { trustEnterWait = oldEnterWait })
	dir := filepath.Join(t.TempDir(), "shared")
	frame, focus, selected, _ := trustFiles(t, dir)
	shown, notes := runTrustScript(t, dir,
		`cat "$0"; key 32 2 >/dev/null; k=$(key 3 1); [ -z "$k" ] || { echo "DONE-EARLY-$k"; exit 0; }; `+
			`cat "$1"; k=$(key 3 5); [ "$k" = 1b5b42 ] || { echo "DONE-NO-DOWN-$k"; exit 0; }; `+
			`cat "$2"; v=$(key 1 5); [ "$v" = 0d ] || { echo "DONE-NO-ENTER-$v"; exit 0; }; sleep 0.05; echo DONE-OK`,
		frame, focus, selected)
	if !shown.has("DONE-OK") {
		t.Fatalf("Down ушёл до фокуса окна или не ушёл: %q", shown.text())
	}
	if !hasNote(notes, "подтвердил доверие к "+dir) {
		t.Fatalf("нет строки о подтверждении: %q", notes)
	}
}

// Enter не принят (после него снова подсказка и «❯ No») — cl повторяет Down и Enter, а «подтвердил» пишет только
// после окна, которое ушло.
func TestTrustRetriesWhenEnterNotTaken(t *testing.T) {
	setTrustTiming(t, 2*time.Second, 20*time.Millisecond)
	dir := filepath.Join(t.TempDir(), "shared")
	frame, focus, selected, hint := trustFiles(t, dir)
	shown, notes := runTrustScript(t, dir,
		`cat "$0" "$1"; key 32 2 >/dev/null; k=$(key 3 5); [ "$k" = 1b5b42 ] || { echo "DONE-NO-DOWN-$k"; exit 0; }; `+
			`cat "$2"; v=$(key 1 5); [ "$v" = 0d ] || { echo "DONE-NO-ENTER-$v"; exit 0; }; `+
			`cat "$3" "$1"; k=$(key 3 5); [ "$k" = 1b5b42 ] || { echo "DONE-NO-RETRY-$k"; exit 0; }; `+
			`cat "$2"; v=$(key 1 5); [ "$v" = 0d ] || { echo "DONE-NO-ENTER2-$v"; exit 0; }; sleep 0.05; echo DONE-OK`,
		frame, focus, selected, hint)
	if !shown.has("DONE-OK") {
		t.Fatalf("после непринятого Enter нет повтора: %q", shown.text())
	}
	if n := countNotes(notes, "подтвердил доверие к "+dir); n != 1 {
		t.Fatalf("строк о подтверждении %d, нужна одна: %q", n, notes)
	}
}

// Курсор поля ввода после закрытия окна не означает, что выбор «Yes» не встал.
func TestTrustEnterIgnoresInputCursor(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shared")
	frame, focus, selected, _ := trustFiles(t, dir)
	cursor := filepath.Join(t.TempDir(), "cursor.raw")
	if err := os.WriteFile(cursor, []byte("\x1b[1B❯Claude Code input"), 0o600); err != nil {
		t.Fatal(err)
	}
	shown, notes := runTrustScript(t, dir,
		`cat "$0" "$1"; key 32 2 >/dev/null; k=$(key 3 5); [ "$k" = 1b5b42 ] || { echo "DONE-NO-DOWN-$k"; exit 0; }; `+
			`cat "$2"; v=$(key 1 5); [ "$v" = 0d ] || { echo "DONE-NO-ENTER-$v"; exit 0; }; `+
			`cat "$3"; k=$(key 1 1.5); [ -z "$k" ] && echo DONE-OK || echo DONE-EXTRA-KEY-$k`,
		frame, focus, selected, cursor)
	if !shown.has("DONE-OK") {
		t.Fatalf("после кадра поля ввода cl отправил лишний ввод: %q", shown.text())
	}
	if n := countNotes(notes, "подтвердил доверие к "+dir); n != 1 {
		t.Fatalf("строк о подтверждении %d, нужна одна: %q", n, notes)
	}
}

func (s *screen) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func hasNote(notes []string, want string) bool { return countNotes(notes, want) > 0 }

func countNotes(notes []string, want string) int {
	n := 0
	for _, s := range notes {
		if strings.Contains(s, want) {
			n++
		}
	}
	return n
}
