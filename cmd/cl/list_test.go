package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/akit/proc"
	"github.com/aiseeq/akit/session"
)

func TestRunListUnderFiltersLiveSessionsByPathComponent(t *testing.T) {
	root := t.TempDir()
	registry := session.Registry{Dir: filepath.Join(root, "sessions"), ProcRoot: filepath.Join(root, "proc")}
	work := filepath.Join(root, "work")
	writeListSession(t, registry, 101, "live", filepath.Join(work, "x"), true)
	writeListSession(t, registry, 102, "sibling", filepath.Join(root, "work2"), true)
	writeListSession(t, registry, 103, "dead", filepath.Join(work, "y"), false)

	var stdout, stderr bytes.Buffer
	code := runList([]string{"--under", work + string(filepath.Separator)}, registry, &stdout, &stderr)
	want := fmt.Sprintf("live\t101\t%s\n", filepath.Join(work, "x"))
	if code != 0 || stdout.String() != want || stderr.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q; want stdout=%q", code, stdout.String(), stderr.String(), want)
	}
}

func TestRunListRelativeUnder(t *testing.T) {
	root := t.TempDir()
	registry := session.Registry{Dir: filepath.Join(root, "sessions"), ProcRoot: filepath.Join(root, "proc")}
	work := filepath.Join(root, "work")
	writeListSession(t, registry, 111, "relative", filepath.Join(work, "x"), true)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	under, err := filepath.Rel(cwd, work)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runList([]string{"--under", under}, registry, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "relative\t111\t") || stderr.Len() != 0 {
		t.Fatalf("relative under code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := runList([]string{"--under", ""}, registry, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), listUsage) || stdout.Len() != 0 {
		t.Fatalf("empty under code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestRunListJSONAndEmpty(t *testing.T) {
	root := t.TempDir()
	registry := session.Registry{Dir: filepath.Join(root, "sessions"), ProcRoot: filepath.Join(root, "proc")}
	writeListSession(t, registry, 202, "alpha", root, true)
	writeListSession(t, registry, 201, "alpha", root, true)
	writeListSession(t, registry, 203, "zeta", root, true)
	var stdout, stderr bytes.Buffer
	if code := runList([]string{"--json"}, registry, &stdout, &stderr); code != 0 {
		t.Fatalf("json code=%d stderr=%q", code, stderr.String())
	}
	var got []listedSession
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Name != "alpha" || got[0].Pid != 201 || got[1].Name != "alpha" || got[1].Pid != 202 || got[2].Name != "zeta" {
		t.Fatalf("json sessions=%+v", got)
	}
	stdout.Reset()
	stderr.Reset()
	if code := runList([]string{"--under", filepath.Join(root, "absent"), "--json"}, registry, &stdout, &stderr); code != 0 || stdout.String() != "[]\n" || stderr.Len() != 0 {
		t.Fatalf("empty code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestRunListErrors(t *testing.T) {
	root := t.TempDir()
	registry := session.Registry{Dir: filepath.Join(root, "missing"), ProcRoot: filepath.Join(root, "proc")}
	var stdout, stderr bytes.Buffer
	if code := runList([]string{"--unknown"}, registry, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), listUsage) {
		t.Fatalf("unknown code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if err := os.WriteFile(registry.Dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := runList(nil, registry, &stdout, &stderr); code != 2 || stderr.Len() == 0 || stdout.Len() != 0 {
		t.Fatalf("registry error code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestRunListPrintsReadableRecordsWhenOneIsMalformed(t *testing.T) {
	root := t.TempDir()
	registry := session.Registry{Dir: filepath.Join(root, "sessions"), ProcRoot: filepath.Join(root, "proc")}
	writeListSession(t, registry, 121, "readable", filepath.Join(root, "project"), true)
	if err := os.WriteFile(filepath.Join(registry.Dir, "122.json"), []byte(`{"pid":`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runList(nil, registry, &stdout, &stderr)
	want := fmt.Sprintf("readable\t121\t%s\n", filepath.Join(root, "project"))
	if code != 0 || stdout.String() != want || stderr.Len() == 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q; want stdout=%q and an error", code, stdout.String(), stderr.String(), want)
	}
}

func TestMainListCommandRoutesEarly(t *testing.T) {
	home := t.TempDir()
	stat, err := proc.ReadStat("/proc", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(home, "project")
	if err := os.MkdirAll(filepath.Join(home, ".claude", "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	entry := fmt.Sprintf(`{"pid":%d,"name":"test-session","cwd":%q,"procStart":%q}`, os.Getpid(), cwd, stat.Start)
	if err := os.WriteFile(filepath.Join(home, ".claude", "sessions", fmt.Sprintf("%d.json", os.Getpid())), []byte(entry), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "--cl-ls", "--json")
	cmd.Env = []string{"CL_TEST_RUN_MAIN=1", "HOME=" + home, "PATH=" + home}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("cl --cl-ls --json: %v; stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	var got []listedSession
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout %q: %v", stdout.String(), err)
	}
	if len(got) != 1 || got[0].Name != "test-session" || got[0].Pid != os.Getpid() || got[0].Cwd != cwd || stderr.Len() != 0 {
		t.Fatalf("sessions=%+v stdout=%q stderr=%q", got, stdout.String(), stderr.String())
	}
}

func writeListSession(t *testing.T, registry session.Registry, pid int, name, cwd string, alive bool) {
	t.Helper()
	if err := os.MkdirAll(registry.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(registry.Dir, fmt.Sprintf("%d.json", pid)), fmt.Appendf(nil,
		`{"pid":%d,"name":%q,"cwd":%q,"procStart":"start-%d"}`, pid, name, cwd, pid), 0o600); err != nil {
		t.Fatal(err)
	}
	if !alive {
		return
	}
	procDir := filepath.Join(registry.ProcRoot, fmt.Sprint(pid))
	if err := os.MkdirAll(procDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fields := []string{"S", "1"}
	for len(fields) < 19 {
		fields = append(fields, "0")
	}
	fields = append(fields, fmt.Sprintf("start-%d", pid))
	stat := fmt.Sprintf("%d (test) %s\n", pid, strings.Join(fields, " "))
	if err := os.WriteFile(filepath.Join(procDir, "stat"), []byte(stat), 0o600); err != nil {
		t.Fatal(err)
	}
}
