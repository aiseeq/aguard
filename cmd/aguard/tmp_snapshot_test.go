package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/activity"
	"github.com/aiseeq/aguard/internal/cpuwatch"
	"github.com/aiseeq/aguard/internal/policy"
	"github.com/aiseeq/aguard/internal/record"
	"github.com/aiseeq/akit/proc"
	"github.com/aiseeq/akit/session"
)

func TestTmpWorkerJobOwnerAndLiveUnit(t *testing.T) {
	root, units := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "J"), 0o700); err != nil {
		t.Fatal(err)
	}
	job := filepath.Join(root, "J", "job.json")
	if err := os.WriteFile(job, []byte(`{"id":"J","master":"alpha-master","unit":"worker-J"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(units, "worker-J-2.service"), 0o700); err != nil {
		t.Fatal(err)
	}
	live := &liveSessions{jobs: &cpuwatch.Jobs{Dir: root, Units: units},
		byName: map[string]session.Info{"alpha-master": {Name: "alpha-master", Cwd: "/home/u/work/project"}}}
	// Первый unit умер, но папку использует повторный запуск того же задания.
	jobInfo, ok, err := live.WorkerByUnit("worker-J.service")
	if err != nil || !ok || !jobInfo.Live || jobInfo.ID != "J" || jobInfo.Master != "alpha-master" ||
		jobInfo.Cwd != "/home/u/work/project" {
		t.Fatalf("задание worker: %+v, %v, %v", jobInfo, ok, err)
	}
	if err := os.Remove(filepath.Join(units, "worker-J-2.service")); err != nil {
		t.Fatal(err)
	}
	jobInfo, ok, err = live.WorkerByUnit("worker-J-2.service")
	if err != nil || !ok || jobInfo.Live {
		t.Fatalf("завершённое задание worker: %+v, %v, %v", jobInfo, ok, err)
	}
	if err := os.WriteFile(job, []byte(`{"id":"J","unit":"worker-J"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	live.jobs = &cpuwatch.Jobs{Dir: root, Units: units}
	if jobInfo, ok, err := live.WorkerByUnit("worker-J.service"); err != nil || ok || jobInfo.Master != "" {
		t.Fatalf("задание без заказчика: %+v, %v, %v", jobInfo, ok, err)
	}
}

func TestTmpRoundResolvesWorkerJobForRouting(t *testing.T) {
	a, log := quietObservedApp(t)
	a.out = &bytes.Buffer{}
	a.uid = os.Getuid()
	a.workerJobsDir, a.workerUnitsDir = t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(a.workerJobsDir, "J"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.workerJobsDir, "J", "job.json"),
		[]byte(`{"id":"J","master":"alpha-master","unit":"worker-J"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(a.workerUnitsDir, "worker-J-2.service"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(a.workerJobsDir, "K"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.workerJobsDir, "K", "job.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	stat, err := proc.ReadStat("/proc", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	a.reg = session.Registry{Dir: filepath.Join(t.TempDir(), "sessions"), ProcRoot: "/proc"}
	if err := os.MkdirAll(a.reg.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	info, err := json.Marshal(session.Info{Pid: os.Getpid(), ProcStart: stat.Start, Name: "alpha-master",
		Cwd: "/home/u/work/project", SocketPath: "@tmp-round-no-send"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.reg.Dir, fmt.Sprint(os.Getpid())+".json"), info, 0o600); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	run, track := t.TempDir(), t.TempDir()
	owner := &record.Owner{Proc: record.Proc{Pid: os.Getpid(), Comm: "go", UID: os.Getuid(),
		Cgroup: "/user.slice/user-1000.slice/user@1000.service/app.slice/worker-J.service"}}
	brokenOwner := &record.Owner{Proc: record.Proc{Pid: os.Getpid(), Comm: "go", UID: os.Getuid(),
		Cgroup: "/user.slice/user-1000.slice/user@1000.service/app.slice/worker-K.service"}}
	snapshot, err := json.Marshal(record.State{Updated: now, Entries: []record.Entry{
		{Path: "/tmp/w-J-go", UID: os.Getuid(), Born: now.Add(-10 * time.Minute), Bytes: 1 << 30, Owner: owner},
		{Path: "/tmp/w-K-go", UID: os.Getuid(), Born: now.Add(-10 * time.Minute), Bytes: 1 << 30, Owner: brokenOwner},
	}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := policy.Default(a.home)
	cfg.Mode, cfg.Lead = policy.ModeObserve, "lead"
	cfg.RunDir, cfg.TrackDir = run, track
	cfg.Policy.MinEntry, cfg.Policy.MinOwner, cfg.Policy.BudgetGBM = 1, 1, 1
	for _, dir := range []string{run, track} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, record.StateFile), snapshot, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.tmpRound(&cfg, true); err != nil {
		t.Fatal(err)
	}
	if out := a.out.(*bytes.Buffer).String(); !strings.Contains(out, "=== сводка alpha-master") ||
		!strings.Contains(out, "задание J идёт, папку уберёт worker") || strings.Contains(out, "сводка lead") {
		t.Fatalf("tmpRound не направил живое задание заказчику: %s", out)
	}
	if !strings.Contains(log.String(), "часть заданий worker не прочиталась") {
		t.Fatalf("ошибка job.json не записана в журнал: %s", log.String())
	}
	if err := a.act.Flush(time.Now(), activityKeep); err != nil {
		t.Fatal(err)
	}
	noted := false
	if _, err := activity.NewStore(a.dir("activity")).Read(time.Time{}, func(r *activity.Record) {
		noted = noted || r.Watcher == "tmp" && r.Kind == "worker_lookup_failed"
	}); err != nil {
		t.Fatal(err)
	}
	if !noted {
		t.Fatal("ошибка поиска задания не попала в учёт")
	}
}

// Запись без born исключается с WARN, остальные записи снимка остаются (tmp-measure-after-cleanup).
func TestTmpSnapshotDropsEntryWithoutBornWithWarn(t *testing.T) {
	a, log := quietObservedApp(t)
	run, track := t.TempDir(), t.TempDir()
	now := time.Now()
	b, _ := json.Marshal(record.State{Updated: now, Entries: []record.Entry{
		{Path: "/tmp/noborn", Bytes: 10},
		{Path: "/tmp/ok", Bytes: 20, Born: now.Add(-time.Hour)},
	}})
	if err := os.WriteFile(filepath.Join(run, record.StateFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := a.snapshot(&policy.Config{RunDir: run, TrackDir: track})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Entries) != 1 || s.Entries[0].Path != "/tmp/ok" {
		t.Fatalf("записи снимка: %+v", s.Entries)
	}
	if out := log.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "/tmp/noborn") {
		t.Fatalf("ожидался WARN о записи без born: %s", out)
	}
}
