package main

import (
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestSuperviseWiresUserAddDirBlock(t *testing.T) {
	setTrustTiming(t, 2*time.Second, 20*time.Millisecond)
	for _, tc := range []struct {
		name       string
		userArgs   []string
		wantMarker string
		wantLog    bool
	}{
		{name: "pre flag is cl owned", wantMarker: "ENTER", wantLog: false},
		{name: "user flag blocks", userArgs: []string{"--add-dir=/tmp/user-choice"}, wantMarker: "NO-KEY", wantLog: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			own := filepath.Join(home, ".cache", "worktrees")
			if err := os.MkdirAll(own, 0o700); err != nil {
				t.Fatal(err)
			}
			raw := firstScreen(readTrust(t, "testdata/trust-dialog-2.1.326.raw"))
			raw = replaceAddDir(t, raw, own)
			fixture := filepath.Join(t.TempDir(), "dialog.raw")
			if err := os.WriteFile(fixture, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			selected := filepath.Join(t.TempDir(), "selected.raw")
			if err := os.WriteFile(selected, recordedYesRedraw(t), 0o600); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(t.TempDir(), "input.log")
			systemdRun := fakeTool(t, filepath.Join(t.TempDir(), "scope-bin"), "systemd-run",
				`while [ "$1" != "--" ]; do shift; done
shift
exec "$@"`)
			claude := fakeTool(t, filepath.Join(t.TempDir(), "claude-bin"), "claude",
				"stty raw -echo\n"+
					"cat "+shellQuote(fixture)+"\n"+
					`k=$(timeout --foreground 5 dd bs=1 count=3 2>/dev/null | od -An -tx1 | tr -d ' \n')
if [ "$k" = 1b5b42 ]; then
  printf 'DOWN\n' >> `+shellQuote(marker)+`
  cat `+shellQuote(selected)+`
  v=$(timeout --foreground 5 dd bs=1 count=1 2>/dev/null | od -An -tx1 | tr -d ' \n')
  [ "$v" = 0d ] && printf 'ENTER\n' >> `+shellQuote(marker)+`
else
  printf 'NO-KEY\n' >> `+shellQuote(marker)+`
fi`)
			config := filepath.Join(home, "cl-config.json")
			journal := filepath.Join(home, "cl.log")
			write(t, config, `{"restart":{"mode":"log","journal":"`+journal+`"}}`)
			_, tty, _ := outerTerminal(t)
			var noteMu sync.Mutex
			var notes []string
			c := openConsole(tty, tty, func(s string) { noteMu.Lock(); notes = append(notes, s); noteMu.Unlock() })
			l := launcher{systemdRun: systemdRun, claude: claude, home: home, configFile: config, name: "trust-test",
				dir: t.TempDir(), check: time.Hour, console: c}
			t.Cleanup(func() { signal.Reset(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP) })
			// The pre arguments model flags cl adds itself. They must not trigger the user-argument guard.
			code := l.supervise([]string{"--add-dir=" + own}, tc.userArgs)
			if code != 0 {
				t.Fatalf("supervise exit code = %d", code)
			}
			got, err := os.ReadFile(marker)
			if err != nil {
				t.Fatalf("stub Claude input log: %v", err)
			}
			if !strings.Contains(string(got), tc.wantMarker) {
				t.Fatalf("Claude input = %q, want marker %q", got, tc.wantMarker)
			}
			noteMu.Lock()
			logged := strings.Contains(strings.Join(notes, "\n"), "в аргументах есть пользовательский --add-dir")
			noteMu.Unlock()
			if logged != tc.wantLog {
				t.Fatalf("guard log mismatch: %q", notes)
			}
		})
	}
}

func TestUserAddDirBlockDoesNotBlockHomeTrust(t *testing.T) {
	raw := firstScreen(readTrust(t, "testdata/trust-dialog-2.1.316.raw"))
	got := make(chan trustWindow, 4)
	w := newTrustWatchWithCallbacks("/home/alice", nil, func(win trustWindow) { got <- win }, trustWatchCallbacks{blockAddDir: true})
	defer w.close()
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	select {
	case win := <-got:
		if win.key != "home" || win.blocked {
			t.Fatalf("user add-dir guard must leave home trust unblocked: %#v", win)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("home trust window not delivered")
	}
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
