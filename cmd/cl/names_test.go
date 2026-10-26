package main

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/aiseeq/aguard/internal/names"
)

func TestNameFor(t *testing.T) {
	home := "/home/u"
	table := map[string]string{home: "lead", "/home/u/git": "admin-sc2", "/home/u/sc2/demu/farm/hard": "bench-master"}
	cases := map[string]string{
		"/home/u":                    "lead",
		"/home/u/git":                "admin-sc2",
		"/home/u/git/sc2demu":        "sc2demu-master",
		"/home/u/work/Shop/":         "shop-master",
		"/home/u/sc2/demu/farm/hard": "bench-master",
		"/home/u/git/sc2demu/cmd":    "",
		"/tmp":                       "",
	}
	for cwd, want := range cases {
		if got := names.For(cwd, home, table); got != want {
			t.Errorf("nameFor(%q) = %q, want %q", cwd, got, want)
		}
	}
}

func TestFree(t *testing.T) {
	taken := map[string]bool{"sc2demu-master": true, "sc2demu-master-2": true}
	if got := free("sc2demu-master", taken); got != "sc2demu-master-3" {
		t.Errorf("free = %q", got)
	}
	if got := free("lead", taken); got != "lead" {
		t.Errorf("free = %q", got)
	}
}

func TestInteractive(t *testing.T) {
	if !interactive(nil) || !interactive([]string{"--resume", "x"}) || !interactive([]string{"привет"}) {
		t.Error("сессия должна получать флаги")
	}
	if interactive([]string{"mcp", "list"}) || interactive([]string{"-p", "q"}) {
		t.Error("подкоманда и -p — без флагов")
	}
}

func TestReadTableAndLive(t *testing.T) {
	dir := t.TempDir()
	tab := filepath.Join(dir, "names.tsv")
	if err := os.WriteFile(tab, []byte("# c\n~\tlead\n~/git\tadmin-sc2\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	table, err := names.ReadTable(tab, "/home/u")
	if err != nil || table["/home/u"] != "lead" || table["/home/u/git"] != "admin-sc2" {
		t.Fatalf("table = %v, err = %v", table, err)
	}
	if empty, err := names.ReadTable(filepath.Join(dir, "none"), "/home/u"); err != nil || len(empty) != 0 {
		t.Fatalf("нет файла: %v %v", empty, err)
	}
	live := `{"pid":` + strconv.Itoa(os.Getpid()) + `,"name":"sc2demu-master"}`
	dead := `{"pid":999999999,"name":"lead"}`
	write(t, filepath.Join(dir, "1.json"), live)
	write(t, filepath.Join(dir, "2.json"), dead)
	names, err := liveNames(dir)
	if err != nil || !names["sc2demu-master"] || names["lead"] {
		t.Errorf("liveNames = %v", names)
	}
}

func TestSessionArgv(t *testing.T) {
	home := t.TempDir()
	worktrees := filepath.Join(home, ".cache", "worktrees")
	tmp := filepath.Join(home, ".cache", "claude-tmp")

	// Общих папок нет — --add-dir не передаётся, остальной argv прежний.
	if got := sessionFlags(home); !slices.Equal(got, []string{"--dangerously-skip-permissions", searchToolsFlag}) {
		t.Errorf("без папок sessionFlags = %q", got)
	}
	if got := printArgs(home, []string{"-p", "привет"}); !slices.Equal(got, []string{searchToolsFlag, "-p", "привет"}) {
		t.Errorf("без папок -p: %q", got)
	}

	for _, dir := range []string{worktrees, tmp} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dirs := []string{"--add-dir=" + worktrees, "--add-dir=" + tmp}
	want := append([]string{"--dangerously-skip-permissions", searchToolsFlag}, dirs...)
	if got := sessionFlags(home); !slices.Equal(got, want) {
		t.Errorf("sessionFlags = %q, want %q", got, want)
	}
	want = append(append([]string{searchToolsFlag}, dirs...), "-p", "привет")
	if got := printArgs(home, []string{"-p", "привет"}); !slices.Equal(got, want) {
		t.Errorf("-p: %q, want %q", got, want)
	}
	if got := printArgs(home, []string{"mcp", "list"}); !slices.Equal(got, []string{"mcp", "list"}) {
		t.Errorf("подкоманда: %q", got)
	}

	// Папка-файл вместо каталога — не передаётся.
	if err := os.RemoveAll(tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := addDirFlags(home); !slices.Equal(got, dirs[:1]) {
		t.Errorf("claude-tmp файлом: %q", got)
	}
}
