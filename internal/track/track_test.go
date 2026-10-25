package track

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tmpguard/internal/record"
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
	l, err := openEventLog(filepath.Join(dir, record.OwnersFile), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer l.close()
	now := time.Now()
	born := now.Add(-48 * time.Hour)
	tr := &Tracker{
		cfg:     Config{RunDir: dir, StateDir: dir, Young: 15 * time.Minute, Persist: time.Hour},
		log:     l,
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
