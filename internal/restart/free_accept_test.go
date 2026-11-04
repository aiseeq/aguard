package restart

import (
	"testing"
	"time"

	"github.com/aiseeq/akit/session"
)

// Приёмка queue-order-guard: признак «свободен». Написана мастером до кода, боец её не правит.
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
