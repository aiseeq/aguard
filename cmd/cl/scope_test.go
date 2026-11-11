package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aiseeq/aguard/internal/ceiling"
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

func TestMemoryScopeArgvAndEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	base, _ := ceiling.ParseSize("12G", 0)
	swap, _ := ceiling.ParseSize("256M", 0)
	disabled := scopeArgv("systemd-run", "claude", "sess", "123", nil)
	if slices.Contains(disabled, "OOMPolicy=continue") || !slices.Equal(scopeEnv([]string{"PATH=/bin"}, false, "unused"), []string{"PATH=/bin"}) {
		t.Fatalf("disabled scope changed: argv=%q env=%q", disabled, scopeEnv([]string{"PATH=/bin"}, false, "unused"))
	}
	if _, err := os.Stat(ceiling.StatePath(home, "agent-sess-123.scope")); !os.IsNotExist(err) {
		t.Fatalf("disabled scope wrote state in HOME: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(home, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	prefix := filepath.Join(home, "bin", "cl-oom-prefix")
	if err := os.WriteFile(prefix, []byte("stub"), 0700); err != nil {
		t.Fatal(err)
	}
	argv, env := prepareScope(home, "systemd-run", "claude", "sess", "123", nil, memoryConfig{Max: "12G", SwapMax: "256M"}, []string{"PATH=/bin"}, nil)
	for _, want := range []string{"--property=OOMPolicy=continue", fmt.Sprintf("--property=MemoryMax=%d", base), fmt.Sprintf("--property=MemorySwapMax=%d", swap)} {
		if !slices.Contains(argv, want) {
			t.Errorf("argv missing %q: %q", want, argv)
		}
	}
	if !slices.Equal(env, []string{"PATH=/bin", "CLAUDE_CODE_SHELL_PREFIX=" + prefix}) {
		t.Fatalf("enabled env: %q", env)
	}
	state, err := ceiling.Read(ceiling.StatePath(home, scopeUnit("sess", "123")))
	if err != nil || state.By != "cl" || state.Base != base || state.Swap != swap {
		t.Fatalf("temporary state %+v err=%v", state, err)
	}
}

func TestPrepareScopeDisablesLimitWhenStateWriteFails(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "bin", "cl-oom-prefix"), []byte("stub"), 0700); err != nil {
		t.Fatal(err)
	}
	old := writeCeilingState
	writeCeilingState = func(string, ceiling.State) error { return os.ErrPermission }
	t.Cleanup(func() { writeCeilingState = old })
	var logged error
	argv, env := prepareScope(home, "systemd-run", "claude", "sess", "123", nil, memoryConfig{Max: "12G"}, []string{"PATH=/bin"}, func(err error) { logged = err })
	for _, arg := range argv {
		if strings.Contains(arg, "MemoryMax=") || strings.Contains(arg, "OOMPolicy=") {
			t.Fatalf("limit remained after failed state write: %q", argv)
		}
	}
	if len(env) != 1 || logged == nil {
		t.Fatalf("env=%q logged=%v", env, logged)
	}
}

func TestScopeUnitAndProcessIDVariants(t *testing.T) {
	if got := scopeUnit("", "123-c2"); got != "agent-claude-123-c2.scope" {
		t.Fatalf("scope unit with empty name: %q", got)
	}
	for id, want := range map[string]int{"123-r4": 123, "456-c2": 456} {
		got, err := scopeProcessID(id)
		if err != nil || got != want {
			t.Fatalf("scopeProcessID(%q)=%d err=%v", id, got, err)
		}
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
