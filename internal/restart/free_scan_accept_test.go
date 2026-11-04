package restart

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Приёмка queue-order-guard: ожидание на переднем плане видно из настоящего транскрипта и переживает дочитывание файла.
func TestPendingWaitsFromTranscript(t *testing.T) {
	since, err := time.Parse(time.RFC3339, "2026-10-24T09:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, input string
		want        int
	}{
		{"worker wait", `{"command":"worker wait 1103-1913-x --timeout 9m"}`, 1},
		{"wfor", `{"command":"wfor line DONE out.log --timeout 30m"}`, 1},
		{"сборка", `{"command":"go build ./..."}`, 0},
	}
	for _, c := range cases {
		f := scanLines(t, since, since, use(tsNew, "t1", "Bash", c.input))
		if len(f.Pending) != 1 || f.PendingWaits != c.want {
			t.Errorf("%s: %+v", c.name, f.Pending)
		}
	}
	file := filepath.Join(t.TempDir(), "s.jsonl")
	write(t, file, use(tsNew, "t1", "Bash", `{"command":"worker wait x"}`)+"\n")
	stat := func() os.FileInfo {
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		return info
	}
	cache := &scanCache{}
	if _, err := cache.read(file, stat(), since, since); err != nil {
		t.Fatal(err)
	}
	h, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.WriteString(notice(tsNew, "user", "x") + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := cache.read(file, stat(), since, since)
	if err != nil {
		t.Fatal(err)
	}
	if f.PendingWaits != 1 || len(f.Pending) != 1 {
		t.Fatalf("после дочитывания: %+v, ожиданий %d", f.Pending, f.PendingWaits)
	}
}
