package record

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseEvents(t *testing.T) {
	born := time.Date(2026, 10, 19, 22, 0, 0, 0, time.UTC)
	create := Event{T: born, Ev: EvCreate, Path: "/tmp/claude-tmp/x", Dir: true, UID: 1000, Owner: &Owner{
		Proc:    Proc{Pid: 42, Comm: "mkdir", Args: "mkdir /tmp/claude-tmp/x", UID: 1000, State: ProcOK},
		Chain:   []Link{{Pid: 41, Comm: "bash"}, {Pid: 40, Comm: "claude"}},
		Session: &Session{Pid: 40, Name: "lead", Cwd: "/home/alice", ID: "abc", ProcStart: "123", Via: "ppid"},
	}}
	del := Event{T: born.Add(time.Hour), Ev: EvDelete, Path: "/tmp/claude-tmp/x", UID: 1000, Owner: create.Owner,
		Born: born, LifeS: 3600, MaxBytes: 1 << 30, ByteSec: 1.5e12, Measured: true}
	var sb strings.Builder
	for _, e := range []Event{create, del} {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		sb.Write(b)
		sb.WriteString("\n")
	}
	sb.WriteString("\n{\"t\":\"битая\n") // оборванная строка при падении
	sb.WriteString(`{"t":"2026-10-19T22:00:00Z","path":"/tmp/y"}` + "\n")
	var got []Event
	bad, err := ParseEvents(strings.NewReader(sb.String()), func(e Event) { got = append(got, e) })
	if err != nil {
		t.Fatal(err)
	}
	if bad != 2 || len(got) != 2 {
		t.Fatalf("bad=%d, событий %d", bad, len(got))
	}
	if got[0].Owner.Session.Name != "lead" || got[0].Owner.Chain[1].Comm != "claude" || got[0].Closed() {
		t.Errorf("create разобран неверно: %+v", got[0])
	}
	if !got[1].Closed() || got[1].MaxBytes != 1<<30 || !got[1].Born.Equal(born) || got[1].LifeS != 3600 {
		t.Errorf("delete разобран неверно: %+v", got[1])
	}
	// пустой born не пишется в журнал
	b, _ := json.Marshal(Event{Ev: EvCreate, Path: "/tmp/z"})
	if strings.Contains(string(b), "born") {
		t.Errorf("пустой born в строке: %s", b)
	}
}

func TestReadEventFilesRotation(t *testing.T) {
	dir := t.TempDir()
	line := func(p string) string {
		return `{"t":"2026-10-19T22:00:00Z","ev":"create","path":"` + p + `","uid":0}` + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, RotatedOwner), []byte(line("/tmp/old")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, OwnersFile), []byte(line("/tmp/new")), 0o644); err != nil {
		t.Fatal(err)
	}
	var paths []string
	if _, err := ReadEventFiles(dir, func(e Event) { paths = append(paths, e.Path) }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(paths, ",") != "/tmp/old,/tmp/new" {
		t.Errorf("порядок чтения: %v", paths)
	}
}

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, StateFile)
	if s, err := ReadState(p); err != nil || len(s.Entries) != 0 {
		t.Fatalf("нет файла — пустой снимок: %v", err)
	}
	s := State{Updated: time.Now().UTC().Round(0), Entries: []Entry{{Path: "/tmp/a", Bytes: 10, Born: time.Unix(100, 0).UTC()}}}
	b, _ := json.Marshal(s)
	if err := WriteFileAtomic(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadState(p)
	if err != nil || len(got.Entries) != 1 || got.Entries[0].Bytes != 10 {
		t.Fatalf("снимок: %+v %v", got, err)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("права снимка %v", fi.Mode().Perm())
	}
}

func TestReadStateDropsEntryWithoutBorn(t *testing.T) {
	p := filepath.Join(t.TempDir(), StateFile)
	b, _ := json.Marshal(State{Entries: []Entry{{Path: "/tmp/noborn", Bytes: 10}, {Path: "/tmp/ok", Bytes: 20, Born: time.Unix(100, 0)}}})
	if err := WriteFileAtomic(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := ReadState(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Entries) != 1 || s.Entries[0].Path != "/tmp/ok" || !slices.Equal(s.NoBorn, []string{"/tmp/noborn"}) {
		t.Fatalf("запись без born — в NoBorn, остальные годны: %+v", s)
	}
}
