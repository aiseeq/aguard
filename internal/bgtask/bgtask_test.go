package bgtask

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/proc"
)

// Задача — процессы, чей stdout — файл вывода; сессия — CLAUDE_PID; прочие процессы не задачи.
func TestFind(t *testing.T) {
	root, tmp := t.TempDir(), t.TempDir()
	out := filepath.Join(tmp, "-home-alice", "8d06de04", "tasks", "b7xi68io0.output")
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	written := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(out, written, written); err != nil {
		t.Fatal(err)
	}
	stdout := func(pid, target string) {
		dir := filepath.Join(root, pid, "fd")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, "1")); err != nil {
			t.Fatal(err)
		}
	}
	stdout("20", out)
	stdout("21", out)
	stdout("30", "/dev/pts/3")
	start := time.Now().Add(-5 * time.Hour)
	snap := &proc.Snapshot{Procs: map[int]*proc.Proc{
		10: {Pid: 10, UID: 1000, Comm: "claude"},
		20: {Pid: 20, UID: 1000, Session: 10, StartAt: start.Add(time.Second)},
		21: {Pid: 21, UID: 1000, Session: 10, StartAt: start},
		30: {Pid: 30, UID: 1000, Session: 10, StartAt: start},
		40: {Pid: 40, UID: 0, Session: 10, StartAt: start}, // чужой uid
	}}
	got, err := Find(snap, root, 1000)
	if err != nil || len(got) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	task := got[0]
	if task.ID != "b7xi68io0" || task.Holder != 10 || !task.Start.Equal(start) || len(task.Pids) != 2 || !task.Written.Equal(written) {
		t.Errorf("%+v", task)
	}
	now := time.Now()
	if !task.Stale(now, time.Hour, time.Hour) || task.Stale(now, time.Hour, 3*time.Hour) || task.Stale(now, 6*time.Hour, time.Hour) {
		t.Error("застывшая: идёт 5 ч, вывод молчит 2 ч")
	}
}

// Вызов ищется в транскрипте беседы и её суб-агентов; по таймауту — timedOutAfterMs в результате.
func TestFindOrigin(t *testing.T) {
	projects := t.TempDir()
	write := func(rel string, lines ...string) {
		p := filepath.Join(projects, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("-home-alice/s1.jsonl",
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_bg","name":"Bash","input":{"command":"bash wait.sh","description":"Ждать итог","run_in_background":true}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_bg","content":"Command running in background with ID: bwait1."}]},"toolUseResult":{"backgroundTaskId":"bwait1"}}`)
	write("-home-alice/s1/subagents/agent-ab805f71.jsonl",
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_g","name":"Bash","input":{"command":"grep -o x f.jsonl","description":"Grep transcript for user quote"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_g","content":"moved to the background (ID: b7xi68io0)"}]},"toolUseResult":{"backgroundTaskId":"b7xi68io0","timedOutAfterMs":120000}}`)
	task := func(id string) *Task {
		return &Task{ID: id, Output: "/c/claude-1000/-home-alice/s1/tasks/" + id + ".output"}
	}
	o, err := FindOrigin(projects, task("b7xi68io0"))
	if err != nil || !o.Found || !o.Auto || o.Agent != "ab805f71" || o.Description != "Grep transcript for user quote" {
		t.Errorf("по таймауту у суб-агента: %+v %v", o, err)
	}
	o, err = FindOrigin(projects, task("bwait1"))
	if err != nil || !o.Found || o.Auto || o.Agent != "" || o.Command != "bash wait.sh" {
		t.Errorf("нарочно фоном: %+v %v", o, err)
	}
	if o, err := FindOrigin(projects, task("bnone")); err != nil || o.Found {
		t.Errorf("нет вызова: %+v %v", o, err)
	}
}

func TestMessage(t *testing.T) {
	now := time.Date(2026, 10, 27, 11, 0, 0, 0, time.UTC)
	task := &Task{ID: "b7xi68io0", Start: now.Add(-(4*time.Hour + 30*time.Minute)), Written: now.Add(-65 * time.Minute)}
	got := Message(task, &Origin{Description: "Grep transcript for user quote", Agent: "ab805f71"}, now)
	want := "aguard: фоновая задача b7xi68io0 «Grep transcript for user quote» суб-агента ab805f71 идёт 4 ч 30 мин, " +
		"вывод не меняется 1 ч 05 мин; это обычная команда, которую Claude Code увёл в фон по таймауту. Нужна ли? " +
		"Если нет — TaskStop b7xi68io0."
	if got != want {
		t.Errorf("\n got %s\nwant %s", got, want)
	}
	if strings.Contains(got, "\n") {
		t.Error("письмо — одной строкой")
	}
	got = WaitMessage(task, &Origin{Command: "until grep -q DONE out.txt; do sleep 60; done"}, now)
	want = "aguard: фоновое ожидание b7xi68io0 «until grep -q DONE out.txt; do sleep 60; done» идёт 4 ч 30 мин, срок по " +
		"правилу — час: проверь, нужен ли новый срок, или сними — TaskStop b7xi68io0."
	if got != want {
		t.Errorf("\n got %s\nwant %s", got, want)
	}
}

// Ожидание или прогон — по команде, а запуск скрипта — по его тексту (примеры — из замера 20–27.10).
func TestWaitHow(t *testing.T) {
	home := t.TempDir()
	write := func(name, text string) {
		if err := os.WriteFile(filepath.Join(home, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("series.sh", "while pgrep -f 'farm lane' >/dev/null; do\n  sleep 60\ndone\necho done\n")
	write("base-full.sh", "cd ~/sc2/demu && graft test\n")
	for cmd, want := range map[string]string{
		"until grep -q DONE /x/out.txt; do sleep 20; done; date":                       "loop",
		"worker wait 1023-1636-move-late --timeout 60m":                                "script",
		"bash ~/series.sh > /x/series.log 2>&1":                                        "loop",
		"bash ~/wait-series.sh":                                                        "script",
		"bash " + filepath.Join(home, "series.sh"):                                     "loop",
		"bash ~/base-full.sh 3146e1b > /x/base.log":                                    "",
		"bash ~/missing.sh":                                                            "",
		"cat /proc/loadavg && ~/.claude/improve/run.sh step evals > /x/evals.log 2>&1": "",
		"cd ~/work/portal && graft test-unit > /x/unit.log":                            "",
	} {
		if got, err := WaitHow(cmd, home); err != nil || got != want {
			t.Errorf("%s: %q %v, нужно %q", cmd, got, err, want)
		}
	}
}

// Состояние хранит только живые задачи.
func TestState(t *testing.T) {
	file := filepath.Join(t.TempDir(), "state.json")
	st, err := LoadState(file)
	if err != nil || len(st) != 0 {
		t.Fatal(st, err)
	}
	st["a"], st["b"] = Seen{Auto: true}, Seen{}
	if err := st.Save(file, []Task{{ID: "a"}}); err != nil {
		t.Fatal(err)
	}
	st, err = LoadState(file)
	if err != nil || len(st) != 1 || !st["a"].Auto {
		t.Errorf("%+v %v", st, err)
	}
}

func TestValidate(t *testing.T) {
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, spoil := range map[string]func(*Config){
		"режим": func(c *Config) { c.Mode = "kill" },
		"age":   func(c *Config) { c.Age = 0 },
		"quiet": func(c *Config) { c.Quiet = 0 },
	} {
		c := Default()
		spoil(&c)
		if c.Validate() == nil {
			t.Errorf("%s: прошло", name)
		}
	}
}
