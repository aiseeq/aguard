package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aiseeq/aguard/internal/ceiling"
)

// копия scopeArgv из HEAD (git show HEAD:cmd/cl/scope.go)
func oldScopeArgv(systemdRun, claude, name, id string, args []string) []string {
	if name == "" {
		name = "claude"
	}
	unit := "agent-" + unitSafe(name) + "-" + unitSafe(id)
	argv := []string{systemdRun, "--user", "--scope", "--quiet", "--collect", "--slice=" + agentsSlice, "--property=IOAccounting=yes", "--unit=" + unit, "--", claude}
	return append(argv, args...)
}

func countFiles(t *testing.T, dir string) int {
	n := 0
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			n++
		}
		return nil
	})
	return n
}

func TestReviewDisabledByteForByte(t *testing.T) {
	home := t.TempDir()
	cfgDir := t.TempDir()
	env := []string{"PATH=/bin", "CLAUDE_CODE_SHELL_PREFIX=/x", "HOME=/h"}
	cases := map[string]string{
		"nofile": "",
		"broken": "{not json",
		"nomem":  `{"restart":{"mode":"log"}}`,
		"empty":  `{"memory":{"max":"","swap_max":"1G"},"restart":{"mode":"log"}}`,
		"badmax": `{"memory":{"max":"zzz"},"restart":{"mode":"log"}}`,
	}
	for label, body := range cases {
		file := filepath.Join(cfgDir, label+".json")
		if body != "" {
			if err := os.WriteFile(file, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
		}
		cfg, _ := loadConfig(file, home)
		for _, c := range []struct {
			name, id string
			args     []string
		}{{"llmark-master", "4242", []string{"-n", "x", "--resume"}}, {"", "7", []string{"mcp", "list"}}, {"мастер sc2", "9-r3", nil}} {
			called := false
			argv, gotEnv := prepareScope(home, "/usr/bin/systemd-run", "/c/claude", c.name, c.id, c.args, cfg.Memory, env, func(error) { called = true })
			want := oldScopeArgv("/usr/bin/systemd-run", "/c/claude", c.name, c.id, c.args)
			if !slices.Equal(argv, want) || !slices.Equal(gotEnv, env) || called {
				t.Errorf("%s %q: argv %q want %q env %q logged=%v", label, c.name, argv, want, gotEnv, called)
			}
		}
	}
	if n := countFiles(t, home); n != 0 {
		t.Fatalf("disabled path wrote %d files into home", n)
	}
}

// supervise.start/compactSession строят имя unit для updateScopeClaudePID без умолчания "claude".
func TestReviewEmptyNameUnitMismatch(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "bin", "cl-oom-prefix"), []byte("x"), 0700); err != nil {
		t.Fatal(err)
	}
	_, _ = prepareScope(home, "sr", "c", "", "123", nil, ceiling.ClMemory{Max: "1G"}, nil, func(e error) { t.Fatal(e) })
	if _, err := os.Stat(ceiling.StatePath(home, "agent-claude-123.scope")); err != nil {
		t.Fatalf("state for prepareScope: %v", err)
	}
	err := updateScopeClaudePID(home, scopeUnit("", "123"), 999)
	t.Logf("updateScopeClaudePID with supervise unit name: err=%v", err)
	if err != nil {
		t.Fatal("mismatch remains")
	}
}

// compaction id "<pid>-c<n>" — потолок не ставится.
func TestReviewCompactionScopeSkipped(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "bin", "cl-oom-prefix"), []byte("x"), 0700); err != nil {
		t.Fatal(err)
	}
	var logged error
	argv, _ := prepareScope(home, "sr", "c", "x-master", "123-c1", nil, ceiling.ClMemory{Max: "1G"}, nil, func(e error) { logged = e })
	t.Logf("compaction: logged=%v argv=%q", logged, argv)
	if logged != nil || !strings.Contains(strings.Join(argv, " "), "MemoryMax=") {
		t.Fatal("compaction still skipped")
	}
}
