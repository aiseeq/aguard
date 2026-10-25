package memwatch

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/session"
)

func TestParseKeyed(t *testing.T) {
	m, err := parseKeyed(strings.NewReader("anon 100\nfile 200\n"))
	if err != nil || m["anon"] != 100 || m["file"] != 200 {
		t.Fatalf("parseKeyed: %v %v", m, err)
	}
	if v, err := parseInt("max\n"); err != nil || v != -1 {
		t.Errorf("max: %d %v", v, err)
	}
}

// fakeSlice — слайс с одним scope сессии и /proc с одним процессом; memory.peak
// здесь обычный файл: take читает его и пишет «reset», как в ядре.
func fakeSlice(t *testing.T) (slice, proc, reg string) {
	t.Helper()
	root := t.TempDir()
	slice, proc, reg = filepath.Join(root, "agents.slice"), filepath.Join(root, "proc"), filepath.Join(root, "sessions")
	sc := filepath.Join(slice, "agent-lead-41.scope")
	files := map[string]string{
		filepath.Join(sc, "memory.peak"):         "4096\n",
		filepath.Join(sc, "memory.current"):      "2048\n",
		filepath.Join(sc, "memory.swap.current"): "0\n",
		filepath.Join(sc, "memory.high"):         "max\n",
		filepath.Join(sc, "memory.stat"):         "anon 1024\nfile 1024\n",
		filepath.Join(sc, "memory.events"):       "low 0\nhigh 3\nmax 0\nsock_throttled 9\n",
		filepath.Join(sc, "cgroup.procs"):        "42\n",
		filepath.Join(proc, "42", "statm"):       "100 5 0 0 0 0 0\n",
		filepath.Join(proc, "42", "stat"):        "42 (claude) S 41 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 777 0 0\n",
		filepath.Join(proc, "42", "cmdline"):     "claude\x00-n\x00lead\x00",
		filepath.Join(reg, "42.json"):            `{"pid":42,"sessionId":"s-1","cwd":"/home/u","name":"lead"}`,
	}
	for p, body := range files {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(slice, "not-a-session"), 0o700); err != nil {
		t.Fatal(err)
	}
	return slice, proc, reg
}

func TestPass(t *testing.T) {
	slice, proc, reg := fakeSlice(t)
	w := NewWatcher(session.Registry{Dir: reg, ProcRoot: proc})
	w.ProcRoot = proc
	defer w.Close()
	cfg := Default()
	cfg.Slice = slice
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.Local)
	got, err := w.Pass(&cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("записей %d, хотим 1", len(got))
	}
	s := got[0]
	if s.Name != "lead" || s.Pid != 42 || s.Session != "s-1" || s.Peak != 4096 || s.Current != 2048 || s.High != -1 {
		t.Errorf("запись: %+v", s)
	}
	if s.Events["high"] != 3 || s.Events["sock_throttled"] != 0 {
		t.Errorf("события: %v", s.Events)
	}
	if len(s.Top) != 1 || s.Top[0].Comm != "claude" || s.Top[0].RSS != 5*int64(os.Getpagesize()) || s.Top[0].Args != "claude -n lead" {
		t.Errorf("процессы: %+v", s.Top)
	}
	b, err := os.ReadFile(filepath.Join(slice, "agent-lead-41.scope", "memory.peak"))
	if err != nil || !bytes.HasPrefix(b, []byte("reset")) {
		t.Errorf("пик не сброшен: %q %v", b, err)
	}
	if err := os.RemoveAll(filepath.Join(slice, "agent-lead-41.scope")); err != nil {
		t.Fatal(err)
	}
	if got, err := w.Pass(&cfg, now); err != nil || len(got) != 0 || len(w.peaks) != 0 {
		t.Errorf("ушедший scope: %v %v, открытых %d", got, err, len(w.peaks))
	}
}

func TestStoreRotateRead(t *testing.T) {
	st := Store{Dir: t.TempDir()}
	day1 := time.Date(2026, 10, 24, 23, 0, 0, 0, time.Local)
	day2 := day1.Add(2 * time.Hour)
	if err := st.Append([]Sample{{At: day1, Name: "lead", Scope: "a", Peak: 5 << 30}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Append([]Sample{{At: day2, Name: "lead", Scope: "a", Peak: 1 << 30}, {At: day2, Name: "x", Scope: "b", Peak: 9 << 30}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Rotate(day2, 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(st.Dir, "2026-10-24.jsonl.gz")); err != nil {
		t.Errorf("прошлый день не сжат: %v", err)
	}
	if _, err := os.Stat(filepath.Join(st.Dir, "2026-10-25.jsonl")); err != nil {
		t.Errorf("сегодняшний файл тронут: %v", err)
	}
	r, err := Collect(func(fn func(*Sample)) (int, error) { return st.Read(day1.Add(-time.Hour), fn) })
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Roles) != 2 || r.Roles[0].Name != "x" || r.Roles[1].Max.Peak != 5<<30 {
		t.Errorf("сводка: %+v", r.Roles)
	}
	for _, o := range r.Over {
		if o.Limit == 4<<30 && (o.Samples != 2 || len(o.Sessions) != 2) {
			t.Errorf("порог 4 ГБ: %+v", o)
		}
	}
	var out bytes.Buffer
	if err := r.Print(&out); err != nil || !strings.Contains(out.String(), "4.0 ГБ: интервалов 2, запусков 2") {
		t.Errorf("печать: %v\n%s", err, out.String())
	}
	if err := st.Rotate(day2.Add(40*24*time.Hour), 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(st.Dir); len(ents) != 0 {
		t.Errorf("старые файлы не удалены: %v", ents)
	}
}
