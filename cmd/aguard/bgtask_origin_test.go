package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/bgtask"
	"github.com/aiseeq/aguard/internal/policy"
	"github.com/aiseeq/akit/proc"
	"github.com/aiseeq/akit/session"
)

func TestBgtaskAlreadyNotifiedStateSkipsSearch(t *testing.T) {
	a := &app{}
	task := &bgtask.Task{ID: "balready"}
	state := bgtask.State{task.ID: {At: time.Now(), Auto: true}}
	if state[task.ID].Missing {
		t.Fatal("fixture must represent an earlier notification")
	}
	if a.bgtaskShouldSearch(state, task, true, false) {
		t.Fatal("задача с уже отправленным письмом снова попала в поиск")
	}
}

func TestBgtaskRoundDoesNotNotifyAlreadySeenTaskAgain(t *testing.T) {
	var logs strings.Builder
	home, root, stateDir := t.TempDir(), t.TempDir(), t.TempDir()
	registryDir := filepath.Join(t.TempDir(), "sessions")
	for _, dir := range []string{registryDir, filepath.Join(stateDir, "bgtask")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	const uid, pid, holder = 1000, 500, 501
	started := time.Now().Add(-2 * time.Hour)
	output := filepath.Join(t.TempDir(), "-home-alice", "s1", "tasks", "balready.output")
	if err := os.MkdirAll(filepath.Dir(output), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, []byte("old output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(output, started, started); err != nil {
		t.Fatal(err)
	}
	fdDir := filepath.Join(root, "500", "fd")
	if err := os.MkdirAll(fdDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(output, filepath.Join(fdDir, "1")); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(session.ProjectsDir(home), "-home-alice", "s1.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	content := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_old","name":"Bash","input":{"command":"echo old","description":"Ранее найденная задача"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_old","content":"moved to the background (ID: balready"}]},"toolUseResult":{"backgroundTaskId":"balready","timedOutAfterMs":120000}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(transcript, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := json.Marshal(session.Info{Pid: holder, Name: "test", SocketPath: "/tmp/test.sock"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(registryDir, "501.json"), info, 0o600); err != nil {
		t.Fatal(err)
	}
	a := &app{
		home: home, stateDir: stateDir, procDir: root, uid: uid,
		reg: session.Registry{Dir: registryDir},
		log: slog.New(slog.NewTextHandler(&logs, nil)),
		bgtaskSnapshot: func(string, int, time.Time) (*proc.Snapshot, error) {
			return &proc.Snapshot{Procs: map[int]*proc.Proc{
				pid:    {Pid: pid, UID: uid, Session: holder, SessionID: "s1", StartAt: started},
				holder: {Pid: holder, UID: uid, StartAt: started},
			}}, nil
		},
	}
	statePath := filepath.Join(stateDir, "bgtask", "state.json")
	initial := bgtask.State{"balready": {At: started, Auto: true}}
	if err := initial.Save(statePath, []bgtask.Task{{ID: "balready"}}); err != nil {
		t.Fatal(err)
	}
	cfg := bgtask.Default()
	cfg.Mode = bgtask.ModeLog
	for range 2 {
		if err := a.bgtaskRound(&cfg, false); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Contains(logs.String(), "написал бы") {
		t.Fatalf("bgtaskRound повторно уведомил задачу из состояния:\n%s", logs.String())
	}
	state, err := bgtask.LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !state["balready"].Auto || state["balready"].Missing {
		t.Fatalf("состояние ранее уведомлённой задачи изменилось: %+v", state["balready"])
	}
}

func TestBgtaskRoundWaitLetterUsesConfiguredAge(t *testing.T) {
	var logs strings.Builder
	home, root, stateDir, registryDir := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	sock, delivered := quietListener(t)
	const uid, pid, holder = 1000, 500, 501
	started := time.Now().Add(-2 * time.Hour)
	output := filepath.Join(t.TempDir(), "-home-alice", "s1", "tasks", "bwaitcfg.output")
	if err := os.MkdirAll(filepath.Dir(output), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, []byte("waiting\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fdDir := filepath.Join(root, "500", "fd")
	if err := os.MkdirAll(fdDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(output, filepath.Join(fdDir, "1")); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(session.ProjectsDir(home), "-home-alice", "s1.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	content := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_wait","name":"Bash","input":{"command":"until grep -q DONE out.txt; do sleep 60; done","description":"Ждать завершения"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_wait","content":"Command running in background with ID: bwaitcfg."}]},"toolUseResult":{"backgroundTaskId":"bwaitcfg"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(transcript, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stateDir, "bgtask"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(registryDir, 0o700); err != nil {
		t.Fatal(err)
	}
	info, err := json.Marshal(session.Info{Pid: holder, Name: "test", SocketPath: sock})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(registryDir, "501.json"), info, 0o600); err != nil {
		t.Fatal(err)
	}
	a := &app{
		home: home, stateDir: stateDir, procDir: root, uid: uid,
		reg: session.Registry{Dir: registryDir},
		log: slog.New(slog.NewTextHandler(&logs, nil)),
		bgtaskSnapshot: func(string, int, time.Time) (*proc.Snapshot, error) {
			return &proc.Snapshot{Procs: map[int]*proc.Proc{
				pid:    {Pid: pid, UID: uid, Session: holder, SessionID: "s1", StartAt: started},
				holder: {Pid: holder, UID: uid, StartAt: started},
			}}, nil
		},
	}
	cfg := bgtask.Default()
	cfg.Mode = bgtask.ModeAlert
	cfg.Age = policy.Duration(90 * time.Minute)
	if err := a.bgtaskRound(&cfg, false); err != nil {
		t.Fatal(err)
	}
	var msg string
	select {
	case msg = <-delivered:
	case <-time.After(time.Second):
		t.Fatal("bgtaskRound не доставил письмо")
	}
	var envelope struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(msg), &envelope); err != nil {
		t.Fatalf("доставлено некорректное письмо: %v: %s", err, msg)
	}
	msg = envelope.Message.Content
	for _, want := range []string{
		"срок — 1 ч 30 мин (bgtask.age)",
		"TaskStop bwaitcfg",
		"tk wait <тикет> --until <когда>",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("отправленное письмо не содержит %q:\n%s", want, msg)
		}
	}
}

func TestBgtaskMissingOriginStaleOnceFreshNever(t *testing.T) {
	var logs strings.Builder
	a, _ := bgtaskTestApp(t, &logs)
	now := time.Now()
	stale := &bgtask.Task{ID: "bstale", Output: "/tmp/-home-alice/s1/tasks/bstale.output"}
	fresh := &bgtask.Task{ID: "bfresh", Output: "/tmp/-home-alice/s1/tasks/bfresh.output"}
	state := bgtask.State{}
	stateFile := filepath.Join(t.TempDir(), "state.json")
	alive := []bgtask.Task{*stale, *fresh}

	for pass := range 2 {
		for _, item := range []struct {
			task  *bgtask.Task
			stale bool
		}{{stale, true}, {fresh, false}} {
			if !a.bgtaskShouldSearch(state, item.task, true, false) {
				t.Fatalf("проход %d пропустил поиск задачи %s", pass+1, item.task.ID)
			}
			a.bgtaskHandleOrigin(&bgtask.Config{Mode: bgtask.ModeLog}, state, item.task, item.stale, false, now)
		}
		if err := state.Save(stateFile, alive); err != nil {
			t.Fatal(err)
		}
		var err error
		state, err = bgtask.LoadState(stateFile)
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(logs.String(), "вызов задачи не найден в транскриптах"); n != 1 {
		t.Fatalf("строк об отсутствии вызова %d, ожидалась одна за застывшую задачу и два прохода:\n%s", n, logs.String())
	}
	if !state[stale.ID].Missing {
		t.Fatalf("нет отметки о застывшей задаче: %+v", state[stale.ID])
	}
	if _, ok := state[fresh.ID]; ok {
		t.Fatalf("незастывшая задача попала в состояние: %+v", state[fresh.ID])
	}
}

func TestBgtaskMissingOriginDoesNotStopLaterSearch(t *testing.T) {
	var logs strings.Builder
	a, home := bgtaskTestApp(t, &logs)
	const holder = 4242
	info, err := json.Marshal(session.Info{Pid: holder, Name: "test", SocketPath: "/tmp/test.sock"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.reg.Dir, "4242.json"), info, 0o600); err != nil {
		t.Fatal(err)
	}
	task := &bgtask.Task{ID: "blater", Holder: holder, Output: "/tmp/-home-alice/s1/tasks/blater.output",
		SessionIDs: map[string]struct{}{"s2": {}}}
	state := bgtask.State{}
	stateFile := filepath.Join(t.TempDir(), "state.json")
	alive := []bgtask.Task{*task}
	now := time.Now()

	if !a.bgtaskShouldSearch(state, task, true, false) {
		t.Fatal("первый проход пропустил задачу")
	}
	a.bgtaskHandleOrigin(&bgtask.Config{Mode: bgtask.ModeLog}, state, task, true, false, now)
	if !state[task.ID].Missing {
		t.Fatalf("первый проход не отметил отсутствующий вызов: %+v", state[task.ID])
	}
	if err := state.Save(stateFile, alive); err != nil {
		t.Fatal(err)
	}
	state, err = bgtask.LoadState(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if !a.bgtaskShouldSearch(state, task, true, false) {
		t.Fatal("отметка Missing остановила поиск на следующем проходе")
	}
	transcript := filepath.Join(session.ProjectsDir(home), "-home-alice", "s2.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o755); err != nil {
		t.Fatal(err)
	}
	content := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_later","name":"Bash","input":{"command":"echo later","description":"Появился позже"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_later","content":"moved to the background (ID: blater"}]},"toolUseResult":{"backgroundTaskId":"blater","timedOutAfterMs":120000}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(transcript, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	a.bgtaskHandleOrigin(&bgtask.Config{Mode: bgtask.ModeLog}, state, task, true, false, now.Add(time.Minute))
	if state[task.ID].Missing || !state[task.ID].Auto {
		t.Fatalf("поздний вызов не прошёл обычный путь: %+v", state[task.ID])
	}
	if n := strings.Count(logs.String(), "вызов задачи не найден в транскриптах"); n != 1 {
		t.Fatalf("строк об отсутствии вызова %d, ожидалась одна:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "написал бы") {
		t.Fatalf("поздний вызов не привёл к обычному уведомлению в режиме log:\n%s", logs.String())
	}
}

func bgtaskTestApp(t *testing.T, logs *strings.Builder) (*app, string) {
	t.Helper()
	home := t.TempDir()
	registry := filepath.Join(t.TempDir(), "sessions")
	if err := os.MkdirAll(registry, 0o700); err != nil {
		t.Fatal(err)
	}
	return &app{home: home, reg: session.Registry{Dir: registry}, log: slog.New(slog.NewTextHandler(logs, nil))}, home
}
