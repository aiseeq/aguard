package bgtask

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
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
	stdout("22", out)
	stdout("30", "/dev/pts/3")
	start := time.Now().Add(-5 * time.Hour)
	snap := &proc.Snapshot{Procs: map[int]*proc.Proc{
		10: {Pid: 10, UID: 1000, Comm: "claude"},
		20: {Pid: 20, UID: 1000, Session: 10, SessionID: "session-new", StartAt: start.Add(time.Second)},
		21: {Pid: 21, UID: 1000, Session: 10, SessionID: "session-new", StartAt: start},
		22: {Pid: 22, UID: 1000, Session: 10, StartAt: start.Add(2 * time.Second)},
		30: {Pid: 30, UID: 1000, Session: 10, StartAt: start},
		40: {Pid: 40, UID: 0, Session: 10, StartAt: start}, // чужой uid
	}}
	got, err := Find(snap, root, 1000)
	if err != nil || len(got) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	task := got[0]
	if task.ID != "b7xi68io0" || task.Holder != 10 || !task.Start.Equal(start) || len(task.Pids) != 3 ||
		!task.Written.Equal(written) || len(task.SessionIDs) != 1 {
		t.Errorf("%+v", task)
	}
	if _, ok := task.SessionIDs["session-new"]; !ok {
		t.Errorf("session id снимка не записан: %v", task.SessionIDs)
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
	write("-home-alice/s2/subagents/agent-newagent.jsonl",
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_new","name":"Bash","input":{"command":"echo compacted","description":"После сжатия"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_new","content":"moved to the background (ID: bnew"}]},"toolUseResult":{"backgroundTaskId":"bnew","timedOutAfterMs":120000}}`)
	write("-home-alice/s2.jsonl",
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_main","name":"Bash","input":{"command":"echo main","description":"Основная беседа"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_main","content":"moved to the background (ID: bmain"}]},"toolUseResult":{"backgroundTaskId":"bmain","timedOutAfterMs":120000}}`)
	task := func(id string) *Task {
		return &Task{ID: id, Output: "/c/claude-1000/-home-alice/s1/tasks/" + id + ".output",
			SessionIDs: map[string]struct{}{"s2": {}, "": {}, "s1": {}}}
	}
	o, err := FindOrigin(projects, task("b7xi68io0"), nil)
	if err != nil || !o.Found || !o.Auto || o.Agent != "ab805f71" || o.Description != "Grep transcript for user quote" {
		t.Errorf("по таймауту у суб-агента: %+v %v", o, err)
	}
	o, err = FindOrigin(projects, task("bwait1"), nil)
	if err != nil || !o.Found || o.Auto || o.Agent != "" || o.Command != "bash wait.sh" {
		t.Errorf("нарочно фоном: %+v %v", o, err)
	}
	o, err = FindOrigin(projects, task("bnew"), nil)
	if err != nil || !o.Found || o.Agent != "newagent" || o.Description != "После сжатия" {
		t.Errorf("суб-агент новой беседы: %+v %v", o, err)
	}
	o, err = FindOrigin(projects, task("bmain"), nil)
	if err != nil || !o.Found || o.Agent != "" || o.Description != "Основная беседа" {
		t.Errorf("новая основная беседа: %+v %v", o, err)
	}
	if n := countPath(o.Paths, filepath.Join(projects, "-home-alice", "s2.jsonl")); n != 1 {
		t.Errorf("путь новой беседы повторился %d раз: %v", n, o.Paths)
	}
	if n := countPath(o.Paths, filepath.Join(projects, "-home-alice", "s1.jsonl")); n != 1 {
		t.Errorf("id из пути вывода добавлен повторно %d раз: %v", n, o.Paths)
	}
	if o, err := FindOrigin(projects, task("bnone"), nil); err != nil || o.Found {
		t.Errorf("нет вызова: %+v %v", o, err)
	}
}

func TestFindOriginIncrementalRead(t *testing.T) {
	projects := t.TempDir()
	file := filepath.Join(projects, "-home-alice", "s1.jsonl")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	cache := &OriginCache{}
	task := &Task{ID: "blater", Output: "/tmp/-home-alice/s1/tasks/blater.output"}
	prefixLine := `{"type":"user","message":{"content":[]},"padding":"` + strings.Repeat("x", 900) + `"}` + "\n"
	prefix := strings.Repeat(prefixLine, 64)
	if err := os.WriteFile(file, []byte(prefix), 0o600); err != nil {
		t.Fatal(err)
	}
	if o, err := FindOrigin(projects, task, cache); err != nil || o.Found {
		t.Fatalf("первый поиск: %+v %v", o, err)
	}
	fileOffset := cache.files[task.ID][file].offset
	if fileOffset != int64(len(prefix)) {
		t.Fatalf("смещение после префикса %d, ожидалось %d", fileOffset, len(prefix))
	}
	before, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	decoy := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_decoy","input":{"command":"echo decoy","description":"Подложка"}},{"type":"tool_result","tool_use_id":"toolu_decoy"}]},"toolUseResult":{"backgroundTaskId":"blater"}}`
	decoyLine := decoy + strings.Repeat(" ", len(prefixLine)-1-len(decoy)) + "\n"
	if len(decoyLine) != len(prefixLine) {
		t.Fatalf("строка-подложка длиной %d вместо %d", len(decoyLine), len(prefixLine))
	}
	fileHandle, err := os.OpenFile(file, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n, writeErr := fileHandle.WriteAt([]byte(decoyLine), 0); writeErr != nil || n != len(decoyLine) {
		_ = fileHandle.Close()
		t.Fatalf("запись строки-подложки: записано %d байт, err=%v", n, writeErr)
	}
	if err := fileHandle.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() {
		t.Fatalf("подложка изменила inode или размер: same=%v size=%d → %d", os.SameFile(before, after), before.Size(), after.Size())
	}
	changedPrefix := decoyLine + prefix[len(prefixLine):]
	added := `{"type":"user","message":{"content":[]}}` + "\n"
	if err := os.WriteFile(file, []byte(changedPrefix+added), 0o600); err != nil {
		t.Fatal(err)
	}
	if o, err := FindOrigin(projects, task, cache); err != nil || o.Found {
		t.Fatalf("поиск по дописанной строке без результата: %+v %v", o, err)
	}
	if got, want := cache.files[task.ID][file].offset-fileOffset, int64(len(added)); got != want {
		t.Fatalf("второй поиск продвинулся на %d байт, ожидалось %d дописанных байт", got, want)
	}
	fileOffset = cache.files[task.ID][file].offset
	result := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_later"}]},"toolUseResult":{"backgroundTaskId":"blater","timedOutAfterMs":120000}}` + "\n"
	use := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_later","input":{"command":"echo later","description":"Поздний вызов"}}]}}` + "\n"
	if err := os.WriteFile(file, []byte(changedPrefix+added+result+use), 0o600); err != nil {
		t.Fatal(err)
	}
	o, err := FindOrigin(projects, task, cache)
	if err != nil || !o.Found || o.Command != "echo later" {
		t.Fatalf("дописанный вызов: %+v %v", o, err)
	}
	if got := cache.files[task.ID][file].offset; got != fileOffset {
		t.Errorf("найденный результат передвинул смещение: %d → %d", fileOffset, got)
	}
	o2, err := FindOrigin(projects, task, cache)
	if err != nil || !o2.Found || o2.Command != o.Command || o2.Description != o.Description {
		t.Errorf("повторный поиск с тем же кешем: %+v %v; первый: %+v", o2, err, o)
	}
}

func TestFindOriginIncrementalResetsChangedFile(t *testing.T) {
	for _, mode := range []string{"truncate", "replace"} {
		t.Run(mode, func(t *testing.T) {
			projects := t.TempDir()
			file := filepath.Join(projects, "-home-alice", "s1.jsonl")
			if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
				t.Fatal(err)
			}
			cache := &OriginCache{}
			task := &Task{ID: "breset", Output: "/tmp/-home-alice/s1/tasks/breset.output"}
			if err := os.WriteFile(file, []byte(strings.Repeat("x", 500)+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := FindOrigin(projects, task, cache); err != nil {
				t.Fatal(err)
			}
			result := `{"message":{"content":[{"type":"tool_result","tool_use_id":"toolu_reset"}]},"toolUseResult":{"backgroundTaskId":"breset"}}` + "\n"
			use := `{"message":{"content":[{"type":"tool_use","id":"toolu_reset","input":{"command":"true"}}]}}` + "\n"
			content := []byte(result + use)
			if mode == "replace" {
				content = append(content, []byte(strings.Repeat(`{"type":"user","message":{"content":[]}}`+"\n", 30))...)
			}
			if mode == "truncate" {
				if err := os.WriteFile(file, content, 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				tmp := filepath.Join(filepath.Dir(file), "replacement.jsonl")
				if err := os.WriteFile(tmp, content, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(tmp, file); err != nil {
					t.Fatal(err)
				}
			}
			o, err := FindOrigin(projects, task, cache)
			if err != nil || !o.Found || o.Command != "true" {
				t.Fatalf("после %s вызов не найден с начала: %+v %v", mode, o, err)
			}
		})
	}
}

func TestFindOriginIncrementalUnterminatedResult(t *testing.T) {
	projects := t.TempDir()
	file := filepath.Join(projects, "-home-alice", "s1.jsonl")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	cache := &OriginCache{}
	task := &Task{ID: "bfinal", Output: "/tmp/-home-alice/s1/tasks/bfinal.output"}
	use := `{"message":{"content":[{"type":"tool_use","id":"toolu_final","input":{"command":"echo final","description":"Последняя строка"}}]}}` + "\n"
	result := `{"message":{"content":[{"type":"tool_result","tool_use_id":"toolu_final"}]},"toolUseResult":{"backgroundTaskId":"bfinal"}}`
	if err := os.WriteFile(file, []byte(use+result), 0o600); err != nil {
		t.Fatal(err)
	}
	o, err := FindOrigin(projects, task, cache)
	if err != nil || !o.Found || o.Command != "echo final" || o.Description != "Последняя строка" {
		t.Fatalf("результат без завершающего перевода строки: %+v %v", o, err)
	}
}

func TestOriginCacheRetainTasks(t *testing.T) {
	cache := &OriginCache{files: map[string]map[string]originPosition{
		"active": {"a.jsonl": {}},
		"gone":   {"b.jsonl": {}},
	}}
	cache.RetainTasks([]Task{{ID: "active"}})
	if len(cache.files) != 1 || cache.files["active"] == nil {
		t.Fatalf("кеш активной задачи потерян: %+v", cache.files)
	}
	if _, ok := cache.files["gone"]; ok {
		t.Fatalf("кеш завершённой задачи остался: %+v", cache.files)
	}
}

func TestFindOriginIncrementalIncompleteLine(t *testing.T) {
	projects := t.TempDir()
	file := filepath.Join(projects, "-home-alice", "s1.jsonl")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	cache := &OriginCache{}
	task := &Task{ID: "bpartial", Output: "/tmp/-home-alice/s1/tasks/bpartial.output"}
	partial := `{"message":{"content":[{"type":"tool_result","tool_use_id":"toolu_partial"}]},"toolUseResult":{"backgroundTaskId":"bpartial"}`
	if err := os.WriteFile(file, []byte(partial), 0o600); err != nil {
		t.Fatal(err)
	}
	if o, err := FindOrigin(projects, task, cache); err != nil || o.Found {
		t.Fatalf("неполная строка преждевременно найдена: %+v %v", o, err)
	}
	use := `{"message":{"content":[{"type":"tool_use","id":"toolu_partial","input":{"command":"echo complete"}}]}}` + "\n"
	if err := os.WriteFile(file, []byte(partial+"}\n"+use), 0o600); err != nil {
		t.Fatal(err)
	}
	o, err := FindOrigin(projects, task, cache)
	if err != nil || !o.Found || o.Command != "echo complete" {
		t.Fatalf("дописанная неполная строка не найдена целиком: %+v %v", o, err)
	}
}

func countPath(paths []string, want string) int {
	n := 0
	for _, path := range paths {
		if path == want {
			n++
		}
	}
	return n
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
	cfg := Default()
	cfg.Age = policy.Duration(90 * time.Minute)
	got = WaitMessage(task, &Origin{Command: "until grep -q DONE out.txt; do sleep 60; done"}, now, cfg)
	want = "aguard: фоновое ожидание b7xi68io0 «until grep -q DONE out.txt; do sleep 60; done» идёт 4 ч 30 мин, " +
		"срок — 1 ч 30 мин (bgtask.age): сними — TaskStop b7xi68io0; ждать дольше — отложи тикет: " +
		"tk wait <тикет> --until <когда>."
	if got != want {
		t.Errorf("\n got %s\nwant %s", got, want)
	}
	if got := Span(time.Hour); got != "1 ч" {
		t.Errorf("Span(time.Hour) = %q, want %q", got, "1 ч")
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
