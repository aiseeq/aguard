package restart

import (
	"slices"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/screen"
	"github.com/aiseeq/akit/session"
)

// idleFacts — сессия, которую можно перезапускать: старая версия, ход закончен, давно тихо, ничего не идёт.
func idleFacts() facts {
	return facts{
		Info:      session.Info{Name: "sc2go-master", Kind: "interactive", Status: "idle", SessionID: "s1"},
		Running:   "2.1.309",
		Installed: "2.1.310",
		HookState: "done",
		Quiet:     15 * time.Minute,
		TTYQuiet:  20 * time.Minute, // ввод — отправка последней реплики, до конца хода
		TurnQuiet: 15 * time.Minute,
		Prompt:    screen.Empty,
	}
}

var leadRole = Role{Names: []string{"lead"}, RestorableCrons: []string{"Сон (скилл sleep)", "Дайджест (скилл digest)"}}

func codes(v Verdict) []string {
	var out []string
	for _, r := range v.Against {
		out = append(out, r.Code)
	}
	return out
}

func TestDecideRestartsIdleSession(t *testing.T) {
	v := decide(idleFacts(), allowance{idle: 10 * time.Minute})
	if !v.Restart || v.Fresh || len(v.Against) != 0 {
		t.Fatalf("простаивающую старую сессию перезапускать: %+v", v)
	}
	f := idleFacts()
	f.HookState = "waiting" // минутное напоминание после конца хода
	if !decide(f, allowance{idle: 10 * time.Minute}).Restart {
		t.Error("waiting без идущих инструментов — тоже простой")
	}
}

func TestDecideFreshVersion(t *testing.T) {
	f := idleFacts()
	f.Running = "2.1.310"
	v := decide(f, allowance{idle: 10 * time.Minute})
	if v.Restart || !v.Fresh {
		t.Errorf("свежую версию не перезапускать: %+v", v)
	}
	f.Running = "2.1.311"
	if v := decide(f, allowance{}); v.Restart || !v.Fresh {
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
		v := decide(f, allowance{idle: 10 * time.Minute})
		if v.Restart || !slices.Contains(codes(v), code) {
			t.Errorf("%s: %+v", code, v)
		}
	}
	if v := decide(idleFacts(), allowance{idle: 10 * time.Minute, argsProblem: "свой флаг claude --bare"}); v.Restart {
		t.Error("невосстановимые аргументы держат перезапуск")
	}
	if v := decide(facts{Info: session.Info{Name: "x"}, HookState: "", Errors: nil}, allowance{}); v.Restart {
		t.Error("пустые сведения — сомнение")
	}
}

func TestDecideLeadCrons(t *testing.T) {
	f := idleFacts()
	f.Name, f.HookState = "lead", "paused"
	f.Transcript.Crons = []cronJob{{ID: "j1", Prompt: "Сон (скилл sleep)", Recurring: true}, {ID: "j2", Prompt: "Дайджест (скилл digest)", Recurring: true}}
	if v := decide(f, allowance{idle: 10 * time.Minute, role: leadRole}); !v.Restart {
		t.Errorf("Сон и Дайджест лид ставит заново: %+v", v)
	}
	f.Transcript.Crons = append(f.Transcript.Crons, cronJob{ID: "j3", Prompt: "замер темпа Codex"})
	if v := decide(f, allowance{idle: 10 * time.Minute, role: leadRole}); v.Restart || !slices.Contains(codes(v), "crons") {
		t.Errorf("чужой таймер лида пропал бы: %+v", v)
	}
	f.Transcript.Crons = f.Transcript.Crons[:2]
	f.HookTasks = true
	if v := decide(f, allowance{idle: 10 * time.Minute, role: leadRole}); v.Restart {
		t.Error("paused с фоновыми задачами на Stop — не перезапускать")
	}
	f.HookTasks, f.Transcript.Crons = false, nil
	if v := decide(f, allowance{idle: 10 * time.Minute, role: leadRole}); v.Restart || !slices.Contains(codes(v), "hook-paused") {
		t.Errorf("paused без таймеров и задач — непонятно чего ждёт: %+v", v)
	}
}

func TestVerdictKeyIgnoresNumbers(t *testing.T) {
	a, b := idleFacts(), idleFacts()
	a.Quiet, b.Quiet = 3*time.Minute, 4*time.Minute
	p := allowance{idle: 10 * time.Minute}
	if decide(a, p).Key() != decide(b, p).Key() {
		t.Error("ключ журнала не должен меняться от минут")
	}
}

func TestDecideDraftWithinTTYGranularity(t *testing.T) {
	f := idleFacts()
	f.TTYQuiet = f.TurnQuiet + 5*time.Second // atime pts отстаёт до 8 с: ввод мог быть уже после конца хода
	if v := decide(f, allowance{idle: 10 * time.Minute}); v.Restart || !slices.Contains(codes(v), "draft") {
		t.Errorf("ввод в пределах 8 с до конца хода — тоже возможный черновик: %+v", v)
	}
	f.TTYQuiet = f.TurnQuiet + 30*time.Second
	if v := decide(f, allowance{idle: 10 * time.Minute}); !v.Restart {
		t.Errorf("ввод за 30 с до конца хода — отправка реплики, не черновик: %+v", v)
	}
}

// Черновик набран во время хода: ввод в терминал старше конца хода, проверка draft его не видит — видит экран.
func TestDecideScreen(t *testing.T) {
	for name, spoil := range map[string]func(*facts){
		"черновик во время хода": func(f *facts) { f.Prompt = screen.Draft },
		"рамки не видно":         func(f *facts) { f.Prompt = screen.Unknown },
		"экран не прочитался":    func(f *facts) { f.Prompt, f.PromptErr = screen.Unknown, "терминал не Konsole и не tmux" },
	} {
		f := idleFacts()
		spoil(&f)
		if v := decide(f, allowance{idle: 10 * time.Minute}); v.Restart || !slices.Equal(codes(v), []string{"screen"}) {
			t.Errorf("%s: %+v", name, v)
		}
	}
}
