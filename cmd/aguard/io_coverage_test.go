package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/iowatch"
)

// Покрытие замера в `aguard io report` (тикет io-report-coverage): ноль случаев отличим от «замера не было».

// coverageJSON — поле coverage итога и его ключи.
func coverageJSON(t *testing.T, raw []byte) (iowatch.Coverage, []string) {
	t.Helper()
	var sum struct {
		Coverage *iowatch.Coverage `json:"coverage"`
	}
	var keys struct {
		Coverage map[string]json.RawMessage `json:"coverage"`
	}
	if err := json.Unmarshal(raw, &sum); err != nil || sum.Coverage == nil {
		t.Fatalf("нет поля coverage: %v: %s", err, raw)
	}
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(keys.Coverage))
	for k := range keys.Coverage {
		names = append(names, k)
	}
	slices.Sort(names)
	return *sum.Coverage, names
}

// Служба отвечает и файл без службы — одна форма поля и одни пробелы: до начала учёта, служба стояла, mode=off.
func TestIOReportCoverageServiceAndFile(t *testing.T) {
	s, out, socket := ioLiveService(t, t.Context())
	now := time.Now().Round(0).Truncate(time.Second)
	rec := iowatch.CoverRecord{Start: now.Add(-3 * time.Hour), Spans: []iowatch.Span{
		{From: now.Add(-3 * time.Hour), To: now.Add(-2 * time.Hour), Mode: iowatch.ModeAlert},
		{From: now.Add(-90 * time.Minute), To: now.Add(-time.Hour), Mode: iowatch.ModeOff},
	}}
	s.cover = iowatch.NewCoverer(rec, now)
	go s.serve()
	go s.wait(time.Minute)
	acceptWaitSocket(t, socket)
	since, until := now.Add(-4*time.Hour), now
	if err := s.a.ioReport(s.path, true, since, until, 0, "", true); err != nil {
		t.Fatal(err)
	}
	live, liveKeys := coverageJSON(t, out.Bytes())
	want := []iowatch.Gap{
		{From: since, To: now.Add(-3 * time.Hour), Why: iowatch.GapNoRecord},
		{From: now.Add(-2 * time.Hour), To: now.Add(-90 * time.Minute), Why: iowatch.GapNotRunning},
		{From: now.Add(-90 * time.Minute), To: now.Add(-time.Hour), Why: iowatch.GapModeOff},
		{From: now.Add(-time.Hour), To: until, Why: iowatch.GapNotRunning},
	}
	if live.Source != iowatch.CoverService || live.CoveredMS != time.Hour.Milliseconds() || live.WindowMS != (4*time.Hour).Milliseconds() || len(live.Gaps) != len(want) {
		t.Fatalf("покрытие от службы: %+v", live)
	}
	for i := range want {
		if g := live.Gaps[i]; !g.From.Equal(want[i].From) || !g.To.Equal(want[i].To) || g.Why != want[i].Why {
			t.Fatalf("пробел %d: %+v, ждали %+v", i, g, want[i])
		}
	}

	f, fout, _ := ioLiveService(t, t.Context()) // служба не запущена: сокета нет
	if err := iowatch.SaveCoverage(filepath.Join(filepath.Dir(f.path), ioCoverageFile), rec); err != nil {
		t.Fatal(err)
	}
	if err := f.a.ioReport(f.path, true, since, until, 0, "", true); err != nil {
		t.Fatal(err)
	}
	file, fileKeys := coverageJSON(t, fout.Bytes())
	if file.Source != iowatch.CoverFile || !reflect.DeepEqual(liveKeys, fileKeys) || file.CoveredMS != live.CoveredMS || !reflect.DeepEqual(file.Gaps, live.Gaps) {
		t.Fatalf("файл и служба разной формы:\nслужба %v %+v\nфайл   %v %+v", liveKeys, live, fileKeys, file)
	}
}

// Файл учёта битый, службы нет: отчёт — явная ошибка, не пустое покрытие; replay маршрута учёт не читает и идёт.
func TestIOReportCoverageBrokenFile(t *testing.T) {
	s, _, _ := ioLiveService(t, t.Context())
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(s.path), ioCoverageFile), []byte("{битая"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	err := s.a.ioReport(s.path, true, now.Add(-time.Hour), now, 0, "", true)
	if err == nil || !strings.Contains(err.Error(), "учёт покрытия") {
		t.Fatalf("битый файл учёта: %v", err)
	}
	if err := s.a.ioRouteReplay(s.path, true, now.Add(-time.Hour), false); err != nil {
		t.Fatalf("io route на битом учёте: %v", err)
	}
}

// Окно глубже cases_keep (14 сут. по умолчанию): до границы хранения — before_keep, дальше — по учёту.
func TestIOReportCoverageBeforeKeep(t *testing.T) {
	s, out, _ := ioLiveService(t, t.Context())
	now := time.Now()
	if err := s.a.ioReport(s.path, false, now.Add(-20*24*time.Hour), now, 0, "", true); err != nil {
		t.Fatal(err)
	}
	cov, _ := coverageJSON(t, out.Bytes())
	border := now.Add(-iowatch.Default().CasesKeep.D())
	if len(cov.Gaps) != 2 || cov.Gaps[0].Why != iowatch.GapBeforeKeep || cov.Gaps[0].To.Sub(border).Abs() > time.Minute || cov.Gaps[1].Why != iowatch.GapNoRecord {
		t.Fatalf("глубже cases_keep: %+v", cov)
	}
}

// Текст отчёта — одна строка покрытия с пробелами.
func TestIOReportCoverageText(t *testing.T) {
	s, out, _ := ioLiveService(t, t.Context())
	now := time.Now().Round(0)
	if err := s.a.ioReport(s.path, false, now.Add(-time.Hour), now, 0, "", false); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out.String(), "\n")
	if len(lines) < 2 || !strings.HasPrefix(lines[1], "покрытие замера (файл): 0 с из 1 ч (0%)") || !strings.Contains(lines[1], "до начала учёта") {
		t.Fatalf("строка покрытия:\n%s", out.String())
	}
}

// Шаги службы продлевают интервал её режима; цикл без диска (ErrNoDevice) — не замер: интервал закрыт, хвост
// не покрыт.
func TestIOServiceCoverageStepsAndNoDevice(t *testing.T) {
	a, _ := testIOApp(t)
	s := newIOService(t.Context(), a)
	s.load()
	s.cfg = iowatch.Default()
	s.cfg.Mode = iowatch.ModeLog
	s.rec = iowatch.NewRecorder(s.cfg)
	base := time.Now().Round(0)
	for i := range 30 {
		s.step(iowatch.Step{At: base.Add(time.Duration(i) * 100 * time.Millisecond)})
	}
	last := base.Add(2900 * time.Millisecond)
	rec := s.cover.Record(last)
	if n := len(rec.Spans); n != 1 || rec.Spans[0].Mode != iowatch.ModeLog || !rec.Spans[0].From.Equal(base) || !rec.Spans[0].To.Equal(last) {
		t.Fatalf("интервал по шагам: %+v", rec)
	}
	cfg := iowatch.Default()
	cfg.Device = "нет-такого-диска"
	s.rec = nil
	if err := s.run(cfg); err == nil {
		t.Fatal("цикл без диска не вернул ошибку")
	}
	if r := s.cover.Record(last.Add(time.Second)); !r.Spans[0].To.Equal(last) {
		t.Fatalf("цикл без диска продлил покрытие: %+v", r.Spans)
	}
}

// mode=off: служба отвечает, а учёт пишет off — в ответе cases пробел mode_off; при остановке учёт в файле.
func TestIOLoopModeOffCoverage(t *testing.T) {
	a, _ := testIOApp(t)
	socket := filepath.Join(t.TempDir(), "io-state.sock")
	if err := os.WriteFile(a.cfgPath, fmt.Appendf(nil, "[io]\nmode = \"off\"\nsocket = %q\n", socket), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(done)
		a.ioLoop(ctx, nil)
	}()
	acceptWaitSocket(t, socket)
	ans, err := askIO(socket, iowatch.Request{Q: iowatch.QueryCases})
	if err != nil {
		t.Fatal(err)
	}
	if ans.Coverage == nil {
		t.Fatal("ответ cases без учёта покрытия")
	}
	answered := time.Now()
	cov := iowatch.Cover(*ans.Coverage, iowatch.CoverService, start, answered, start.Add(-time.Hour))
	off := slices.IndexFunc(cov.Gaps, func(g iowatch.Gap) bool { return g.Why == iowatch.GapModeOff })
	// хвост после ответа (миллисекунды до answered) — not_running: ответ продлил off до своего момента
	if cov.CoveredMS != 0 || off < 0 || off < len(cov.Gaps)-2 {
		t.Fatalf("mode=off в ответе службы: %+v", cov)
	}
	cancel()
	<-done
	rec, err := iowatch.LoadCoverage(filepath.Join(a.dir("io"), ioCoverageFile))
	if err != nil || len(rec.Spans) != 1 || rec.Spans[0].Mode != iowatch.ModeOff {
		t.Fatalf("учёт при остановке: %+v %v", rec, err)
	}
}

// Остановка службы пишет учёт в сроке cases_keep: интервалы глубже срезаны, начало учёта — на границе.
func TestIOServiceCoverageSaveStopKeep(t *testing.T) {
	a, _ := testIOApp(t)
	s := newIOService(t.Context(), a)
	now := time.Now().Round(0)
	s.cover = iowatch.NewCoverer(iowatch.CoverRecord{Start: now.Add(-20 * 24 * time.Hour), Spans: []iowatch.Span{
		{From: now.Add(-20 * 24 * time.Hour), To: now.Add(-19 * 24 * time.Hour), Mode: iowatch.ModeAlert},
		{From: now.Add(-time.Hour), To: now.Add(-30 * time.Minute), Mode: iowatch.ModeAlert},
	}}, now)
	s.saveStop(now)
	rec, err := iowatch.LoadCoverage(s.coverPath)
	border := now.Add(-iowatch.Default().CasesKeep.D())
	if err != nil || !rec.Start.Equal(border) || len(rec.Spans) != 1 || !rec.Spans[0].From.Equal(now.Add(-time.Hour)) {
		t.Fatalf("учёт при остановке: %+v %v", rec, err)
	}
}

// mode=off: первый проход пишет копию учёта сразу (housekeep в off не идёт), дальше — раз в ioSaveEvery.
func TestIOServiceOffPassSaves(t *testing.T) {
	a, _ := testIOApp(t)
	s := newIOService(t.Context(), a)
	now := time.Now()
	s.offPass(now)
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if rec, err := iowatch.LoadCoverage(s.coverPath); err == nil && len(rec.Spans) == 1 && rec.Spans[0].Mode == iowatch.ModeOff {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("проход off не записал учёт")
		}
	}
	if !s.offSaveAt.Equal(now.Add(ioSaveEvery)) {
		t.Fatalf("следующая копия учёта в off: %v", s.offSaveAt)
	}
}

// Старт службы: нет файла учёта — засев из посекундного ряда; битый файл — отложен, учёт с нуля (до старта —
// no_record), а не пустое покрытие; копия раз в час пишет учёт.
func TestIOServiceCoverageLoadSeedBrokenSave(t *testing.T) {
	a, _ := testIOApp(t)
	var logged bytes.Buffer
	a.log = slog.New(slog.NewTextHandler(&logged, nil))
	s := newIOService(context.Background(), a)
	first := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	var seconds []iowatch.MachineSecond
	for i := range 10 {
		seconds = append(seconds, iowatch.MachineSecond{At: first.Add(time.Duration(i) * time.Second)})
	}
	if err := iowatch.SaveSeries(s.seriesPath, seconds); err != nil {
		t.Fatal(err)
	}
	s.load()
	rec := s.cover.Record(time.Now())
	if !rec.Start.Equal(first) || len(rec.Spans) != 1 || rec.Spans[0].Mode != iowatch.SpanSeries || !rec.Spans[0].To.Equal(first.Add(10*time.Second)) {
		t.Fatalf("засев из ряда: %+v", rec)
	}

	s.cfg = iowatch.Default()
	s.save()
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if saved, err := iowatch.LoadCoverage(s.coverPath); err == nil && saved.Start.Equal(first) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("часовая копия не записала учёт")
		}
	}

	if err := os.WriteFile(s.coverPath, []byte("{битая"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	broken := newIOService(context.Background(), a)
	broken.load()
	moved, _ := filepath.Glob(s.coverPath + ".broken-*")
	r := broken.cover.Record(time.Now())
	if len(moved) != 1 || r.Start.Before(before) || len(r.Spans) != 0 || !strings.Contains(logged.String(), "файл отложен") {
		t.Fatalf("битый учёт: отложено %v, учёт %+v, журнал:\n%s", moved, r, logged.String())
	}
}
