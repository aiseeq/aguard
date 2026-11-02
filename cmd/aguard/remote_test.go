package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
	"github.com/aiseeq/aguard/internal/remotebroken"
	"github.com/aiseeq/akit/session"
)

// Проход remote: переход на ноут по очереди PC — строка «не поломка» без письма, ssh — «написал бы»; смещение
// журнала после прохода — конец файла, события очереди в состояние писем не попадают.
func TestRemoteRoundQueuedNotBroken(t *testing.T) {
	dir := t.TempDir()
	var logs strings.Builder
	a := &app{home: dir, stateDir: filepath.Join(dir, "state"), uid: 1000,
		reg: session.Registry{Dir: filepath.Join(dir, "sessions"), ProcRoot: dir}, log: slog.New(slog.NewTextHandler(&logs, nil))}
	file := filepath.Join(dir, "remote-broken.jsonl")
	if err := os.MkdirAll(a.dir("remote"), 0o700); err != nil { // папки состояния создаёт демон при старте
		t.Fatal(err)
	}
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := remotebroken.Config{Mode: remotebroken.ModeLog, File: file, Repeat: policy.Duration(time.Hour), Lead: "lead"}
	if err := a.remoteRound(&cfg, false, "", false); err != nil { // засев: смещение — конец пустого файла
		t.Fatal(err)
	}
	lines := `{"at":"2026-11-02T03:24:45+01:00","project":"/home/u/git/sc2demu","task":"exec","host":"pc","reason":"run exited 125: pc: не впущен очередью graft за 176 с — шаг уходит на ноут (код 125)"}
{"at":"2026-11-02T03:25:00+01:00","project":"/home/u/git/graft","task":"exec","host":"pc","reason":"ssh: connect to host fedora-pc.local port 1: No route to host"}
`
	if err := os.WriteFile(file, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.remoteRound(&cfg, false, "", false); err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	if strings.Count(out, "очередь PC, не поломка") != 1 || !strings.Contains(out, "написал бы") || strings.Contains(out, "sc2demu на pc не дошла") {
		t.Fatalf("журнал прохода:\n%s", out)
	}
	st, err := remotebroken.LoadState(filepath.Join(a.dir("remote"), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Offset != int64(len(lines)) {
		t.Fatalf("смещение %d, файл %d", st.Offset, len(lines))
	}
	for key := range st.Sent {
		if strings.Contains(key, "sc2demu") {
			t.Fatalf("событие очереди в состоянии писем: %q", key)
		}
	}
}
