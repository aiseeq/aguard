package iowatch

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// Два писателя одного файла друг другу не мешают (копия сводок раз в час и остановка службы — разные горутины): у
// каждого свой временный файл; сбой записи временного файла не оставляет (01c863b).
func TestWriteAtomicTwoWritersAndFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cases.jsonl")
	err := writeAtomic(path, func(w *bufio.Writer) error {
		if _, err := w.WriteString("первый\n"); err != nil {
			return err
		}
		return writeAtomic(path, func(w *bufio.Writer) error { // второй — пока первый не дописал
			_, err := w.WriteString("второй\n")
			return err
		})
	})
	if err != nil {
		t.Fatalf("два писателя: %v", err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "первый\n" {
		t.Fatalf("файл после двух писателей: %q %v", b, err)
	}
	boom := errors.New("сбой")
	if err := writeAtomic(path, func(*bufio.Writer) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("сбой записи: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "cases.jsonl" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("после сбоя в папке %v, ждал только cases.jsonl", names)
	}
}

// Временный файл снимка (<имя>.jsonl.<случайное>.part), брошенный падением, уходит вместе со старыми снимками.
func TestCleanupRemovesOldTempPart(t *testing.T) {
	dir := t.TempDir()
	part := filepath.Join(dir, "manual-20261029-120000.jsonl.123456.part")
	writeFile(t, part, "{}\n")
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(part, past, past); err != nil {
		t.Fatal(err)
	}
	if err := CleanupSnapshots(dir, time.Hour, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Fatalf("старый временный файл снимка остался: %v", err)
	}
}

// -who: случай — где подстрока есть в главной cgroup, в имени процесса топов записи, чтения и свопа или в его
// cmdline; случай без виновников (короче case_min) не подходит; пусто — все случаи.
func TestFilterCasesWho(t *testing.T) {
	at := time.Date(2026, 11, 8, 4, 25, 48, 0, time.Local)
	k := func(c Culprit) *Culprit { return &c }
	cases := []Case{
		{Start: at, Culprits: k(Culprit{Main: "/user.slice/app.slice/snapperd.service"})},
		{Start: at.Add(1), Culprits: k(Culprit{ProcWrite: []Ranked{{Name: "chrome(5788)"}}})},
		{Start: at.Add(2), Culprits: k(Culprit{ProcRead: []Ranked{{Name: "2.1.319(2244828)"}},
			ProcessDetails: map[string]ProcessDetails{"2.1.319(2244828)": {Cmdline: "ugrep -r x /home/aisee/.claude/projects", CWD: "/srv/gocache"}}})},
		{Start: at.Add(3), Culprits: k(Culprit{ProcMajflt: []Ranked{{Name: "gopls(9)"}}})},
		{Start: at.Add(4)},
		{Start: at.Add(5), Culprits: k(Culprit{CgroupWrite: []Ranked{{Name: "chrome"}}, MainProc: "chrome(1)",
			ProcessDetails: map[string]ProcessDetails{"chrome(1)": {Cmdline: "chrome"}, "dd(2)": {Cmdline: "ugrep"}}})},
	}
	for who, want := range map[string][]int{"snapperd": {0}, "chrome": {1}, "ugrep": {2}, "gopls": {3}, "gocache": nil, "": {0, 1, 2, 3, 4, 5}} {
		got := FilterCases(cases, who)
		var idx []int
		for _, c := range got {
			idx = append(idx, int(c.Start.Sub(at)))
		}
		if !slices.Equal(idx, want) {
			t.Errorf("-who %q: случаи %v, ждали %v", who, idx, want)
		}
	}
}
