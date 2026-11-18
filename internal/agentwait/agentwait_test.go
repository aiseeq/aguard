package agentwait

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/transcript"
	"github.com/aiseeq/akit/transcript/waits"
)

var stamp = time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)

func at(min int) string { return stamp.Add(time.Duration(min) * time.Minute).Format(time.RFC3339) }

func hasKey[K comparable, V any](m map[K]V, key K) bool {
	_, ok := m[key]
	return ok
}

func call(min int, id, command string, bg bool) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"cwd":"/home/alice/git/aguard","message":{"role":"assistant","content":[{"type":"tool_use","id":%q,"name":"Bash","input":{"command":%q,"run_in_background":%v}}]}}`, at(min), id, command, bg)
}

func result(min int, id, text string) string {
	return fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":%q}]}}`, at(min), id, text)
}

func notification(min int, id string) string {
	return fmt.Sprintf(`{"type":"user","origin":{"kind":"task-notification"},"timestamp":%q,"message":{"role":"user","content":"<task-notification><task-id>b1</task-id><tool-use-id>%s</tool-use-id><status>completed</status><summary>Background command completed (exit code 0)</summary></task-notification>"}}`, at(min), id)
}

func appendLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		t.Fatal(err)
	}
}

// Незаконченное ожидание хранит распознаватель и продолжает с конца файла после JSON round-trip.
func TestRunRereadsOpen(t *testing.T) {
	projects := t.TempDir()
	file := filepath.Join(projects, "-home-alice-git-aguard", "s1.jsonl")
	appendLines(t, file, call(0, "t1", "go test ./...", true), result(0, "t1", "Command running in background with ID: b1."),
		call(1, "t2", "sleep 30", false), result(2, "t2", ""), call(3, "t3", "ls", false), result(3, "t3", "a"))
	p := &Pass{Projects: projects, Workers: t.TempDir(), Backfill: time.Hour, Budget: 1 << 20, MaxOpen: 6 * time.Hour,
		Name: func(cwd string) string { return filepath.Base(cwd) + "-master" }}
	st := &State{Started: true, Files: map[string]*File{}}
	now := stamp.Add(5 * time.Minute)
	got, err := p.Run(st, now)
	if err != nil || len(got) != 1 || got[0].How != "sleep" || got[0].Ms != 60_000 || got[0].Name != "aguard-master" || got[0].Session != "s1" {
		t.Fatalf("первый проход: %+v %v", got, err)
	}
	if s := st.Files["claude/-home-alice-git-aguard/s1.jsonl"]; s.Offset == 0 || s.Pending == nil || !hasKey(s.Seen, "t2") {
		t.Fatalf("конец файла и открытое ожидание сохранены: %+v", s)
	}
	b, err := json.Marshal(st.Files["claude/-home-alice-git-aguard/s1.jsonl"])
	if err != nil {
		t.Fatal(err)
	}
	var resumed File
	if err := json.Unmarshal(b, &resumed); err != nil {
		t.Fatal(err)
	}
	st.Files["claude/-home-alice-git-aguard/s1.jsonl"] = &resumed
	appendLines(t, file, notification(10, "t1"))
	got, err = p.Run(st, now.Add(10*time.Minute))
	if err != nil || len(got) != 1 || got[0].How != "background" || got[0].Class != "test" || got[0].Ms != 600_000 || got[0].Name != "aguard-master" {
		t.Fatalf("второй проход: %+v %v", got, err)
	}
	if got, err = p.Run(st, now.Add(20*time.Minute)); err != nil || len(got) != 0 {
		t.Fatalf("третий проход пуст: %+v %v", got, err)
	}
	if s := st.Files["claude/-home-alice-git-aguard/s1.jsonl"]; s.Seen != nil {
		t.Errorf("открытых нет — записанные не хранятся: %+v", s)
	}
}

func TestReadOpenIncremental(t *testing.T) {
	projects, workers := t.TempDir(), t.TempDir()
	file := filepath.Join(projects, "p", "s.jsonl")
	appendLines(t, file, call(0, "before", "sleep 10", false), result(1, "before", ""),
		call(2, "bg", "go test ./...", true), result(2, "bg", "Command running in background with ID: b1."))
	initialInfo, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	p := &Pass{Projects: projects, Workers: workers, Budget: 1 << 20, MaxOpen: 6 * time.Hour}
	files, err := List(projects, workers)
	if err != nil || len(files) != 1 {
		t.Fatalf("List: %v, %v", files, err)
	}
	s := &File{}
	if _, _, err := p.read(files[0], s, stamp.Add(time.Minute), 1<<20); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var resumed File
	if err := json.Unmarshal(b, &resumed); err != nil {
		t.Fatal(err)
	}
	s = &resumed
	previous := initialInfo.Size()
	appendLines(t, file, `{"type":"summary"}`, `{"type":"summary"}`)
	info, _ := os.Stat(file)
	files, err = List(projects, workers)
	if err != nil {
		t.Fatal(err)
	}
	added := info.Size() - previous
	got2, n2, err := p.read(files[0], s, stamp.Add(2*time.Minute), 1<<20)
	if err != nil || n2 != added || len(got2) != 0 {
		t.Fatalf("второй проход: %+v, прочитал %d байт, дописано %d: %v", got2, n2, added, err)
	}
	previous = info.Size()
	appendLines(t, file, notification(3, "bg"))
	info, _ = os.Stat(file)
	files, err = List(projects, workers)
	if err != nil {
		t.Fatal(err)
	}
	added = info.Size() - previous
	got, n3, err := p.read(files[0], s, stamp.Add(3*time.Minute), 1<<20)
	if err != nil || n3 != added || len(got) != 1 {
		t.Fatalf("третий проход: %+v, прочитано %d из %d: %v", got, n3, added, err)
	}
	full, _, err := p.read(files[0], &File{}, stamp.Add(3*time.Minute), 1<<20)
	if err != nil || len(full) != 2 {
		t.Fatalf("полный проход: %+v %v", full, err)
	}
	var expected *Wait
	for i := range full {
		if full[i].How == "background" {
			expected = &full[i]
		}
	}
	if expected == nil || got[0] != *expected {
		t.Fatalf("результат с продолжением %+v, полный проход %+v", got[0], expected)
	}
}

func TestReadOpenRestoresToolCall(t *testing.T) {
	projects, workers := t.TempDir(), t.TempDir()
	file := filepath.Join(projects, "p", "s.jsonl")
	appendLines(t, file, call(0, "sleep", "sleep 60", false))
	p := &Pass{Projects: projects, Workers: workers, Budget: 1 << 20, MaxOpen: 6 * time.Hour}
	files, err := List(projects, workers)
	if err != nil || len(files) != 1 {
		t.Fatalf("List: %v, %v", files, err)
	}
	s := &File{}
	if got, _, err := p.read(files[0], s, stamp.Add(time.Minute), 1<<20); err != nil || len(got) != 0 {
		t.Fatalf("первый проход: %+v %v", got, err)
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var resumed File
	if err := json.Unmarshal(b, &resumed); err != nil {
		t.Fatal(err)
	}
	s = &resumed
	previous := s.Offset
	appendLines(t, file, result(1, "sleep", ""), call(2, "other", "ls", false))
	info, _ := os.Stat(file)
	files, err = List(projects, workers)
	if err != nil {
		t.Fatal(err)
	}
	got, n, err := p.read(files[0], s, stamp.Add(2*time.Minute), 1<<20)
	if err != nil || n != info.Size()-previous || len(got) != 1 || got[0].How != "sleep" {
		t.Fatalf("продолжение: %+v, прочитано %d байт: %v", got, n, err)
	}
	full, _, err := p.read(files[0], &File{}, stamp.Add(2*time.Minute), 1<<20)
	if err != nil || len(full) != 1 || got[0] != full[0] {
		t.Fatalf("продолжение %+v, полный проход %+v: %v", got, full, err)
	}
}

// Файл больше бюджета читается кусками по остатку бюджета, хотя хвост помельче идёт первым. Снимок распознавателя
// сохраняет открытые ожидания между кусками, каждое законченное отдаётся ровно один раз.
func TestRunChunksBig(t *testing.T) {
	projects := t.TempDir()
	small := filepath.Join(projects, "p", "small.jsonl")
	big := filepath.Join(projects, "p", "big.jsonl")
	appendLines(t, small, call(0, "s1", "sleep 5", false), result(1, "s1", ""))
	const n = 40
	for i := range n {
		id := fmt.Sprintf("b%d", i)
		appendLines(t, big, call(2*i, id, fmt.Sprintf("sleep %d", 60+i), false), result(2*i+1, id, ""), call(2*i+1, id+"l", "ls", false), result(2*i+1, id+"l", "a"))
	}
	info, err := os.Stat(big)
	if err != nil {
		t.Fatal(err)
	}
	p := &Pass{Projects: projects, Workers: t.TempDir(), Backfill: 7 * 24 * time.Hour, Budget: info.Size() / 5, MaxOpen: 6 * time.Hour}
	st := &State{Files: map[string]*File{}}
	got := 0
	for pass := range 10 {
		ws, err := p.Run(st, stamp.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		got += len(ws)
		if s := st.Files["claude/p/big.jsonl"]; s.Offset == info.Size() {
			break
		}
		if pass == 9 {
			t.Fatalf("большой файл не дочитан: %+v из %d, ожиданий %d", st.Files["claude/p/big.jsonl"], info.Size(), got)
		}
	}
	if got != n {
		t.Errorf("ожиданий %d, нужно %d", got, n)
	}
}

// Открытое дольше MaxOpen закрывается с исходом none, смещение уходит в конец.
func TestRunFlushesStale(t *testing.T) {
	projects := t.TempDir()
	file := filepath.Join(projects, "p", "s1", "subagents", "agent-a1.jsonl")
	appendLines(t, file, call(0, "t1", "make e2e", true), result(0, "t1", "Command running in background with ID: b1."))
	p := &Pass{Projects: projects, Workers: t.TempDir(), Backfill: time.Hour, Budget: 1 << 20, MaxOpen: 6 * time.Hour}
	st := &State{Started: true, Files: map[string]*File{}}
	if got, err := p.Run(st, stamp.Add(time.Minute)); err != nil || len(got) != 0 {
		t.Fatalf("первый проход: %+v %v", got, err)
	}
	key := "claude/p/s1/subagents/agent-a1.jsonl"
	b, err := json.Marshal(st.Files[key])
	if err != nil {
		t.Fatal(err)
	}
	var resumed File
	if err := json.Unmarshal(b, &resumed); err != nil {
		t.Fatal(err)
	}
	st.Files[key] = &resumed
	closed := 0
	for hour := 1; hour <= 8; hour++ {
		got, err := p.Run(st, stamp.Add(time.Duration(hour)*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 0 {
			continue
		}
		if len(got) != 1 || got[0].Outcome != "none" || got[0].Agent != "a1" || got[0].Session != "s1" {
			t.Fatalf("проход %d: %+v", hour, got)
		}
		closed++
		if hour != 6 {
			t.Errorf("просроченное ожидание закрылось на часу %d, нужно 6", hour)
		}
	}
	if closed != 1 {
		t.Fatalf("без дописывания закрытий %d, нужно 1", closed)
	}
	info, _ := os.Stat(file)
	if s := st.Files[key]; s.Offset != info.Size() {
		t.Errorf("смещение: %+v", s)
	}
}

func TestReadKeepsLegacySeen(t *testing.T) {
	projects, workers := t.TempDir(), t.TempDir()
	file := filepath.Join(projects, "p", "s.jsonl")
	p := &Pass{Projects: projects, Workers: workers, Budget: 1 << 20, MaxOpen: 6 * time.Hour}
	appendLines(t, file, call(0, "old", "sleep 60", false), result(1, "old", ""), call(2, "done", "ls", false), result(2, "done", "ok"))
	files, err := List(projects, workers)
	if err != nil {
		t.Fatal(err)
	}
	base, _, err := p.read(files[0], &File{}, stamp.Add(time.Minute), 1<<20)
	if err != nil || len(base) != 1 {
		t.Fatalf("базовое ожидание: %+v %v", base, err)
	}
	s := &File{Seen: map[string]int64{"old": 0}}
	got, _, err := p.read(files[0], s, stamp.Add(time.Minute), 1<<20)
	if err != nil || len(got) != 0 {
		t.Fatalf("legacy Seen подавляет повтор: %+v %v", got, err)
	}
}

func TestReadLineBaseOrdersPending(t *testing.T) {
	projects, workers := t.TempDir(), t.TempDir()
	file := filepath.Join(projects, "p", "s.jsonl")
	appendLines(t, file,
		call(0, "a", "sleep 1", false), result(0, "a", ""),
		call(0, "b", "sleep 1", false), result(0, "b", ""),
		call(0, "c", "sleep 1", false), result(0, "c", ""),
		call(0, "d", "sleep 1", false), result(0, "d", ""),
		call(0, "old", "go test ./...", true))
	p := &Pass{Projects: projects, Workers: workers, Budget: 1 << 20, MaxOpen: 6 * time.Hour}
	files, err := List(projects, workers)
	if err != nil {
		t.Fatal(err)
	}
	s := &File{}
	if _, _, err := p.read(files[0], s, stamp.Add(time.Minute), 1<<20); err != nil {
		t.Fatal(err)
	}
	appendLines(t, file, call(420, "fresh", "go test ./...", true))
	files, err = List(projects, workers)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := p.read(files[0], s, stamp.Add(7*time.Hour), 1<<20)
	if err != nil || len(got) != 2 {
		t.Fatalf("старое ожидание должно определить срок обоих ожиданий: %+v %v", got, err)
	}
}

func TestReadSkipsWhenFileShrinks(t *testing.T) {
	projects, workers := t.TempDir(), t.TempDir()
	file := filepath.Join(projects, "p", "s.jsonl")
	appendLines(t, file,
		call(0, "a", "sleep 1", false), result(1, "a", ""),
		call(2, "b", "sleep 1", false), result(3, "b", ""),
		call(4, "c", "sleep 1", false), result(5, "c", ""),
		call(6, "old", "sleep 30", false))
	p := &Pass{Projects: projects, Workers: workers, Budget: 1 << 20, MaxOpen: 6 * time.Hour}
	files, err := List(projects, workers)
	if err != nil {
		t.Fatal(err)
	}
	s := &File{}
	if _, _, err := p.read(files[0], s, stamp.Add(time.Minute), 1<<20); err != nil {
		t.Fatal(err)
	}
	short := result(2, "old", "") + "\n"
	if err := os.WriteFile(file, []byte(short), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err = List(projects, workers)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := p.read(files[0], s, stamp.Add(3*time.Minute), 1<<20)
	if err != nil || len(got) != 0 || s.Pending == nil || s.Offset != int64(len(short)) {
		t.Fatalf("пропустить укороченный файл: %+v, state=%+v, err=%v", got, s, err)
	}
}

func TestRunSkipsShortenedFileWithoutPending(t *testing.T) {
	projects, workers := t.TempDir(), t.TempDir()
	file := filepath.Join(projects, "p", "s.jsonl")
	completed := []string{call(0, "sleep", "sleep 60", false), result(1, "sleep", ""), call(2, "done", "ls", false), result(2, "done", "ok")}
	appendLines(t, file, append(completed, strings.Repeat(`{"type":"summary"}`, 24))...)
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	p := &Pass{Projects: projects, Workers: workers, Backfill: time.Hour, Budget: 1 << 20, MaxOpen: 6 * time.Hour}
	key := "claude/p/s.jsonl"
	st := &State{Started: true, Files: map[string]*File{key: {Offset: info.Size(), Seen: map[string]int64{"sleep": 0}}}}
	if err := os.WriteFile(file, []byte(strings.Join(completed, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	short, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Run(st, stamp.Add(time.Minute))
	if err != nil || len(got) != 0 || st.Files[key].Offset != short.Size() {
		t.Fatalf("укороченный файл: got=%+v offset=%d want=%d err=%v", got, st.Files[key].Offset, short.Size(), err)
	}
}

func TestRunSkipsShortenedFileWithPending(t *testing.T) {
	projects, workers := t.TempDir(), t.TempDir()
	file := filepath.Join(projects, "p", "s.jsonl")
	lines := []string{
		call(0, "sleep", "sleep 60", false), result(1, "sleep", ""),
		call(2, "bg", "go test ./...", true), result(2, "bg", "Command running in background with ID: b1."),
		`{"type":"summary"}`, `{"type":"summary"}`, `{"type":"summary"}`, `{"type":"summary"}`, `{"type":"summary"}`,
	}
	appendLines(t, file, lines...)
	p := &Pass{Projects: projects, Workers: workers, Backfill: time.Hour, Budget: 1 << 20, MaxOpen: 6 * time.Hour}
	st := &State{Started: true, Files: map[string]*File{}}
	key := "claude/p/s.jsonl"
	got, err := p.Run(st, stamp.Add(3*time.Minute))
	if err != nil || len(got) != 1 || got[0].How != "sleep" {
		t.Fatalf("первый проход: %+v %v", got, err)
	}
	s := st.Files[key]
	if s.Pending == nil {
		t.Fatal("открытый фон не сохранён")
	}
	opened := 0
	p.openFile = func(path string) (io.ReaderAt, io.Closer, error) {
		opened++
		f, err := os.Open(path)
		return f, f, err
	}
	pendingBefore, err := json.Marshal(s.Pending)
	if err != nil {
		t.Fatal(err)
	}
	seenBefore, err := json.Marshal(s.Seen)
	if err != nil {
		t.Fatal(err)
	}
	// Переписывание удалило пять строк после уже выданного sleep и открытого фона.
	if err := os.WriteFile(file, []byte(strings.Join(lines[:4], "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	got, err = p.Run(st, stamp.Add(4*time.Minute))
	if err != nil || len(got) != 0 || s.Offset != info.Size() || s.Pending == nil || opened != 0 {
		t.Fatalf("укороченный файл с ожиданием: got=%+v state=%+v err=%v", got, s, err)
	}
	pendingAfter, err := json.Marshal(s.Pending)
	if err != nil {
		t.Fatal(err)
	}
	seenAfter, err := json.Marshal(s.Seen)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pendingBefore, pendingAfter) || !bytes.Equal(seenBefore, seenAfter) {
		t.Fatalf("снимок изменился: pending %s -> %s, seen %s -> %s", pendingBefore, pendingAfter, seenBefore, seenAfter)
	}
}

type failedReader struct {
	data   []byte
	failAt int64
}

func (r *failedReader) ReadAt(p []byte, off int64) (int, error) {
	if off >= r.failAt {
		return 0, fmt.Errorf("synthetic read failure")
	}
	end := min(int64(len(r.data)), r.failAt)
	n := copy(p, r.data[off:end])
	if off+int64(n) == r.failAt {
		return n, fmt.Errorf("synthetic read failure")
	}
	return n, nil
}
func (*failedReader) Close() error { return nil }

func TestReadErrorDoesNotCommitRecognizer(t *testing.T) {
	projects, workers := t.TempDir(), t.TempDir()
	file := filepath.Join(projects, "p", "s.jsonl")
	appendLines(t, file, call(0, "bg", "go test ./...", true), result(0, "bg", "Command running in background with ID: b1."))
	p := &Pass{Projects: projects, Workers: workers, Budget: 1 << 20, MaxOpen: 6 * time.Hour}
	files, err := List(projects, workers)
	if err != nil {
		t.Fatal(err)
	}
	s := &File{}
	if _, _, err := p.read(files[0], s, stamp.Add(time.Minute), 1<<20); err != nil {
		t.Fatal(err)
	}
	before, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	appendLines(t, file, call(1, "sleep", "sleep 60", false), result(2, "sleep", ""), call(3, "done", "ls", false), result(3, "done", "ok"))
	files, err = List(projects, workers)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	p.openFile = func(string) (io.ReaderAt, io.Closer, error) {
		return &failedReader{data: data, failAt: int64(len(data))}, nilCloser{}, nil
	}
	if got, _, err := p.read(files[0], s, stamp.Add(3*time.Minute), 1<<20); err == nil || len(got) != 0 {
		t.Fatalf("ошибка чтения должна отменить проход: %+v %v", got, err)
	}
	after, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("состояние изменилось после ошибки:\n%s\n%s", before, after)
	}
	p.openFile = nil
	appendLines(t, file, notification(4, "bg"))
	files, err = List(projects, workers)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := p.read(files[0], s, stamp.Add(4*time.Minute), 1<<20)
	if err != nil || len(got) != 2 || got[0].How != "sleep" || got[1].How != "background" {
		t.Fatalf("повтор после ошибки: %+v %v", got, err)
	}
}

func TestRunRecoversCorruptPendingOnce(t *testing.T) {
	projects, workers := t.TempDir(), t.TempDir()
	file := filepath.Join(projects, "p", "s.jsonl")
	appendLines(t, file, call(0, "done", "sleep 10", false), result(1, "done", ""),
		call(2, "open", "go test ./...", true), result(2, "open", "Command running in background with ID: b1."))
	p := &Pass{Projects: projects, Workers: workers, Budget: 1 << 20, MaxOpen: 6 * time.Hour}
	st := &State{Started: true, Files: map[string]*File{}}
	if got, err := p.Run(st, stamp.Add(3*time.Minute)); err != nil || st.Files["claude/p/s.jsonl"].Pending == nil {
		t.Fatalf("первый проход: %+v, state=%+v, %v", got, st.Files["claude/p/s.jsonl"], err)
	}
	key := "claude/p/s.jsonl"
	data, err := json.Marshal(st.Files[key])
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	fields["pending"] = json.RawMessage(`{"pending":"corrupt"}`)
	data, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	var damaged File
	if err := json.Unmarshal(data, &damaged); err != nil {
		t.Fatal(err)
	}
	if !damaged.PendingBad {
		t.Fatal("повреждённый снимок не отмечен")
	}
	st.Files[key] = &damaged
	got, err := p.Run(st, stamp.Add(4*time.Minute))
	if err == nil || !strings.Contains(err.Error(), "снимок распознавателя") || len(got) != 0 {
		t.Fatalf("восстановление снимка: %+v %v", got, err)
	}
	if st.Files[key].Pending == nil || st.Files[key].PendingBad {
		t.Fatalf("ожидание не восстановлено: %+v", st.Files[key])
	}
	if _, err := p.Run(st, stamp.Add(5*time.Minute)); err != nil {
		t.Fatalf("повторная ошибка после восстановления: %v", err)
	}
}

func TestRunCorruptPendingAtTruncatedEndKeepsSeen(t *testing.T) {
	projects, workers := t.TempDir(), t.TempDir()
	file := filepath.Join(projects, "p", "s.jsonl")
	prefix := []string{
		call(0, "sleep", "sleep 60", false), result(1, "sleep", ""),
		call(2, "done", "ls", false), result(2, "done", "ok"),
	}
	appendLines(t, file, prefix...)
	p := &Pass{Projects: projects, Workers: workers, Budget: 1 << 20, MaxOpen: 6 * time.Hour}
	st := &State{Started: true, Files: map[string]*File{}}
	key := "claude/p/s.jsonl"
	got, err := p.Run(st, stamp.Add(3*time.Minute))
	if err != nil || len(got) != 1 || got[0].How != "sleep" {
		t.Fatalf("sleep должен выдаться первым проходом: %+v %v", got, err)
	}
	prefixSize, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	appendLines(t, file, call(4, "bg", "go test ./...", true), result(4, "bg", "Command running in background with ID: b1."))
	got, err = p.Run(st, stamp.Add(5*time.Minute))
	if err != nil || len(got) != 0 || st.Files[key].Pending == nil || st.Files[key].PendingAt != prefixSize.Size() {
		t.Fatalf("второй проход должен сохранить начало нового фона: waits=%+v state=%+v err=%v", got, st.Files[key], err)
	}
	data, err := json.Marshal(st.Files[key])
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	fields["pending"] = json.RawMessage(`{"pending":"corrupt"}`)
	data, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	var damaged File
	if err := json.Unmarshal(data, &damaged); err != nil {
		t.Fatal(err)
	}
	if !damaged.PendingBad {
		t.Fatal("повреждённый снимок не отмечен")
	}
	st.Files[key] = &damaged
	if err := os.WriteFile(file, []byte(strings.Join(prefix, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = p.Run(st, stamp.Add(6*time.Minute))
	if err == nil || len(got) != 0 || st.Files[key].Offset != prefixSize.Size() {
		t.Fatalf("восстановление не должно повторно выдать sleep: waits=%+v state=%+v err=%v", got, st.Files[key], err)
	}
}

func TestRunCorruptPendingKeepsSeen(t *testing.T) {
	projects, workers := t.TempDir(), t.TempDir()
	file := filepath.Join(projects, "p", "s.jsonl")
	appendLines(t, file, call(0, "bg", "go test ./...", true), result(0, "bg", "Command running in background with ID: b1."),
		call(1, "sleep", "sleep 60", false), result(2, "sleep", ""), call(3, "done", "ls", false), result(3, "done", "ok"))
	p := &Pass{Projects: projects, Workers: workers, Budget: 1 << 20, MaxOpen: 6 * time.Hour}
	st := &State{Started: true, Files: map[string]*File{}}
	got, err := p.Run(st, stamp.Add(4*time.Minute))
	if err != nil || len(got) != 1 || got[0].How != "sleep" {
		t.Fatalf("первый проход: %+v %v", got, err)
	}
	key := "claude/p/s.jsonl"
	data, err := json.Marshal(st.Files[key])
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	fields["pending"] = json.RawMessage(`{"pending":"corrupt"}`)
	data, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	var damaged File
	if err := json.Unmarshal(data, &damaged); err != nil {
		t.Fatal(err)
	}
	st.Files[key] = &damaged
	got, err = p.Run(st, stamp.Add(5*time.Minute))
	if err == nil || len(got) != 0 {
		t.Fatalf("повреждённый снимок повторил выданный sleep: %+v %v", got, err)
	}
	appendLines(t, file, notification(6, "bg"))
	got, err = p.Run(st, stamp.Add(7*time.Minute))
	if err != nil || len(got) != 1 || got[0].How != "background" {
		t.Fatalf("после восстановления: %+v %v", got, err)
	}
}

type nilCloser struct{}

func (nilCloser) Close() error { return nil }

// До первого прохода старые файлы пропускаются, свежие читаются с начала; codex — по папке задачи.
func TestRunBackfill(t *testing.T) {
	projects, workers := t.TempDir(), t.TempDir()
	old := filepath.Join(projects, "p", "old.jsonl")
	appendLines(t, old, call(0, "t1", "sleep 10", false), result(1, "t1", ""))
	ago := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(old, ago, ago); err != nil {
		t.Fatal(err)
	}
	rollout := filepath.Join(workers, "1027-fix", "codex", "sessions", "2026", "10", "27", "rollout-2026-10-27T10-00-00-abc.jsonl")
	appendLines(t, rollout, `{"timestamp":"2026-10-27T10:00:00Z","type":"session_meta","payload":{"id":"abc","cwd":"/w"}}`)
	p := &Pass{Projects: projects, Workers: workers, Backfill: 7 * 24 * time.Hour, Budget: 1 << 20, MaxOpen: 6 * time.Hour}
	st := &State{Files: map[string]*File{}}
	if got, err := p.Run(st, time.Now()); err != nil || len(got) != 0 {
		t.Fatalf("%+v %v", got, err)
	}
	info, _ := os.Stat(old)
	if s := st.Files["claude/p/old.jsonl"]; s.Offset != info.Size() {
		t.Errorf("старый файл не пропущен: %+v", s)
	}
	key := "codex/1027-fix/codex/sessions/2026/10/27/rollout-2026-10-27T10-00-00-abc.jsonl"
	if s := st.Files[key]; s == nil || s.Offset == 0 {
		t.Errorf("rollout не прочитан: %+v", st.Files)
	}
	files, err := List(projects, workers)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Runner == RunnerCodex && (f.Project != "1027-fix" || f.Session != "2026-10-27T10-00-00-abc") {
			t.Errorf("codex: %+v", f)
		}
	}
}

func TestWorkerCacheFindsNewTasksContinueAndTail(t *testing.T) {
	projects, workers := t.TempDir(), t.TempDir()
	writeTask := func(task, phase, session string) string {
		base := filepath.Join(workers, task)
		if err := os.MkdirAll(base, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, "status.json"), fmt.Appendf(nil, `{"phase":%q}`, phase), 0o600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(base, "codex", "sessions", "2026", "11", "18", "rollout-"+session+".jsonl")
		appendLines(t, path, `{"timestamp":"2026-11-18T10:00:00Z","type":"session_meta","payload":{"id":"`+session+`"}}`)
		return path
	}
	p := &Pass{Projects: projects, Workers: workers, Budget: 1 << 20, MaxOpen: 6 * time.Hour}
	st := &State{Files: map[string]*File{}}
	writeTask("new-task", "готово", "first")
	if _, err := p.Run(st, stamp); err != nil {
		t.Fatal(err)
	}
	key := "codex/new-task/codex/sessions/2026/11/18/rollout-first.jsonl"
	if st.Files[key] == nil || st.Files[key].Offset == 0 {
		t.Fatalf("новая задача не считана: %+v", st.Files[key])
	}
	info, err := os.Stat(filepath.Join(workers, "new-task", "codex", "sessions", "2026", "11", "18", "rollout-first.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	cached := st.Workers.Tasks["new-task"].Sources[0]
	if cached.Size != info.Size() || cached.Mtime != info.ModTime().UnixNano() {
		t.Fatalf("size/mtime не сохранены: %+v, info=%+v", cached, info)
	}
	stateJSON, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stateJSON), workers) {
		t.Fatal("абсолютный путь транскрипта дублируется в worker-кеше")
	}
	var resumed State
	if err := json.Unmarshal(stateJSON, &resumed); err != nil {
		t.Fatal(err)
	}
	st = &resumed
	cached = st.Workers.Tasks["new-task"].Sources[0]
	if cached.Size != info.Size() || cached.Mtime != info.ModTime().UnixNano() {
		t.Fatalf("JSON round-trip потерял size/mtime: %+v", cached)
	}
	opened := 0
	listed := 0
	p.openFile = func(path string) (io.ReaderAt, io.Closer, error) {
		opened++
		f, err := os.Open(path)
		return f, f, err
	}
	p.listWorker = func(root, task string) ([]Source, error) {
		listed++
		return listWorkerTask(root, task)
	}
	if _, err := p.Run(st, stamp.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if opened != 0 || listed != 0 {
		t.Fatalf("готовая задача обойдена: transcript-открытий %d, списков каталогов %d", opened, listed)
	}
	if st.Files[key] == nil || st.Files[key].Offset == 0 {
		t.Fatalf("неперечисленный кэшем offset удалён: %+v", st.Files[key])
	}

	// worker continue оставляет задачу готовой, но меняет status.json и добавляет новый rollout.
	writeTask("new-task", "готово", "continued")
	status := filepath.Join(workers, "new-task", "status.json")
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(status, future, future); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Run(st, stamp.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if listed == 0 {
		t.Fatal("continue не вызвал повторное перечисление rollout")
	}
	continuedKey := "codex/new-task/codex/sessions/2026/11/18/rollout-continued.jsonl"
	if st.Files[continuedKey] == nil || st.Files[continuedKey].Offset == 0 {
		t.Fatalf("continue не найден: %+v", st.Files)
	}

	// Новая задача после построения кэша находится по mtime каталога worker.
	live := writeTask("another-task", "работает", "live")
	if _, err := p.Run(st, stamp.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	liveKey := "codex/another-task/codex/sessions/2026/11/18/rollout-live.jsonl"
	before := st.Files[liveKey].Offset
	appendLines(t, live, `{"timestamp":"2026-11-18T10:01:00Z","type":"session_meta","payload":{"id":"live"}}`)
	if _, err := p.Run(st, stamp.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if st.Files[liveKey].Offset <= before {
		t.Fatalf("дописанный хвост не прочитан: до %d после %d", before, st.Files[liveKey].Offset)
	}
}

func TestWorkerCacheExpiresSkippedDoneTask(t *testing.T) {
	projects, workers := t.TempDir(), t.TempDir()
	path := filepath.Join(workers, "done", "codex", "sessions", "2026", "11", "18", "rollout-open.jsonl")
	appendLines(t, path, `{"timestamp":"2026-10-27T10:00:00Z","type":"session_meta","payload":{"id":"open"}}`)
	if err := os.WriteFile(filepath.Join(workers, "done", "status.json"), []byte(`{"phase":"готово"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &Pass{Projects: projects, Workers: workers, Budget: 1 << 20, MaxOpen: time.Hour}
	st := &State{Started: true, Files: map[string]*File{}}
	if _, err := p.Run(st, stamp); err != nil {
		t.Fatal(err)
	}
	key := "codex/done/codex/sessions/2026/11/18/rollout-open.jsonl"
	r := waits.New()
	r.Add(transcript.Event{Role: transcript.Cmd, Who: "Bash", Text: "sleep 30", CallID: "wait", Time: stamp,
		Raw: []byte(`{"command":"sleep 30"}`), Line: 1})
	st.Files[key].Pending = r
	got, err := p.Run(st, stamp.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Outcome != "none" {
		t.Fatalf("ожидание завершённой пропущенной задачи не истекло: %+v", got)
	}
}

func TestWorkerCacheSidecarNotWrittenWhenClean(t *testing.T) {
	projects, workers := t.TempDir(), t.TempDir()
	rvTask(t, workers, "done", "готово", "s", 1)
	offsets := filepath.Join(t.TempDir(), "offsets.json")
	p := &Pass{Projects: projects, Workers: workers, Budget: 1 << 20}
	st := &State{Files: map[string]*File{}}
	if _, err := p.Run(st, stamp); err != nil {
		t.Fatal(err)
	}
	st.workersDirty = true
	if err := st.Save(offsets); err != nil {
		t.Fatal(err)
	}
	cachePath := workerCachePath(offsets)
	cacheBefore, err := os.Stat(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	offsetBytes, err := os.ReadFile(offsets)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(offsetBytes), `"workers"`) {
		t.Fatal("offsets.json содержит worker-кеш")
	}
	loaded, err := LoadState(offsets)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Run(loaded, stamp.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if loaded.workersDirty {
		t.Fatal("неизменившийся кеш отмечен грязным")
	}
	if err := loaded.Save(offsets); err != nil {
		t.Fatal(err)
	}
	cacheAfter, err := os.Stat(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if cacheAfter.Size() != cacheBefore.Size() || !cacheAfter.ModTime().Equal(cacheBefore.ModTime()) {
		t.Fatalf("sidecar переписан без изменений: before=%v after=%v", cacheBefore, cacheAfter)
	}
}

func TestWorkerCachePendingBadDoesNotSkipDoneTask(t *testing.T) {
	projects, workers := t.TempDir(), t.TempDir()
	rvTask(t, workers, "done", "готово", "s", 1)
	p := &Pass{Projects: projects, Workers: workers, Budget: 1 << 20, MaxOpen: time.Hour}
	st := &State{Started: true, Files: map[string]*File{}}
	if _, err := p.Run(st, stamp); err != nil {
		t.Fatal(err)
	}
	key := "codex/done/codex/sessions/2026/11/18/rollout-s.jsonl"
	st.Files[key].PendingBad = true
	opened := 0
	p.openFile = func(path string) (io.ReaderAt, io.Closer, error) {
		opened++
		f, err := os.Open(path)
		return f, f, err
	}
	_, _ = p.Run(st, stamp.Add(time.Minute))
	if opened == 0 {
		t.Fatal("готовая задача с PendingBad пропущена без чтения")
	}
}

func TestWorkerCacheStatusErrorKeepsKnownSources(t *testing.T) {
	projects, workers := t.TempDir(), t.TempDir()
	path := rvTask(t, workers, "known", "готово", "s", 1)
	p := &Pass{Projects: projects, Workers: workers, Budget: 1 << 20, MaxOpen: time.Hour}
	st := &State{Started: true, Files: map[string]*File{}}
	if _, err := p.Run(st, stamp); err != nil {
		t.Fatal(err)
	}
	key := "codex/known/codex/sessions/2026/11/18/rollout-s.jsonl"
	oldOffset := st.Files[key].Offset
	status := filepath.Join(workers, "known", "status.json")
	if err := os.Remove(status); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(status, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := p.Run(st, stamp.Add(time.Minute))
	if err == nil {
		t.Fatal("ожидалась ошибка чтения status.json-каталога")
	}
	if got := st.Files[key]; got == nil || got.Offset < oldOffset {
		t.Fatalf("ошибка статуса удалила или откатила прежний файл: before=%d after=%+v", oldOffset, got)
	}
	if len(st.Workers.Tasks["known"].Sources) != 1 {
		t.Fatalf("прежний список транскриптов потерян: %+v", st.Workers.Tasks["known"])
	}
	_ = path
}

func TestWorkerCacheRootModRetriesNewTaskAfterStatusError(t *testing.T) {
	projects, workers := t.TempDir(), t.TempDir()
	p := &Pass{Projects: projects, Workers: workers, Budget: 1 << 20}
	st := &State{Started: true, Files: map[string]*File{}}
	if _, err := p.Run(st, stamp); err != nil {
		t.Fatal(err)
	}
	oldRootMod := st.Workers.RootMod
	base := filepath.Join(workers, "transient")
	if err := os.MkdirAll(filepath.Join(base, "codex", "sessions", "2026", "11", "18"), 0o755); err != nil {
		t.Fatal(err)
	}
	appendLines(t, filepath.Join(base, "codex", "sessions", "2026", "11", "18", "rollout-s.jsonl"),
		`{"timestamp":"2026-11-18T10:00:00Z","type":"session_meta","payload":{"id":"s"}}`)
	if err := os.Mkdir(filepath.Join(base, "status.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Run(st, stamp.Add(time.Minute)); err == nil {
		t.Fatal("ошибка статуса новой задачи не возвращена")
	}
	if st.Workers.RootMod != oldRootMod {
		t.Fatalf("RootMod сдвинулся при ошибке новой задачи: before=%d after=%d", oldRootMod, st.Workers.RootMod)
	}
	if err := os.Remove(filepath.Join(base, "status.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "status.json"), []byte(`{"phase":"работает"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Run(st, stamp.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	key := "codex/transient/codex/sessions/2026/11/18/rollout-s.jsonl"
	if st.Files[key] == nil || st.Files[key].Offset == 0 {
		t.Fatalf("новая задача после временной ошибки не найдена: %+v", st.Files)
	}
}

func TestWorkerCacheStatusChangeMarksCacheDirtyAndPersists(t *testing.T) {
	projects, workers := t.TempDir(), t.TempDir()
	status := filepath.Join(workers, "done", "status.json")
	rvTask(t, workers, "done", "готово", "s", 1)
	offsets := filepath.Join(t.TempDir(), "offsets.json")
	p := &Pass{Projects: projects, Workers: workers, Budget: 1 << 20}
	st := &State{Started: true, Files: map[string]*File{}}
	if _, err := p.Run(st, stamp); err != nil {
		t.Fatal(err)
	}
	if err := st.Save(offsets); err != nil {
		t.Fatal(err)
	}
	st, err := LoadState(offsets)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(status, []byte(`{"phase":"работает"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(status, future, future); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Run(st, stamp.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if !st.workersDirty {
		t.Fatal("изменившаяся задача не пометила worker-кеш как изменённый")
	}
	if err := st.Save(offsets); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadState(offsets)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Workers.Tasks["done"].Done {
		t.Fatal("LoadState не увидел сохранённое изменение статуса задачи")
	}
}

// Сводка: окна, классы по времени ожидания, примеры «прочего», очередь graft только верхнего уровня.
func TestCollect(t *testing.T) {
	now := stamp.Add(24 * time.Hour)
	waits := []Wait{
		{At: now.Add(-time.Hour), Ms: 600_000, Runner: "claude", Session: "s1", How: "loop", Class: "test", Calls: 3, Repeats: 2, Tokens: 300_000, Outcome: "done"},
		{At: now.Add(-2 * time.Hour), Ms: 30_000, Runner: "codex", Session: "x", How: "stdin_poll", Class: "other", Command: "go test", Calls: 5, Tokens: 250_000, Outcome: "done"},
		{At: now.Add(-3 * 24 * time.Hour), Ms: 5_000, Runner: "claude", Session: "s2", How: "sleep", Class: "other", Command: "sleep 5", Calls: 1, Outcome: "done"},
	}
	graft := filepath.Join(t.TempDir(), "stats.jsonl")
	var b bytes.Buffer
	for _, l := range []string{
		`{"time":%q,"kind":"task","top":"exec","wall_s":50,"wait_s":36,"wait_why":{"swap":6,"settle":30}}`,
		`{"time":%q,"kind":"task","top":"commit","in":"gate","wait_s":20,"wait_why":{"queue":20}}`,
		`{"time":%q,"kind":"task","top":"test","wait_s":4}`,
		`{"time":%q,"kind":"step","top":"exec","wall_s":1}`,
	} {
		fmt.Fprintf(&b, l+"\n", now.Add(-time.Hour).Format(time.RFC3339))
	}
	b.WriteString("не json\n")
	if err := os.WriteFile(graft, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Collect(now, func(fn func(*Wait)) (int, error) {
		for i := range waits {
			fn(&waits[i])
		}
		return 0, nil
	}, []string{graft + ".1", graft})
	if err != nil {
		t.Fatal(err)
	}
	day, week := s.Windows["day"], s.Windows["week"]
	if day.Total.Waits != 2 || week.Total.Waits != 3 || day.Total.Calls != 8 || day.Total.Tokens != 550_000 {
		t.Errorf("итог: день %+v, неделя %+v", day.Total, week.Total)
	}
	if day.Classes[0].Class != "test" || day.Classes[0].Label != "тест или гейт" || day.Classes[0].Repeats != 2 {
		t.Errorf("классы: %+v", day.Classes[0])
	}
	if len(week.Other) != 2 || week.Other[0].Runner != "codex" { // при равном числе — дольше ждали выше
		t.Errorf("прочее: %+v", week.Other)
	}
	if g := day.Graft; g.Tasks != 2 || g.Waited != 2 || g.WaitS != 40 || g.Why["settle"] != 30 || g.Why[WhyUnknown] != 4 || g.ByTop["exec"] != 36 {
		t.Errorf("graft: %+v", g)
	}
	if s.Bad != 1 {
		t.Errorf("bad %d", s.Bad)
	}
	var out bytes.Buffer
	if err := s.Print(&out, "week"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"тест или гейт", "всего", "Прочее", "Очередь graft: из 2 задач ждали 2"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("нет %q в\n%s", want, out.String())
		}
	}
}

func TestValidate(t *testing.T) {
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.MaxOpen = 0
	if c.Validate() == nil {
		t.Error("max_open 0 прошёл")
	}
}

func TestGraftStatsRotated(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".cache", "graft")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"stats.jsonl.1", "stats.jsonl.10", "stats.jsonl.2", "stats.jsonl.tmp", "stats.jsonl.0"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f := filepath.Join(dir, "stats.jsonl")
	want := []string{f + ".10", f + ".2", f + ".1", f}
	got, err := GraftStats(home)
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("GraftStats = %q, %v; want %q", got, err, want)
	}
}
