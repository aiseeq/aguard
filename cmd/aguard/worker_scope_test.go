package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/cpuwatch"
	"github.com/aiseeq/aguard/internal/policy"
	"github.com/aiseeq/aguard/internal/record"
	"github.com/aiseeq/akit/session"
)

const workerScopeCgroup = "/user.slice/user-1000.slice/user@1000.service/agents.slice/worker-io-2230260.scope"

func scopeJob(t *testing.T, root, dir string, pid int, created, ended string, master string) {
	t.Helper()
	path := filepath.Join(root, dir)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	job, err := json.Marshal(map[string]any{"id": dir, "pid": pid, "created": created, "master": master, "dir": "/work/" + dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "job.json"), job, 0o600); err != nil {
		t.Fatal(err)
	}
	if ended != "" {
		status, _ := json.Marshal(map[string]string{"ended": ended})
		if err := os.WriteFile(filepath.Join(path, "status.json"), status, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func resolveScope(t *testing.T, jobs *cpuwatch.Jobs, born time.Time) (policy.Item, error) {
	t.Helper()
	item := policy.Item{Class: policy.ClassNonAgent, Entry: record.Entry{Born: born, Owner: &record.Owner{
		Proc: record.Proc{Cgroup: workerScopeCgroup},
	}}}
	live := &liveSessions{jobs: jobs, byName: map[string]session.Info{}}
	items := []policy.Item{item}
	err := policy.ResolveWorkerJobs(items, live)
	return items[0], err
}

func TestWorkerScopeRoutesByPidAndBorn(t *testing.T) {
	root := t.TempDir()
	created := "2026-11-27T18:21:14+01:00"
	ended := "2026-11-27T18:27:22+01:00"
	scopeJob(t, root, "1127-1821-ask-review", 2230260, created, ended, "ask")
	born, _ := time.Parse(time.RFC3339, "2026-11-27T18:21:48+01:00")
	item, err := resolveScope(t, &cpuwatch.Jobs{Dir: root}, born)
	if err != nil || item.WorkerMaster != "ask" || item.OwnerKey != "w:ask" || item.WorkerJobID != "1127-1821-ask-review" {
		t.Fatalf("scope не назначен заданию: item=%+v err=%v", item, err)
	}
}

func TestWorkerScopeRejectsBornOutsideInterval(t *testing.T) {
	root := t.TempDir()
	scopeJob(t, root, "1127-1821-ask-review", 2230260, "2026-11-27T18:21:14+01:00", "2026-11-27T18:27:22+01:00", "ask")
	born, _ := time.Parse(time.RFC3339, "2026-11-27T18:30:00+01:00")
	item, err := resolveScope(t, &cpuwatch.Jobs{Dir: root}, born)
	if err != nil || item.WorkerMaster != "" {
		t.Fatalf("вне интервала назначено заданию: %+v err=%v", item, err)
	}
}

func TestWorkerScopeChoosesNewestMatchingCreated(t *testing.T) {
	root := t.TempDir()
	scopeJob(t, root, "1127-1800-old", 2230260, "2026-11-27T18:00:00+01:00", "2026-11-27T18:40:00+01:00", "old")
	scopeJob(t, root, "1127-1810-new", 2230260, "2026-11-27T18:10:00+01:00", "2026-11-27T18:40:00+01:00", "new")
	born, _ := time.Parse(time.RFC3339, "2026-11-27T18:20:00+01:00")
	item, err := resolveScope(t, &cpuwatch.Jobs{Dir: root}, born)
	if err != nil || item.WorkerMaster != "new" || item.WorkerJobID != "1127-1810-new" {
		t.Fatalf("выбрано не новое задание: %+v err=%v", item, err)
	}
}

func TestWorkerScopeRealOwnerAndJobFixture(t *testing.T) {
	data := "testdata/worker-scope"
	line, err := os.ReadFile(filepath.Join(data, "owner.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var event record.Event
	if err := json.Unmarshal(line, &event); err != nil {
		t.Fatal(err)
	}
	if event.Owner == nil || event.Owner.Proc.Cgroup != workerScopeCgroup {
		t.Fatalf("в fixture строка scope изменилась: owner=%+v", event.Owner)
	}
	root := t.TempDir()
	dir := "1127-1821-ask-ревью"
	if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"job.json", "status.json"} {
		b, err := os.ReadFile(filepath.Join(data, dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Имя папки задания настоящее: по его минуте (MMDD-HHMM) поиск выбирает кандидатов.
	items := []policy.Item{{Class: policy.ClassNonAgent, Entry: record.Entry{Born: event.Born, Owner: event.Owner}}}
	live := &liveSessions{jobs: &cpuwatch.Jobs{Dir: root}, byName: map[string]session.Info{}}
	err = policy.ResolveWorkerJobs(items, live)
	item := items[0]
	if err != nil || item.WorkerMaster != "ask" || item.OwnerKey != "w:ask" {
		t.Fatalf("настоящая запись не назначена ask: %+v err=%v", item, err)
	}
}

func TestWorkerScopeLiveRequiresWorkerComm(t *testing.T) {
	root, procRoot := t.TempDir(), t.TempDir()
	scopeJob(t, root, "1127-1821-live", 2230260, "2026-11-27T18:21:14+01:00", "", "ask")
	if err := os.MkdirAll(filepath.Join(procRoot, "2230260"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(procRoot, "2230260", "comm"), []byte("worker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	born, _ := time.Parse(time.RFC3339, "2026-11-27T18:21:48+01:00")
	jobs := &cpuwatch.Jobs{Dir: root, ProcRoot: procRoot}
	item, err := resolveScope(t, jobs, born)
	if err != nil || !item.WorkerLive {
		t.Fatalf("worker с comm=worker не признан живым: %+v err=%v", item, err)
	}
	if err := os.WriteFile(filepath.Join(procRoot, "2230260", "comm"), []byte("bash\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	item, err = resolveScope(t, &cpuwatch.Jobs{Dir: root, ProcRoot: procRoot}, born)
	if err != nil || item.WorkerLive {
		t.Fatalf("чужой процесс признан живым: %+v err=%v", item, err)
	}
}

func TestWorkerScopeLookupReturnsBrokenStatusError(t *testing.T) {
	root := t.TempDir()
	dir := "1127-1821-bad-status"
	scopeJob(t, root, dir, 2230260, "2026-11-27T18:21:14+01:00", "", "ask")
	if err := os.WriteFile(filepath.Join(root, dir, "status.json"), []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	born, _ := time.Parse(time.RFC3339, "2026-11-27T18:21:48+01:00")
	live := &liveSessions{
		jobs:   &cpuwatch.Jobs{Dir: root},
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		byName: map[string]session.Info{},
	}
	job, ok, err := live.WorkerByPid(2230260, born)
	if !ok || job.ID != dir || err == nil {
		t.Fatalf("WorkerByPid потерял атрибуцию или ошибку: job=%+v ok=%v err=%v", job, ok, err)
	}
}

func TestWorkerScopeLookupBrokenStatusStillRoutesToMaster(t *testing.T) {
	root := t.TempDir()
	dir := "1127-1821-bad-status"
	scopeJob(t, root, dir, 2230260, "2026-11-27T18:21:14+01:00", "", "ask")
	if err := os.WriteFile(filepath.Join(root, dir, "status.json"), []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	born, _ := time.Parse(time.RFC3339, "2026-11-27T18:21:48+01:00")
	item, err := resolveScope(t, &cpuwatch.Jobs{Dir: root}, born)
	if item.WorkerMaster != "ask" || item.OwnerKey != "w:ask" {
		t.Fatalf("битый status.json увёл запись лиду: %+v err=%v", item, err)
	}
	if err == nil {
		t.Fatal("сбой чтения status.json потерян")
	}
}
