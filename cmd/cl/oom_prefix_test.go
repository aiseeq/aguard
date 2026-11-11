package main

import (
	"errors"
	"reflect"
	"testing"
)

func TestOOMPrefixRunsCommandAfterAdjFailure(t *testing.T) {
	adjErr := errors.New("permission denied")
	executed := false
	writeErr, execErr := executeOOMPrefix([]string{"echo hi"}, func([]byte) error { return adjErr }, func(string, []string, []string) error {
		executed = true
		return nil
	}, nil)
	if !executed || !errors.Is(writeErr, adjErr) || execErr != nil {
		t.Fatalf("executed=%v write=%v exec=%v", executed, writeErr, execErr)
	}
}

func TestOOMPrefixWrapsSingleCommand(t *testing.T) {
	var adj []byte
	var path string
	var argv, env []string
	writeErr, execErr := executeOOMPrefix([]string{"echo hi"}, func(b []byte) error { adj = append([]byte(nil), b...); return nil }, func(p string, a, e []string) error { path = p; argv = a; env = e; return nil }, []string{"X=1"})
	if writeErr != nil || execErr != nil || string(adj) != "1000" || path != "/bin/bash" || !reflect.DeepEqual(argv, []string{"/bin/bash", "-c", "echo hi"}) || !reflect.DeepEqual(env, []string{"X=1"}) {
		t.Fatalf("write=%v exec=%v adj=%q path=%q argv=%q env=%q", writeErr, execErr, adj, path, argv, env)
	}
}

func TestOOMPrefixPassesMultipleArgsThrough(t *testing.T) {
	var path string
	var argv []string
	_, _ = executeOOMPrefix([]string{"/usr/bin/printf", "ok"}, func([]byte) error { return nil }, func(p string, a, e []string) error { path = p; argv = a; return nil }, nil)
	if path != "/usr/bin/printf" || !reflect.DeepEqual(argv, []string{"/usr/bin/printf", "ok"}) {
		t.Fatalf("%q %q", path, argv)
	}
}
