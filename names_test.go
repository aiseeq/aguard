package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestNameFor(t *testing.T) {
	home := "/home/u"
	table := map[string]string{home: "lead", "/home/u/git": "admin-sc2", "/home/u/sc2/demu/farm/hard": "master-bench"}
	cases := map[string]string{
		"/home/u":                    "lead",
		"/home/u/git":                "admin-sc2",
		"/home/u/git/sc2demu":        "master-sc2demu",
		"/home/u/work/Shop/":         "master-shop",
		"/home/u/sc2/demu/farm/hard": "master-bench",
		"/home/u/git/sc2demu/cmd":    "",
		"/tmp":                       "",
	}
	for cwd, want := range cases {
		if got := nameFor(cwd, home, table); got != want {
			t.Errorf("nameFor(%q) = %q, want %q", cwd, got, want)
		}
	}
}

func TestFree(t *testing.T) {
	taken := map[string]bool{"master-sc2demu": true, "master-sc2demu-2": true}
	if got := free("master-sc2demu", taken); got != "master-sc2demu-3" {
		t.Errorf("free = %q", got)
	}
	if got := free("lead", taken); got != "lead" {
		t.Errorf("free = %q", got)
	}
}

func TestInteractiveAndName(t *testing.T) {
	if !interactive(nil) || !interactive([]string{"--resume", "x"}) || !interactive([]string{"привет"}) {
		t.Error("сессия должна получать флаги")
	}
	if interactive([]string{"mcp", "list"}) || interactive([]string{"-p", "q"}) {
		t.Error("подкоманда и -p — без флагов")
	}
	if !hasName([]string{"--name=x"}) || !hasName([]string{"-n", "x"}) || hasName([]string{"-c"}) {
		t.Error("hasName")
	}
}

func TestReadTableAndLive(t *testing.T) {
	dir := t.TempDir()
	tab := filepath.Join(dir, "names.tsv")
	if err := os.WriteFile(tab, []byte("# c\n~\tlead\n~/git\tadmin-sc2\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	table, err := readTable(tab, "/home/u")
	if err != nil || table["/home/u"] != "lead" || table["/home/u/git"] != "admin-sc2" {
		t.Fatalf("table = %v, err = %v", table, err)
	}
	if empty, err := readTable(filepath.Join(dir, "none"), "/home/u"); err != nil || len(empty) != 0 {
		t.Fatalf("нет файла: %v %v", empty, err)
	}
	live := `{"pid":` + strconv.Itoa(os.Getpid()) + `,"name":"master-sc2demu"}`
	dead := `{"pid":999999999,"name":"lead"}`
	_ = os.WriteFile(filepath.Join(dir, "1.json"), []byte(live), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "2.json"), []byte(dead), 0o600)
	names, err := liveNames(dir)
	if err != nil || !names["master-sc2demu"] || names["lead"] {
		t.Errorf("liveNames = %v", names)
	}
}
