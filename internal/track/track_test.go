package track

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
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

func TestApplyUsageIntegral(t *testing.T) {
	born := time.Unix(1000, 0)
	e := &record.Entry{Born: born, Owner: &record.Owner{}}
	applyUsage(e, 100, 1, born.Add(10*time.Second)) // рост от нуля: 100/2*10
	if e.ByteSec != 500 {
		t.Fatalf("первый замер: %v", e.ByteSec)
	}
	applyUsage(e, 300, 2, born.Add(20*time.Second)) // трапеция (100+300)/2*10
	if e.ByteSec != 2500 || e.MaxBytes != 300 || e.Bytes != 300 {
		t.Fatalf("второй замер: %+v", e)
	}
	applyUsage(e, 0, 0, born.Add(30*time.Second))
	if e.MaxBytes != 300 || e.MaxFiles != 2 || e.ByteSec != 4000 {
		t.Fatalf("после удаления содержимого: %+v", e)
	}
	old := &record.Entry{Born: born} // старше сторожа: размер считаем постоянным
	applyUsage(old, 100, 1, born.Add(10*time.Second))
	if old.ByteSec != 1000 {
		t.Fatalf("запись без создателя: %v", old.ByteSec)
	}
}

func TestParseDirName(t *testing.T) {
	h := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	body := make([]byte, 8+8)
	binary.LittleEndian.PutUint32(body[8:], uint32(len(h)))
	binary.LittleEndian.PutUint32(body[12:], 0x81)
	body = append(append(body, h...), []byte("claude-tmp\x00\x00\x00")...)
	dn := parseDirName(body)
	if dn == nil || dn.name != "claude-tmp" || dn.key != "129:0102030405060708" {
		t.Fatalf("parseDirName: %+v", dn)
	}
	if parseDirName(body[:10]) != nil {
		t.Error("короткая запись разобрана")
	}
}

// Удаление должно доходить до журнала и снимка, даже когда замерять нечего
// (все записи старые и уже замерены) — иначе отчёт показывал бы удалённое.
func TestDeleteReachesLogAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	l, err := openEventLog(filepath.Join(dir, record.OwnersFile), 0, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer l.close()
	now := time.Now()
	born := now.Add(-48 * time.Hour)
	tr := &Tracker{
		cfg:     Config{RunDir: dir, StateDir: dir, Young: 15 * time.Minute, Persist: time.Hour},
		owners:  l,
		log:     slog.New(slog.DiscardHandler),
		entries: map[string]*record.Entry{},
		watched: map[string]string{},
	}
	for _, p := range []string{"/tmp/old-a", "/tmp/old-b"} {
		tr.entries[p] = &record.Entry{Path: p, Born: born, Bytes: 100 << 20, MaxBytes: 100 << 20, MeasuredAt: now}
	}
	tr.save(false)
	tr.remove("/tmp/old-a", record.EvDelete, "", now)
	if !tr.dirty {
		t.Fatal("удаление не пометило таблицу изменённой")
	}
	tr.startMeasure(make(chan measureDone, 1), false) // молодых нет: замера не будет
	if tr.measuring {
		t.Fatal("замер запущен без записей")
	}
	st, err := record.ReadState(filepath.Join(dir, record.StateFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Entries) != 1 || st.Entries[0].Path != "/tmp/old-b" {
		t.Fatalf("в снимке осталось удалённое: %+v", st.Entries)
	}
	f, err := os.Open(filepath.Join(dir, record.OwnersFile))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var got []record.Event
	if _, err := record.ParseEvents(f, func(e record.Event) { got = append(got, e) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Ev != record.EvDelete || got[0].LifeS < 47*3600 || got[0].MaxBytes != 100<<20 || !got[0].Measured {
		t.Fatalf("строка удаления: %+v", got)
	}
}

// Потоковый снимок читается в то же, что json.Marshal(record.State).
func TestWriteStateMatchesMarshal(t *testing.T) {
	now := time.Date(2026, 11, 2, 2, 40, 0, 0, time.UTC)
	own := &record.Owner{Proc: record.Proc{Pid: 7, Comm: "node", Args: `node <x> & "y"`, UID: 1000, State: record.ProcOK},
		Chain: []record.Link{{Pid: 6, Comm: "claude"}}, Session: &record.Session{Pid: 6, Via: "ppid"}}
	for _, es := range [][]*record.Entry{
		nil,
		{{Path: "/tmp/a", Ino: 1, UID: 0, Born: now}, {Path: "/tmp/b", Dir: true, Ino: 2, UID: 1000, Born: now, Owner: own, Bytes: 4096, MeasuredAt: now}},
	} {
		s := record.State{Updated: now, Started: now.Add(-time.Hour), Roots: []string{"/tmp"}}
		var buf bytes.Buffer
		if err := writeState(&buf, &s, es); err != nil {
			t.Fatal(err)
		}
		want := s
		want.Entries = make([]record.Entry, 0, len(es))
		for _, e := range es {
			want.Entries = append(want.Entries, *e)
		}
		wb, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		var got, exp any
		if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
			t.Fatalf("снимок не JSON: %v\n%s", err, buf.String())
		}
		if err := json.Unmarshal(wb, &exp); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, exp) {
			t.Fatalf("снимок расходится с json.Marshal:\n%s\n%s", buf.String(), wb)
		}
	}
}

// Сохранение снимка не держит его целиком в памяти: на 3 000 записей —
// не больше ~50 выделений на запись (было: копия всех записей и буфер во весь JSON).
func BenchmarkSave(b *testing.B) {
	dir := b.TempDir()
	tr := &Tracker{cfg: Config{RunDir: dir, StateDir: dir}, entries: map[string]*record.Entry{}, log: slog.New(slog.DiscardHandler)}
	own := &record.Owner{Proc: record.Proc{Pid: 7, Comm: "chrome", Args: strings.Repeat("--flag=value ", 40), UID: 1000, State: record.ProcOK},
		Chain: []record.Link{{Pid: 6, Comm: "node"}, {Pid: 5, Comm: "claude"}}}
	for i := range 3000 {
		p := fmt.Sprintf("/tmp/playwright-artifacts-%06d", i)
		tr.entries[p] = &record.Entry{Path: p, Dir: true, Ino: uint64(i), UID: 1000, Born: time.Now(), Owner: own}
	}
	b.ReportAllocs()
	for b.Loop() {
		tr.save(false)
	}
}
