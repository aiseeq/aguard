package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/iowatch"
	"github.com/aiseeq/aguard/internal/linesock"
)

// ioLiveService — служба с сокетом в t.TempDir() по конфигу; шаги и проход уже в Recorder.
func ioLiveService(t *testing.T, ctx context.Context) (*ioService, *bytes.Buffer, string) {
	t.Helper()
	a, out := testIOApp(t)
	socket := filepath.Join(t.TempDir(), "io-state.sock")
	if err := os.WriteFile(a.cfgPath, fmt.Appendf(nil, "[io]\nsocket = %q\n", socket), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newIOService(ctx, a)
	s.cfg = iowatch.Default()
	s.rec = iowatch.NewRecorder(s.cfg)
	s.rec.Attach(s.series, s.profiles, s.live)
	return s, out, socket
}

// Живые запросы через настоящий unix-сокет: службы нет — «не запущена»; now (с писателями и сводом
// вида), series и profiles; неизвестный запрос и другая версия — ошибка в ответе.
func TestIOSocketNowSeriesProfiles(t *testing.T) {
	ctx := t.Context()
	s, out, socket := ioLiveService(t, ctx)
	a := s.a
	if err := a.ioLive(iowatch.Request{Q: iowatch.QueryNow}, false); !errors.Is(err, errIONotRunning) {
		t.Fatalf("службы нет: %v", err)
	}
	base := time.Now().Add(-3 * time.Second).Truncate(time.Second)
	for i := range 25 {
		at := base.Add(time.Duration(i) * 100 * time.Millisecond)
		if i%10 == 0 {
			s.rec.Detail(iowatch.Step{At: at, Dirty: 40 << 20, Cgroups: []iowatch.Cgroup{{Path: "/user.slice/run-dd.scope", WriteBytes: 30 << 20}}})
		}
		s.rec.Step(iowatch.Step{At: at, WriteBytes: 1 << 20, WIOS: 1, WMS: 3})
	}
	s.ended = []iowatch.Profile{{Kind: "worker:glint-правило", Name: "worker-a.service", End: time.Now().Add(-time.Hour), WriteB: 20 << 20, PeakWriteBps: 8 << 20, DurMS: 90_000},
		{Kind: iowatch.KindAgent, Name: "x-master", End: time.Now().Add(-time.Hour)}}
	s.live.SetProfiles(iowatch.SummarizeProfiles(s.ended))
	go s.serve()
	go s.wait(time.Minute) // горутина цикла: исполняет extra
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("сокет не открылся")
		}
	}
	if info, _ := os.Stat(socket); info.Mode().Perm() != 0o600 {
		t.Fatalf("права сокета %v", info.Mode().Perm())
	}

	if err := a.ioLive(iowatch.Request{Q: iowatch.QueryNow, Kind: "worker:glint-правило"}, true); err != nil {
		t.Fatal(err)
	}
	var now iowatch.Answer
	if err := json.Unmarshal(out.Bytes(), &now); err != nil || now.Now == nil {
		t.Fatalf("now JSON %v: %s", err, out)
	}
	if !now.At.Equal(base.Add(2400*time.Millisecond)) || now.W1s.WriteBps != 10<<20 || now.W1s.WAwaitMS != 3 || now.Dirty != 40<<20 ||
		len(now.Writers) != 1 || now.Writers[0].Name != "run-dd.scope" || now.Writers[0].WriteBps != 30<<20 ||
		now.Profile == nil || now.Profile.Count != 1 || now.Profile.PeakWriteBps.P50 != 8<<20 {
		t.Fatalf("now: %+v", now.Now)
	}
	out.Reset()
	if err := a.ioLive(iowatch.Request{Q: iowatch.QueryNow}, false); err != nil || strings.Count(out.String(), "\n") != 2 ||
		!strings.Contains(out.String(), "пишут: run-dd.scope 30 МБ/с") {
		t.Fatalf("now текстом (%v):\n%s", err, out)
	}

	out.Reset()
	if err := a.ioLiveSince(iowatch.Request{Q: iowatch.QuerySeries}, "1h", true); err != nil {
		t.Fatal(err)
	}
	var series iowatch.Answer
	if err := json.Unmarshal(out.Bytes(), &series); err != nil || len(series.Series) != 2 || series.Series[0].WriteB != 10<<20 || series.Now != nil {
		t.Fatalf("series (%v): %s", err, out)
	}

	out.Reset()
	if err := a.ioLiveSince(iowatch.Request{Q: iowatch.QueryProfiles, Kind: "worker:glint-правило"}, "7d", false); err != nil ||
		!strings.Contains(out.String(), "worker:glint-правило  worker-a.service  W 20 МБ (пик 8 МБ/с)") || strings.Contains(out.String(), "x-master") {
		t.Fatalf("profiles текстом (%v):\n%s", err, out)
	}

	for req, want := range map[string]string{`{"v":1,"q":"bogus"}`: "неизвестный запрос", `{"v":2,"q":"now"}`: "версия протокола 2"} {
		var raw iowatch.Answer
		if err := linesock.Ask(socket, time.Second, json.RawMessage(req), &raw); err != nil {
			t.Fatalf("%s: %v", req, err)
		}
		if raw.V != iowatch.ProtoVersion || !strings.Contains(raw.Error, want) || raw.Now != nil {
			t.Fatalf("%s: %+v", req, raw)
		}
	}
}

// Ряд и профили переживают перезапуск: остановка пишет файлы, старт читает с отсечением по
// series_keep и profiles_keep; битый файл откладывается, история начинается заново.
func TestSeriesProfilesSurviveRestartKeepAndBrokenFile(t *testing.T) {
	a, _ := testIOApp(t)
	var logged bytes.Buffer
	a.log = slog.New(slog.NewTextHandler(&logged, nil))
	now := time.Now()
	s := newIOService(context.Background(), a)
	s.cfg = iowatch.Default()
	for _, at := range []time.Time{now.Add(-25 * time.Hour), now.Add(-2 * time.Hour), now.Add(-time.Hour)} {
		s.series.Add(iowatch.MachineSecond{At: at, WriteB: 5 << 20, Dirty: 3 << 20})
	}
	s.ended = []iowatch.Profile{{Kind: "agent", End: now.Add(-31 * 24 * time.Hour)}, {Kind: "worker:x", End: now.Add(-time.Hour), WriteB: 70 << 20}}
	s.saveStop(now)
	next := newIOService(context.Background(), a)
	next.load()
	seconds := next.series.Rows(time.Time{}).Seconds()
	if len(seconds) != 2 || seconds[0].WriteB != 5<<20 || seconds[0].Dirty != 3<<20 || len(next.ended) != 1 || next.ended[0].WriteB != 70<<20 {
		t.Fatalf("после перезапуска: ряд %+v, профили %+v", seconds, next.ended)
	}
	if n, _ := next.live.Now("worker:x"); n.Profile != nil {
		t.Fatal("now без шагов отдал свод")
	}
	for _, path := range []string{s.seriesPath, s.profilesPath} {
		if err := os.WriteFile(path, []byte("{битая\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	broken := newIOService(context.Background(), a)
	broken.load()
	moved, _ := filepath.Glob(filepath.Join(filepath.Dir(s.seriesPath), "*.broken-*"))
	if broken.series.Len() != 0 || len(broken.ended) != 0 || len(moved) != 2 || strings.Count(logged.String(), "файл отложен") != 2 {
		t.Fatalf("битые файлы: ряд %d, профили %d, отложено %v, журнал:\n%s", broken.series.Len(), len(broken.ended), moved, logged.String())
	}
}

// Конец cgroup ищет горутина housekeep раз в минуту: папки нет — профиль с видом из job.json в
// память и в свод now.
func TestCheckCgroupsEndsProfile(t *testing.T) {
	ctx := t.Context()
	s, _, _ := ioLiveService(t, ctx)
	s.cgroupRoot, s.workerState = t.TempDir(), t.TempDir()
	unit := "/user.slice/user-1000.slice/user@1000.service/app.slice/worker-j1.service"
	writeIOFile(t, filepath.Join(s.workerState, "j1", "job.json"), `{"class":"правка"}`)
	writeIOFile(t, filepath.Join(s.cgroupRoot, unit, "cgroup.procs"), "1\n")
	base := time.Now().Add(-time.Minute)
	for i := range 3 {
		s.rec.Detail(iowatch.Step{At: base.Add(time.Duration(i) * time.Second), Cgroups: []iowatch.Cgroup{{Path: unit, WriteBytes: 8 << 20}}})
	}
	run := func() {
		done := make(chan struct{})
		go func() { s.checkCgroups(ctx, time.Now()); close(done) }()
		for {
			select {
			case f := <-s.extra:
				f()
			case <-done:
				for len(s.extra) > 0 {
					(<-s.extra)()
				}
				return
			}
		}
	}
	run()
	if len(s.ended) != 0 || len(s.profiles.Open()) != 1 {
		t.Fatalf("живая cgroup закрыта: %+v", s.ended)
	}
	if err := os.RemoveAll(filepath.Join(s.cgroupRoot, unit)); err != nil {
		t.Fatal(err)
	}
	run()
	if len(s.ended) != 1 || s.ended[0].Kind != "worker:правка" || s.ended[0].WriteB != 24<<20 || len(s.profiles.Open()) != 0 {
		t.Fatalf("профиль: %+v", s.ended)
	}
	s.rec.Step(iowatch.Step{At: time.Now()})
	if n, ok := s.live.Now("worker:правка"); !ok || n.Profile == nil || n.Profile.Count != 1 {
		t.Fatalf("свод вида в now: %+v", n.Profile)
	}
}

func writeIOFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
