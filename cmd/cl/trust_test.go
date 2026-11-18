package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/cllog"
)

const trustDialog = "testdata/trust-dialog.raw"

func readTrust(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func setTrustTiming(t *testing.T, wait, retryPause time.Duration) {
	t.Helper()
	oldWait, oldPause, oldFocusWait, oldEnterWait := trustSelectWait, trustRetryPause, trustFocusWait, trustEnterWait
	trustSelectWait, trustRetryPause, trustFocusWait, trustEnterWait = wait, retryPause, retryPause, retryPause
	t.Cleanup(func() {
		trustSelectWait, trustRetryPause, trustFocusWait, trustEnterWait = oldWait, oldPause, oldFocusWait, oldEnterWait
	})
}

func trackTrustFinish(t *testing.T, finish func()) func() {
	t.Helper()
	var once sync.Once
	tracked := func() { once.Do(finish) }
	t.Cleanup(tracked)
	return tracked
}

func firstScreen(raw []byte) []byte {
	if before, _, ok := bytes.Cut(raw, []byte("Claude\x1b[19GCode")); ok {
		return before
	}
	return raw
}

func firstAddDir(t *testing.T, raw []byte) string {
	t.Helper()
	windows, _ := scanTrustWindows(raw, "", nil)
	for _, win := range windows {
		if win.key == "foreign" {
			return win.dir
		}
	}
	t.Fatal("в записи нет окна Adding a new directory")
	return ""
}

func replaceAddDir(t *testing.T, raw []byte, dir string) []byte {
	t.Helper()
	old := firstAddDir(t, raw)
	return bytes.ReplaceAll(raw, []byte(old), []byte(dir))
}

func recordedYesRedraw(t *testing.T) []byte {
	t.Helper()
	raw := readTrust(t, "testdata/trust-dialog-resume-2.1.326.raw")
	frame := []byte("\x1b[1B\x1b[38;5;153m❯\x1b[4GYes, add it")
	if !bytes.Contains(raw, frame) {
		t.Fatal("цветной кадр выбора «Yes, add it» отсутствует в записи")
	}
	return frame
}

func TestTrustWatchHomeUsesUnifiedScanner(t *testing.T) {
	for _, tc := range []struct {
		name, fixture, home string
		chunk               int
		reader              bool
	}{
		{"2.1.312", trustDialog, "/home/alice", 4096, false},
		{"2.1.316 fragmented", "testdata/trust-dialog-2.1.316.raw", "/home/alice", 7, false},
		{"screen reader", "testdata/trust-dialog-sr-2.1.322.raw", "/home/alice", 4096, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := readTrust(t, tc.fixture)
			windows, _ := scanTrustWindows(raw, tc.home, nil)
			if len(windows) != 1 || windows[0].key != "home" || windows[0].dir != tc.home {
				t.Fatalf("окно home не распознано общим scanner: %#v", windows)
			}
			if windows[0].reader != tc.reader {
				t.Fatalf("режим экранного чтеца: got %v want %v", windows[0].reader, tc.reader)
			}
			wrong, _ := scanTrustWindows(raw, "/home/other", nil)
			if len(wrong) != 0 {
				t.Fatalf("чужая workspace-папка распознана как home: %#v", wrong)
			}
			w := newTrustWatch(tc.home, nil, func(trustWindow) {})
			defer w.close()
			for i := 0; i < len(raw); i += tc.chunk {
				if n, err := w.Write(raw[i:min(i+tc.chunk, len(raw))]); err != nil || n != min(tc.chunk, len(raw)-i) {
					t.Fatalf("Write: %d, %v", n, err)
				}
			}
		})
	}
}

func TestTrustWatchResumeHistoryIgnored(t *testing.T) {
	raw := readTrust(t, "testdata/trust-dialog-resume-2.1.326.raw")
	addDir := filepath.Join(t.TempDir(), "other")
	raw = replaceAddDir(t, raw, addDir)
	windows, complete := scanTrustWindows(raw, "", trustDirSet([]string{addDir}, ""))
	if !complete || len(windows) != 1 || windows[0].key != "add-dir" || windows[0].dir != addDir {
		t.Fatalf("startup-окно и граница REPL: complete=%v windows=%#v", complete, windows)
	}
	banner := bytes.Index(raw, []byte("Claude\x1b[19GCode"))
	if banner < 0 {
		t.Fatal("в resume-записи не найден баннер REPL")
	}
	history, historyComplete := scanTrustWindows(raw[banner:], "", trustDirSet([]string{addDir}, ""))
	if !historyComplete || len(history) != 0 {
		t.Fatalf("текст истории принят за окно доверия: complete=%v windows=%#v", historyComplete, history)
	}
}

func TestTrustWatchColoredAddDir(t *testing.T) {
	raw := readTrust(t, "testdata/trust-dialog-resume-2.1.326.raw")
	addDir := filepath.Join(t.TempDir(), "other")
	raw = replaceAddDir(t, raw, addDir)
	windows, _ := scanTrustWindows(raw, "", trustDirSet([]string{addDir}, ""))
	if len(windows) != 1 || windows[0].key != "add-dir" || windows[0].dir != addDir {
		t.Fatalf("цветное окно --add-dir не распознано: %#v", windows)
	}
	if !bytes.Contains(raw, []byte("\x1b[1B\x1b[38;5;153m❯\x1b[4GYes, add it")) {
		t.Fatal("цветной выбор не сохранён в fixture")
	}
}

func TestTrustWatchWrappedPathStaysForUser(t *testing.T) {
	raw := readTrust(t, "testdata/trust-dialog-wrap-38-2.1.326.raw")
	home := t.TempDir()
	full := filepath.Join(home, "long-parent-component-that-wraps", "otherEVIL")
	raw = bytes.ReplaceAll(raw, []byte("/tmp/claude-tmp/rv-adddir.j926/otherEVIL"), []byte(full))
	windows, _ := scanTrustWindows(raw, home, trustDirSet([]string{full}, home))
	if len(windows) != 1 || windows[0].key != "foreign" || !windows[0].ambiguous || !windows[0].wrapped {
		t.Fatalf("путь, перенесённый при ширине 38, должен остаться пользователю: %#v", windows)
	}
	var notes []string
	(&console{note: func(s string) { notes = append(notes, s) }}).handleTrustWindow(windows[0], nil)
	if len(notes) != 1 || !strings.Contains(notes[0], "путь перенесён на строки, оставляю окно пользователю") {
		t.Fatalf("перенесённый путь не записан в журнал: %q", notes)
	}
}

func TestTrustCursorGapPreservesSpace(t *testing.T) {
	raw := readTrust(t, "testdata/trust-dialog-space-2.1.326.raw")
	root := "/tmp/claude-tmp/rv-adddir.j926/h2"
	own := map[string]bool{
		root + "/.cache/worktrees":  true,
		root + "/.cache/claude-tmp": true,
	}
	windows, _ := scanTrustWindows(raw, "", own)
	want := root + "/.cache/work trees"
	var found *trustWindow
	for i := range windows {
		if windows[i].dir == want {
			found = &windows[i]
			break
		}
	}
	if found == nil || found.key != "foreign" || found.ambiguous {
		t.Fatalf("путь с курсорным пробелом ошибочно совпал со своей папкой: got=%#v want=%q", windows, want)
	}
	var notes []string
	(&console{note: func(s string) { notes = append(notes, s) }}).handleTrustWindow(*found, nil)
	if len(notes) != 1 || !strings.Contains(notes[0], "оставляю окно пользователю") {
		t.Fatalf("чужая папка должна попасть в журнал без ввода: %q", notes)
	}
}

func TestTrustAmbiguousCursorPathIsLogged(t *testing.T) {
	raw := readTrust(t, "testdata/trust-dialog-space-2.1.326.raw")
	raw = bytes.Replace(raw, []byte("work\x1b[48Gtrees"), []byte("work\x1b[2Gtrees"), 1)
	root := "/tmp/claude-tmp/rv-adddir.j926/h2"
	windows, _ := scanTrustWindows(raw, "", map[string]bool{root + "/.cache/worktrees": true})
	var ambiguous *trustWindow
	for i := range windows {
		if windows[i].ambiguous {
			ambiguous = &windows[i]
			break
		}
	}
	if ambiguous == nil || ambiguous.key != "foreign" {
		t.Fatalf("путь с перезаписью не признан неоднозначным: %#v", windows)
	}
	var notes []string
	(&console{note: func(s string) { notes = append(notes, s) }}).handleTrustWindow(*ambiguous, nil)
	if len(notes) != 1 || !strings.Contains(notes[0], "путь неоднозначен") {
		t.Fatalf("неоднозначное окно не попало в журнал: %q", notes)
	}
}

func TestTrustMarkAmbiguousCursorVerticalMove(t *testing.T) {
	for _, move := range []byte{'A', 'B'} {
		t.Run(string(move), func(t *testing.T) {
			b := trustScreenBuilder{lineHasText: true, column: 12, columnKnown: true}
			b.markAmbiguous()
			if !bytes.Contains(b.text, []byte{0}) || b.columnKnown {
				t.Fatalf("вертикальный сдвиг внутри строки не пометил текст неоднозначным: %#v", b)
			}
		})
	}
}

func TestTrustWrappedPathStaysForUser(t *testing.T) {
	fixtures := []string{
		"testdata/trust-dialog-wrap-38-2.1.326.raw",
		"testdata/trust-dialog-space-74.raw",
		"testdata/trust-dialog-space-75.raw",
		"testdata/trust-dialog-space-76.raw",
		"testdata/trust-dialog-space-77.raw",
		"testdata/trust-dialog-space-80.raw",
		"testdata/trust-dialog-space-sr-76.raw",
		"testdata/trust-dialog-space2-76.raw",
		"testdata/trust-dialog-space2-77.raw",
		"testdata/trust-dialog-space3-76.raw",
	}
	for _, fixture := range fixtures {
		t.Run(filepath.Base(fixture), func(t *testing.T) {
			home := t.TempDir()
			raw := readTrust(t, fixture)
			for _, source := range []string{
				"/home/aisee/.cache/claude-tmp/aguard-briefs/rv-adddir.j926/h2",
				"/home/aisee/.cache/claude-tmp/aguard-briefs/rv-adddir.j926/h3",
			} {
				raw = bytes.ReplaceAll(raw, []byte(source), []byte(home))
			}
			own := filepath.Join(home, ".cache", "worktrees")
			if err := os.MkdirAll(own, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(home, ".cache", "claude-tmp"), 0o700); err != nil {
				t.Fatal(err)
			}
			allowed := trustDirSet([]string{own, filepath.Join(home, ".cache", "claude-tmp")}, home)
			windows, _ := scanTrustWindows(raw, home, allowed)
			if len(windows) != 1 || windows[0].key != "foreign" || !windows[0].ambiguous || !windows[0].wrapped {
				t.Fatalf("перенесённый путь должен остаться пользователю: %#v", windows)
			}
			var notes []string
			(&console{note: func(s string) { notes = append(notes, s) }}).handleTrustWindow(windows[0], nil)
			if len(notes) != 1 || !strings.Contains(notes[0], "путь перенесён на строки, оставляю окно пользователю") {
				t.Fatalf("перенесённое окно не записано в журнал: %q", notes)
			}
		})
	}
}

func TestTrustSelectionPromptRetry(t *testing.T) {
	setTrustTiming(t, 5*time.Second, 20*time.Millisecond)
	if raw := readTrust(t, "testdata/trust-dialog-retry-live3.raw"); !trustRetryPrompt(compactText(escSeq.ReplaceAll(raw, nil))) {
		t.Fatal("частичная перерисовка подсказки без начальной E не распознана")
	}
	for _, fixture := range []string{"testdata/trust-dialog-retry-live5.raw", "testdata/trust-dialog-retry-live8.raw", "testdata/trust-dialog-retry-live3.raw"} {
		t.Run(filepath.Base(fixture), func(t *testing.T) {
			raw := readTrust(t, fixture)
			promptAt := bytes.Index(raw, []byte("Esc to exit"))
			if promptAt < 0 {
				promptAt = bytes.Index(raw, []byte("sc to exit"))
			}
			if promptAt < 0 {
				t.Fatal("запись без подсказки о повторном Down")
			}
			promptEnd := bytes.Index(raw[promptAt:], []byte("\r\r\n"))
			if promptEnd < 0 {
				t.Fatal("подсказка не завершена переводом строки")
			}
			prompt := raw[promptAt : promptAt+promptEnd+4]
			dialog := firstScreen(readTrust(t, "testdata/trust-dialog-2.1.326.raw"))
			dir := filepath.Join(t.TempDir(), "own directory")
			dialog = replaceAddDir(t, dialog, dir)
			dialogFile, promptFile, selectedFile := filepath.Join(t.TempDir(), "dialog"), filepath.Join(t.TempDir(), "prompt"), filepath.Join(t.TempDir(), "selected")
			for name, contents := range map[string][]byte{dialogFile: dialog, promptFile: prompt, selectedFile: recordedYesRedraw(t)} {
				if err := os.WriteFile(name, contents, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			typer, tty, shown := outerTerminal(t)
			var mu sync.Mutex
			var notes []string
			c := openConsole(tty, tty, func(s string) { mu.Lock(); notes = append(notes, s); mu.Unlock() })
			cmd := exec.Command("sh", "-c",
				`stty raw -echo; cat "$0"; dd bs=1 count=32 of=/dev/null 2>/dev/null; k=$(dd bs=1 count=3 2>/dev/null | od -An -tx1 | tr -d ' \n'); [ "$k" = 1b5b42 ] || { echo FIRST-DOWN-$k; exit 0; }; cat "$1"; k=$(dd bs=1 count=3 2>/dev/null | od -An -tx1 | tr -d ' \n'); [ "$k" = 1b5b42 ] || { echo RETRY-DOWN-$k; exit 0; }; cat "$2"; v=$(dd bs=1 count=1 2>/dev/null | od -An -tx1 | tr -d ' \n'); [ "$v" = 0d ] && sleep 0.05 && echo RETRY-ENTER-OK`,
				dialogFile, promptFile, selectedFile)
			finish, err := c.start(cmd, "", false, dir)
			if err != nil {
				t.Fatal(err)
			}
			finish = trackTrustFinish(t, finish)
			if _, err := typer.WriteString("\x1b_Gi=31;OK\x1b\\\x1b[6;28;13t\x1b[?62;1;4c"); err != nil {
				t.Fatal(err)
			}
			for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
				if shown.has("RETRY-ENTER-OK") {
					break
				}
			}
			if !shown.has("RETRY-ENTER-OK") {
				shown.mu.Lock()
				got := shown.buf.String()
				shown.mu.Unlock()
				t.Fatalf("повторное подтверждение не завершилось; экран: %q", got)
			}
			if err := cmd.Wait(); err != nil {
				t.Fatal(err)
			}
			finish()
			waitTrustNote(t, &mu, &notes, "подтвердил доверие к "+dir)
		})
	}
}

func TestTrustSelectionRetriesAtMostThreeTimes(t *testing.T) {
	setTrustTiming(t, 750*time.Millisecond, 20*time.Millisecond)
	raw := readTrust(t, "testdata/trust-dialog-retry-live5.raw")
	promptAt := bytes.Index(raw, []byte("Esc to exit"))
	if promptAt < 0 {
		t.Fatal("в live5 нет подсказки для повторного Down")
	}
	promptEnd := bytes.Index(raw[promptAt:], []byte("\r\r\n"))
	if promptEnd < 0 {
		t.Fatal("подсказка live5 не завершена переводом строки")
	}
	prompt := raw[promptAt : promptAt+promptEnd+4]
	dialog := firstScreen(readTrust(t, "testdata/trust-dialog-2.1.326.raw"))
	dir := filepath.Join(t.TempDir(), "retry-limit")
	dialog = replaceAddDir(t, dialog, dir)
	dialogFile, promptFile := filepath.Join(t.TempDir(), "dialog"), filepath.Join(t.TempDir(), "prompt")
	if err := os.WriteFile(dialogFile, dialog, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(promptFile, prompt, 0o600); err != nil {
		t.Fatal(err)
	}
	typer, tty, shown := outerTerminal(t)
	var mu sync.Mutex
	var notes []string
	c := openConsole(tty, tty, func(s string) { mu.Lock(); notes = append(notes, s); mu.Unlock() })
	cmd := exec.Command("sh", "-c",
		`stty raw -echo; cat "$0"; dd bs=1 count=32 of=/dev/null 2>/dev/null; k=$(dd bs=1 count=3 2>/dev/null | od -An -tx1 | tr -d ' \n'); [ "$k" = 1b5b42 ] || { echo BAD-FIRST-$k; exit 0; }; i=0; while [ "$i" -lt 3 ]; do cat "$1"; k=$(timeout --foreground 3 dd bs=1 count=3 2>/dev/null | od -An -tx1 | tr -d ' \n'); [ "$k" = 1b5b42 ] || { echo BAD-RETRY-$i-$k; exit 0; }; i=$((i+1)); done; cat "$1"; k=$(timeout --foreground 1 dd bs=1 count=1 2>/dev/null | od -An -tx1 | tr -d ' \n'); [ -z "$k" ] && echo MAX-RETRIES || echo EXTRA-DOWN-$k`,
		dialogFile, promptFile)
	finish, err := c.start(cmd, "", false, dir)
	if err != nil {
		t.Fatal(err)
	}
	finish = trackTrustFinish(t, finish)
	if _, err := typer.WriteString("\x1b_Gi=31;OK\x1b\\\x1b[6;28;13t\x1b[?62;1;4c"); err != nil {
		t.Fatal(err)
	}
	shown.wait(t, "MAX-RETRIES")
	if shown.has("EXTRA-DOWN") {
		t.Fatal("после четырёх подсказок ушёл пятый Down")
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	finish()
	waitTrustNote(t, &mu, &notes, "выбор не встал на «Yes» за 750ms")
	mu.Lock()
	retryCount := 0
	for _, note := range notes {
		if strings.Contains(note, "повтор Down") {
			retryCount++
		}
	}
	mu.Unlock()
	if retryCount != 3 {
		t.Fatalf("в журнале %d повторов, ожидалось 3: %q", retryCount, notes)
	}
}

func TestTrustWatchFindsWideWindowAfter8KB(t *testing.T) {
	raw := readTrust(t, "testdata/trust-dialog-wide-250-2.1.326.raw")
	if i := bytes.Index(raw[8<<10:], []byte("Adding")); i < 0 {
		t.Fatal("в записи нет окна после 8 КБ")
	}
	home := "/tmp/claude-tmp/rv-adddir.j926/h2"
	dirs := []string{filepath.Join(home, ".cache/worktrees"), filepath.Join(home, ".cache/claude-tmp")}
	got := make(chan trustWindow, 16)
	var limit int
	w := newTrustWatchWithCallbacks(home, dirs, func(win trustWindow) { got <- win }, trustWatchCallbacks{
		scanLimit: func(n int) { limit = n },
	})
	defer w.close()
	for i := 0; i < len(raw); i += 512 {
		if _, err := w.Write(raw[i:min(i+512, len(raw))]); err != nil {
			t.Fatal(err)
		}
	}
	var windows []trustWindow
	deadline := time.After(2 * time.Second)
	for len(windows) < 4 {
		select {
		case win := <-got:
			windows = append(windows, win)
		case <-deadline:
			t.Fatalf("найдено %d окон, ожидается минимум 4; scan-limit=%d", len(windows), limit)
		}
	}
	if limit != 0 {
		t.Fatalf("поиск завершился по размеру до REPL: %d", limit)
	}
	if !slices.ContainsFunc(windows, func(win trustWindow) bool {
		return strings.Contains(win.dir, "/.cache/work trees") && win.key == "foreign"
	}) {
		t.Fatalf("позднее окно с work trees не найдено среди %#v", windows)
	}
}

func TestTrustScanLimitIsReported(t *testing.T) {
	var got int
	w := newTrustWatchWithCallbacks("", nil, func(trustWindow) {}, trustWatchCallbacks{scanLimit: func(n int) { got = n }})
	defer w.close()
	if _, err := w.Write(bytes.Repeat([]byte("x"), trustScanMax)); err != nil {
		t.Fatal(err)
	}
	if !w.scanDone || got != trustScanMax {
		t.Fatalf("остановка поиска: scanDone=%v reported=%d limit=%d", w.scanDone, got, trustScanMax)
	}
}

func TestTrustPathResolveErrorUsesCallback(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "loop")
	if err := os.Symlink(dir, dir); err != nil {
		t.Fatal(err)
	}
	var notes []string
	w := newTrustWatchWithCallbacks("", []string{dir}, func(trustWindow) {}, trustWatchCallbacks{
		pathError: func(path string, err error) {
			notes = append(notes, cllog.TrustPathResolveFailed(path, err))
		},
	})
	w.close()
	if len(notes) != 1 || !strings.Contains(notes[0], "не удалось разрешить симлинки") {
		t.Fatalf("ошибка EvalSymlinks не записана через callback: %q", notes)
	}
}

func TestTrustWatchResolvesSymlink(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real-worktrees")
	alias := filepath.Join(root, "worktrees-link")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	raw := readTrust(t, "testdata/trust-dialog-2.1.326.raw")
	raw = replaceAddDir(t, raw, real)
	windows, _ := scanTrustWindows(firstScreen(raw), "", trustDirSet([]string{alias}, ""))
	if len(windows) != 1 || windows[0].key != "add-dir" || windows[0].dir != real {
		t.Fatalf("разрешённый симлинк не совпал: %#v", windows)
	}
}

func TestTrustCleanDirPreservesEdgeWhitespace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worktrees")
	for _, value := range []string{" " + path, path + " "} {
		if got := cleanTrustDir(value, ""); got != value {
			t.Errorf("cleanTrustDir(%q)=%q, want to preserve edge whitespace", value, got)
		}
	}
}

func TestHasUserAddDirArg(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{"equals form anywhere", []string{"--resume", "id", "--add-dir=/tmp/x"}, true},
		{"separate form", []string{"--add-dir", "/tmp/x"}, true},
		{"variadic values", []string{"--add-dir", "A", "B"}, true},
		{"value with space", []string{"--add-dir=/tmp/a b"}, true},
		{"BOM value", []string{"--add-dir=/tmp/x\uFEFF"}, true},
		{"no add-dir", []string{"--resume", "id"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasUserAddDirArg(tc.args); got != tc.want {
				t.Fatalf("hasUserAddDirArg(%q)=%v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

func TestTrustWatchGivesUpBeforeREPL(t *testing.T) {
	w := newTrustWatch("", nil, func(trustWindow) { t.Error("окно появилось после первого экрана") })
	defer w.close()
	chunk := bytes.Repeat([]byte("history line\r\n"), trustScanMax/14+2)
	if _, err := w.Write(chunk); err != nil {
		t.Fatal(err)
	}
	if !w.scanDone || w.buf != nil {
		t.Fatalf("после %d КБ без окна поиск продолжается", trustScanMax>>10)
	}
	resume := readTrust(t, "testdata/trust-dialog-resume-2.1.326.raw")
	if _, err := w.Write(resume); err != nil {
		t.Fatal(err)
	}
}

func TestForeignAddDirDoesNotType(t *testing.T) {
	var notes []string
	c := &console{note: func(s string) { notes = append(notes, s) }}
	c.handleTrustWindow(trustWindow{dir: "/tmp/unrequested", key: "foreign"}, nil)
	want := "окно доверия к /tmp/unrequested: папки нет среди --add-dir cl, оставляю окно пользователю"
	if !slices.Contains(notes, want) || len(notes) != 1 {
		t.Fatalf("чужая папка должна только записаться в журнал: %q", notes)
	}
}

func TestUserAddDirBlocksRecordedWindows(t *testing.T) {
	const source = "/home/aisee/.cache/claude-tmp/aguard-briefs/rv-adddir.j926/h2"
	const wantNote = "в аргументах есть пользовательский --add-dir, окна добавления папок оставляю пользователю"
	home := t.TempDir()
	for _, rel := range sharedDirs {
		if err := os.MkdirAll(filepath.Join(home, rel), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(home, ".cache", "worktrees")
	ownDirs := addDirsFromFlags(addDirFlags(home), home)
	argSets := []struct {
		name string
		args []string
	}{
		{"equals", []string{"--add-dir=" + dir}},
		{"separate", []string{"--add-dir", dir}},
		{"variadic", []string{"--add-dir", "A", "B"}},
		{"trailing space", []string{"--add-dir=" + dir + " "}},
		{"leading space", []string{"--add-dir= " + dir}},
		{"BOM suffix", []string{"--add-dir=" + dir + "\uFEFF"}},
	}
	for _, fixture := range []string{"testdata/trust-dialog-trail-w100.raw", "testdata/trust-dialog-variadic-w140.raw"} {
		raw := bytes.ReplaceAll(readTrust(t, fixture), []byte(source), []byte(home))
		for _, tc := range argSets {
			t.Run(filepath.Base(fixture)+"/"+tc.name, func(t *testing.T) {
				block := hasUserAddDirArg(tc.args)
				if !block {
					t.Fatalf("пользовательский --add-dir не найден в %q", tc.args)
				}
				got := make(chan trustWindow, 4)
				w := newTrustWatchWithCallbacks(home, ownDirs, func(win trustWindow) { got <- win }, trustWatchCallbacks{blockAddDir: block})
				defer w.close()
				if _, err := w.Write(raw); err != nil {
					t.Fatal(err)
				}
				var win trustWindow
				select {
				case win = <-got:
				case <-time.After(time.Second):
					t.Fatal("окно записи не распознано")
				}
				if !win.addDir || !win.blocked {
					t.Fatalf("пользовательский --add-dir не заблокировал окно: %#v", win)
				}
				var notes []string
				c := &console{note: func(s string) { notes = append(notes, s) }}
				c.noteUserAddDir(block)
				c.handleTrustWindow(win, nil)
				if len(notes) != 1 || notes[0] != wantNote || notes[0] != cllog.TrustUserAddDir() {
					t.Fatalf("ожидалась одна запись о пользовательском --add-dir: %q", notes)
				}
			})
		}
	}

	// Тот же короткий путь без пользовательского флага остаётся разрешённым для автоматического подтверждения.
	trail := bytes.ReplaceAll(readTrust(t, "testdata/trust-dialog-trail-w100.raw"), []byte(source), []byte(home))
	windows, _ := scanTrustWindows(trail, home, trustDirSet(ownDirs, home))
	if len(windows) != 1 || windows[0].key != "add-dir" || windows[0].blocked {
		t.Fatalf("без пользовательского --add-dir свои папки должны работать как раньше: %#v", windows)
	}
}

func TestConfirmAddDirSelects(t *testing.T) {
	setTrustTiming(t, 2*time.Second, 20*time.Millisecond)
	raw := firstScreen(readTrust(t, "testdata/trust-dialog-2.1.326.raw"))
	dir := filepath.Join(t.TempDir(), "shared")
	raw = replaceAddDir(t, raw, dir)
	recorded := filepath.Join(t.TempDir(), "dialog.raw")
	if err := os.WriteFile(recorded, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	selected := filepath.Join(t.TempDir(), "selected.raw")
	if err := os.WriteFile(selected, recordedYesRedraw(t), 0o600); err != nil {
		t.Fatal(err)
	}
	typer, tty, shown := outerTerminal(t)
	var mu sync.Mutex
	var notes []string
	c := openConsole(tty, tty, func(s string) { mu.Lock(); notes = append(notes, s); mu.Unlock() })
	cmd := exec.Command("sh", "-c",
		`stty raw -echo; cat "$0"; dd bs=1 count=32 of=/dev/null 2>/dev/null; k=$(dd bs=1 count=3 2>/dev/null | od -An -tx1 | tr -d ' \n'); [ "$k" = 1b5b42 ] || { echo "BAD-KEY-$k"; exit 0; }; cat "$1"; v=$(dd bs=1 count=1 2>/dev/null | od -An -tx1 | tr -d ' \n'); [ "$v" = 0d ] && sleep 0.05 && echo ENTER-OK`, recorded, selected)
	finish, err := c.start(cmd, "", false, dir)
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
	waitTrustNote(t, &mu, &notes, "подтвердил доверие к "+dir)
}

func TestConfirmTwoAddDirsSelects(t *testing.T) {
	setTrustTiming(t, 2*time.Second, 20*time.Millisecond)
	raw := firstScreen(readTrust(t, "testdata/trust-dialog-2.1.326.raw"))
	one, two := filepath.Join(t.TempDir(), "shared-one"), filepath.Join(t.TempDir(), "shared-two")
	first, second := replaceAddDir(t, raw, one), replaceAddDir(t, raw, two)
	firstFile, secondFile := filepath.Join(t.TempDir(), "first.raw"), filepath.Join(t.TempDir(), "second.raw")
	for name, contents := range map[string][]byte{firstFile: first, secondFile: second} {
		if err := os.WriteFile(name, contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	selected := filepath.Join(t.TempDir(), "selected.raw")
	if err := os.WriteFile(selected, recordedYesRedraw(t), 0o600); err != nil {
		t.Fatal(err)
	}
	typer, tty, shown := outerTerminal(t)
	var mu sync.Mutex
	var notes []string
	c := openConsole(tty, tty, func(s string) { mu.Lock(); notes = append(notes, s); mu.Unlock() })
	cmd := exec.Command("sh", "-c",
		`stty raw -echo; cat "$0"; dd bs=1 count=32 of=/dev/null 2>/dev/null; k=$(dd bs=1 count=3 2>/dev/null | od -An -tx1 | tr -d ' \n'); [ "$k" = 1b5b42 ] || { echo "BAD-DOWN-$k"; exit 0; }; cat "$2"; v=$(dd bs=1 count=1 2>/dev/null | od -An -tx1 | tr -d ' \n'); [ "$v" = 0d ] || { echo "BAD-ENTER-$v"; exit 0; }; cat "$1"; k=$(dd bs=1 count=3 2>/dev/null | od -An -tx1 | tr -d ' \n'); [ "$k" = 1b5b42 ] || { echo "BAD-DOWN-$k"; exit 0; }; cat "$2"; v=$(dd bs=1 count=1 2>/dev/null | od -An -tx1 | tr -d ' \n'); [ "$v" = 0d ] || { echo "BAD-ENTER-$v"; exit 0; }; sleep 0.05; echo TWO-ENTER-OK`,
		firstFile, secondFile, selected)
	finish, err := c.start(cmd, "", false, one, two)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := typer.WriteString("\x1b_Gi=31;OK\x1b\\\x1b[6;28;13t\x1b[?62;1;4c"); err != nil {
		t.Fatal(err)
	}
	shown.wait(t, "TWO-ENTER-OK")
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	finish()
	waitTrustNote(t, &mu, &notes, "подтвердил доверие к "+one)
	waitTrustNote(t, &mu, &notes, "подтвердил доверие к "+two)
}

func TestSecondTrustWindowNeedsNewSelection(t *testing.T) {
	setTrustTiming(t, 300*time.Millisecond, 20*time.Millisecond)
	raw := firstScreen(readTrust(t, "testdata/trust-dialog-2.1.326.raw"))
	one, two := filepath.Join(t.TempDir(), "one"), filepath.Join(t.TempDir(), "two")
	d1, d2 := replaceAddDir(t, raw, one), replaceAddDir(t, raw, two)
	selected := filepath.Join(t.TempDir(), "selected.raw")
	if err := os.WriteFile(selected, recordedYesRedraw(t), 0o600); err != nil {
		t.Fatal(err)
	}
	first, second := filepath.Join(t.TempDir(), "first.raw"), filepath.Join(t.TempDir(), "second.raw")
	if err := os.WriteFile(first, d1, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, d2, 0o600); err != nil {
		t.Fatal(err)
	}
	noise := filepath.Join(t.TempDir(), "noise.raw")
	if err := os.WriteFile(noise, []byte("unrelated screen redraw\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	typer, tty, shown := outerTerminal(t)
	var mu sync.Mutex
	var notes []string
	c := openConsole(tty, tty, func(s string) { mu.Lock(); notes = append(notes, s); mu.Unlock() })
	cmd := exec.Command("sh", "-c",
		`stty raw -echo; cat "$0"; dd bs=1 count=32 of=/dev/null 2>/dev/null; k=$(dd bs=1 count=3 2>/dev/null | od -An -tx1 | tr -d ' \n'); [ "$k" = 1b5b42 ] || exit 2; cat "$2"; dd bs=1 count=1 of=/dev/null 2>/dev/null; cat "$1"; k=$(dd bs=1 count=3 2>/dev/null | od -An -tx1 | tr -d ' \n'); [ "$k" = 1b5b42 ] || exit 3; cat "$3"; k=$(timeout --foreground 1 dd bs=1 count=1 2>/dev/null | od -An -tx1 | tr -d ' \n'); [ -z "$k" ] && echo SECOND-NO-KEY || echo SECOND-EXTRA-KEY-$k`, first, second, selected, noise)
	finish, err := c.start(cmd, "", false, one, two)
	if err != nil {
		t.Fatal(err)
	}
	finish = trackTrustFinish(t, finish)
	if _, err := typer.WriteString("\x1b_Gi=31;OK\x1b\\\x1b[6;28;13t\x1b[?62;1;4c"); err != nil {
		t.Fatal(err)
	}
	shown.wait(t, "SECOND-NO-KEY")
	if shown.has("SECOND-EXTRA-KEY") {
		t.Fatal("после Down во втором окне ушёл ещё один байт без перерисовки выбора")
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	finish()
	waitTrustNote(t, &mu, &notes, "окно доверия к "+two+": выбор не встал")
}

func TestConfirmTrustSelects(t *testing.T) {
	setTrustTiming(t, 2*time.Second, 20*time.Millisecond)
	_, tty, shown := outerTerminal(t)
	var mu sync.Mutex
	var notes []string
	c := openConsole(tty, tty, func(s string) { mu.Lock(); notes = append(notes, s); mu.Unlock() })
	cmd := exec.Command("sh", "-c",
		`stty raw -echo; cat "$0"; dd bs=1 count=3 of=/dev/null 2>/dev/null; cat "$1"; y=$(dd bs=1 count=1 2>/dev/null | od -An -tu1 | tr -d ' '); [ "$y" = 13 ] && sleep 0.05 && echo ENTER-OK`,
		"testdata/trust-dialog-2.1.316.raw", "testdata/trust-down-2.1.316.raw")
	finish, err := c.start(cmd, "/home/alice", false)
	if err != nil {
		t.Fatal(err)
	}
	shown.wait(t, "ENTER-OK")
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	finish()
	waitTrustNote(t, &mu, &notes, "подтвердил доверие к /home/alice")
}

func TestConfirmTrustDoesNotSelect(t *testing.T) {
	setTrustTiming(t, 100*time.Millisecond, 20*time.Millisecond)
	_, tty, shown := outerTerminal(t)
	var mu sync.Mutex
	var notes []string
	c := openConsole(tty, tty, func(s string) { mu.Lock(); notes = append(notes, s); mu.Unlock() })
	cmd := exec.Command("sh", "-c",
		`stty raw -echo; cat "$0"; k=$(dd bs=1 count=3 2>/dev/null | od -An -tx1 | tr -d ' \n'); [ "$k" = 1b5b42 ] || { echo BAD-FIRST-DOWN-$k; exit 0; }; k=$(timeout --foreground 1 dd bs=1 count=1 2>/dev/null | od -An -tx1 | tr -d ' \n'); [ -z "$k" ] && echo NO-RETRY || echo UNEXPECTED-RETRY-$k`,
		"testdata/trust-dialog-2.1.316.raw")
	finish, err := c.start(cmd, "/home/alice", false)
	if err != nil {
		t.Fatal(err)
	}
	finish = trackTrustFinish(t, finish)
	shown.wait(t, "NO-RETRY")
	if shown.has("UNEXPECTED-RETRY") {
		t.Fatal("Down повторился без подсказки Claude Code")
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	finish()
	waitTrustNote(t, &mu, &notes, "окно доверия к /home/alice: выбор не встал на «Yes» за 100ms")
}

func TestConfirmTrustDoesNotRetryOnUnrelatedOutput(t *testing.T) {
	setTrustTiming(t, 1500*time.Millisecond, 20*time.Millisecond)
	noise := filepath.Join(t.TempDir(), "noise.raw")
	if err := os.WriteFile(noise, []byte("unrelated screen redraw\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, tty, shown := outerTerminal(t)
	var mu sync.Mutex
	var notes []string
	c := openConsole(tty, tty, func(s string) { mu.Lock(); notes = append(notes, s); mu.Unlock() })
	cmd := exec.Command("sh", "-c",
		`stty raw -echo; cat "$0"; k=$(dd bs=1 count=3 2>/dev/null | od -An -tx1 | tr -d ' \n'); [ "$k" = 1b5b42 ] || { echo BAD-FIRST-DOWN-$k; exit 0; }; cat "$1"; k=$(timeout --foreground 2 dd bs=1 count=1 2>/dev/null | od -An -tx1 | tr -d ' \n'); [ -z "$k" ] && echo NO-RETRY-ON-OUTPUT || echo UNEXPECTED-RETRY-$k`,
		"testdata/trust-dialog-2.1.316.raw", noise)
	finish, err := c.start(cmd, "/home/alice", false)
	if err != nil {
		t.Fatal(err)
	}
	finish = trackTrustFinish(t, finish)
	shown.wait(t, "NO-RETRY-ON-OUTPUT")
	if shown.has("UNEXPECTED-RETRY") {
		t.Fatal("Down повторился после постороннего вывода без подсказки")
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	finish()
	waitTrustNote(t, &mu, &notes, "выбор не встал на «Yes»")
}

func TestConfirmTrustScreenReader(t *testing.T) {
	setTrustTiming(t, 500*time.Millisecond, 20*time.Millisecond)
	for _, tc := range []struct{ echo, want, note string }{
		{"testdata/trust-y-sr-2.1.322.raw", "ENTER-OK", "подтвердил доверие к /home/alice"},
		{"", "NO-ENTER", "выбор не встал на «Yes»"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			echo := tc.echo
			if echo == "" {
				dialog := readTrust(t, "testdata/trust-dialog-sr-2.1.322.raw")
				echo = filepath.Join(t.TempDir(), "no-echo.raw")
				redraw := append(dialog, "\x1b(B\x0f\x1b[?2004h\x1b[<u\x1b[11G\x1b[13G\r\n\x1b[3Gy. Yes, I trust this folder\r\n\x1b[3Gn. No, exit\r\n\x1b[5Gyou\x1b[7Gy/n:\x1b[1;12H y\x1b[13G\x1b[3Gy"...)
				if err := os.WriteFile(echo, redraw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, tty, shown := outerTerminal(t)
			var mu sync.Mutex
			var notes []string
			c := openConsole(tty, tty, func(s string) { mu.Lock(); notes = append(notes, s); mu.Unlock() })
			cmd := exec.Command("sh", "-c",
				`stty raw -echo; cat "$0"; k=$(dd bs=1 count=1 2>/dev/null); [ "$k" = y ] || { echo KEY-$k; exit 0; }; cat "$1"; v=$(timeout --foreground 2 dd bs=1 count=1 2>/dev/null | od -An -tu1 | tr -d ' '); [ "$v" = 13 ] && echo ENTER-OK || echo NO-ENTER`,
				"testdata/trust-dialog-sr-2.1.322.raw", echo)
			finish, err := c.start(cmd, "/home/alice", false)
			if err != nil {
				t.Fatal(err)
			}
			finish = trackTrustFinish(t, finish)
			shown.wait(t, tc.want)
			if err := cmd.Wait(); err != nil {
				t.Fatal(err)
			}
			finish()
			waitTrustNote(t, &mu, &notes, tc.note)
		})
	}
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
