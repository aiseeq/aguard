package tail

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type ev struct {
	src  Source
	text string
}

func appendLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestLines — номера строк событий: у файла, бывшего до первого прохода, — подсчётом при первом событии.
func TestLines(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "-home-u", "s1.jsonl")
	appendLines(t, old, `{"type":"agent-name"}`, "a", "b", "c")
	tl := &Tail[ev]{Projects: dir, Lines: true,
		Parse: func(line []byte) (ev, bool, string, error) {
			if bytes.HasPrefix(line, []byte("X")) {
				return ev{text: string(line)}, true, "", nil
			}
			if bytes.Contains(line, []byte("agent-name")) {
				return ev{}, false, "lead", nil
			}
			return ev{}, false, "", nil
		},
		Stamp: func(e *ev, src Source) { e.src = src }}
	st := &State{Files: map[string]*File{}}
	if evs, err := tl.Pass(st, time.Now()); err != nil || len(evs) != 0 {
		t.Fatalf("первый проход: %v %v", evs, err)
	}
	appendLines(t, old, "d", "X1", "X2")
	appendLines(t, filepath.Join(dir, "-home-u", "s2.jsonl"), "X3")
	evs, err := tl.Pass(st, time.Now())
	if err != nil || len(evs) != 3 {
		t.Fatalf("второй проход: %v %v", evs, err)
	}
	if evs[0].src.Line != 6 || evs[1].src.Line != 7 || evs[2].src.Line != 1 || evs[2].src.Session != "s2" {
		t.Errorf("строки: %+v", evs)
	}
	appendLines(t, old, "X4")
	if evs, _ := tl.Pass(st, time.Now()); len(evs) != 1 || evs[0].src.Line != 8 || !st.Files["-home-u/s1.jsonl"].Counted {
		t.Errorf("третий проход: %+v", evs)
	}
}
