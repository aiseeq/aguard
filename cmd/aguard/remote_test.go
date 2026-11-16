package main

import (
	"fmt"
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

func TestRemoteOwnerByProjectWhenEventHasNoWorker(t *testing.T) {
	dir := t.TempDir()
	project := "/home/aisee/.cache/worktrees/akit/page-shot-fonts-upd"
	id := "1115-2252-page-shot-fonts-upd"
	jobDir := filepath.Join(dir, ".local", "state", "worker", id)
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		t.Fatal(err)
	}
	job := `{"id":"` + id + `","dir":"` + project + `","master":"akit-master"}`
	if err := os.WriteFile(filepath.Join(jobDir, "job.json"), []byte(job), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	a := &app{home: dir, log: slog.New(slog.NewTextHandler(&logs, nil))}
	cfg := &remotebroken.Config{Lead: "lead"}
	e := remotebroken.Event{Project: project, Task: "exec", Host: "pc"}
	got := a.remoteOwners(cfg, []remotebroken.Event{e})
	if len(got) != 1 || got[0] != "akit-master" {
		t.Fatalf("владелец = %v, хотим akit-master; журнал: %s", got, logs.String())
	}
	if strings.Contains(logs.String(), "владелец события неизвестен") {
		t.Fatalf("неожиданный WARN: %s", logs.String())
	}
}

func TestRemoteWorkerOwnerBeatsNewerProjectJob(t *testing.T) {
	dir := t.TempDir()
	project := filepath.Join(dir, "project")
	now := time.Now()
	var eventWorkerID string
	writeJob := func(ago time.Duration, id, master string) {
		t.Helper()
		id = now.Add(-ago).Format("0102-1504") + "-" + id
		job := fmt.Sprintf(`{"id":%q,"dir":%q,"master":%q,"created":%q}`, id, project, master, now.Add(-ago).Format(time.RFC3339Nano))
		if err := os.MkdirAll(filepath.Join(dir, ".local", "state", "worker", id), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".local", "state", "worker", id, "job.json"), []byte(job), 0o600); err != nil {
			t.Fatal(err)
		}
		if master == "worker-a-master" {
			eventWorkerID = id
		}
	}
	writeJob(2*time.Hour, "worker-a", "worker-a-master")
	writeJob(time.Hour, "newer-b", "project-b-master")
	a := &app{home: dir, log: slog.New(slog.NewTextHandler(&strings.Builder{}, nil))}
	got := a.remoteOwners(&remotebroken.Config{Lead: "lead"}, []remotebroken.Event{{
		Project: project, Task: "exec", Host: "pc", Worker: eventWorkerID,
	}})
	if len(got) != 1 || got[0] != "worker-a-master" {
		t.Fatalf("владелец события = %v, хотим worker-a-master", got)
	}
}

func TestRemoteSkipsGraftMergeProjectLookup(t *testing.T) {
	dir := t.TempDir()
	workerState := filepath.Join(dir, ".local", "state", "worker")
	id := time.Now().Format("0102-1504") + "-broken"
	if err := os.MkdirAll(filepath.Join(workerState, id), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workerState, id, "job.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	a := &app{home: dir, log: slog.New(slog.NewTextHandler(&logs, nil))}
	project := filepath.Join(os.TempDir(), "graft-merge-test", "tree")
	a.remoteOwners(&remotebroken.Config{Lead: "lead"}, []remotebroken.Event{{
		Project: project, Task: "gate", Host: "pc",
	}})
	if strings.Contains(logs.String(), "не удалось определить владельца по project") {
		t.Fatalf("временное дерево запустило поиск заданий: %s", logs.String())
	}
}
