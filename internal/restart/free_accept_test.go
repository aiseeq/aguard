package restart

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiseeq/akit/session"
)

// Приёмка queue-order-guard: признак «свободен». Написана мастером до кода, боец её не правит.
func TestFreeFreshConsoleNoTranscript(t *testing.T) {
	now := time.Now()
	w := newSessionWorld(t, worldTimes{now: now, turnAgo: 10 * time.Minute, ttyAgo: 10 * time.Minute})
	w.link(t, 4242)
	path := filepath.Join(w.p.projects, "-home-u-git-x", "s1.jsonl")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	j := NewJudge(w.dir)
	j.paths = &w.p
	if ok, why := j.Free(&Config{}, 4242, 2*time.Minute, now); !ok {
		t.Fatalf("нет транскрипта: %s", why)
	}
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatal(err)
	}
	if ok, _ := j.Free(&Config{}, 4242, 2*time.Minute, now); ok {
		t.Fatal("ошибка чтения принята за простой")
	}
}

// Будильник: фоновая задача или суб-агент держат мастера занятым, монитор артефакта и ожидание команды — как задумано.
func TestWakeFree(t *testing.T) {
	now := time.Now()
	const after = 2 * time.Minute
	cases := []struct {
		name, tasks string
		want        bool
	}{
		{"без задач", "", true},
		{"фоновая задача", `{"tasks":[{"type":"bash","description":"сборка"}]}`, false},
		{"суб-агент", `{"tasks":[{"type":"subagent","description":"проверка"}]}`, false},
		{"монитор артефакта", `{"tasks":[{"type":"monitor","description":"live updates for artifact x (auto-armed on publish)"}]}`, true},
	}
	for _, c := range cases {
		w := newSessionWorld(t, worldTimes{now: now, turnAgo: 10 * time.Minute, ttyAgo: 10 * time.Minute})
		w.link(t, 4242)
		if c.tasks != "" {
			write(t, filepath.Join(w.p.hookStates, "s1.tasks"), c.tasks)
		}
		j := NewJudge(w.dir)
		j.paths = &w.p
		got, err := j.WakeFree(&Config{}, 4242, after, now)
		if err != nil || got != c.want {
			t.Errorf("%s: %v %v", c.name, got, err)
		}
	}
}

// worker wait на переднем плане — свобода для перезапуска, но не для будильника: мастер в ходе.
func TestWakeFreeForegroundWaitIsBusy(t *testing.T) {
	now := time.Now()
	const after = 2 * time.Minute
	ts := now.Add(-20 * time.Minute).UTC().Format("2006-01-02T15:04:05.000Z")
	w := newSessionWorld(t, worldTimes{now: now, turnAgo: 10 * time.Minute, ttyAgo: 10 * time.Minute}, use(ts, "1", "Bash", `{"command":"worker wait j --timeout 9m"}`))
	w.link(t, 4242)
	write(t, filepath.Join(w.p.hookStates, "s1"), "working\n")
	j := NewJudge(w.dir)
	j.paths = &w.p
	if ok, why := j.Free(&Config{}, 4242, after, now); !ok || why != "ожидание команды" {
		t.Fatalf("предусловие: %v %s", ok, why)
	}
	if got, err := j.WakeFree(&Config{}, 4242, after, now); err != nil || got {
		t.Fatal(got, err)
	}
}

func TestFree(t *testing.T) {
	const m2 = 2 * time.Minute
	base := func() facts {
		return facts{
			Info:      session.Info{Kind: "interactive", Status: "idle"},
			HookState: "done", TurnQuiet: 3 * time.Minute, TTYQuiet: 3 * time.Minute,
		}
	}
	cases := []struct {
		name string
		mod  func(*facts)
		want bool
	}{
		{"ход кончен 3 мин, человек молчит", func(*facts) {}, true},
		{"вкладку открыли, idle", func(f *facts) { f.HookState = "idle" }, true},
		{"ход кончен 1 мин", func(f *facts) { f.TurnQuiet = time.Minute }, false},
		{"человек вводил 1 мин назад", func(f *facts) { f.TTYQuiet = time.Minute }, false},
		{"идёт ход", func(f *facts) { f.HookState = "working"; f.Status = "busy" }, false},
		{"вопрос человеку", func(f *facts) { f.HookState = "waiting" }, false},
		{"ход оборван ошибкой", func(f *facts) { f.HookState = "error" }, false},
		{"пауза с фоновыми задачами: бойцы не занятость", func(f *facts) { f.HookState = "paused"; f.HookTasks = true }, true},
		{"пауза с суб-агентами: не занятость", func(f *facts) { f.HookState = "paused"; f.HookTasks, f.HookAgents = true, true }, true},
		{"дети процесса не занятость", func(f *facts) { f.Children = []string{"worker"} }, true},
		{"фоновые задачи в транскрипте не занятость", func(f *facts) { f.Transcript.Tasks = []string{"a"} }, true},
		{"на переднем плане worker wait: не занятость", func(f *facts) {
			f.HookState, f.Status = "working", "busy"
			f.Transcript.Pending, f.Transcript.PendingWaits = []string{"Bash 1"}, 1
		}, true},
		{"на переднем плане wait, транскрипт живой", func(f *facts) {
			f.HookState, f.Status, f.TurnQuiet = "working", "busy", time.Minute
			f.Transcript.Pending, f.Transcript.PendingWaits = []string{"Bash 1"}, 1
		}, false},
		{"рядом с wait идёт сборка", func(f *facts) {
			f.HookState, f.Status = "working", "busy"
			f.Transcript.Pending, f.Transcript.PendingWaits = []string{"Bash 1", "Bash 2"}, 1
		}, false},
		{"инструмент без результата при done: ждёт разрешения", func(f *facts) { f.Transcript.Pending = []string{"Bash 1"} }, false},
		{"нет записи хука, запросы были", func(f *facts) { f.HookState = ""; f.Transcript.Context.Request = time.Now() }, false},
		{"нет записи хука, запросов не было", func(f *facts) { f.HookState = "" }, true},
		{"не интерактивная", func(f *facts) { f.Kind = "print" }, false},
		{"что-то не прочиталось", func(f *facts) { f.Errors = []string{"x"} }, false},
	}
	for _, c := range cases {
		f := base()
		c.mod(&f)
		if got, why := free(f, m2); got != c.want {
			t.Errorf("%s: %v (%s)", c.name, got, why)
		}
	}
}
