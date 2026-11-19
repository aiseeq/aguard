package testutil

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const commandGuardLogEnv = "AGUARD_TEST_COMMAND_LOG"
const commandGuardDirEnv = "AGUARD_TEST_COMMAND_GUARD_DIR"
const commandGuardOriginalPathEnv = "AGUARD_TEST_COMMAND_ORIGINAL_PATH"
const commandGuardMode os.FileMode = 0700

// RunWithCommandGuard prevents package tests from reaching host observer commands.
func RunWithCommandGuard(m *testing.M, packageName string, commands ...string) int {
	dir, logPath, inherited, err := inheritedCommandGuard(commands)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: use inherited command guards: %v\n", packageName, err)
		return 2
	}
	if !inherited {
		dir, logPath, err = createCommandGuard(commands)
		if err != nil {
			return commandGuardSetupFailed(packageName, dir, "create command guards", err)
		}
	}
	code := m.Run()
	if inherited {
		return code
	}
	return finishCommandGuard(packageName, dir, logPath, code)
}

func inheritedCommandGuard(commands []string) (dir, logPath string, inherited bool, err error) {
	dir, hasDir := os.LookupEnv(commandGuardDirEnv)
	logPath, hasLog := os.LookupEnv(commandGuardLogEnv)
	if !hasDir && !hasLog {
		return "", "", false, nil
	}
	if hasDir != hasLog {
		return "", "", false, fmt.Errorf("incomplete environment")
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", "", false, fmt.Errorf("invalid directory %q: %w", dir, err)
	}
	if !info.IsDir() || logPath == "" {
		return "", "", false, fmt.Errorf("invalid inherited command guard directory %q", dir)
	}
	for _, command := range commands {
		if _, err := os.Stat(filepath.Join(dir, command)); err != nil {
			return "", "", false, fmt.Errorf("%s guard is missing: %w", command, err)
		}
	}
	path := os.Getenv("PATH")
	prefix := dir + string(os.PathListSeparator)
	if !strings.HasPrefix(path, prefix) {
		if err := os.Setenv("PATH", prefix+path); err != nil {
			return "", "", false, fmt.Errorf("restore guarded PATH: %w", err)
		}
	}
	return dir, logPath, true, nil
}

func createCommandGuard(commands []string) (dir, logPath string, err error) {
	originalPath := os.Getenv("PATH")
	dir, err = os.MkdirTemp("", "aguard-test-commands-")
	if err != nil {
		return "", "", fmt.Errorf("make guard directory: %w", err)
	}
	logPath = filepath.Join(dir, "calls.log")
	for _, command := range commands {
		shim := filepath.Join(dir, command)
		script := fmt.Sprintf("#!/bin/sh\nprintf '%%s %%s %%s\\n' \"$PPID\" \"${0##*/}\" \"$*\" >> \"$%s\"\nprintf 'aguard tests: unexpected %s invocation: %%s\\n' \"$*\" >&2\nexit 1\n", commandGuardLogEnv, command)
		if err := os.WriteFile(shim, []byte(script), commandGuardMode); err != nil {
			return dir, "", fmt.Errorf("write %s guard: %w", command, err)
		}
	}
	for _, setting := range [][2]string{
		{commandGuardLogEnv, logPath},
		{commandGuardDirEnv, dir},
		{commandGuardOriginalPathEnv, originalPath},
		{"PATH", dir + string(os.PathListSeparator) + originalPath},
	} {
		if err := os.Setenv(setting[0], setting[1]); err != nil {
			return dir, "", fmt.Errorf("set %s: %w", setting[0], err)
		}
	}
	return dir, logPath, nil
}

func finishCommandGuard(packageName, dir, logPath string, code int) int {
	if data, err := os.ReadFile(logPath); err == nil && len(data) > 0 {
		fmt.Fprintf(os.Stderr, "%s: tests invoked guarded command (%d call(s)):\n%s", packageName, strings.Count(string(data), "\n"), data)
		code = 1
	} else if err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "%s: read command test guard log: %v\n", packageName, err)
		code = 1
	}
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintf(os.Stderr, "%s: remove command test guards: %v\n", packageName, err)
		code = 1
	}
	return code
}

// LiveTestEnabled parses opt-in boolean environment variables consistently in live tests.
func LiveTestEnabled(t *testing.T, name string) bool {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		return false
	}
	enabled, err := strconv.ParseBool(value)
	if err != nil {
		t.Fatalf("parse %s=%q: %v", name, value, err)
	}
	return enabled
}

// UseOriginalCommandPath gives a live test the PATH that TestMain captured before installing guards.
func UseOriginalCommandPath(t *testing.T) {
	t.Helper()
	path, ok := os.LookupEnv(commandGuardOriginalPathEnv)
	if !ok {
		t.Fatal("original PATH for live command test is not set")
	}
	t.Setenv("PATH", path)
}

func commandGuardSetupFailed(packageName, dir, action string, cause error) int {
	fmt.Fprintf(os.Stderr, "%s: %s: %v\n", packageName, action, cause)
	if dir != "" {
		if err := os.RemoveAll(dir); err != nil {
			fmt.Fprintf(os.Stderr, "%s: remove command test guards: %v\n", packageName, err)
		}
	}
	return 2
}

// CheckCommandGuardsInstalled verifies that each command resolves to its test guard.
func CheckCommandGuardsInstalled(commands ...string) error {
	dir := os.Getenv(commandGuardDirEnv)
	if dir == "" {
		return fmt.Errorf("command test guard directory is not set")
	}
	for _, command := range commands {
		want := filepath.Join(dir, command)
		path, err := exec.LookPath(command)
		if err != nil {
			return fmt.Errorf("%s test guard is missing from PATH: %w", command, err)
		}
		if path != want {
			return fmt.Errorf("%s resolved to %q, want test guard %q", command, path, want)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s test guard: %w", command, err)
		}
		if !strings.Contains(string(data), "unexpected "+command+" invocation") {
			return fmt.Errorf("%s test guard has no diagnostic for unexpected calls", command)
		}
	}
	return nil
}
