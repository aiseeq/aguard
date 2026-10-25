package daily

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

type rec struct {
	At time.Time `json:"at"`
	N  int       `json:"n"`
}

func TestAppendByDay(t *testing.T) {
	st := Store[rec]{Dir: t.TempDir(), What: "записи", At: func(r *rec) time.Time { return r.At }}
	day1 := time.Date(2026, 10, 24, 23, 0, 0, 0, time.Local)
	day2 := day1.Add(2 * time.Hour)
	// задним числом: одна пачка с записями двух дней
	if err := st.Append([]rec{{At: day2, N: 2}, {At: day1, N: 1}}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"2026-10-24.jsonl", "2026-10-25.jsonl"} {
		if _, err := os.Stat(filepath.Join(st.Dir, name)); err != nil {
			t.Errorf("нет файла дня: %v", err)
		}
	}
	if err := st.Rotate(day2, 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	var got []int
	bad, err := st.Read(day1, func(r *rec) { got = append(got, r.N) })
	if err != nil || bad != 0 || !slices.Equal(got, []int{1, 2}) {
		t.Errorf("чтение: %v, битых %d, %v", got, bad, err)
	}
}

func TestDayOf(t *testing.T) {
	for name, want := range map[string]bool{
		"2026-10-25.jsonl": true, "2026-10-25.jsonl.gz": true,
		"2026-1-25.jsonl": false, "2026x10-25.jsonl": false, "state.json": false,
	} {
		if _, _, ok := dayOf(name); ok != want {
			t.Errorf("dayOf(%q) = %v", name, ok)
		}
	}
}

// TestAppendToCompressedDay — запись задним числом в уже сжатый день не теряет прежние записи.
func TestAppendToCompressedDay(t *testing.T) {
	st := Store[rec]{Dir: t.TempDir(), What: "записи", At: func(r *rec) time.Time { return r.At }}
	day1 := time.Date(2026, 10, 24, 12, 0, 0, 0, time.Local)
	now := day1.Add(30 * time.Hour)
	for _, n := range []int{1, 2} {
		if err := st.Append([]rec{{At: day1, N: n}}); err != nil {
			t.Fatal(err)
		}
		if err := st.Rotate(now, 30*24*time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	var got []int
	if _, err := st.Read(day1.Add(-time.Hour), func(r *rec) { got = append(got, r.N) }); err != nil || !slices.Equal(got, []int{1, 2}) {
		t.Errorf("после двух сжатий: %v, %v", got, err)
	}
}
