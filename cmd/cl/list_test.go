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
	"time"

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
	if code := runList([]string{"--timers"}, registry, &stdout, &stderr); code != 2 || stderr.Len() == 0 || stdout.Len() != 0 {
		t.Fatalf("registry error code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestRunListTimers(t *testing.T) {
	root := t.TempDir()
	registry := session.Registry{Dir: filepath.Join(root, ".claude", "sessions"), ProcRoot: filepath.Join(root, "proc")}
	started := time.Date(2026, 11, 15, 20, 0, 0, 0, time.UTC)
	writeTimerListSession(t, registry, 301, "lead", filepath.Join(root, "work"), true, "active", started)
	writeTimerListSession(t, registry, 302, "quiet", filepath.Join(root, "work"), true, "quiet", started)
	writeTimerListSession(t, registry, 303, "dead", filepath.Join(root, "work"), false, "dead", started)
	writeTimerListSession(t, registry, 304, "broken", filepath.Join(root, "work"), true, "broken", started)
	projects := filepath.Join(root, ".claude", "projects", "project")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTimerTranscript(t, filepath.Join(projects, "active.jsonl"),
		listCronUseAt(started.Add(-time.Minute), "old", "CronCreate", `{"cron":"0 7 * * *","prompt":"до старта","recurring":true}`),
		listCronResultAt(started.Add(-59*time.Second), "old", `{"id":"old","recurring":true}`),
		listCronUse("c1", "CronCreate", `{"cron":"0 8 * * *","prompt":"первые\n\t`+strings.Repeat("я", 65)+`","recurring":true}`),
		listCronResult("c1", `{"id":"j1","recurring":true}`),
		listCronUse("c2", "CronCreate", `{"cron":"0 9 * * *","prompt":"удалить","recurring":true}`),
		listCronResult("c2", `{"id":"j2","recurring":true}`),
		listCronUse("c3", "CronDelete", `{"id":"j2"}`),
		listCronResult("c3", `{"id":"j2"}`),
		listCronUse("c4", "CronCreate", `{"cron":"0 10 16 11 *","prompt":"уже сработал","recurring":false}`),
		listCronResult("c4", `{"id":"j3","recurring":false}`),
		fmt.Sprintf(`{"type":"system","subtype":"scheduled_task_fire","timestamp":%q,"taskId":"j3"}`, started.Add(time.Minute).Format(time.RFC3339Nano)),
	)
	writeTimerTranscript(t, filepath.Join(projects, "dead.jsonl"),
		listCronUse("dc", "CronCreate", `{"cron":"0 11 * * *","prompt":"мёртвый","recurring":true}`),
		listCronResult("dc", `{"id":"jd","recurring":true}`),
	)
	writeTimerTranscript(t, filepath.Join(projects, "broken.jsonl"), `{"type":"assistant","message":{"content":[{"type":"tool_use"}`)

	var stdout, stderr bytes.Buffer
	if code := runList([]string{"--timers"}, registry, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	wantPrompt := "первые  " + string([]rune(strings.Repeat("я", 65))[:52]) + "…"
	lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "broken\tошибка: ") || !strings.Contains(lines[0], "транскрипт") ||
		lines[1] != "lead\t0 8 * * *\t"+wantPrompt || stderr.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q; want transcript error and lead timer", stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := runList([]string{"--timers", "--json"}, registry, &stdout, &stderr); code != 0 {
		t.Fatalf("json code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var got []listedSession
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Name != "broken" || got[0].Timers != nil || got[0].TimersError == "" ||
		got[1].Name != "lead" || got[1].Timers == nil || len(*got[1].Timers) != 1 ||
		got[2].Name != "quiet" || got[2].Timers == nil || len(*got[2].Timers) != 0 {
		t.Fatalf("timer json sessions=%+v", got)
	}
}

func writeTimerListSession(t *testing.T, registry session.Registry, pid int, name, cwd string, alive bool, id string, started time.Time) {
	t.Helper()
	writeListSession(t, registry, pid, name, cwd, alive)
	entry := fmt.Sprintf(`{"pid":%d,"name":%q,"cwd":%q,"procStart":%q,"sessionId":%q,"startedAt":%d}`,
		pid, name, cwd, fmt.Sprintf("start-%d", pid), id, started.UnixMilli())
	path := filepath.Join(registry.Dir, fmt.Sprintf("%d.json", pid))
	if err := os.WriteFile(path, []byte(entry), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeTimerTranscript(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func listCronUse(id, name, input string) string {
	return listCronUseAt(time.Date(2026, 11, 15, 20, 1, 0, 0, time.UTC), id, name, input)
}

func listCronUseAt(at time.Time, id, name, input string) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"content":[{"type":"tool_use","id":%q,"name":%q,"input":%s}]}}`,
		at.Format(time.RFC3339Nano), id, name, input)
}

func listCronResult(id, result string) string {
	return listCronResultAt(time.Date(2026, 11, 15, 20, 1, 1, 0, time.UTC), id, result)
}

func listCronResultAt(at time.Time, id, result string) string {
	return fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"content":[{"type":"tool_result","tool_use_id":%q,"content":"ok"}]},"toolUseResult":%s}`,
		at.Format(time.RFC3339Nano), id, result)
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
