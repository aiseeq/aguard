package main

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestScopeArgv(t *testing.T) {
	got := scopeArgv("/usr/bin/systemd-run", "/home/u/.local/bin/claude", "llmark-master", "4242", []string{"-n", "llmark-master", "--resume"})
	want := []string{"/usr/bin/systemd-run", "--user", "--scope", "--quiet", "--collect", "--slice=agents.slice",
		"--property=IOAccounting=yes", "--unit=agent-llmark-master-4242", "--", "/home/u/.local/bin/claude", "-n", "llmark-master", "--resume"}
	if !slices.Equal(got, want) {
		t.Errorf("scopeArgv = %q\nwant %q", got, want)
	}
	if got := scopeArgv("sr", "c", "", "7", []string{"mcp", "list"}); got[7] != "--unit=agent-claude-7" {
		t.Errorf("без имени: %q", got[7])
	}
}

func TestUnitSafe(t *testing.T) {
	if got := unitSafe("мастер sc2/demu:1"); got != "_______sc2_demu_1" {
		t.Errorf("unitSafe = %q", got)
	}
}

func TestNameArg(t *testing.T) {
	cases := map[string][]string{
		"lead":   {"-n", "lead"},
		"x":      {"--resume", "--name=x"},
		"y":      {"--name", "y", "привет"},
		"":       {"-n"},
		"absent": nil,
	}
	for want, args := range cases {
		if want == "absent" {
			want = ""
		}
		if got := nameArg(args); got != want {
			t.Errorf("nameArg(%q) = %q, want %q", args, got, want)
		}
	}
}

func TestSliceInstalled(t *testing.T) {
	dir := t.TempDir()
	if err := sliceInstalled(filepath.Join(dir, "agents.slice")); err == nil {
		t.Error("нет файла — нужна ошибка")
	}
	if err := sliceInstalled("agents.slice"); err != nil {
		t.Errorf("файл репо: %v", err)
	}
}
