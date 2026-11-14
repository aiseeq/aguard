package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
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

func TestTmpRoundAndReportHonorUserTmpfiles(t *testing.T) {
	a, logs := quietObservedApp(t)
	a.log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	a.out = &bytes.Buffer{}
	a.uid = os.Getuid()
	stat, err := proc.ReadStat("/proc", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	a.reg = session.Registry{Dir: filepath.Join(t.TempDir(), "sessions"), ProcRoot: "/proc"}
	if err := os.MkdirAll(a.reg.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	info, err := json.Marshal(session.Info{Pid: os.Getpid(), ProcStart: stat.Start, Name: "lead", SocketPath: "@tmp-config-test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.reg.Dir, fmt.Sprint(os.Getpid())+".json"), info, 0o600); err != nil {
		t.Fatal(err)
	}

	configDir := filepath.Join(a.home, ".config", "user-tmpfiles.d")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	content := "# rules\n\nd! /tmp/from-d 0700 - - - -\nD /tmp/from-D 0700 - - - -\nv /tmp/from-v 0700 - - - -\nq /tmp/from-q 0700 - - - -\nQ /tmp/from-Q 0700 - - - -\nL+ /tmp/from-link - - - - /tmp/from-d\nd /tmp/%h/from-spec 0700 - - - -\nd relative-path 0700 - - - -\nd \"/tmp/from-quoted\" 0700 - - - -\n"
	confPath := filepath.Join(configDir, "custom.conf")
	if err := os.WriteFile(confPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	paths := []string{"/tmp/from-d", "/tmp/from-D", "/tmp/from-v", "/tmp/from-q", "/tmp/from-Q",
		"/tmp/from-link", "/tmp/%h/from-spec", "relative-path", "/var/tmp/outside", "/tmp/from-quoted", "/tmp/neighbor", "/tmp/from-d2"}
	entries := make([]record.Entry, 0, len(paths))
	for _, path := range paths {
		entries = append(entries, record.Entry{Path: path, Born: now.Add(-time.Hour), Bytes: 20 << 20})
	}
	run, track := t.TempDir(), t.TempDir()
	stateBytes, err := json.Marshal(record.State{Updated: now, Entries: entries})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run, record.StateFile), stateBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(track, record.StateFile), stateBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(track, record.OwnersFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := policy.Default(a.home)
	cfg.RunDir, cfg.TrackDir, cfg.Lead = run, track, "lead"
	cfg.Policy.MinEntry, cfg.Policy.MinOwner, cfg.Policy.BudgetGBM, cfg.Policy.MaxListed = 1, 1, 0.01, 25
	if err := a.tmpRound(&cfg, true); err != nil {
		t.Fatal(err)
	}
	hasPath := func(output, path string) bool { return strings.Contains(output, path+" —") }
	summary := a.out.(*bytes.Buffer).String()
	for _, path := range []string{"/tmp/from-d", "/tmp/from-D", "/tmp/from-v", "/tmp/from-q", "/tmp/from-Q"} {
		if hasPath(summary, path) {
			t.Errorf("путь из правила каталога попал в сводку: %s\n%s", path, summary)
		}
	}
	for _, path := range []string{"/tmp/from-link", "/tmp/%h/from-spec", "relative-path", "/var/tmp/outside", "/tmp/from-quoted", "/tmp/neighbor", "/tmp/from-d2"} {
		if !hasPath(summary, path) {
			t.Errorf("путь не из применимого правила отсутствует в сводке: %s", path)
		}
	}

	a.out = &bytes.Buffer{}
	if err := a.tmpReport(&cfg, 0); err != nil {
		t.Fatal(err)
	}
	reportText := a.out.(*bytes.Buffer).String()
	if !strings.Contains(reportText, "живых 7 ") {
		t.Fatalf("tmp report должен учитывать семь путей без правила каталога: %s", reportText)
	}
	if !strings.Contains(logs.String(), `reason="спецификатор %"`) || !strings.Contains(logs.String(), `text="d /tmp/%h/from-spec 0700 - - - -"`) ||
		!strings.Contains(logs.String(), `reason="путь не абсолютный"`) || !strings.Contains(logs.String(), `text="d relative-path 0700 - - - -"`) {
		t.Fatalf("пропуск спецификатора не записан с причиной и строкой: %s", logs.String())
	}

	// d /tmp — корень сводки: строка пропускается, сводка та же, что без правил.
	summaryWith := func(conf string) string {
		t.Helper()
		if err := os.WriteFile(confPath, []byte(conf), 0o600); err != nil {
			t.Fatal(err)
		}
		a.out = &bytes.Buffer{}
		if err := a.tmpRound(&cfg, true); err != nil {
			t.Fatal(err)
		}
		return a.out.(*bytes.Buffer).String()
	}
	baseline := summaryWith("")
	logs.Reset()
	rootSummary := summaryWith("d /tmp 0700 - - - -\n")
	if rootSummary != baseline {
		t.Errorf("правило d /tmp изменило сводку:\n%s\nбез правил:\n%s", rootSummary, baseline)
	}
	if !strings.Contains(baseline, "/tmp/") {
		t.Fatalf("сводка без правил пуста — проверка корня ничего не доказывает:\n%s", baseline)
	}
	if !strings.Contains(logs.String(), `reason="корень сводки tmp"`) {
		t.Errorf("пропуск d /tmp без причины в журнале: %s", logs.String())
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
