package statefile

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestTornJournalTailIsDiscardedAndRepaired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"values":{"a":1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Save(path, []byte(`{"values":{"a":2}}`)); err != nil {
		t.Fatal(err)
	}
	jpath := path + ".journal"
	f, err := os.OpenFile(jpath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"set":{"broken":`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil || !strings.Contains(string(got), `"a":2`) {
		t.Fatalf("Load = %s, %v", got, err)
	}
	if err := Save(path, []byte(`{"values":{"a":3}}`)); err != nil {
		t.Fatal(err)
	}
	got, err = Load(path)
	if err != nil || !strings.Contains(string(got), `"a":3`) {
		t.Fatalf("after repair Load = %s, %v", got, err)
	}
}

func TestCorruptCompleteJournalRecordIsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"value":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".journal", []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted a corrupt complete journal record")
	}
}

func TestSnapshotBeforeJournalTruncateIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"values":{"a":1,"b":1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Save(path, []byte(`{"values":{"a":2,"b":1}}`)); err != nil {
		t.Fatal(err)
	}
	// Model a crash after atomic snapshot replacement but before journal truncation.
	if err := os.WriteFile(path, []byte(`{"values":{"a":2,"b":1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil || string(got) != `{"values":{"a":2,"b":1}}` {
		t.Fatalf("replay over compacted snapshot = %s, %v", got, err)
	}
	if err := Save(path, []byte(`{"values":{"a":2,"b":3}}`)); err != nil {
		t.Fatal(err)
	}
	got, err = Load(path)
	if err != nil || string(got) != `{"values":{"a":2,"b":3}}` {
		t.Fatalf("save after interrupted compaction = %s, %v", got, err)
	}
}

func TestDeletedKeysStayDeleted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"queue":[1],"pause":"later","last":{"s":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Save(path, []byte(`{"queue":[],"last":{}}`)); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), `"pause"`) || strings.Contains(string(got), `"s"`) || !strings.Contains(string(got), `"queue":[]`) {
		t.Fatalf("deleted state resurrected: %s", got)
	}
}

func TestConcurrentSavesKeepJournalReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"writer":0,"count":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errCh := make(chan error, 4)
	for writer := range 4 {
		wg.Add(1)
		go func(writer int) {
			defer wg.Done()
			for n := range 25 {
				state := []byte(`{"writer":` + strconv.Itoa(writer) + `,"count":` + strconv.Itoa(n) + `}`)
				if err := Save(path, state); err != nil {
					errCh <- err
					return
				}
			}
		}(writer)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("concurrent journal is unreadable: %v", err)
	}
}
