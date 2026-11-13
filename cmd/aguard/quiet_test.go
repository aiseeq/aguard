package main

import (
	"bytes"
	"context"
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
	"github.com/aiseeq/aguard/internal/restart"
	"github.com/aiseeq/akit/proc"
	"github.com/aiseeq/akit/session"
)

func quietTestApp(t *testing.T) *app {
	t.Helper()
	home := t.TempDir()
	return &app{
		home: home, stateDir: filepath.Join(home, "state"), log: slog.New(slog.NewTextHandler(io.Discard, nil)), out: io.Discard,
		quietRuntime: &quietRuntime{afterBoot: 45 * time.Minute},
		bootTime:     func() (time.Time, error) { return time.Now().Add(-48 * time.Hour), nil },
		bootID:       func() (string, error) { return "test-boot", nil },
	}
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
	if err := a.sendSocket(0, "lead", sock, false, "tmp.summary", "строка письма\nтело"); !errors.Is(err, errQuiet) {
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
	if err := a.sendSocket(0, "lead", sock, false, "tmp.summary", "ещё тихо"); !errors.Is(err, errQuiet) {
		t.Fatalf("до срока ожидался errQuiet: %v", err)
	}
	time.Sleep(time.Until(until) + 50*time.Millisecond)
	if err := a.sendSocket(0, "lead", sock, false, "tmp.summary", "проверка доставки"); err != nil {
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
		if err := a.sendSocket(0, "lead", sock, false, "tmp.summary", "без режима"); err != nil {
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
			err := a.sendSocket(0, "lead", sock, false, "tmp.summary", "не отправлять")
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
		err := a.sendSocket(0, "lead", "unused", false, "tmp.summary", "не отправлять")
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
	if strings.Contains(string(b), "boot_id") || strings.Contains(string(b), "boot_off") {
		t.Fatalf("state.json перестал быть совместимым со старым форматом: %s", b)
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

func TestQuietAfterBootQueueWaitsUrgentPassesAndRetries(t *testing.T) {
	a := quietTestApp(t)
	boot := time.Date(2026, 11, 13, 8, 0, 0, 0, time.Local)
	now := boot.Add(10 * time.Minute)
	a.now = func() time.Time { return now }
	a.bootTime = func() (time.Time, error) { return boot, nil }
	a.bootID = func() (string, error) { return "boot-one", nil }
	a.cfgPath = filepath.Join(a.home, "config.toml")
	if err := os.WriteFile(a.cfgPath, []byte("[quiet]\nafter_boot = \"45m\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.setQuietWindow(45 * time.Minute)

	urgentSock, urgentGot := quietListener(t)
	if err := a.sendUrgent(map[string]session.Info{"lead": {Name: "lead", SocketPath: urgentSock}}, "lead", false, "sigguard.denied", "авария"); err != nil {
		t.Fatalf("аварийное письмо через окно: %v", err)
	}
	select {
	case msg := <-urgentGot:
		if !strings.Contains(msg, "авария") {
			t.Fatalf("аварийное письмо: %s", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("аварийное письмо не прошло через окно")
	}

	now = boot.Add(10 * time.Minute)
	queueSock, queueGot := quietListener(t)
	alive := map[string]session.Info{"lead": {Name: "lead", SocketPath: queueSock}}
	view := queue.View{Now: now, Lead: "lead", Serious: func(string) bool { return true }, Alive: func(string) bool { return true }}
	state := queue.State{}
	ticket := queue.Ticket{Key: "p/c", Project: "p", Slug: "c", Title: "critical", Prio: "CRIT", Status: "открыт"}
	_ = state.Step([]queue.Ticket{ticket}, view)
	notices := state.Step([]queue.Ticket{ticket}, view)
	if len(notices) == 0 {
		notices = state.Step([]queue.Ticket{ticket}, view)
	}
	if len(notices) == 0 {
		t.Fatal("queue не создала уведомление")
	}
	a.deliverQueueNotices(&config.Config{Queue: queue.Config{Mode: queue.ModeOn}}, alive, &state, view, notices, now)
	if !state.Tickets[ticket.Key].Crit.At.IsZero() {
		t.Fatal("queue подтвердила уведомление в окне загрузки")
	}

	now = boot.Add(46 * time.Minute)
	view.Now = now
	notices = state.Step([]queue.Ticket{ticket}, view)
	if len(notices) == 0 {
		t.Fatal("после окна не осталось повтора queue")
	}
	a.deliverQueueNotices(&config.Config{Queue: queue.Config{Mode: queue.ModeOn}}, alive, &state, view, notices, now)
	select {
	case msg := <-queueGot:
		if !strings.Contains(msg, "critical") {
			t.Fatalf("неожиданное письмо queue: %s", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("queue не повторила письмо после окна")
	}
}

func TestQuietAfterBootOffAndExplicitQuietPrecedence(t *testing.T) {
	a := quietTestApp(t)
	now := time.Now().Truncate(time.Second)
	boot := now.Add(-10 * time.Minute)
	a.now = func() time.Time { return now }
	a.bootTime = func() (time.Time, error) { return boot, nil }
	a.bootID = func() (string, error) { return "boot-one", nil }
	a.cfgPath = filepath.Join(a.home, "config.toml")
	if err := os.WriteFile(a.cfgPath, []byte("[quiet]\nafter_boot = \"45m\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	a.out = &output
	if err := a.quietCmd(nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "после загрузки машины в ") || !strings.Contains(output.String(), "after_boot = 45m") {
		t.Fatalf("статус окна: %s", output.String())
	}
	want := fmt.Sprintf("тихо до %s (осталось 35m0s): после загрузки машины в %s, [quiet] after_boot = 45m\n", boot.Add(45*time.Minute).Local().Format("15:04:05 MST"), boot.Local().Format("15:04"))
	if output.String() != want {
		t.Fatalf("статус окна = %q, хотим %q", output.String(), want)
	}
	if err := a.quietCmd([]string{"off"}); err != nil {
		t.Fatal(err)
	}
	if _, active, err := a.readQuietAt(now); err != nil || active {
		t.Fatalf("off не снял окно: active=%v err=%v", active, err)
	}
	boot = boot.Add(time.Hour)
	a.bootID = func() (string, error) { return "boot-two", nil }
	now = boot.Add(time.Minute)
	if _, active, err := a.readQuietAt(now); err != nil || !active {
		t.Fatalf("новая загрузка не включила окно: active=%v err=%v", active, err)
	}

	// Явный срок длиннее окна загрузки должен определять статус и причину.
	writeQuietState(t, a, now.Add(2*time.Hour))
	state, active, err := a.readQuietAt(now)
	if err != nil || !active || state.Why != "тест" || !state.Until.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("явная тишина не победила короткое окно: %+v active=%v err=%v", state, active, err)
	}
}

func TestQuietAfterBootOffSurvivesQuietOnAndExpiry(t *testing.T) {
	a := quietTestApp(t)
	boot := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	now := boot.Add(10 * time.Minute)
	a.now = func() time.Time { return now }
	a.bootTime = func() (time.Time, error) { return boot, nil }
	bootID := "same-boot"
	a.bootID = func() (string, error) { return bootID, nil }
	a.cfgPath = filepath.Join(a.home, "config.toml")
	if err := os.WriteFile(a.cfgPath, []byte("[quiet]\nafter_boot = \"45m\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.quietCmd([]string{"off"}); err != nil {
		t.Fatal(err)
	}
	if err := a.quietCmd([]string{"on", "--until", now.Add(5 * time.Minute).Format(time.RFC3339), "--why", "коротко", "--by", "test"}); err != nil {
		t.Fatal(err)
	}
	if _, active, err := a.readQuietAt(now.Add(6 * time.Minute)); err != nil || active {
		t.Fatalf("окно вернулось после конца explicit: active=%v err=%v", active, err)
	}
	if _, err := os.Stat(a.quietBootOffPath()); err != nil {
		t.Fatalf("off отметка не пережила quiet on: %v", err)
	}
	if err := a.quietCmd([]string{"off"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(a.quietBootOffPath())
	if err != nil {
		t.Fatal(err)
	}
	var repeatOutput bytes.Buffer
	a.out = &repeatOutput
	if err := a.quietCmd([]string{"off"}); err != nil {
		t.Fatal(err)
	}
	if repeatOutput.String() != "не был включён\n" {
		t.Fatalf("повторный off: %q", repeatOutput.String())
	}
	after, err := os.Stat(a.quietBootOffPath())
	if err != nil || !os.SameFile(info, after) {
		t.Fatalf("повторный off изменил отметку: before=%v after=%v err=%v", info.ModTime(), after, err)
	}
}

func TestQuietOffRemovesBrokenAndExpiredExplicitState(t *testing.T) {
	for _, tc := range []struct{ name, content string }{
		{name: "broken", content: "{broken"},
		{name: "expired", content: "expired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := quietTestApp(t)
			if tc.name == "expired" {
				writeQuietState(t, a, time.Now().Add(-time.Minute))
			} else {
				if err := os.MkdirAll(a.dir("quiet"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(a.quietPath(), []byte(tc.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var out bytes.Buffer
			a.out = &out
			if err := a.quietCmd([]string{"off"}); err != nil {
				t.Fatal(err)
			}
			if out.String() != "громко\n" {
				t.Fatalf("off output %q", out.String())
			}
			if _, err := os.Stat(a.quietPath()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("state не удалён: %v", err)
			}
		})
	}
}

func TestQuietBootReadErrorDoesNotBlockStatusOrSend(t *testing.T) {
	a, logs := quietObservedApp(t)
	bootErr := errors.New("нет proc stat")
	a.bootTime = func() (time.Time, error) { return time.Time{}, bootErr }
	var out bytes.Buffer
	a.out = &out
	if err := a.quietCmd(nil); err != nil {
		t.Fatalf("quiet status: %v", err)
	}
	if out.String() != "громко\n" {
		t.Fatalf("status при boot ошибке: %q", out.String())
	}
	sock, got := quietListener(t)
	if err := a.sendSocket(0, "lead", sock, false, "queue.crit", "письмо"); err != nil {
		t.Fatalf("boot ошибка остановила отправку: %v", err)
	}
	select {
	case msg := <-got:
		if !strings.Contains(msg, "письмо") {
			t.Fatalf("сообщение: %s", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("письмо не отправлено")
	}
	if strings.Count(logs.String(), "окно после загрузки недоступно") != 1 {
		t.Fatalf("предупреждения не ограничены/отсутствуют: %s", logs.String())
	}
	if err := a.quietCmd([]string{"off"}); err != nil {
		t.Fatalf("quiet off при boot ошибке: %v", err)
	}
}

func TestQuietConfigControlsCLIAndReloadedDaemonWindow(t *testing.T) {
	a := quietTestApp(t)
	now := time.Date(2026, 11, 13, 8, 50, 0, 0, time.Local)
	boot := now.Add(-50 * time.Minute)
	a.now = func() time.Time { return now }
	a.bootTime = func() (time.Time, error) { return boot, nil }
	a.bootID = func() (string, error) { return "config-boot", nil }
	a.cfgPath = filepath.Join(a.home, "config.toml")
	writeConfig := func(value string) {
		t.Helper()
		if err := os.WriteFile(a.cfgPath, []byte("[quiet]\nafter_boot = \""+value+"\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sock, got := quietListener(t)
	writeConfig("0")
	if _, err := a.config(); err != nil {
		t.Fatal(err)
	}
	if err := a.sendSocket(0, "lead", sock, false, "queue.crit", "отправить при after_boot=0"); err != nil {
		t.Fatalf("CLI после загрузки с after_boot=0: %v", err)
	}
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("CLI не отправил письмо при after_boot=0")
	}
	writeConfig("2h")
	if _, err := a.config(); err != nil {
		t.Fatal(err)
	}
	if err := a.sendSocket(0, "lead", sock, false, "queue.crit", "подавить при after_boot=2h"); !errors.Is(err, errQuiet) {
		t.Fatalf("CLI не применил after_boot=2h после 50 минут: %v", err)
	}
	writeConfig("10m")
	if _, err := a.config(); err != nil {
		t.Fatal(err)
	}
	if _, active, err := a.readQuietAt(now); err != nil || active {
		t.Fatalf("after_boot=10m не применился: active=%v err=%v", active, err)
	}
	boot = now.Add(-5 * time.Minute)
	state, active, err := a.readQuietAt(now)
	if err != nil || !active || !state.Until.Equal(boot.Add(10*time.Minute)) {
		t.Fatalf("короткое окно не сохраняет срок 10m: state=%+v active=%v err=%v", state, active, err)
	}
	writeConfig("45m")
	boot = now.Add(-5 * time.Minute)
	if _, err := a.config(); err != nil {
		t.Fatal(err)
	}
	if _, active, err := a.readQuietAt(now); err != nil || !active {
		t.Fatalf("после reload 45m окно не включилось: active=%v err=%v", active, err)
	}
	writeConfig("[broken")
	if _, err := a.config(); err == nil {
		t.Fatal("битый конфиг принят")
	}
	if _, active, err := a.readQuietAt(now); err != nil || active {
		t.Fatalf("битый конфиг не выключил окно: active=%v err=%v", active, err)
	}
}

func TestQuietBrokenBootOffFailsSafeAndQuietOffRepairs(t *testing.T) {
	a := quietTestApp(t)
	var logs bytes.Buffer
	a.log = slog.New(slog.NewTextHandler(&logs, nil))
	boot := time.Now().Add(-5 * time.Minute)
	a.now = func() time.Time { return boot.Add(5 * time.Minute) }
	a.bootTime = func() (time.Time, error) { return boot, nil }
	a.bootID = func() (string, error) { return "broken-marker-boot", nil }
	a.cfgPath = filepath.Join(a.home, "config.toml")
	if err := os.WriteFile(a.cfgPath, []byte("[quiet]\nafter_boot = \"45m\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.loadQuietConfig(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(a.dir("quiet"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"{broken", `{"boot_id":""}`} {
		logs.Reset()
		a.quietRuntime.mu.Lock()
		a.quietRuntime.warnedAt = nil
		a.quietRuntime.mu.Unlock()
		if err := os.WriteFile(a.quietBootOffPath(), []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, active, err := a.readQuiet(); err != nil || !active {
			t.Fatalf("битая отметка %q должна считать окно активным: active=%v err=%v", bad, active, err)
		}
		if !strings.Contains(logs.String(), "окно после загрузки недоступно") {
			t.Fatalf("битая отметка %q не вызвала предупреждение: %s", bad, logs.String())
		}
		if err := a.quietCmd([]string{"off"}); err != nil {
			t.Fatal(err)
		}
		off, err := a.readQuietBootOff()
		if err != nil || off != "broken-marker-boot" {
			t.Fatalf("quiet off не починил отметку %q: id=%q err=%v", bad, off, err)
		}
	}
}

func TestQuietWarningsRepeatPerReasonAfterTenMinutes(t *testing.T) {
	a, logs := quietObservedApp(t)
	now := time.Date(2026, 11, 13, 8, 0, 0, 0, time.Local)
	a.now = func() time.Time { return now }
	errConfig, errBoot := errors.New("bad config"), errors.New("bad boot time")
	a.quietBootWarning("config", errConfig)
	a.quietBootWarning("boot_time", errBoot)
	now = now.Add(9 * time.Minute)
	a.quietBootWarning("config", errConfig)
	now = now.Add(time.Minute)
	a.quietBootWarning("config", errConfig)
	if got := strings.Count(logs.String(), "окно после загрузки недоступно"); got != 3 {
		t.Fatalf("число предупреждений до/после 10 минут и для второй причины: %d, журнал %s", got, logs.String())
	}
}

func TestQueueNoticesPassesBootTimeIntoView(t *testing.T) {
	a := quietTestApp(t)
	a.reg = session.ForHome(a.home)
	a.namesMem.loaded = true
	a.namesMem.table = map[string]string{}
	boot := time.Date(2026, 11, 13, 8, 0, 0, 0, time.Local)
	a.bootTime = func() (time.Time, error) { return boot, nil }
	queueCfg := queue.Config{}
	dirs, err := queueFingerprintDirs(&queueCfg)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := queue.Fingerprint(dirs)
	if err != nil {
		t.Fatal(err)
	}
	now := boot.Add(10 * time.Minute)
	ticket := queue.Ticket{Key: "p/task", Project: "p", Slug: "task", Dir: filepath.Join(a.home, "p"), Title: "missing master", Prio: "HIGH", Status: "открыт", Master: "gone"}
	a.queueCache = queueTicketCache{Loaded: true, Fingerprint: fingerprint, ScannedAt: now, Tickets: []queue.Ticket{ticket}}
	cfg := &config.Config{}
	cfg.Queue.Mode = queue.ModeOn
	cfg.Queue.BootGrace = policy.Duration(time.Hour)
	if err := os.MkdirAll(a.dir("queue"), 0o700); err != nil {
		t.Fatal(err)
	}
	seeded, err := json.Marshal(queue.State{Seeded: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.dir("queue"), "state.json"), seeded, 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	a.out = &output
	if err := a.queueNotices(context.Background(), cfg, restart.NewJudge(a.home), true, now); err != nil {
		t.Fatal(err)
	}
	if output.Len() != 0 {
		t.Fatalf("queue сообщила об отсутствующей сессии в boot grace; вызов queueBootGrace мог отсутствовать: %s", output.String())
	}
}

func TestQueueBootTimeErrorUsesQueueWarningAccounting(t *testing.T) {
	a, logs := quietObservedApp(t)
	a.bootTime = func() (time.Time, error) { return time.Time{}, errors.New("stat unavailable") }
	view := queue.View{}
	a.queueBootGrace(&view, time.Hour)
	if !strings.Contains(logs.String(), "aguard queue: время загрузки") {
		t.Fatalf("нет предупреждения очереди об ошибке btime: %s", logs.String())
	}
	if err := a.act.Flush(time.Now(), activityKeep); err != nil {
		t.Fatal(err)
	}
	count := 0
	_, err := activity.NewStore(a.dir("activity")).Read(time.Time{}, func(r *activity.Record) {
		if r.Watcher == "queue" && r.Kind == activity.KindWarn && r.What == "время загрузки" {
			count += r.Count
		}
	})
	if err != nil || count != 1 {
		t.Fatalf("учёт WARN queue: count=%d err=%v", count, err)
	}
}

func TestRunLogsQuietStartupWindow(t *testing.T) {
	a := quietTestApp(t)
	now := time.Date(2026, 11, 13, 8, 10, 0, 0, time.Local)
	boot := now.Add(-10 * time.Minute)
	a.now = func() time.Time { return now }
	a.bootTime = func() (time.Time, error) { return boot, nil }
	a.bootID = func() (string, error) { return "run-boot", nil }
	a.cfgPath = filepath.Join(a.home, "config.toml")
	if err := os.WriteFile(a.cfgPath, []byte("[quiet]\nafter_boot = \"45m\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	a.log = slog.New(slog.NewTextHandler(&logs, nil))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.run(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "тихо после загрузки до ") {
		t.Fatalf("run не журналирует окно: %s", logs.String())
	}
}

func TestQuietBadConfigDisablesWindowButKeepsQuietCommandsWorking(t *testing.T) {
	a, logs := quietObservedApp(t)
	a.cfgPath = filepath.Join(a.home, "broken.toml")
	if err := os.WriteFile(a.cfgPath, []byte("[quiet\nafter_boot = \"45m\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a.out = &out
	if err := a.quietCmd(nil); err != nil {
		t.Fatalf("status with bad config: %v", err)
	}
	if out.String() != "громко\n" {
		t.Fatalf("bad config still enabled window: %q", out.String())
	}
	if !strings.Contains(logs.String(), "конфиг:") {
		t.Fatalf("нет предупреждения о конфиге: %s", logs.String())
	}
	sock, got := quietListener(t)
	if err := a.sendSocket(0, "lead", sock, false, "queue.crit", "deliver"); err != nil {
		t.Fatalf("bad config blocked send: %v", err)
	}
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("письмо не отправилось")
	}
	if err := a.quietCmd([]string{"off"}); err != nil {
		t.Fatalf("off with bad config: %v", err)
	}
}

func TestQuietBootBoundaryAndLongerThanExplicit(t *testing.T) {
	a := quietTestApp(t)
	boot := time.Date(2026, 11, 13, 8, 0, 0, 0, time.Local)
	a.bootTime = func() (time.Time, error) { return boot, nil }
	a.bootID = func() (string, error) { return "boundary-boot", nil }
	for _, tc := range []struct {
		at   time.Time
		want bool
	}{
		{at: boot.Add(45*time.Minute - time.Nanosecond), want: true},
		{at: boot.Add(45 * time.Minute), want: false},
	} {
		_, active, err := a.readQuietAt(tc.at)
		if err != nil || active != tc.want {
			t.Errorf("на границе %s: active=%v err=%v хотим %v", tc.at, active, err, tc.want)
		}
	}
	short := boot.Add(20 * time.Minute)
	writeQuietState(t, a, short)
	state, active, err := a.readQuietAt(boot.Add(10 * time.Minute))
	if err != nil || !active || !state.BootWindow || !state.Until.Equal(boot.Add(45*time.Minute)) {
		t.Fatalf("длинное окно не победило явное: %+v active=%v err=%v", state, active, err)
	}
}

func TestQuietStartupLineWhenBootWindowActive(t *testing.T) {
	a := quietTestApp(t)
	boot := time.Now().Add(-10 * time.Minute)
	a.now = time.Now
	a.bootTime = func() (time.Time, error) { return boot, nil }
	var logs bytes.Buffer
	a.log = slog.New(slog.NewTextHandler(&logs, nil))
	a.logQuietStartup()
	if !strings.Contains(logs.String(), "тихо после загрузки до ") {
		t.Fatalf("нет стартовой строки окна: %s", logs.String())
	}
	writeQuietState(t, a, time.Now().Add(2*time.Hour))
	logs.Reset()
	a.logQuietStartup()
	if !strings.Contains(logs.String(), "тихо после загрузки: действует явная тишина до ") {
		t.Fatalf("для более длинной явной тишины нет стартовой строки: %s", logs.String())
	}
}

func TestQueueViewReceivesBootTimeAndGrace(t *testing.T) {
	a := quietTestApp(t)
	boot := time.Date(2026, 11, 13, 8, 0, 0, 0, time.Local)
	a.bootTime = func() (time.Time, error) { return boot, nil }
	view := queue.View{}
	a.queueBootGrace(&view, time.Hour)
	if !view.BootAt.Equal(boot) || view.BootGrace != time.Hour {
		t.Fatalf("queue view boot info: BootAt=%s BootGrace=%s", view.BootAt, view.BootGrace)
	}
}

func TestSystemBootTimeUsesAkitSnapshot(t *testing.T) {
	want, err := proc.BootTime(procRoot)
	if err != nil {
		t.Fatal(err)
	}
	got, err := (&app{}).systemBootTime()
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(want) {
		t.Fatalf("system boot time = %s, akit says %s", got, want)
	}
}

func TestQuietAfterBootDisabledOrExpired(t *testing.T) {
	for _, tc := range []struct {
		name   string
		after  string
		upTime time.Duration
	}{
		{name: "disabled", after: "0", upTime: time.Minute},
		{name: "shorter-than-uptime", after: "5m", upTime: 10 * time.Minute},
		{name: "expired", after: "45m", upTime: time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := quietTestApp(t)
			now := time.Date(2026, 11, 13, 8, 0, 0, 0, time.Local)
			boot := now.Add(-tc.upTime)
			a.now = func() time.Time { return now }
			a.bootTime = func() (time.Time, error) { return boot, nil }
			a.bootID = func() (string, error) { return "test-boot", nil }
			a.cfgPath = filepath.Join(a.home, "config.toml")
			cfg := fmt.Sprintf("[quiet]\nafter_boot = %q\n", tc.after)
			if err := os.WriteFile(a.cfgPath, []byte(cfg), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.after == "0" {
				a.setQuietWindow(0)
			} else {
				d, err := time.ParseDuration(tc.after)
				if err != nil {
					t.Fatal(err)
				}
				a.setQuietWindow(d)
			}
			if state, active, err := a.readQuietAt(now); err != nil || active {
				t.Fatalf("окно действует: %+v active=%v err=%v", state, active, err)
			}
		})
	}
}

func TestQuietOffWithAfterBootZeroDoesNotCreateBootMarker(t *testing.T) {
	a := quietTestApp(t)
	a.cfgPath = filepath.Join(a.home, "config.toml")
	if err := os.WriteFile(a.cfgPath, []byte("[quiet]\nafter_boot = \"0\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a.out = &out
	if err := a.quietCmd([]string{"off"}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "не был включён\n" {
		t.Fatalf("quiet off с нулевым окном: %q", out.String())
	}
	if _, err := os.Stat(a.quietBootOffPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("создана отметка выключения при after_boot=0: %v", err)
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
	a.ioSend(cfg, "lead", "io.urgent", "срочно", "aguard io: фриз", 2500)
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
	if err := a.sendSocket(0, "lead", sock, false, "tmp.summary", "кадр"); err != nil {
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
	if err := a.sendSocket(0, "lead", sock, false, "tmp.summary", first+"\nтело письма"); !errors.Is(err, errQuiet) {
		t.Fatalf("ошибка тихого режима: %v", err)
	}
	out := log.String()
	if !strings.Contains(out, strings.TrimSpace(first)) || strings.Contains(out, "тело письма") || !strings.Contains(out, "to=lead") {
		t.Fatalf("строка «тихо»: нужна первая строка целиком, без тела: %s", out)
	}
}
