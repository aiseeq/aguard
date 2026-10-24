package main

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// idleFacts — сессия, которую можно перезапускать: старая версия, ход закончен, давно тихо, ничего не идёт.
func idleFacts() facts {
	return facts{
		session:   session{Name: "sc2go-master", Kind: "interactive", Status: "idle", SessionID: "s1"},
		Running:   "2.1.309",
		Installed: "2.1.310",
		HookState: "done",
		Quiet:     15 * time.Minute,
		TTYQuiet:  20 * time.Minute, // ввод — отправка последней реплики, до конца хода
		TurnQuiet: 15 * time.Minute,
	}
}

var leadRole = role{Names: []string{"lead"}, RestorableCrons: []string{"Сон (скилл sleep)", "Дайджест (скилл digest)"}}

func codes(v verdict) []string {
	var out []string
	for _, r := range v.Against {
		out = append(out, r.code)
	}
	return out
}

func TestDecideRestartsIdleSession(t *testing.T) {
	v := decide(idleFacts(), policy{idle: 10 * time.Minute})
	if !v.Restart || v.Fresh || len(v.Against) != 0 {
		t.Fatalf("простаивающую старую сессию перезапускать: %+v", v)
	}
	f := idleFacts()
	f.HookState = "waiting" // минутное напоминание после конца хода
	if !decide(f, policy{idle: 10 * time.Minute}).Restart {
		t.Error("waiting без идущих инструментов — тоже простой")
	}
}

func TestDecideFreshVersion(t *testing.T) {
	f := idleFacts()
	f.Running = "2.1.310"
	v := decide(f, policy{idle: 10 * time.Minute})
	if v.Restart || !v.Fresh {
		t.Errorf("свежую версию не перезапускать: %+v", v)
	}
	f.Running = "2.1.311"
	if v := decide(f, policy{}); v.Restart || !v.Fresh {
		t.Errorf("процесс новее установленной: %+v", v)
	}
}

func TestDecideAgainst(t *testing.T) {
	cases := map[string]func(*facts){
		"error":    func(f *facts) { f.Errors = []string{"нет реестра"} },
		"version":  func(f *facts) { f.Installed = "latest" },
		"kind":     func(f *facts) { f.Kind = "background" },
		"status":   func(f *facts) { f.Status = "busy" },
		"hook":     func(f *facts) { f.HookState = "working" },
		"quiet":    func(f *facts) { f.Quiet = 9 * time.Minute },
		"tty":      func(f *facts) { f.TTYQuiet = 2 * time.Minute },
		"draft":    func(f *facts) { f.TTYQuiet = 12 * time.Minute },
		"children": func(f *facts) { f.Children = []string{"42 bash"} },
		"pending":  func(f *facts) { f.Transcript.Pending = []string{"AskUserQuestion t1"} },
		"tasks":    func(f *facts) { f.Transcript.Tasks = []string{"Agent «ревью» a1"} },
		"crons":    func(f *facts) { f.Transcript.Crons = []cronJob{{ID: "j1", Prompt: "Сон (скилл sleep)"}} },
		"wakeup":   func(f *facts) { f.Transcript.Wakeups = []string{"ScheduleWakeup до 19:00"} },
	}
	for code, spoil := range cases {
		f := idleFacts()
		spoil(&f)
		v := decide(f, policy{idle: 10 * time.Minute})
		if v.Restart || !slices.Contains(codes(v), code) {
			t.Errorf("%s: %+v", code, v)
		}
	}
	if v := decide(idleFacts(), policy{idle: 10 * time.Minute, argsProblem: "свой флаг claude --bare"}); v.Restart {
		t.Error("невосстановимые аргументы держат перезапуск")
	}
	if v := decide(facts{session: session{Name: "x"}, HookState: "", Errors: nil}, policy{}); v.Restart {
		t.Error("пустые сведения — сомнение")
	}
}

func TestDecideLeadCrons(t *testing.T) {
	f := idleFacts()
	f.Name, f.HookState = "lead", "paused"
	f.Transcript.Crons = []cronJob{{ID: "j1", Prompt: "Сон (скилл sleep)", Recurring: true}, {ID: "j2", Prompt: "Дайджест (скилл digest)", Recurring: true}}
	if v := decide(f, policy{idle: 10 * time.Minute, role: leadRole}); !v.Restart {
		t.Errorf("Сон и Дайджест лид ставит заново: %+v", v)
	}
	f.Transcript.Crons = append(f.Transcript.Crons, cronJob{ID: "j3", Prompt: "замер темпа Codex"})
	if v := decide(f, policy{idle: 10 * time.Minute, role: leadRole}); v.Restart || !slices.Contains(codes(v), "crons") {
		t.Errorf("чужой таймер лида пропал бы: %+v", v)
	}
	f.Transcript.Crons = f.Transcript.Crons[:2]
	f.HookTasks = true
	if v := decide(f, policy{idle: 10 * time.Minute, role: leadRole}); v.Restart {
		t.Error("paused с фоновыми задачами на Stop — не перезапускать")
	}
	f.HookTasks, f.Transcript.Crons = false, nil
	if v := decide(f, policy{idle: 10 * time.Minute, role: leadRole}); v.Restart || !slices.Contains(codes(v), "hook-paused") {
		t.Errorf("paused без таймеров и задач — непонятно чего ждёт: %+v", v)
	}
}

func TestVerdictKeyIgnoresNumbers(t *testing.T) {
	a, b := idleFacts(), idleFacts()
	a.Quiet, b.Quiet = 3*time.Minute, 4*time.Minute
	p := policy{idle: 10 * time.Minute}
	if decide(a, p).key() != decide(b, p).key() {
		t.Error("ключ журнала не должен меняться от минут")
	}
}

func TestJournalLine(t *testing.T) {
	f := idleFacts()
	v := decide(f, policy{idle: 10 * time.Minute})
	if got := journalLine(modeLog, "x", 7, f, v); got != "sc2go-master pid=7 2.1.309→2.1.310 log: перезапустил бы: простаивает, ничего не идёт" {
		t.Errorf("journalLine = %q", got)
	}
	f.Status = "busy"
	got := journalLine(modeAct, "x", 7, f, decide(f, policy{idle: 10 * time.Minute}))
	if !strings.Contains(got, "act: не перезапускаю: Claude Code: busy") {
		t.Errorf("journalLine = %q", got)
	}
}

func TestDecideDraftWithinTTYGranularity(t *testing.T) {
	f := idleFacts()
	f.TTYQuiet = f.TurnQuiet + 5*time.Second // atime pts отстаёт до 8 с: ввод мог быть уже после конца хода
	if v := decide(f, policy{idle: 10 * time.Minute}); v.Restart || !slices.Contains(codes(v), "draft") {
		t.Errorf("ввод в пределах 8 с до конца хода — тоже возможный черновик: %+v", v)
	}
	f.TTYQuiet = f.TurnQuiet + 30*time.Second
	if v := decide(f, policy{idle: 10 * time.Minute}); !v.Restart {
		t.Errorf("ввод за 30 с до конца хода — отправка реплики, не черновик: %+v", v)
	}
}
