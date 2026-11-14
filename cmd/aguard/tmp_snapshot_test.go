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
	if out := a.out.(*bytes.Buffer).String(); !strings.Contains(out, "=== отложено: alpha-master") ||
		!strings.Contains(out, "задание J идёт, папку уберёт worker") ||
		!strings.Contains(out, "не названные: строка без действия, отдельным письмом не шлётся") || strings.Contains(out, "сводка lead") {
		t.Fatalf("tmpRound неверно отложил живое задание без действия: %s", out)
	}
	if !strings.Contains(log.String(), "часть заданий worker не прочиталась") {
		t.Fatalf("ошибка job.json не записана в журнал: %s", log.String())
	}
	a.out = &bytes.Buffer{}
	if err := a.tmpSendTest(&cfg, "alpha-master", true); err != nil {
		t.Fatal(err)
	}
	sendTestText := a.out.(*bytes.Buffer).String()
	lineAt := strings.Index(sendTestText, "задание J идёт, папку уберёт worker")
	if lineAt < 0 {
		t.Fatalf("send-test не показал живое задание: %s", sendTestText)
	}
	lineEnd := strings.IndexByte(sendTestText[lineAt:], '\n')
	if lineEnd < 0 || strings.Contains(sendTestText[lineAt:lineAt+lineEnd], "rm -rf") {
		t.Fatalf("send-test предложил удалить живую папку: %s", sendTestText)
	}
	workerItem := policy.Item{Entry: record.Entry{Path: "/tmp/w-J-go", Born: now.Add(-10 * time.Minute)}}
	workerKey := policy.EntryKey(&workerItem)
	if err := os.MkdirAll(filepath.Dir(a.tmpStatePath()), 0o700); err != nil {
		t.Fatal(err)
	}
	state := &policy.UserState{Mode: cfg.Mode, Alerts: map[string]*policy.AlertState{
		workerKey: {LastTarget: "alpha-master", LastSent: now.Add(-2 * time.Hour), LastBytes: 1 << 30},
	}}
	if err := a.saveUserState(state); err != nil {
		t.Fatal(err)
	}
	if err := a.tmpRound(&cfg, false); err != nil {
		t.Fatal(err)
	}
	if got := a.loadUserState().Alerts[workerKey].LastBytes; got != 1<<30 {
		t.Fatalf("tmpRound потерял last_bytes при записи/чтении состояния: %d", got)
	}
	if err := a.saveUserState(&policy.UserState{Mode: cfg.Mode}); err != nil {
		t.Fatal(err)
	}
	a.out = &bytes.Buffer{}
	if err := a.tmpReport(&cfg, 0); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a.out.(*bytes.Buffer).String(), "без срока (попутно)") {
		t.Fatalf("tmp report не показал пустой NextAt как попутный: %s", a.out.(*bytes.Buffer).String())
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

type tmpPlanLive struct{ target policy.Target }

func (l tmpPlanLive) ByPid(pid int, _ string) (policy.Target, bool) {
	return l.target, pid == l.target.Pid
}

func (l tmpPlanLive) ByName(name string) (policy.Target, bool) {
	return l.target, name == l.target.Name
}

func (tmpPlanLive) WorkerByUnit(string) (policy.WorkerJob, bool, error) {
	return policy.WorkerJob{}, false, nil
}

func TestPrintDryWorkerDeferredReasons(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	target := policy.Target{Name: "alpha-master", Pid: 42}
	live := tmpPlanLive{target: target}
	worker := func(path string, size int64) policy.Item {
		return policy.Item{Entry: record.Entry{Path: path, Born: now.Add(-time.Hour), Bytes: size},
			WorkerLive: true, WorkerMaster: target.Name, WorkerJobID: "J"}
	}
	newConfig := func() policy.Config {
		c := policy.Default("/home/u")
		c.Lead = target.Name
		return c
	}
	printPlan := func(t *testing.T, c *policy.Config, groups []policy.Group, st *policy.UserState) string {
		t.Helper()
		send, deferred := c.Plan(now, groups, live, st, 1)
		out := &bytes.Buffer{}
		(&app{out: out}).printDry(c, groups, send, deferred, now)
		return out.String()
	}

	t.Run("unnamed and no send uses its cause", func(t *testing.T) {
		c := newConfig()
		item := worker("/tmp/worker-A", 100<<20)
		out := printPlan(t, &c, []policy.Group{{Items: []policy.Item{item}}}, &policy.UserState{})
		if !strings.Contains(out, "не названные: строка без действия, отдельным письмом не шлётся") ||
			!strings.Contains(out, "строки живых заданий отложены") {
			t.Fatalf("dry-run неверно описал неназванную строку: %s", out)
		}
	})
	t.Run("full summary says it did not fit", func(t *testing.T) {
		c := newConfig()
		c.Policy.MaxListed = 1
		owner := &record.Owner{Proc: record.Proc{UID: 1000, Comm: "go"}, Session: &record.Session{
			Pid: target.Pid, Name: target.Name, Cwd: "/home/u", ProcStart: "start",
		}}
		action := c.Items([]record.Entry{{Path: "/tmp/action", UID: 1000, Born: now.Add(-time.Hour), Bytes: 1 << 30, Owner: owner}}, 1000)
		item := worker("/tmp/worker-A", 100<<20)
		out := printPlan(t, &c, []policy.Group{{Items: action}, {Items: []policy.Item{item}}}, &policy.UserState{})
		if !strings.Contains(out, "не поместились в сводку: уйдут, когда в ней будет место") ||
			strings.Contains(out, "строка без действия") || strings.Contains(out, "повтор при росте") {
			t.Fatalf("dry-run неверно описал переполненную сводку: %s", out)
		}
	})
	t.Run("named repeat prints prior size and no expired time", func(t *testing.T) {
		c := newConfig()
		item := worker("/tmp/worker-A", 100<<20)
		key := policy.EntryKey(&item)
		st := &policy.UserState{Alerts: map[string]*policy.AlertState{
			key: {LastTarget: target.Name, LastSent: now.Add(-2 * time.Hour), LastBytes: 100 << 20},
		}}
		out := printPlan(t, &c, []policy.Group{{Items: []policy.Item{item}}}, st)
		if !strings.Contains(out, "названные: повтор при росте в 4 раза или давлении") ||
			!strings.Contains(out, "при прошлом письме 100 МБ") || strings.Contains(out, "не раньше") {
			t.Fatalf("dry-run неверно описал отложенный повтор: %s", out)
		}
	})
	t.Run("named and unnamed workers name both causes", func(t *testing.T) {
		c := newConfig()
		a, b := worker("/tmp/worker-A", 100<<20), worker("/tmp/worker-B", 100<<20)
		st := &policy.UserState{Alerts: map[string]*policy.AlertState{
			policy.EntryKey(&a): {LastTarget: target.Name, LastSent: now.Add(-5 * time.Minute), LastBytes: 100 << 20},
		}}
		out := printPlan(t, &c, []policy.Group{{Items: []policy.Item{a}}, {Items: []policy.Item{b}}}, st)
		if !strings.Contains(out, "названные: повтор") || !strings.Contains(out, "не названные: строка без действия") ||
			strings.Count(out, "без действия") != 1 {
			t.Fatalf("dry-run не различил причины отложенных строк: %s", out)
		}
	})
	t.Run("unnamed worker beside ladder repeat is not said to be known", func(t *testing.T) {
		c := newConfig()
		item := worker("/tmp/worker-A", 100<<20)
		ordinary := policy.Item{Entry: record.Entry{Path: "/tmp/ordinary", Born: now.Add(-time.Hour), Bytes: 1 << 30}, Class: policy.ClassNonAgent}
		key := policy.EntryKey(&ordinary)
		st := &policy.UserState{Alerts: map[string]*policy.AlertState{
			key: {Sent: c.Escalation.After, LastTarget: target.Name, LastSent: now.Add(-5 * time.Minute)},
		}}
		out := printPlan(t, &c, []policy.Group{{Items: []policy.Item{ordinary}}, {Items: []policy.Item{item}}}, st)
		if !strings.Contains(out, "строки лестницы уже получали") ||
			!strings.Contains(out, "живые задания: не названные: строка без действия") || strings.Contains(out, "уже получал(а) эти записи") {
			t.Fatalf("dry-run неверно разделил причины лестницы и worker: %s", out)
		}
	})
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
