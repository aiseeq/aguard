package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/activity"
	"github.com/aiseeq/aguard/internal/config"
	"github.com/aiseeq/aguard/internal/disk"
	"github.com/aiseeq/aguard/internal/iowatch"
	"github.com/aiseeq/aguard/internal/night"
	"github.com/aiseeq/aguard/internal/policy"
	"github.com/aiseeq/aguard/internal/queue"
	"github.com/aiseeq/aguard/internal/record"
	"github.com/aiseeq/akit/session"
)

func quietTestApp(t *testing.T) *app {
	t.Helper()
	home := t.TempDir()
	return &app{home: home, stateDir: filepath.Join(home, "state"), log: slog.New(slog.NewTextHandler(io.Discard, nil)), out: io.Discard}
}

func quietObservedApp(t *testing.T) (*app, *bytes.Buffer) {
	t.Helper()
	a := quietTestApp(t)
	var log bytes.Buffer
	a.log = slog.New(slog.NewTextHandler(&log, nil))
	a.act = activity.New(a.dir("activity"), time.Now())
	return a, &log
}

func assertQuietAccounting(t *testing.T, a *app, log *bytes.Buffer, watcher string) {
	t.Helper()
	if strings.Contains(log.String(), "level=WARN") {
		t.Fatalf("WARN в тихом режиме: %s", log.String())
	}
	if err := a.act.Flush(time.Now(), activityKeep); err != nil {
		t.Fatal(err)
	}
	quiet, failed := 0, 0
	_, err := activity.NewStore(a.dir("activity")).Read(time.Time{}, func(r *activity.Record) {
		if r.Watcher != watcher {
			return
		}
		if r.Kind == "quiet" {
			quiet += r.Count
		}
		if strings.HasSuffix(r.Kind, "_failed") {
			failed += r.Count
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if quiet == 0 || failed != 0 {
		t.Fatalf("учёт %s: quiet=%d failed=%d", watcher, quiet, failed)
	}
	// на каждое подавленное письмо — ровно одна строка INFO с адресатом и первой строкой
	var lines []string
	for l := range strings.SplitSeq(log.String(), "\n") {
		if strings.Contains(l, `msg="aguard тихо: не отправил"`) {
			lines = append(lines, l)
		}
	}
	if len(lines) != quiet {
		t.Fatalf("строк «тихо» %d, подавленных писем %d: %s", len(lines), quiet, log.String())
	}
	for _, l := range lines {
		if !strings.Contains(l, "level=INFO") || !strings.Contains(l, " to=") || !strings.Contains(l, " msg=") || strings.Count(l, `msg=`) < 2 {
			t.Fatalf("строка «тихо» без уровня INFO, адресата или первой строки письма: %s", l)
		}
	}
}

func writeQuietState(t *testing.T, a *app, until time.Time) {
	t.Helper()
	if err := os.MkdirAll(a.dir("quiet"), 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(quietState{Until: until, Why: "тест", By: "test", At: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.quietPath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func quietListener(t *testing.T) (string, <-chan string) {
	t.Helper()
	// AF_UNIX paths are short; alias the per-test temp folder through a short symlink.
	alias := filepath.Join(os.TempDir(), fmt.Sprintf("q-%x", time.Now().UnixNano()))
	if err := os.Symlink(t.TempDir(), alias); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(alias) })
	sock := filepath.Join(alias, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	got := make(chan string, 1)
	go func() {
		defer ln.Close()
		conn, err := ln.Accept()
		if err != nil {
			got <- "accept error: " + err.Error()
			return
		}
		defer conn.Close()
		b, err := io.ReadAll(conn)
		if err != nil {
			got <- "read error: " + err.Error()
			return
		}
		got <- string(b)
	}()
	return sock, got
}

func TestQuietSendSocketAndExpiryWithoutReload(t *testing.T) {
	a := quietTestApp(t)
	sock, got := quietListener(t)
	writeQuietState(t, a, time.Now().Add(time.Minute))
	if err := a.sendSocket(0, "lead", sock, false, "строка письма\nтело"); !errors.Is(err, errQuiet) {
		t.Fatalf("ошибка тихого режима: %v", err)
	}
	select {
	case msg := <-got:
		t.Fatalf("письмо ушло в сокет: %s", msg)
	default:
	}
	// срок истекает сам: файл тот же, его не переписываем и не перечитываем конфиг
	until := time.Now().Add(300 * time.Millisecond)
	writeQuietState(t, a, until)
	if err := a.sendSocket(0, "lead", sock, false, "ещё тихо"); !errors.Is(err, errQuiet) {
		t.Fatalf("до срока ожидался errQuiet: %v", err)
	}
	time.Sleep(time.Until(until) + 50*time.Millisecond)
	if err := a.sendSocket(0, "lead", sock, false, "проверка доставки"); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-got:
		if !strings.Contains(msg, "проверка доставки") {
			t.Fatalf("неожиданное письмо: %s", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("письмо не попало в настоящий unix-сокет")
	}
}

func TestQuietSendMissingBrokenAndUnreadableState(t *testing.T) {
	t.Run("нет файла", func(t *testing.T) {
		a := quietTestApp(t)
		sock, got := quietListener(t)
		if err := a.sendSocket(0, "lead", sock, false, "без режима"); err != nil {
			t.Fatal(err)
		}
		if msg := <-got; !strings.Contains(msg, "без режима") {
			t.Fatalf("письмо: %s", msg)
		}
	})
	for _, tc := range []string{"битый JSON", "нечитаемый путь"} {
		t.Run(tc, func(t *testing.T) {
			a := quietTestApp(t)
			if tc == "битый JSON" {
				if err := os.MkdirAll(a.dir("quiet"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(a.quietPath(), []byte("{broken"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.MkdirAll(a.stateDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(a.dir("quiet"), []byte("file"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			sock, _ := quietListener(t)
			err := a.sendSocket(0, "lead", sock, false, "не отправлять")
			if err == nil || errors.Is(err, errQuiet) || !strings.Contains(err.Error(), a.quietPath()) {
				t.Fatalf("ожидалась ошибка чтения с путём, errQuiet=false: %v", err)
			}
		})
	}
	t.Run("chmod 000", func(t *testing.T) {
		a := quietTestApp(t)
		if err := os.MkdirAll(a.dir("quiet"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(a.quietPath(), []byte(`{"until":"2099-01-01T00:00:00Z","why":"test"}`), 0o000); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(a.quietPath(), 0o600)
		_, _, readErr := a.readQuiet()
		if readErr == nil {
			t.Skip("текущий пользователь может читать файл с mode 000")
		}
		err := a.sendSocket(0, "lead", "unused", false, "не отправлять")
		if err == nil || errors.Is(err, errQuiet) || !errors.Is(err, fs.ErrPermission) || !strings.Contains(err.Error(), a.quietPath()) {
			t.Fatalf("ожидалась ошибка прав чтения с путём, errQuiet=false: %v", err)
		}
	})
}

func TestQuietCommandValidationAndAtomicState(t *testing.T) {
	for _, tc := range []struct {
		name  string
		until string
		why   string
		by    string
	}{
		{name: "без срока", why: "причина", by: "alice"},
		{name: "без автора", until: time.Now().Add(time.Hour).Format(time.RFC3339), why: "причина"},
		{name: "прошлое", until: time.Now().Add(-time.Minute).Format(time.RFC3339), why: "причина", by: "alice"},
		{name: "дальше недели", until: time.Now().Add(8 * 24 * time.Hour).Format(time.RFC3339), why: "причина", by: "alice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := quietTestApp(t)
			args := []string{"on"}
			if tc.until != "" {
				args = append(args, "--until", tc.until)
			}
			args = append(args, "--why", tc.why)
			if tc.by != "" {
				args = append(args, "--by", tc.by)
			}
			if err := a.quietCmd(args); err == nil {
				t.Fatal("ожидалась ошибка валидации")
			}
			if _, err := os.Stat(a.quietPath()); !os.IsNotExist(err) {
				t.Fatalf("файл создан при ошибке: %v", err)
			}
		})
	}
	a := quietTestApp(t)
	until := time.Now().Add(time.Hour).Format(time.RFC3339)
	if err := a.quietCmd([]string{"on", "--until", until, "--why", "проверка", "--by", "alice"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(a.quietPath())
	if err != nil {
		t.Fatal(err)
	}
	var state quietState
	if err := json.Unmarshal(b, &state); err != nil || state.Why != "проверка" || state.By != "alice" || state.At.IsZero() {
		t.Fatalf("состояние: %+v, ошибка %v", state, err)
	}
	if mode, err := os.Stat(a.quietPath()); err != nil || mode.Mode().Perm() != 0o600 {
		t.Fatalf("права файла: %v, ошибка %v", mode, err)
	}
	if err := a.quietCmd([]string{"off"}); err != nil {
		t.Fatal(err)
	}
	if err := a.quietCmd([]string{"off"}); err != nil {
		t.Fatal(err)
	}
}

func TestQuietQueueNoticeRemainsPendingUntilSent(t *testing.T) {
	a, log := quietObservedApp(t)
	sock, _ := quietListener(t)
	writeQuietState(t, a, time.Now().Add(time.Minute))
	now := time.Now()
	v := queue.View{Now: now, Lead: "lead", Serious: func(string) bool { return true }, Alive: func(string) bool { return true }}
	state := queue.State{}
	ticket := queue.Ticket{Key: "p/c", Project: "p", Slug: "c", Title: "critical", Prio: "CRIT", Status: "открыт"}
	_ = state.Step([]queue.Ticket{ticket}, v) // seed initial state
	notices := state.Step([]queue.Ticket{ticket}, v)
	if len(notices) == 0 {
		notices = state.Step([]queue.Ticket{ticket}, v)
	}
	if len(notices) == 0 {
		t.Fatal("не создано уведомление queue")
	}
	before, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	alive := map[string]session.Info{"lead": {Name: "lead", SocketPath: sock}}
	a.deliverQueueNotices(&config.Config{SendAuth: false, Queue: queue.Config{Mode: queue.ModeOn}}, alive, &state, v, notices, now)
	after, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("queue подтвердила недоставленное письмо: до %s, после %s", before, after)
	}
	assertQuietAccounting(t, a, log, "queue")
	if err := os.Remove(a.quietPath()); err != nil {
		t.Fatal(err)
	}
	sock, got := quietListener(t)
	alive["lead"] = session.Info{Name: "lead", SocketPath: sock}
	// следующий проход очереди: неподтверждённое уведомление снова в плане
	v.Now = now.Add(time.Minute)
	notices = state.Step([]queue.Ticket{ticket}, v)
	if len(notices) == 0 {
		t.Fatal("следующий проход очереди не вернул неподтверждённое уведомление")
	}
	a.deliverQueueNotices(&config.Config{Queue: queue.Config{Mode: queue.ModeOn}}, alive, &state, v, notices, v.Now)
	select {
	case msg := <-got:
		if !strings.Contains(msg, "critical") {
			t.Fatalf("queue отправила неожиданное письмо: %s", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("queue не повторила письмо после снятия режима")
	}
	after, err = json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) == string(after) {
		t.Fatal("queue не подтвердила доставленное письмо")
	}
}

func TestQuietNightDoesNotWarnOrRecordEmailFailure(t *testing.T) {
	a, log := quietObservedApp(t)
	writeQuietState(t, a, time.Now().Add(time.Hour))
	_ = liveLead(t, a)
	if err := os.MkdirAll(a.dir("night"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := night.Default(a.home)
	cfg.Lead, cfg.DreamBy, cfg.DreamFallback = "lead", "00:00", ""
	cfg.DreamFallbackBy = "23:59" // окно письма о Сне — до 23:59+3 ч: тест не зависит от часа запуска
	cfg.AlsoTo, cfg.DreamTo = nil, nil
	cfg.DreamMarker = filepath.Join(a.home, "absent-dream-marker")
	cfg.DigestBy, cfg.DigestMarker = "23:59", filepath.Join(a.home, "digest-marker")
	if err := os.WriteFile(cfg.DigestMarker, []byte(time.Now().Format("2006-01-02")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.nightRound(&cfg, false); err != nil {
		t.Fatal(err)
	}
	assertQuietAccounting(t, a, log, "night")
}

func TestQuietDiskDoesNotWarnOrRecordFailure(t *testing.T) {
	a, log := quietObservedApp(t)
	writeQuietState(t, a, time.Now().Add(time.Hour))
	_ = liveLead(t, a)
	if err := os.MkdirAll(a.dir("disk"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := disk.Config{FreeMode: disk.FreeAlert, FreeMin: 1, Lead: "lead"}
	if err := a.diskFree(&cfg, &disk.State{}, time.Now(), 0); err != nil {
		t.Fatal(err)
	}
	assertQuietAccounting(t, a, log, "disk")
}

func TestQuietIOAlertDoneDoesNotWarnOrRecordFailure(t *testing.T) {
	a, log := quietObservedApp(t)
	writeQuietState(t, a, time.Now().Add(time.Hour))
	_ = liveLead(t, a)
	cfg := iowatch.Default()
	cfg.Mode, cfg.Lead = iowatch.ModeAlert, "lead"
	if err := a.alertIO(cfg, []iowatch.Case{alertCase(time.Now().Add(-time.Minute), 2500)}); err != nil {
		t.Fatalf("alertIO error: %v", err)
	}
	s := &ioService{a: a, alerting: true}
	s.alertDone(fmt.Errorf("письмо io: %w", errQuiet))
	assertQuietAccounting(t, a, log, "io")
}

func TestQuietTmpDoesNotWarnOrRecordSendFailure(t *testing.T) {
	a, log := quietObservedApp(t)
	writeQuietState(t, a, time.Now().Add(time.Hour))
	_ = liveLead(t, a)
	info, err := a.reg.Get(os.Getpid())
	if err != nil || info == nil {
		t.Fatalf("registry lead: %v", err)
	}
	now := time.Now()
	entry := record.Entry{Path: filepath.Join(a.home, "entry"), UID: os.Getuid(), Born: now.Add(-time.Hour), Bytes: 10 << 20,
		ByteSec: (1 << 30) * 60, Owner: &record.Owner{Session: &record.Session{Pid: os.Getpid(), Name: "lead", ProcStart: info.ProcStart}}}
	snapshot := record.State{Updated: now, Entries: []record.Entry{entry}}
	b, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	cfg := policy.Default(a.home)
	cfg.Mode, cfg.Lead = policy.ModeAlert, "lead"
	cfg.RunDir, cfg.TrackDir = filepath.Join(a.home, "run"), filepath.Join(a.home, "track")
	cfg.Policy.MinEntry, cfg.Policy.MinOwner, cfg.Policy.BudgetGBM = 1, 1, 0.01
	cfg.Escalation.After = 1
	for _, dir := range []string{cfg.RunDir, cfg.TrackDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, record.StateFile), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(a.dir("tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := a.tmpRound(&cfg, false); err != nil {
		t.Fatal(err)
	}
	assertQuietAccounting(t, a, log, "tmp")
}

func TestQuietUnixListenerReadsWholeFrame(t *testing.T) {
	// Сохраняем покрытие простого пути записи полного кадра в настоящую папку сокета.
	a := quietTestApp(t)
	sock, got := quietListener(t)
	if err := a.sendSocket(0, "lead", sock, false, "кадр"); err != nil {
		t.Fatal(err)
	}
	frame := <-got
	if !strings.Contains(frame, `"type":"user"`) {
		t.Fatalf("кадр: %q", frame)
	}
}

func TestQuietBrokenStateLoggedByObserver(t *testing.T) {
	a, log := quietObservedApp(t)
	sock, got := quietListener(t)
	if err := os.MkdirAll(a.dir("quiet"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.quietPath(), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	v := queue.View{Now: now, Lead: "lead", Serious: func(string) bool { return true }, Alive: func(string) bool { return true }}
	state := queue.State{}
	ticket := queue.Ticket{Key: "p/c", Project: "p", Slug: "c", Title: "critical", Prio: "CRIT", Status: "открыт"}
	notices := state.Step([]queue.Ticket{ticket}, v)
	if len(notices) == 0 {
		t.Fatal("не создано уведомление queue")
	}
	alive := map[string]session.Info{"lead": {Name: "lead", SocketPath: sock}}
	a.deliverQueueNotices(&config.Config{Queue: queue.Config{Mode: queue.ModeOn}}, alive, &state, v, notices, now)
	select {
	case msg := <-got:
		t.Fatalf("при битом файле тихого режима письмо ушло: %s", msg)
	case <-time.After(100 * time.Millisecond):
	}
	out := log.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, a.quietPath()) || strings.Contains(out, "aguard тихо") {
		t.Fatalf("битый файл тихого режима: ожидался WARN с путём, не «тихо»: %s", out)
	}
}

func TestQuietLogLineHasWholeFirstLine(t *testing.T) {
	a, log := quietObservedApp(t)
	sock, _ := quietListener(t)
	writeQuietState(t, a, time.Now().Add(time.Minute))
	first := "aguard: " + strings.Repeat("длинная первая строка ", 20)
	if err := a.sendSocket(0, "lead", sock, false, first+"\nтело письма"); !errors.Is(err, errQuiet) {
		t.Fatalf("ошибка тихого режима: %v", err)
	}
	out := log.String()
	if !strings.Contains(out, strings.TrimSpace(first)) || strings.Contains(out, "тело письма") || !strings.Contains(out, "to=lead") {
		t.Fatalf("строка «тихо»: нужна первая строка целиком, без тела: %s", out)
	}
}
