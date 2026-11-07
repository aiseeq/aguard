package guiwatch

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// Образец структуры и выборок — slices/0001e6dc834a0d531b17917891e2c313.json от 04.11.2026.
func TestLoadSliceMemory(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "slices")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 11, 4, 0, 4, 11, 0, time.UTC)
	item := Slice{Target: "chrome-renderer", PID: 2817335, TID: 2817335, Starttime: 153374723, Kind: "D", Start: start, End: start.Add(time.Second), PSI: map[string]string{"cpu": "some avg10=0.00 avg60=0.00 avg300=0.00 total=123456789"}}
	for i := range 130 {
		item.Samples = append(item.Samples, Sample{AtMS: int64(i*250 - 5000), State: "S", Majflt: 365788, Wchan: "futex_do_wait"})
	}
	for i := range 4000 {
		item.ID = fmt.Sprintf("%032x", i)
		b, err := json.MarshalIndent(item, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 && (len(b) < 19000 || len(b) > 23000) {
			t.Fatalf("размер образца %d байт", len(b))
		}
		if err := os.WriteFile(filepath.Join(root, item.ID+".json"), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	w := New("", dir, Default(), nil)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if err := w.load(); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(w)
	used := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("4000 срезов: %d байт (%.2f МиБ)", used, float64(used)/(1<<20))
	if used > 10<<20 {
		t.Fatalf("срезы занимают больше 10 МиБ")
	}
}

func TestCompactSliceMergeAndUpdate(t *testing.T) {
	dir := t.TempDir()
	at := time.Now()
	w := New(t.TempDir(), dir, Default(), nil)
	w.now = func() time.Time { return at.Add(10 * time.Second) }
	t.Setenv("PATH", t.TempDir())
	defer w.closeAll()
	old := Slice{ID: "original", Target: "victim", PID: 4321, TID: 4321, Starttime: 100, Kind: "D", Start: at, End: at.Add(time.Second), Samples: []Sample{{AtMS: -1000, State: "D"}}, Threads: []Thread{{TID: 4321, State: "D"}}}
	w.document.Slices = []Slice{old}
	if err := w.save(); err != nil {
		t.Fatal(err)
	}
	w.compactSlices()
	if got := w.document.Slices[0]; !got.metadataOnly || got.Samples != nil || got.Threads != nil {
		t.Fatalf("срез не освобождён: %#v", got)
	}
	s := &targetState{Target: Target{Name: "victim", PID: 4321, Start: 100}, ring: []Sample{{At: at.Add(2 * time.Second), State: "D"}}}
	e := &episode{start: at.Add(2 * time.Second), end: at.Add(3 * time.Second), kind: "D"}
	if err := w.writeSlice(s, e); err != nil {
		t.Fatal(err)
	}
	w.compactSlices()
	s.ring = append(s.ring, Sample{At: at.Add(4 * time.Second), State: "D"})
	e.end = at.Add(4 * time.Second)
	if err := w.writeSlice(s, e); err != nil {
		t.Fatal(err)
	}
	doc, err := ReadReport(dir, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Slices) != 1 {
		t.Fatalf("срезы отчёта: %#v", doc.Slices)
	}
	got := doc.Slices[0]
	if got.Merged != 1 || len(got.Samples) != 4 || got.Samples[0].AtMS != -1000 || len(got.Threads) != 1 {
		t.Fatalf("потеряны данные при склейке или дополнении: %#v", got)
	}
}

// Файл сокращённого среза удалили мимо демона: склейка не обрывает
// наблюдатель, срез дописывается новыми выборками, потеря — в его ошибках.
func TestCompactSliceMissingFileMerges(t *testing.T) {
	dir := t.TempDir()
	at := time.Now()
	w := New(t.TempDir(), dir, Default(), nil)
	w.now = func() time.Time { return at.Add(10 * time.Second) }
	t.Setenv("PATH", t.TempDir())
	defer w.closeAll()
	old := Slice{ID: "original", Target: "victim", PID: 4321, TID: 4321, Starttime: 100, Kind: "D", Start: at, End: at.Add(time.Second), Samples: []Sample{{AtMS: -1000, State: "D"}}}
	w.document.Slices = []Slice{old}
	if err := w.save(); err != nil {
		t.Fatal(err)
	}
	w.compactSlices()
	if err := os.Remove(slicePath(dir, "original")); err != nil {
		t.Fatal(err)
	}
	s := &targetState{Target: Target{Name: "victim", PID: 4321, Start: 100}, ring: []Sample{{At: at.Add(2 * time.Second), State: "D"}}}
	e := &episode{start: at.Add(2 * time.Second), end: at.Add(3 * time.Second), kind: "D"}
	if err := w.writeSlice(s, e); err != nil {
		t.Fatalf("пропавший файл оборвал запись среза: %v", err)
	}
	doc, err := ReadReport(dir, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Slices) != 1 {
		t.Fatalf("срезы отчёта: %#v", doc.Slices)
	}
	got := doc.Slices[0]
	if got.ID != "original" || got.Merged != 1 || !slices.ContainsFunc(got.Errors, func(s string) bool { return strings.Contains(s, "файл среза пропал") }) {
		t.Fatalf("склейка после пропажи файла: %#v", got)
	}
}
