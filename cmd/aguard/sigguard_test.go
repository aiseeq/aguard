package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/activity"
	"github.com/aiseeq/aguard/internal/cpuwatch"
	"github.com/aiseeq/aguard/internal/policy"
	"github.com/aiseeq/aguard/internal/sigguard"
	"github.com/aiseeq/akit/session"
)

func TestRouteSigguard(t *testing.T) {
	dir := t.TempDir()
	jobdir := filepath.Join(dir, "job-123")
	if err := os.MkdirAll(jobdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobdir, "job.json"), []byte(`{"id":"job-123","master":"worker-master"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	jobs := &cpuwatch.Jobs{Dir: dir}
	alive := map[string]session.Info{"session-a": {Pid: 42, Name: "session-a"}, "worker-master": {Pid: 43, Name: "worker-master"}}
	for _, tc := range []struct{ unit, root, want, job string }{{"agent-session-42.scope", "42", "session-a", ""}, {"graft-heavy-1.scope", "42", "session-a", ""}, {"run-u123.scope", "42", "session-a", ""}, {"worker-io-abc.scope", "42", "session-a", ""}, {"worker-settle.service", "42", "session-a", ""}, {"app-foo.scope", "42", "session-a", ""}, {"worker-job-123.service", "42", "worker-master", "job-123"}, {"worker-job-123.serv", "42", "worker-master", "job-123"}, {"worker-io-3428722.sc", "42", "session-a", ""}, {"run-any.scope", "999", "leader", ""}, {"run-u123.scope", "999", "leader", ""}, {"agent-old-9.scope", "999", "leader", ""}} {
		got, job := sigguardRoute(sigguard.Deny{Unit: tc.unit, Root: tc.root}, "leader", alive, jobs)
		if got != tc.want || job != tc.job {
			t.Errorf("%s: got %s %s, want %s %s", tc.unit, got, job, tc.want, tc.job)
		}
	}
	got, job := sigguardRoute(sigguard.Deny{Unit: "worker-job-123.service"}, "leader", map[string]session.Info{}, jobs)
	if got != "leader" || job != "job-123" {
		t.Fatalf("dead master route = %s %s", got, job)
	}
	message := sigguard.Message(sigguard.Group{Denies: []sigguard.Deny{{Comm: "worker", PID: 7}}, Count: 1, Lines: 1}, job)
	if !strings.Contains(message, "Задание worker job-123") {
		t.Fatalf("worker id missing from message: %s", message)
	}
}

func TestSigguardMessageKilledAndSignalName(t *testing.T) {
	d := sigguard.Deny{Kind: "signal", Arg: "15", Count: 10, Comm: "python3", PID: 2267720, PPID: 1, Pcomm: "bash", Unit: "agent-a-1.scope", Target: 9, TargetComm: "sh", TargetUnit: "x.service", Targets: 10, HasTargets: true, Killed: true}
	m := sigguard.Message(sigguard.Group{Denies: []sigguard.Deny{d}, Count: 10, Lines: 1}, "")
	for _, part := range []string{"aguard: авария — ядро убило python3(2267720) SIGKILL после 10 чужих целей — команда агента упала с кодом 137 поэтому", "signal SIGTERM", "разных целей 10", "родитель bash(1)"} {
		if !strings.Contains(m, part) {
			t.Errorf("message missing %q: %s", part, m)
		}
	}
}

// cgroup_write: arg=1 — перенос процесса записью в cgroup.procs (target_unit — куда), arg=0 — запись в чужую cgroup.
func TestSigguardMessageCgroupWrite(t *testing.T) {
	for arg, want := range map[string]string{"1": "отбил перенос процесса в cgroup agent-x-5.scope ×1", "0": "отбил запись в чужую cgroup ×1"} {
		d := sigguard.Deny{Kind: "cgroup_write", Arg: arg, Count: 1, Comm: "bash", PID: 3, Unit: "agent-x-5.scope", TargetUnit: "agent-x-5.scope"}
		if m := sigguard.Message(sigguard.Group{Denies: []sigguard.Deny{d}, Count: 1, Lines: 1}, ""); !strings.Contains(m, want) {
			t.Errorf("arg=%s: нет %q: %s", arg, want, m)
		}
	}
}

func TestSigguardMessageSummarizesWholeGroup(t *testing.T) {
	denies := []sigguard.Deny{
		{Kind: "signal", Arg: "15", Count: 2, Comm: "bash", PID: 1, PPID: 9, Pcomm: "sh"},
		{Kind: "signal", Arg: "15", Count: 8, Comm: "bash", PID: 1, PPID: 9, Pcomm: "sh"},
		{Kind: "cgroup_write", Arg: "1", Count: 8, TargetUnit: "agent-x-5.scope", Comm: "python", PID: 2},
		{Kind: "rlimit", Arg: "7", Count: 7, Comm: "codex", PID: 3},
		{Kind: "ptrace", Arg: "2", Count: 6, Comm: "claude", PID: 4},
		{Kind: "sigio", Arg: "9", Count: 5, Comm: "other", PID: 5, Killed: true, Targets: 11, HasTargets: true, Target: 20, TargetComm: "sleep", TargetUnit: "x.service"},
		{Kind: "oom_score_adj", Arg: "8", Count: 4, Comm: "extra", PID: 6},
		{Kind: "cgroup_write", Arg: "0", Count: 3, Comm: "extra2", PID: 7, Mode: "log"},
	}
	m := sigguard.Message(sigguard.Group{Denies: denies, Count: 43, Lines: 8}, "")
	for _, part := range []string{"ядро убило other(5) SIGKILL после 11 чужих целей", "signal SIGTERM ×10", "перенос процесса в cgroup agent-x-5.scope ×8", "и ещё 2 видов", "bash(1) (родитель sh(9))", "первая цель sleep(20) в x.service", "разных целей 11", "agent-signal-guard в режиме log пропустил (действие прошло) signal SIGTERM ×10", "Агенту нельзя трогать чужие процессы"} {
		if !strings.Contains(m, part) {
			t.Errorf("group message missing %q: %s", part, m)
		}
	}
	if !(strings.Index(m, "signal SIGTERM ×10") < strings.Index(m, "перенос процесса в cgroup agent-x-5.scope ×8") && strings.Index(m, "перенос процесса") < strings.Index(m, "rlimit 7 ×7")) {
		t.Fatalf("kind summaries not count-sorted: %s", m)
	}
	if strings.Contains(m, "extra(6)") || strings.Contains(m, "extra2(7)") {
		t.Errorf("more than three senders: %s", m)
	}
}

func TestSigguardStreamSendAndStale(t *testing.T) {
	makeLine := func(at time.Time) string {
		m := map[string]string{"MESSAGE": `deny kind=signal arg=15 count=2 first=x last=x unit=run-test.service root=999 pid=2 comm="bash" ppid=1 pcomm="systemd" target=4 target_comm="sleep" target_unit="x.service" mode=enforce`, "__CURSOR": "cursor-1", "__REALTIME_TIMESTAMP": fmtInt(at.UnixMicro())}
		b, _ := json.Marshal(m)
		return string(b)
	}
	t.Run("fresh record is delivered once", func(t *testing.T) {
		dir := t.TempDir()
		a := &app{home: dir, stateDir: dir, reg: session.Registry{Dir: filepath.Join(dir, "no-registry"), ProcRoot: dir}, log: slog.New(slog.DiscardHandler), act: activity.New(filepath.Join(dir, "activity"), time.Now())}
		sends := 0
		a.sigSend = func(to, msg string) error {
			sends++
			if to != "lead" || !strings.Contains(msg, "×2") {
				t.Fatalf("route/message %s %s", to, msg)
			}
			return nil
		}
		a.sigJobs = &cpuwatch.Jobs{Dir: filepath.Join(dir, "no-workers")}
		cfg := sigguard.Default()
		cfg.Lead = "lead"
		source := func(_ context.Context, cursor string, initial bool) (io.ReadCloser, error) {
			if cursor != "" || !initial {
				t.Fatalf("cursor=%q initial=%v", cursor, initial)
			}
			return io.NopCloser(strings.NewReader(makeLine(time.Now()) + "\n")), nil
		}
		err := a.sigguardStream(context.Background(), &cfg, source)
		if err == nil {
			t.Fatal("expected source EOF restart")
		}
		if sends != 1 {
			t.Fatalf("send count %d", sends)
		}
		gotCursor, err := os.ReadFile(a.sigguardCursorPath())
		if err != nil || string(gotCursor) != "cursor-1\n" {
			t.Fatalf("saved cursor %q, %v", gotCursor, err)
		}
		records := flushSigguardActivity(t, a)
		if !hasSigguardEvent(records, "sent") || !hasSigguardEvent(records, activity.KindPass) {
			t.Fatal("sent activity event missing")
		}
		info, err := os.Stat(a.sigguardCursorPath())
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("cursor permissions: %v %v", info, err)
		}
	})
	t.Run("stale record is not delivered", func(t *testing.T) {
		dir := t.TempDir()
		a := &app{home: dir, stateDir: dir, reg: session.Registry{Dir: filepath.Join(dir, "no-registry"), ProcRoot: dir}, log: slog.New(slog.DiscardHandler), act: activity.New(filepath.Join(dir, "activity"), time.Now())}
		sends := 0
		a.sigSend = func(string, string) error { sends++; return nil }
		cfg := sigguard.Default()
		cfg.Lead = "lead"
		source := func(context.Context, string, bool) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(makeLine(time.Now().Add(-20*time.Minute)) + "\n")), nil
		}
		_ = a.sigguardStream(context.Background(), &cfg, source)
		if sends != 0 {
			t.Fatalf("sent stale record: %d", sends)
		}
		if !hasSigguardEvent(flushSigguardActivity(t, a), "stale") {
			t.Fatal("stale activity missing")
		}
	})
	t.Run("old systemd line is neither mailed nor stale", func(t *testing.T) {
		dir := t.TempDir()
		a := testSigguardApp(dir)
		sent := 0
		a.sigSend = func(string, string) error { sent++; return nil }
		cfg := sigguard.Default()
		cfg.Lead = "lead"
		line := testSigguardRawJSON("systemd[1]: Started unrelated unit", "old", time.Now().Add(-time.Hour))
		source := func(context.Context, string, bool) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(line)), nil
		}
		_ = a.sigguardStream(context.Background(), &cfg, source)
		records := flushSigguardActivity(t, a)
		if sent != 0 || hasSigguardEvent(records, "stale") {
			t.Fatalf("service line affected: sent=%d records=%+v", sent, records)
		}
	})
	t.Run("saved cursor is passed to source", func(t *testing.T) {
		dir := t.TempDir()
		state := filepath.Join(dir, "sigguard")
		if err := os.MkdirAll(state, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(state, "cursor"), []byte("saved-cursor\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		a := &app{home: dir, stateDir: dir, reg: session.Registry{Dir: filepath.Join(dir, "none"), ProcRoot: dir}, log: slog.New(slog.DiscardHandler), act: activity.New(filepath.Join(dir, "activity"), time.Now())}
		cfg := sigguard.Default()
		var got string
		source := func(_ context.Context, cursor string, initial bool) (io.ReadCloser, error) {
			got = cursor
			if initial {
				t.Error("saved cursor marked as initial")
			}
			return io.NopCloser(strings.NewReader("")), nil
		}
		_ = a.sigguardStream(context.Background(), &cfg, source)
		if got != "saved-cursor" {
			t.Fatalf("source cursor %q", got)
		}
	})
}

func TestSigguardStreamAggregation(t *testing.T) {
	var sample sigguardRecord
	if err := json.Unmarshal([]byte(testSigguardJSON("1", 1)), &sample); err != nil {
		t.Fatal(err)
	}
	if d, ok := sigguard.Parse(sample.message()); !ok {
		t.Fatalf("test source message did not parse: %q (%+v)", sample.message(), d)
	}
	for _, tc := range []struct {
		name  string
		roots []string
		want  int
	}{
		{name: "one root", roots: repeatRoot("1", 1000), want: 1},
		{name: "two roots", roots: []string{"1", "2"}, want: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			a := testSigguardApp(dir)
			cfg := sigguard.Default()
			cfg.Window *= 720 // сохраняем тысячу строк одной группы даже на медленном диске
			cfg.Lead = "lead"
			sent := 0
			a.sigSend = func(_, msg string) error {
				sent++
				if tc.want == 1 && !strings.Contains(msg, "×1000") {
					t.Errorf("summary did not include total: %s", msg)
				}
				return nil
			}
			var lines strings.Builder
			for i, root := range tc.roots {
				lines.WriteString(testSigguardJSON(root, i))
			}
			source := func(context.Context, string, bool) (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader(lines.String())), nil
			}
			_ = a.sigguardStream(context.Background(), &cfg, source)
			if sent != tc.want {
				t.Fatalf("letters=%d want=%d", sent, tc.want)
			}
		})
	}
}

func TestSigguardWindowTimerWithOpenSource(t *testing.T) {
	dir := t.TempDir()
	a := testSigguardApp(dir)
	cfg := sigguard.Default()
	cfg.Window = policy.Duration(30 * time.Millisecond)
	cfg.Lead = "lead"
	sent := make(chan string, 1)
	a.sigSend = func(_, msg string) error { sent <- msg; return nil }
	pr, pw := io.Pipe()
	defer pw.Close()
	closed := make(chan struct{})
	reader := &closeNotifyReader{ReadCloser: pr, closed: closed}
	done := make(chan error, 1)
	source := func(context.Context, string, bool) (io.ReadCloser, error) { return reader, nil }
	go func() { done <- a.sigguardStream(context.Background(), &cfg, source) }()
	if _, err := io.WriteString(pw, testSigguardJSON("1", 1)); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-sent:
		if !strings.Contains(msg, "signal SIGTERM") {
			t.Fatalf("message %s", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("window timer did not send")
	}
	select {
	case <-closed:
		t.Fatal("source closed before timer delivery")
	default:
	}
	_ = pw.Close()
	if err := <-done; err == nil {
		t.Fatal("source EOF should restart stream")
	}
}

func TestSigguardUnparsedBatch(t *testing.T) {
	dir := t.TempDir()
	a := testSigguardApp(dir)
	cfg := sigguard.Default()
	cfg.Lead = "lead"
	first := `deny kind=signal arg=15 count=3 comm="bad format" ` + strings.Repeat("x", 330)
	second := `would-deny kind=ptrace arg=2 missing=root`
	var to, msg string
	a.sigSend = func(dest, text string) error { to, msg = dest, text; return nil }
	line1 := testSigguardRawJSON(first, "a", time.Now())
	line2 := testSigguardRawJSON(second, "b", time.Now())
	source := func(context.Context, string, bool) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(line1 + line2)), nil
	}
	_ = a.sigguardStream(context.Background(), &cfg, source)
	if to != "lead" || !strings.Contains(msg, "не разобрано строк 2") || !strings.Contains(msg, string([]rune(first)[:300])) || strings.Contains(msg, string([]rune(first)[:301])) {
		t.Fatalf("unparsed batch letter: %q to=%q", msg, to)
	}
	// log mode still batches and accounts, but does not send.
	a = testSigguardApp(t.TempDir())
	cfg.Mode = sigguard.ModeLog
	sends := 0
	a.sigSend = func(string, string) error { sends++; return nil }
	_ = a.sigguardStream(context.Background(), &cfg, source)
	records := flushSigguardActivity(t, a)
	if sends != 0 || !hasSigguardEvent(records, "would_send") {
		t.Fatalf("log unparsed sends=%d records=%+v", sends, records)
	}
}

func TestSigguardMessageArrayBytes(t *testing.T) {
	message := `deny kind=signal arg=15 count=1 unit=run-a.scope root=1 pid=2 comm="sh"`
	ints := make([]int, len(message))
	for i, c := range []byte(message) {
		ints[i] = int(c)
	}
	array, _ := json.Marshal(ints)
	record := map[string]json.RawMessage{"MESSAGE": array}
	b, _ := json.Marshal(record)
	var got sigguardRecord
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.message() != message {
		t.Fatalf("MESSAGE bytes decoded %q", got.message())
	}
}

func TestSigguardPassDue(t *testing.T) {
	now := time.Now()
	next := now.Add(10 * time.Minute)
	if sigguardPassDue(now, next) || !sigguardPassDue(next, next) {
		t.Fatal("pass deadline check is wrong")
	}
}

type closeNotifyReader struct {
	io.ReadCloser
	closed chan struct{}
}

func (r *closeNotifyReader) Close() error { err := r.ReadCloser.Close(); close(r.closed); return err }

func TestSigguardLogUnparsedAndActivity(t *testing.T) {
	t.Run("log mode records would_send", func(t *testing.T) {
		dir := t.TempDir()
		a := testSigguardApp(dir)
		sends := 0
		a.sigSend = func(string, string) error { sends++; return nil }
		cfg := sigguard.Default()
		cfg.Mode = sigguard.ModeLog
		cfg.Lead = "lead"
		source := func(context.Context, string, bool) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(testSigguardJSON("9", 1))), nil
		}
		_ = a.sigguardStream(context.Background(), &cfg, source)
		records := flushSigguardActivity(t, a)
		if sends != 0 || !hasSigguardEvent(records, "would_send") {
			t.Fatalf("sends=%d records=%+v", sends, records)
		}
	})
	t.Run("unparsed deny alerts lead", func(t *testing.T) {
		dir := t.TempDir()
		a := testSigguardApp(dir)
		var to, msg string
		a.sigSend = func(dest, text string) error { to, msg = dest, text; return nil }
		cfg := sigguard.Default()
		cfg.Lead = "lead"
		raw := `deny kind=signal arg=15 count=10 unit=x comm="sh"`
		line, _ := json.Marshal(map[string]string{"MESSAGE": raw, "__CURSOR": "c", "__REALTIME_TIMESTAMP": strconv.FormatInt(time.Now().UnixMicro(), 10)})
		source := func(context.Context, string, bool) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(string(line) + "\n")), nil
		}
		_ = a.sigguardStream(context.Background(), &cfg, source)
		records := flushSigguardActivity(t, a)
		if to != "lead" || !strings.Contains(msg, raw) || !hasSigguardEvent(records, "unparsed") {
			t.Fatalf("to=%q msg=%q records=%+v", to, msg, records)
		}
	})
}

func TestSigguardContextFlushAndBackoff(t *testing.T) {
	dir := t.TempDir()
	a := testSigguardApp(dir)
	cfg := sigguard.Default()
	cfg.Lead = "lead"
	sent := 0
	a.sigSend = func(string, string) error { sent++; return nil }
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	reader := &cancelReader{ctx: ctx, data: []byte(testSigguardJSON("1", 1)), ready: ready}
	source := func(context.Context, string, bool) (io.ReadCloser, error) { return reader, nil }
	done := make(chan error, 1)
	go func() { done <- a.sigguardStream(ctx, &cfg, source) }()
	<-ready
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("stream error %v", err)
	}
	if sent != 1 {
		t.Fatalf("unfinished summary sent %d times", sent)
	}
	var waits []time.Duration
	loopCtx, stop := context.WithCancel(context.Background())
	loopApp := testSigguardApp(t.TempDir())
	loopApp.cfgPath = filepath.Join(loopApp.home, "missing.toml")
	loopApp.uid = os.Getuid()
	loopApp.sigSource = func(context.Context, string, bool) (io.ReadCloser, error) { return nil, errors.New("source failure") }
	loopApp.sigPause = func(_ context.Context, d time.Duration) bool {
		waits = append(waits, d)
		if len(waits) == 3 {
			stop()
			return false
		}
		return true
	}
	loopApp.sigguardLoop(loopCtx)
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	if !slices.Equal(waits, want) {
		t.Fatalf("backoff %v want %v", waits, want)
	}
	// Successful input resets the retry delay even when journalctl then exits.
	waits = nil
	loopCtx, stop = context.WithCancel(context.Background())
	calls := 0
	loopApp.sigSource = func(context.Context, string, bool) (io.ReadCloser, error) {
		calls++
		if calls < 3 {
			return nil, errors.New("source failure")
		}
		return io.NopCloser(strings.NewReader(testSigguardJSON("1", 1))), nil
	}
	loopApp.sigPause = func(_ context.Context, d time.Duration) bool {
		waits = append(waits, d)
		if len(waits) == 3 {
			stop()
			return false
		}
		return true
	}
	loopApp.sigguardLoop(loopCtx)
	want = []time.Duration{time.Second, 2 * time.Second, time.Second}
	if !slices.Equal(waits, want) {
		t.Fatalf("backoff after successful read %v want %v", waits, want)
	}
	if got := nextSigguardPause(40 * time.Second); got != time.Minute {
		t.Fatalf("capped backoff %s", got)
	}
}

func TestSigguardJournalArgsAndCursorReset(t *testing.T) {
	if got := journalArgs("", true); !slices.Equal(got, []string{"-u", "agent-signal-guard-log", "-f", "-o", "json", "-n", "0"}) {
		t.Fatalf("initial args %v", got)
	}
	if got := journalArgs("cursor-x", false); !slices.Equal(got, []string{"-u", "agent-signal-guard-log", "-f", "-o", "json", "--after-cursor", "cursor-x"}) {
		t.Fatalf("cursor args %v", got)
	}
	path := filepath.Join(t.TempDir(), "cursor")
	if err := writeSigguardCursor(path, "bad"); err != nil {
		t.Fatal(err)
	}
	if !resetSigguardCursor(path, fmt.Errorf("%w: Failed to seek to cursor", errSigguardCursorSeek)) {
		t.Fatal("seek failure not recognized")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cursor remains after seek failure: %v", err)
	}
}

func TestSigguardLoopResetsCursorAfterSeekError(t *testing.T) {
	dir := t.TempDir()
	a := testSigguardApp(dir)
	a.cfgPath = filepath.Join(dir, "missing.toml")
	a.uid = os.Getuid()
	cursorDir := filepath.Join(dir, "sigguard")
	if err := os.MkdirAll(cursorDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeSigguardCursor(filepath.Join(cursorDir, "cursor"), "broken-cursor"); err != nil {
		t.Fatal(err)
	}
	calls := 0
	a.sigSource = func(_ context.Context, cursor string, initial bool) (io.ReadCloser, error) {
		calls++
		if calls == 1 && (cursor != "broken-cursor" || initial) {
			t.Fatalf("first source cursor=%q initial=%v", cursor, initial)
		}
		if calls == 2 && (cursor != "" || !initial) {
			t.Fatalf("reset source cursor=%q initial=%v", cursor, initial)
		}
		if calls == 1 {
			return &closeErrorReader{ReadCloser: io.NopCloser(strings.NewReader("")), err: fmt.Errorf("%w: exit status 1: Failed to seek to cursor", errSigguardCursorSeek)}, nil
		}
		return nil, errors.New("stop test loop")
	}
	ctx, cancel := context.WithCancel(context.Background())
	waits := 0
	a.sigPause = func(context.Context, time.Duration) bool {
		waits++
		if waits == 2 {
			cancel()
			return false
		}
		return true
	}
	a.sigguardLoop(ctx)
	if calls != 2 || waits != 2 {
		t.Fatalf("source calls=%d pauses=%d", calls, waits)
	}
	if _, err := os.Stat(filepath.Join(cursorDir, "cursor")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cursor not cleared: %v", err)
	}
}

func TestJournalReaderCloseReturnsWaitError(t *testing.T) {
	waitErr := errors.New("wait failed")
	r := &journalReader{ReadCloser: io.NopCloser(strings.NewReader("")), stderr: bytes.NewBufferString("journal diagnostic"), wait: func() error { return waitErr }}
	err := r.Close()
	if !errors.Is(err, waitErr) || !strings.Contains(err.Error(), "journal diagnostic") {
		t.Fatalf("close error %v", err)
	}
}

func TestSigguardPartialAliveListIsUsed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alive map[string]session.Info
		want  string
	}{
		{"session in partial map", map[string]session.Info{"agent": {Name: "agent", Pid: 45}}, "agent"},
		{"missing from partial map", map[string]session.Info{"other": {Name: "other", Pid: 46}}, "lead"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := testSigguardApp(t.TempDir())
			a.sigAlive = func() (map[string]session.Info, error) { return tc.alive, errors.New("partial registry read") }
			a.sigSend = func(to, _ string) error {
				if to != tc.want {
					t.Errorf("sent to %s, want %s", to, tc.want)
				}
				return nil
			}
			cfg := sigguard.Default()
			cfg.Lead = "lead"
			g := sigguard.Group{Count: 1, Lines: 1, Denies: []sigguard.Deny{{Kind: "signal", Arg: "15", Count: 1, Unit: "run-u.scope", Root: "45", PID: 2, Comm: "sh"}}}
			a.sigguardSend(&cfg, g)
			if !hasSigguardEvent(flushSigguardActivity(t, a), activity.KindWarn) {
				t.Fatal("partial registry warning missing")
			}
		})
	}
}

type closeErrorReader struct {
	io.ReadCloser
	err error
}

func (r *closeErrorReader) Close() error { return errors.Join(r.ReadCloser.Close(), r.err) }

type cancelReader struct {
	ctx   context.Context
	data  []byte
	ready chan struct{}
	sent  bool
}

func (r *cancelReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		n := copy(p, r.data)
		close(r.ready)
		return n, nil
	}
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}
func (r *cancelReader) Close() error { return nil }

func testSigguardApp(dir string) *app {
	return &app{home: dir, stateDir: dir, reg: session.Registry{Dir: filepath.Join(dir, "no-registry"), ProcRoot: dir}, log: slog.New(slog.DiscardHandler), act: activity.New(filepath.Join(dir, "activity"), time.Now())}
}
func repeatRoot(root string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = root
	}
	return out
}
func testSigguardJSON(root string, seq int) string {
	message := fmt.Sprintf(`deny kind=signal arg=15 count=1 first=x last=x unit=run-x.scope root=%s pid=2 comm="bash" ppid=1 pcomm="sh" target=4 target_comm="sleep" target_unit="x.service" mode=enforce`, root)
	b, _ := json.Marshal(map[string]string{"MESSAGE": message, "__CURSOR": fmt.Sprintf("c-%d", seq), "__REALTIME_TIMESTAMP": strconv.FormatInt(time.Now().UnixMicro(), 10)})
	return string(b) + "\n"
}
func testSigguardRawJSON(message, cursor string, at time.Time) string {
	b, _ := json.Marshal(map[string]string{"MESSAGE": message, "__CURSOR": cursor, "__REALTIME_TIMESTAMP": strconv.FormatInt(at.UnixMicro(), 10)})
	return string(b) + "\n"
}
func flushSigguardActivity(t *testing.T, a *app) []activity.Record {
	t.Helper()
	if err := a.act.Flush(time.Now().Add(time.Second), 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	var out []activity.Record
	if _, err := activity.NewStore(filepath.Join(a.home, "activity")).Read(time.Time{}, func(r *activity.Record) { out = append(out, *r) }); err != nil {
		t.Fatal(err)
	}
	return out
}
func hasSigguardEvent(rs []activity.Record, kind string) bool {
	for _, r := range rs {
		if r.Watcher == "sigguard" && r.Kind == kind {
			return true
		}
	}
	return false
}

func fmtInt(v int64) string { return strconv.FormatInt(v, 10) }
