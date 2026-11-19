package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/aiseeq/aguard/internal/testutil"
)

func TestMain(m *testing.M) {
	os.Exit(testutil.RunWithCommandGuard(m, "cmd/aguard", "journalctl", "systemctl", "systemd-run", "ps"))
}

func TestJournalctlGuardInstalled(t *testing.T) {
	if err := testutil.CheckCommandGuardsInstalled("journalctl", "systemctl", "systemd-run", "ps"); err != nil {
		t.Fatal(err)
	}
}

func TestCommandGuardCatchesGrandchild(t *testing.T) {
	const parentProbe = "AGUARD_TEST_COMMAND_PARENT_PROBE"
	child := exec.Command(os.Args[0], "-test.run=^TestCommandGuardParentProcessProbe$")
	child.Env = withoutCommandGuardEnv(os.Environ())
	child.Env = append(child.Env, parentProbe+"=1")
	out, err := child.CombinedOutput()
	if err == nil {
		t.Fatalf("supervisor package passed despite grandchild journalctl call:\n%s", out)
	}
	if !strings.Contains(string(out), "tests invoked guarded command") || !strings.Contains(string(out), "journalctl --guard-child-probe") {
		t.Fatalf("supervisor failure did not identify guarded call: %v\n%s", err, out)
	}
}

func TestCommandGuardParentProcessProbe(t *testing.T) {
	if os.Getenv("AGUARD_TEST_COMMAND_PARENT_PROBE") != "1" {
		t.Skip("subprocess-only guard probe")
	}
	child := exec.Command(os.Args[0], "-test.run=^TestCommandGuardChildJournalctlProbe$")
	child.Env = append(os.Environ(), "AGUARD_TEST_COMMAND_CHILD_PROBE=1")
	if out, err := child.CombinedOutput(); err != nil {
		t.Fatalf("guarded child test failed unexpectedly: %v\n%s", err, out)
	}
}

func TestCommandGuardChildJournalctlProbe(t *testing.T) {
	if os.Getenv("AGUARD_TEST_COMMAND_CHILD_PROBE") != "1" {
		t.Skip("subprocess-only guard probe")
	}
	if err := exec.Command("journalctl", "--guard-child-probe").Run(); err == nil {
		t.Fatal("guarded journalctl unexpectedly succeeded")
	}
}

func withoutCommandGuardEnv(env []string) []string {
	const prefix = "AGUARD_TEST_COMMAND_"
	filtered := make([]string, 0, len(env)+1)
	for _, value := range env {
		key, _, _ := strings.Cut(value, "=")
		if strings.HasPrefix(key, prefix) {
			continue
		}
		filtered = append(filtered, value)
	}
	return filtered
}
