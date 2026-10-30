package restart

import (
	"slices"
	"strings"
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
	f.HookState = "idle" // человек открыл вкладку после конца хода
	if !decide(f, allowance{idle: 10 * time.Minute}).Restart {
		t.Error("idle — тоже простой")
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
		"hook":     func(f *facts) { f.HookState = "waiting" }, // вопрос человеку
		"quiet":    func(f *facts) { f.Quiet = 9 * time.Minute },
		"tty":      func(f *facts) { f.TTYQuiet = 2 * time.Minute },
		"draft":    func(f *facts) { f.TTYQuiet, f.Prompt = 12*time.Minute, screen.Unknown },
		"children": func(f *facts) { f.Children = []string{"42 bash"} },
		"pending":  func(f *facts) { f.Transcript.Pending = []string{"AskUserQuestion t1"} },
		"tasks":    func(f *facts) { f.Transcript.Tasks = []string{"Agent «ревью» a1"} },
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

// Свои таймеры текущей беседы --resume возвращает.
func TestDecideCrons(t *testing.T) {
	f := idleFacts()
	f.Name, f.HookState = "lead", "paused"
	f.Transcript.Crons = []cronJob{{ID: "j1", Prompt: "Сон (скилл dream)", Recurring: true, Own: true}, {ID: "j3", Prompt: "замер темпа Codex", Own: true}}
	if v := decide(f, allowance{idle: 10 * time.Minute}); !v.Restart {
		t.Errorf("таймеры --resume вернёт: %+v", v)
	}
	f.HookTasks = true
	if v := decide(f, allowance{idle: 10 * time.Minute}); v.Restart {
		t.Error("paused с фоновыми задачами на Stop — не перезапускать")
	}
	f.HookTasks, f.Transcript.Crons = false, nil
	if v := decide(f, allowance{idle: 10 * time.Minute}); v.Restart || !slices.Contains(codes(v), "hook-paused") {
		t.Errorf("paused без таймеров и задач — непонятно чего ждёт: %+v", v)
	}
	f.HookWaits = true // пауза — из-за монитора артефакта или таймеров по chooks
	if v := decide(f, allowance{idle: 10 * time.Minute}); !v.Restart {
		t.Errorf("пауза объяснена монитором артефакта: %+v", v)
	}
}

func TestDecideRestartInheritedCrons(t *testing.T) {
	f := idleFacts()
	f.Transcript.Crons = []cronJob{{ID: "j1", Cron: "47 1 * * *", Prompt: "Сон (скилл dream)", Recurring: true}}
	v := decide(f, allowance{idle: 10 * time.Minute})
	if v.Restart || !slices.Contains(codes(v), "crons") || !strings.Contains(v.Against[len(v.Against)-1].Text, "j1") {
		t.Fatalf("унаследованный таймер должен держать перезапуск: %+v", v)
	}
	v = decide(f, allowance{idle: 10 * time.Minute, typed: true})
	if v.Restart || !strings.Contains(v.Recreate, `cron "47 1 * * *"`) || !strings.Contains(v.Recreate, `prompt "Сон (скилл dream)"`) {
		t.Errorf("для pty нужна команда восстановления: %+v", v)
	}
	f.Transcript.Crons[0].Own = true
	if v = decide(f, allowance{idle: 10 * time.Minute, typed: true}); !v.Restart || v.Recreate != "" {
		t.Errorf("свои таймеры не держат перезапуск: %+v", v)
	}
	twin := f.Transcript.Crons[0]
	twin.ID = "j2"
	f.Transcript.Crons[0].Own = false
	f.Transcript.Crons = append(f.Transcript.Crons, twin)
	if v = decide(f, allowance{idle: 10 * time.Minute, typed: true}); !v.Restart || v.Recreate != "" {
		t.Errorf("унаследованный с поставленным заново двойником — не потеря, набор не повторять: %+v", v)
	}
	f.Transcript.Crons = f.Transcript.Crons[:1]
	f.Transcript.Wakeups = []string{"ScheduleWakeup до 19:00"}
	if v = decide(f, allowance{idle: 10 * time.Minute, typed: true}); v.Restart || v.Recreate != "" {
		t.Errorf("набор — только когда таймеры единственный довод: %+v", v)
	}
	f.Transcript.Wakeups = nil
	f.Transcript.Crons[0].Cron = ""
	if v = decide(f, allowance{idle: 10 * time.Minute, typed: true}); v.Recreate != "" || v.Restart {
		t.Errorf("неизвестное выражение нельзя набирать: %+v", v)
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
	f.Prompt = screen.Unknown                // поля не видно: черновик — только по времени ввода
	f.TTYQuiet = f.TurnQuiet + 5*time.Second // atime pts отстаёт до 8 с: ввод мог быть уже после конца хода
	if v := decide(f, allowance{idle: 10 * time.Minute}); v.Restart || !slices.Contains(codes(v), "draft") {
		t.Errorf("ввод в пределах 8 с до конца хода — тоже возможный черновик: %+v", v)
	}
	f.TTYQuiet = f.TurnQuiet + 30*time.Second
	if v := decide(f, allowance{idle: 10 * time.Minute}); slices.Contains(codes(v), "draft") {
		t.Errorf("ввод за 30 с до конца хода — отправка реплики, не черновик: %+v", v)
	}
}

// Ввод после конца хода при пустом поле на экране — байты фокуса вкладки, не черновик.
func TestDecideDraftEmptyScreen(t *testing.T) {
	f := idleFacts()
	f.TTYQuiet = f.TurnQuiet - time.Minute
	if v := decide(f, allowance{idle: 10 * time.Minute}); !v.Restart {
		t.Errorf("поле пусто по экрану — ввод после хода не держит: %+v", v)
	}
	f.PromptErr = "терминал не Konsole и не tmux"
	if v := decide(f, allowance{idle: 10 * time.Minute}); !slices.Equal(codes(v), []string{"draft", "screen"}) {
		t.Errorf("экран не прочитался — держит и время ввода: %+v", v)
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

// cl видел весь ввод человека: текст в поле — подсказка, и ввод в терминал после хода (фокус) не держит.
func TestDecideCleanField(t *testing.T) {
	f := idleFacts()
	f.Prompt = screen.Draft
	f.TTYQuiet = f.TurnQuiet - time.Minute
	if v := decide(f, allowance{idle: 10 * time.Minute}); v.Restart || !slices.Contains(codes(v), "screen") {
		t.Fatalf("без знания cl текст в поле держит: %+v", v)
	}
	f.cleanField(true)
	if v := decide(f, allowance{idle: 10 * time.Minute}); !v.Restart {
		t.Errorf("cl: поле чисто — текст на экране подсказка, перезапуск идёт: %+v", v)
	}
	g := idleFacts()
	g.Prompt = screen.Unknown
	g.cleanField(true)
	if v := decide(g, allowance{idle: 10 * time.Minute}); v.Restart {
		t.Errorf("поля не видно — держит и при чистом вводе: %+v", v)
	}
}
