package record

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/aiseeq/aguard/internal/testutil"
)

func TestStreamStateMatchesJSON(t *testing.T) {
	inputs := []string{
		``,
		`null`, `{}`, `{"updated":"2026-11-01T00:00:00Z","roots":["/tmp"],"extra":{"nested":[1,2]},"entries":[{"path":"x","born":"2026-11-01T00:00:00Z"}]}`,
		`{"started":"2026-11-01T01:00:00Z"}`,
		`{"last_full":"2026-11-01T02:00:00Z"}`,
		`{"containers":["/tmp/container"]}`,
		`{"entries":[{}],"entries":null}`,
		`{"ENTRIES":[{"path":"a","uid":4}],"entries":[{"path":"b"}]}`,
		`{"entries":null,"roots":null}`, `{"entries":[]}`, `{"entries":[null]}`,
		`{"entries":[{"uid":"bad"}]}`, `{"entries":{}}`, `{"updated":3}`, `{} {}`, `{"entries":[`,
	}
	for _, input := range inputs {
		var want State
		wantErr := json.Unmarshal([]byte(input), &want)
		got, err := decodeState(strings.NewReader(input))
		if (err != nil) != (wantErr != nil) || err == nil && !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %+v, %v; want %+v, %v", input, got, err, want, wantErr)
		}
	}
}

func TestReadStateAllocationBudget(t *testing.T) {
	testutil.SkipBudgetUnderRace(t)
	var b strings.Builder
	b.WriteString(`{"entries":[`)
	entry := `{"path":"/tmp/x","born":"2026-11-01T00:00:00Z","owner":{"proc":{"args":"` + strings.Repeat("x", 32<<10) + `"}}}`
	for i := range 256 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(entry)
	}
	b.WriteString(`]}`)
	path := filepath.Join(t.TempDir(), StateFile)
	if err := os.WriteFile(path, []byte(b.String()), 0600); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	s, err := ReadState(path)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Entries) != 256 {
		t.Fatalf("entries=%d", len(s.Entries))
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	limit := uint64(b.Len()) * 17 / 10
	t.Logf("allocated=%d input=%d limit=%d", allocated, b.Len(), limit)
	if allocated > limit {
		t.Fatalf("слишком много выделений: %d > %d", allocated, limit)
	}
}
