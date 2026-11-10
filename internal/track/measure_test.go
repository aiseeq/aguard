package track

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/record"
)

// measureFixture — большая запись (папка big/d с 3·yieldEvery файлами и 10 файлов big/z* после неё: в обходе 779
// записей каталога), три мелкие и их папка как контейнер для сверки.
func measureFixture(t *testing.T) measureJob {
	t.Helper()
	root := t.TempDir()
	big := filepath.Join(root, "big")
	if err := os.MkdirAll(filepath.Join(big, "d"), 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range 3 * yieldEvery {
		if err := os.WriteFile(filepath.Join(big, "d", fmt.Sprintf("f%d", i)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 10 {
		if err := os.WriteFile(filepath.Join(big, fmt.Sprintf("z%d", i)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	job := measureJob{start: time.Now(), full: true, items: []usage{{path: big}}, dirs: []string{root}}
	for _, n := range []string{"a", "b", "c"} {
		p := filepath.Join(root, n)
		if err := os.WriteFile(p, []byte(n), 0o600); err != nil {
			t.Fatal(err)
		}
		job.items = append(job.items, usage{path: p})
	}
	return job
}

// Уступка — перед каждой записью и папкой-контейнером и каждые yieldEvery записей каталога внутри обхода большой
// записи: 4 записи + 1 папка + 3 внутри обхода.
func TestMeasureYieldsBetweenEntriesAndInsideWalk(t *testing.T) {
	job := measureFixture(t)
	calls := 0
	d := runMeasure(t.Context(), job, func(context.Context) bool { calls++; return false })
	if want := len(job.items) + len(job.dirs) + 3; calls != want {
		t.Fatalf("уступка спрошена %d раз, ждал %d", calls, want)
	}
	if d.aborted || d.pauses != 0 || len(d.usages) != len(job.items) || d.usages[0].files != 3*yieldEvery+10 {
		t.Fatalf("замер без пауз: aborted %v, пауз %d, записей %d, файлов большой %d", d.aborted, d.pauses,
			len(d.usages), d.usages[0].files)
	}
}

// Ничего не потеряли: замер с паузами даёт те же размеры и листинг, что без уступки; паузы и их время — в итоге.
func TestMeasurePausedSameResult(t *testing.T) {
	job := measureFixture(t)
	plain := runMeasure(t.Context(), job, nil)
	paused := runMeasure(t.Context(), job, func(context.Context) bool { time.Sleep(time.Millisecond); return true })
	if !reflect.DeepEqual(plain.usages, paused.usages) || !reflect.DeepEqual(plain.listing, paused.listing) {
		t.Fatalf("замер с паузами другой:\n%+v\n%+v", plain.usages, paused.usages)
	}
	if paused.aborted || paused.pauses != 8 || paused.paused < 8*time.Millisecond || plain.pauses != 0 {
		t.Fatalf("паузы: %d за %v (без уступки %d), aborted %v", paused.pauses, paused.paused, plain.pauses, paused.aborted)
	}
}

// Отмена в паузе (остановка демона): замер выходит сразу, обход дальше не идёт (ни во вложенной папке, ни в
// записи-родителе), итог помечен прерванным.
func TestMeasureCancelInPause(t *testing.T) {
	job := measureFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	inPause := make(chan struct{})
	calls := 0
	yield := func(ctx context.Context) bool {
		if calls++; calls == 2 { // первая уступка внутри обхода большой записи
			close(inPause)
			<-ctx.Done()
			return true
		}
		return false
	}
	got := make(chan measureDone, 1)
	go func() { got <- runMeasure(ctx, job, yield) }()
	<-inPause
	cancel()
	select {
	case d := <-got:
		if !d.aborted || calls != 2 || len(d.usages) != 1 || d.usages[0].files >= yieldEvery {
			t.Fatalf("после отмены: aborted %v, уступок %d, записей %d", d.aborted, calls, len(d.usages))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("замер висит в паузе после отмены")
	}
}

// Прерванный замер не трогает записи и снимок; полный — строкой INFO: первый (старт) всегда, следующие — если были
// паузы; паузы молодых — в часовую сводку.
func TestFinishMeasureLogsAndSkipsAborted(t *testing.T) {
	dir := t.TempDir()
	var logged bytes.Buffer
	tr := &Tracker{
		cfg:     Config{Name: "track", RunDir: dir, StateDir: dir, Persist: time.Hour},
		log:     slog.New(slog.NewTextHandler(&logged, nil)),
		entries: map[string]*record.Entry{"/tmp/e": {Path: "/tmp/e", Born: time.Now().Add(-time.Hour)}},
	}
	tr.measuring = true
	tr.finishMeasure(measureDone{aborted: true, job: measureJob{full: true}, usages: []usage{{path: "/tmp/e", ok: true, bytes: 5}}})
	if tr.measuring || tr.entries["/tmp/e"].Bytes != 0 || !tr.lastFull.IsZero() {
		t.Fatalf("прерванный замер: measuring %v, байт %d, lastFull %v", tr.measuring, tr.entries["/tmp/e"].Bytes, tr.lastFull)
	}
	if _, err := os.Stat(filepath.Join(dir, record.StateFile)); err == nil {
		t.Fatal("прерванный замер записал снимок")
	}
	full := func(pauses int) measureDone {
		return measureDone{job: measureJob{start: time.Now().Add(-3 * time.Second), full: true, items: []usage{{path: "/tmp/e"}}},
			usages: []usage{{path: "/tmp/e", ok: true, bytes: 5, files: 1}}, listing: map[string][]string{},
			pauses: pauses, paused: time.Duration(pauses) * 2 * time.Second}
	}
	tr.finishMeasure(full(0))
	if s := logged.String(); !strings.Contains(s, "полный замер") || !strings.Contains(s, "pauses=0") ||
		!strings.Contains(s, "entries=1") || !strings.Contains(s, "name=track") {
		t.Fatalf("замер на старте не в журнале:\n%s", s)
	}
	logged.Reset()
	tr.finishMeasure(full(0))
	if logged.Len() != 0 {
		t.Fatalf("полный замер без пауз не на старте — в журнале:\n%s", logged.String())
	}
	tr.finishMeasure(full(2))
	if s := logged.String(); !strings.Contains(s, "pauses=2") || !strings.Contains(s, "paused=4s") {
		t.Fatalf("полный замер с паузами не в журнале:\n%s", s)
	}
	logged.Reset()
	young := full(1)
	young.job.full = false
	tr.finishMeasure(young)
	if logged.Len() != 0 || tr.st.measurePauses != 3 || tr.st.measurePaused != 6*time.Second {
		t.Fatalf("молодой замер: журнал %q, пауз в сводке %d за %v", logged.String(), tr.st.measurePauses, tr.st.measurePaused)
	}
}

// Замер сторожа берёт уступку из Config.Yield.
func TestStartMeasureUsesConfigYield(t *testing.T) {
	job := measureFixture(t)
	calls := 0
	tr := &Tracker{cfg: Config{Yield: func(context.Context) bool { calls++; return false }}, entries: map[string]*record.Entry{}}
	for _, it := range job.items {
		tr.entries[it.path] = &record.Entry{Path: it.path}
	}
	done := make(chan measureDone, 1)
	tr.startMeasure(t.Context(), done, false)
	d := <-done
	if calls != len(job.items)+3 || len(d.usages) != len(job.items) {
		t.Fatalf("уступок %d, записей %d", calls, len(d.usages))
	}
}
