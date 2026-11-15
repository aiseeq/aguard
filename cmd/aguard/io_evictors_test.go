package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/evictors"
	"github.com/aiseeq/aguard/internal/iowatch"
)

func TestIOServiceDetailAggregatesWithoutPerIntervalDiskWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory", "evictors.json")
	service := ioService{a: &app{stateDir: t.TempDir()}, rec: iowatch.NewRecorder(iowatch.Default()), evictors: evictors.NewAggregate(), evictorPath: path}
	step := iowatch.Step{At: time.Date(2026, 11, 15, 1, 0, 0, 0, time.UTC)}
	step.AttachEvictorInterval(&evictors.Interval{At: step.At, Changes: []evictors.Change{{Path: "/app.slice/adream.service", Process: "dreamstats", Anon: 2 << 20, OtherSwap: 3 << 20, Sessions: 2, Candidate: true}}})
	service.detail(step)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("detail записал файл до редкого save: err=%v", err)
	}
	if err := service.saveEvictorsStop(step.At); err != nil {
		t.Fatal(err)
	}
	a, err := evictors.Load(path, step.At)
	if err != nil {
		t.Fatal(err)
	}
	r := a.Report(step.At.Add(-time.Hour), 15)
	if len(r.Rows) != 1 || r.Rows[0].Process != "dreamstats" || r.Rows[0].Swap != 3<<20 {
		t.Fatalf("строка не дошла от detail до файла: %+v", r.Rows)
	}
	var out strings.Builder
	if err := r.Print(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "dreamstats") {
		t.Fatalf("вывод сохранённой сводки: %s", out.String())
	}
}

func TestIOServiceEvictorWriteDoesNotBlockSamplingAndCloseWaits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "evictors.json")
	service := ioService{a: &app{stateDir: dir}, rec: iowatch.NewRecorder(iowatch.Default()), evictors: evictors.NewAggregate(), evictorPath: path}
	at := time.Date(2026, 11, 15, 1, 0, 0, 0, time.UTC)
	service.evictors.Add(&evictors.Interval{At: at, Changes: []evictors.Change{{Path: "/app.slice/first.service", Anon: 2 << 20, OtherSwap: 3 << 20, Candidate: true}}})
	started, release := make(chan int, 2), make(chan struct{}, 2)
	var active, maximum atomic.Int32
	var writes atomic.Int32
	service.evictorWriter = func(path string, snapshot *evictors.Aggregate) error {
		current := active.Add(1)
		for {
			previous := maximum.Load()
			if current <= previous || maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		n := writes.Add(1)
		started <- int(n)
		<-release
		err := evictors.Save(path, snapshot)
		active.Add(-1)
		return err
	}
	if err := service.saveEvictors(at); err != nil {
		t.Fatal(err)
	}
	if n := <-started; n != 1 {
		t.Fatalf("первый writer: %d", n)
	}
	step := iowatch.Step{At: at.Add(time.Hour)}
	step.AttachEvictorInterval(&evictors.Interval{At: step.At, Changes: []evictors.Change{{Path: "/app.slice/second.service", Anon: 2 << 20, OtherSwap: 3 << 20, Candidate: true}}})
	stepDone := make(chan struct{})
	go func() {
		service.detail(step)
		close(stepDone)
	}()
	select {
	case <-stepDone:
	case <-time.After(time.Second):
		t.Fatal("sampling waits for blocked summary writer")
	}
	if err := service.saveEvictors(step.At); err != nil {
		t.Fatal(err)
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- service.saveEvictorsStop(step.At) }()
	release <- struct{}{}
	if n := <-started; n != 2 {
		t.Fatalf("close должен записать более свежий снимок, writer=%d", n)
	}
	select {
	case err := <-closeDone:
		t.Fatalf("закрытие завершилось до окончания последней записи: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	release <- struct{}{}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if writes.Load() != 2 || maximum.Load() != 1 {
		t.Fatalf("записи должны быть последовательны: count=%d max concurrent=%d", writes.Load(), maximum.Load())
	}
	loaded, err := evictors.Load(path, step.At)
	if err != nil || len(loaded.Hours) != 2 {
		t.Fatalf("последний снимок не пережил остановку: hours=%+v err=%v", loaded, err)
	}
}

func TestIOServiceSaveStopWaitsForFinalEvictorWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "evictors.json")
	service := ioService{
		a:    &app{stateDir: dir, log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))},
		path: filepath.Join(dir, "cases.jsonl"), coverPath: filepath.Join(dir, "coverage.json"),
		seriesPath: filepath.Join(dir, "series.jsonl"), profilesPath: filepath.Join(dir, "profiles.json"),
		series: iowatch.NewSeries(iowatch.Default().SeriesKeep.D()), profiles: iowatch.NewProfiles(),
		cover:    iowatch.NewCoverer(iowatch.CoverRecord{}, time.Now()),
		evictors: evictors.NewAggregate(), evictorPath: path,
	}
	at := time.Date(2026, 11, 15, 1, 0, 0, 0, time.UTC)
	service.evictors.Add(&evictors.Interval{At: at, Changes: []evictors.Change{{Path: "/app.slice/final.service", Anon: 2 << 20, OtherSwap: 2 << 20, Candidate: true}}})
	started, release := make(chan struct{}), make(chan struct{})
	service.evictorWriter = func(path string, snapshot *evictors.Aggregate) error {
		close(started)
		<-release
		return evictors.Save(path, snapshot)
	}
	stopDone := make(chan struct{})
	go func() {
		service.saveStop(at)
		close(stopDone)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("saveStop не запустил сохранение последней сводки")
	}
	select {
	case <-stopDone:
		release <- struct{}{}
		t.Fatal("saveStop завершился до конца записи")
	case <-time.After(10 * time.Millisecond):
	}
	release <- struct{}{}
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("saveStop не дождался последней записи")
	}
	loaded, err := evictors.Load(path, at)
	if err != nil || len(loaded.Report(at, 15).Rows) != 1 {
		t.Fatalf("последняя сводка не сохранена: aggregate=%+v err=%v", loaded, err)
	}
}

func TestIOServiceEvictorSaveAcknowledgesUnchangedSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "evictors.json")
	service := ioService{evictors: evictors.NewAggregate(), evictorPath: path}
	at := time.Date(2026, 11, 15, 1, 0, 0, 0, time.UTC)
	service.evictors.Add(&evictors.Interval{At: at, Changes: []evictors.Change{{Path: "/app.slice/once.service", Anon: 2 << 20, OtherSwap: 2 << 20, Candidate: true}}})
	started := make(chan struct{}, 2)
	service.evictorWriter = func(path string, snapshot *evictors.Aggregate) error {
		started <- struct{}{}
		return evictors.Save(path, snapshot)
	}
	if err := service.saveEvictors(at); err != nil {
		t.Fatal(err)
	}
	<-started
	deadline := time.After(time.Second)
	for len(service.evictorSaveDone) == 0 {
		select {
		case <-deadline:
			t.Fatal("первая запись не завершилась")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if err := service.saveEvictors(at.Add(10 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
		t.Fatal("неизменённая сводка записана повторно")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestLoadEvictorsQuarantinesCorruptFileAndWarns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "evictors.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	now := time.Date(2026, 11, 15, 1, 2, 3, 0, time.UTC)
	service := ioService{a: &app{log: slog.New(slog.NewTextHandler(&log, nil))}, evictors: evictors.NewAggregate(), evictorPath: path}
	service.loadEvictors(now)
	bad := path + ".bad-20261115T010203"
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("битый оригинал остался на месте: %v", err)
	}
	data, err := os.ReadFile(bad)
	if err != nil || string(data) != "not json" {
		t.Fatalf("история не сохранена отдельно: %q err=%v", data, err)
	}
	if service.evictors == nil || service.evictors.Dirty() || !strings.Contains(log.String(), "сводка кандидатов не разобрана") {
		t.Fatalf("пустой старт или предупреждение отсутствует: aggregate=%+v log=%s", service.evictors, log.String())
	}
	service.evictors.Add(&evictors.Interval{At: now, Changes: []evictors.Change{{Path: "/app.slice/fresh.service", Anon: 2 << 20, OtherSwap: 2 << 20, Candidate: true}}})
	if err := service.saveEvictorsStop(now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bad); err != nil {
		t.Fatalf("следующая запись не должна затирать отложенную историю: %v", err)
	}
	if _, err := evictors.Load(path, now); err != nil {
		t.Fatalf("новая сводка после битой файла не читается: %v", err)
	}
}

func TestLoadEvictorsDoesNotOverwriteWhenQuarantineFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "evictors.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 15, 1, 2, 3, 0, time.UTC)
	bad := path + ".bad-20261115T010203"
	if err := os.MkdirAll(filepath.Join(bad, "occupied"), 0o700); err != nil {
		t.Fatal(err)
	}
	service := ioService{a: &app{log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))}, evictors: evictors.NewAggregate(), evictorPath: path}
	service.loadEvictors(now)
	service.evictors.Add(&evictors.Interval{At: now, Changes: []evictors.Change{{Path: "/app.slice/fresh.service", Anon: 2 << 20, OtherSwap: 2 << 20, Candidate: true}}})
	if err := service.saveEvictorsStop(now); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "not json" || !service.evictorStoreBlocked {
		t.Fatalf("история должна остаться нетронутой при провале quarantine: %q err=%v blocked=%v", data, err, service.evictorStoreBlocked)
	}
}

func TestMemoryEvictorsReportDoesNotTouchTemporaryFiles(t *testing.T) {
	a, _ := testIOApp(t)
	path := evictorStorePath(a)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	tmp := path + ".in-progress.tmp"
	if err := os.WriteFile(tmp, []byte("writer owns this"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.memoryCmd([]string{"evictors"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(tmp)
	if err != nil || string(data) != "writer owns this" {
		t.Fatalf("команда отчёта изменила временный файл: %q err=%v", data, err)
	}
}

func TestLoadEvictorsCleansTemporaryFilesAtDaemonStartup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "evictors.json")
	tmp := path + ".abandoned.tmp"
	if err := os.WriteFile(tmp, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := ioService{a: &app{log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))}, evictorPath: path}
	service.loadEvictors(time.Date(2026, 11, 15, 1, 0, 0, 0, time.UTC))
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("старт демона не удалил оставленный temp: %v", err)
	}
}

func TestLoadEvictorsTempCleanupFailureDoesNotQuarantineSummary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "evictors.json")
	now := time.Date(2026, 11, 15, 1, 0, 0, 0, time.UTC)
	a := evictors.NewAggregate()
	a.Add(&evictors.Interval{At: now, Changes: []evictors.Change{{Path: "/app.slice/valid.service", Anon: 2 << 20, OtherSwap: 2 << 20, Candidate: true}}})
	if err := evictors.Save(path, a); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	tmpDir := path + ".stuck.tmp"
	if err := os.MkdirAll(filepath.Join(tmpDir, "occupied"), 0o700); err != nil {
		t.Fatal(err)
	}
	service := ioService{a: &app{log: slog.New(slog.NewTextHandler(&log, nil))}, evictorPath: path}
	service.loadEvictors(now)
	if len(service.evictors.Report(now.Add(-time.Hour), 15).Rows) != 1 {
		t.Fatalf("ошибка удаления temp повредила загрузку сводки: %+v", service.evictors)
	}
	if _, err := os.Stat(path + ".bad-20261115T010000"); !os.IsNotExist(err) {
		t.Fatalf("сводка ошибочно перенесена в .bad: %v", err)
	}
	if !strings.Contains(log.String(), "удаление временных сводок кандидатов") {
		t.Fatalf("нет отдельного предупреждения об ошибке удаления: %s", log.String())
	}
}
