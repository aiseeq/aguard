package guiwatch

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// procIO — счётчики rchar и wchar процесса теста (байты read/write системных вызовов).
func procIO(t *testing.T) (rchar, wchar int64) {
	t.Helper()
	f, err := os.Open("/proc/self/io")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ": ")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		switch k {
		case "rchar":
			rchar = n
		case "wchar":
			wchar = n
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return rchar, wchar
}

// TestSaveIOBudgetWithLargeHistory — мера тикета self-io: обновление среза не переписывает и не перечитывает
// всю историю. История — 800 срезов по ~10 КБ в старом формате slices.json (как на машине 01.11: 871 срез, 8,4 МБ),
// перенесённая при старте (load), затем 100 обновлений одного идущего эпизода. Бюджет — 256 КБ чтения и записи на обновление (было ~17 МБ).
// После — отчёт видит всю историю и последнее обновление.
func TestSaveIOBudgetWithLargeHistory(t *testing.T) {
	root, dir, _, at := fixture(t)
	t.Setenv("PATH", filepath.Dir(os.Args[0]))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	const history = 800
	doc := Document{Version: 1, Slices: []Slice{}, Suppressed: 7}
	for i := range history {
		start := at.Add(-time.Duration(history-i) * 2 * time.Minute) // 30 в час: ниже max_per_hour, эпизод не подавлен
		samples := make([]Sample, 100)
		for j := range samples {
			samples[j] = Sample{AtMS: int64(j * 50), State: "D", RunMS: 1, WaitMS: 2, Wchan: "folio_wait_bit_common"}
		}
		doc.Slices = append(doc.Slices, Slice{ID: fmt.Sprintf("old%04d", i), Starttime: 5, Target: "old", PID: 1, TID: 1, Kind: "D",
			Start: start, End: start.Add(2 * time.Second), DurationMS: 2000, Samples: samples})
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "slices.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	cur := at
	w := New(root, dir, Default(), nil)
	w.now = func() time.Time { return cur }
	if err = w.load(); err != nil { // старт демона: история переносится до замера, замер — обновления на ней
		t.Fatal(err)
	}
	s := &targetState{Target: Target{Name: "victim", PID: 2, Start: 9}, ring: []Sample{{At: at, State: "D"}}}
	e := &episode{start: at.Add(-time.Second), end: at, kind: "D"}
	const updates = 100
	r0, w0 := procIO(t)
	for i := range updates {
		cur = at.Add(time.Duration(i) * 500 * time.Millisecond)
		e.end = cur
		if err = w.writeSlice(s, e); err != nil {
			t.Fatal(err)
		}
	}
	r1, w1 := procIO(t)
	read, written := r1-r0, w1-w0
	t.Logf("history %d КБ, %d updates: read %d КБ, written %d КБ", len(b)>>10, updates, read>>10, written>>10)
	if budget := int64(updates * 256 << 10); read+written > budget {
		t.Fatalf("IO on %d updates = %d КБ (read %d + written %d), budget %d КБ", updates, (read+written)>>10, read>>10, written>>10, budget>>10)
	}
	d, err := ReadReport(dir, at.Add(-30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Slices) != history+1 {
		t.Fatalf("report has %d slices, want %d", len(d.Slices), history+1)
	}
	var last *Slice
	for i := range d.Slices {
		if d.Slices[i].Target == "victim" {
			last = &d.Slices[i]
		}
	}
	if last == nil || !last.End.Equal(cur) {
		t.Fatalf("updated slice end lost: %#v, want end %v", last, cur)
	}
	if d.Suppressed != 7 {
		t.Fatalf("suppressed = %d, want 7", d.Suppressed)
	}
}
