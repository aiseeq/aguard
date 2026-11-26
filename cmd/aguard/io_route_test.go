package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/activity"
	"github.com/aiseeq/aguard/internal/cpuwatch"
	"github.com/aiseeq/aguard/internal/ioroute"
	"github.com/aiseeq/aguard/internal/iowatch"
)

// fakeTk — tk: ls отдаёт содержимое ls.json, остальные вызовы — строкой в calls.log (аргументы и stdin).
func fakeTk(t *testing.T, dir, lsJSON string) (tk, calls string) {
	t.Helper()
	tk, calls = filepath.Join(dir, "tk"), filepath.Join(dir, "calls.log")
	if err := os.WriteFile(filepath.Join(dir, "ls.json"), []byte(lsJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nif [ \"$1\" = ls ]; then cat " + filepath.Join(dir, "ls.json") + "; exit 0; fi\n" +
		"echo \"$PWD|$*\" >> " + calls + "\nif [ \"$1\" = new ]; then cat >> " + calls + "; fi\n"
	if err := os.WriteFile(tk, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return tk, calls
}

func TestIOTicketerNewNoteAndNext(t *testing.T) {
	a, _ := testIOApp(t)
	proj := filepath.Join(a.home, "git", "dream")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	c := ioroute.Case{Start: time.Now().Add(-time.Minute), End: time.Now(), DurMS: 3000, Owner: "dream-master", Class: "go", Text: "08.11 13:12:00, 3,0 с"}
	job := ioTicketJob{owner: "dream-master", class: "go", cases: []ioroute.Case{c}, lead: "lead"}
	tkr := &ioTicketer{a: a}
	cases := []struct {
		name, ls, want string
	}{
		{"нет тикета — новый", `[{"slug":"io-freeze-gopls","status":"открыт"}]`, proj + "|new io-freeze-go --prio MED --diff HUNT --by aguard"},
		{"открыт — заметка", `[{"slug":"io-freeze-go","status":"в работе"}]`, proj + "|note io-freeze-go aguard io: ещё 1: 08.11 13:12:00, 3,0 с; --by aguard"},
		{"закрыт — следующий номер со ссылкой", `[{"slug":"io-freeze-go","status":"готово"},{"slug":"io-freeze-go-2","status":"отклонён"}]`,
			proj + "|new io-freeze-go-3 --prio MED"},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		job.tk, _ = fakeTk(t, dir, tc.ls)
		if err := tkr.apply(context.Background(), job); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		b, err := os.ReadFile(filepath.Join(dir, "calls.log"))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !strings.Contains(string(b), tc.want) {
			t.Fatalf("%s: вызовы\n%s\nнет %q", tc.name, b, tc.want)
		}
		if strings.Contains(tc.name, "закрыт") && !strings.Contains(string(b), "Прежний тикет класса: io-freeze-go-2") {
			t.Fatalf("%s: нет ссылки на прежний:\n%s", tc.name, b)
		}
		// Приёмка без окна наблюдения (пользователь 18.11, tl 17c958f7:893): закрыть сразу после проверенной правки.
		if strings.Contains(tc.want, "|new ") && (!strings.Contains(string(b), "тикет закрывается сразу") || strings.Contains(string(b), "неделю после правки")) {
			t.Fatalf("%s: приёмка с окном наблюдения:\n%s", tc.name, b)
		}
	}
}

// Повтор класса после закрытия считается по началу случая (лид 18.11 11:54, io-freeze-rg-3): случай, начавшийся до
// закрытия прежнего тикета, разобран в нём — нового тикета нет; начавшийся после — новый тикет только с ним.
// Время закрытия — из журнала, не из поздней заметки.
func TestIOTicketerOldSeriesAfterClose(t *testing.T) {
	journal := "# x\n\nСтатус: готово\n\n## Журнал\n- 2026-11-18 10:09 открыт — aguard\n- 2026-11-18 11:00 note — aguard: 10:39\n" +
		"- 2026-11-18 11:21 готово — lead: правка\n- 2026-11-18 11:40 note — lead: поздняя заметка\n"
	at := func(hm string) time.Time {
		tm, err := time.ParseInLocation("2006-01-02 15:04:05", "2026-11-18 "+hm, time.Local)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	old := ioroute.Case{Start: at("10:39:39"), End: at("10:49:45"), DurMS: 606000, Class: "rg", Text: "18.11 10:39:39 старый"}
	edge := ioroute.Case{Start: at("11:21:00"), End: at("11:21:05"), DurMS: 5000, Class: "rg", Text: "18.11 11:21:00 на минуте закрытия"}
	// Начался за секунду до минуты закрытия и кончился после — старая серия: граница по началу, не по концу.
	straddle := ioroute.Case{Start: at("11:20:59"), End: at("11:23:00"), DurMS: 121000, Class: "rg", Text: "18.11 11:20:59 через закрытие"}
	late := ioroute.Case{Start: at("11:30:00"), End: at("11:30:05"), DurMS: 5000, Class: "rg", Text: "18.11 11:30:00 после закрытия, до поздней заметки"}
	ls := `[{"slug":"io-freeze-rg","status":"готово"},{"slug":"io-freeze-rg-2","status":"готово","path":"docs/tasks/rg-2.md"}]`
	started := `started="2026-11-18 10:39:39, 2026-11-18 11:20:59"`
	for _, tc := range []struct {
		name  string
		cases []ioroute.Case
		want  []string // в вызовах; nil — вызовов нет
		not   []string
		log   string // в строке журнала демона «находки старой серии»; "" — строки нет
		oldN  int64  // учёт ticket_old_series: число отсеянных; 0 — записи нет
		newN  int64  // учёт ticket_new: случаев в новом тикете
	}{
		{"только старые — без тикета", []ioroute.Case{old, straddle}, nil, nil, started + " fresh=0", 2, 0},
		{"старые и новые — новый тикет без старых", []ioroute.Case{old, straddle, edge, late},
			[]string{"|new io-freeze-rg-3", "Прежний тикет класса: io-freeze-rg-2", edge.Text, late.Text}, []string{old.Text, straddle.Text},
			started + " fresh=2", 2, 2},
		{"только новые — строки старой серии нет", []ioroute.Case{edge, late},
			[]string{"|new io-freeze-rg-3", edge.Text, late.Text}, nil, "", 0, 2},
	} {
		a, _ := testIOApp(t)
		var logBuf bytes.Buffer
		a.log = slog.New(slog.NewTextHandler(&logBuf, nil))
		proj := filepath.Join(a.home, "git", "dream")
		if err := os.MkdirAll(filepath.Join(proj, "docs", "tasks"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(proj, "docs", "tasks", "rg-2.md"), []byte(journal), 0o600); err != nil {
			t.Fatal(err)
		}
		tkr := &ioTicketer{a: a}
		dir := t.TempDir()
		tk, calls := fakeTk(t, dir, ls)
		job := ioTicketJob{owner: "dream-master", class: "rg", cases: tc.cases, tk: tk, lead: "lead"}
		if err := tkr.apply(context.Background(), job); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		assertOldSeriesAccounting(t, a, logBuf.String(), tc.name, tc.log, tc.oldN, tc.newN)
		assertTkCalls(t, tc.name, calls, tc.want, tc.not)
	}
}

func TestTicketClosedAt(t *testing.T) {
	dir := t.TempDir()
	write := func(name, s string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// Переоткрыт и снова закрыт — последняя строка закрытия; «отклонён» и «перенесён» — тоже закрытие.
	p := write("a.md", "- 2026-11-18 09:00 открыт — x\n- 2026-11-18 10:00 готово — x\n- 2026-11-18 10:30 в работе — y\n- 2026-11-18 12:05 отклонён — z: дубль\n- 2026-11-18 13:00 note — z\n")
	got, body, ok, err := ticketClosedAt(p)
	if want := time.Date(2026, 11, 18, 12, 5, 0, 0, time.Local); !ok || err != nil || !got.Equal(want) {
		t.Errorf("закрытие %v %v %v, ждали %v", got, ok, err, want)
	}
	// Текст — до последней строки закрытия: поздняя заметка (13:00) в разборе не участвовала.
	if want := "- 2026-11-18 09:00 открыт — x\n- 2026-11-18 10:00 готово — x\n- 2026-11-18 10:30 в работе — y\n"; body != want {
		t.Errorf("текст до закрытия %q, ждали %q", body, want)
	}
	if _, _, ok, err := ticketClosedAt(write("b.md", "- 2026-11-18 10:00 открыт — x\n- 2026-11-18 10:01 note — x: готово\n")); ok || err != nil {
		t.Errorf("без строки закрытия — нет времени и ошибки: %v %v", ok, err)
	}
	if _, _, ok, err := ticketClosedAt(filepath.Join(dir, "нет.md")); ok || err != nil {
		t.Errorf("нет файла — нет времени и ошибки: %v %v", ok, err)
	}
	if _, _, ok, err := ticketClosedAt(dir); ok || err == nil { // папка вместо файла: ошибка чтения не прячется
		t.Errorf("ошибка чтения: %v %v", ok, err)
	}
	if _, _, ok, err := ticketClosedAt(write("d.md", "- 2026-13-40 10:00 готово — x\n")); ok || err == nil {
		t.Errorf("невозможная дата закрытия — ошибка: %v %v", ok, err)
	}
	if got, _, ok, err := ticketClosedAt(write("c.md", "- 2026-11-18 09:00 перенесён — x\n")); !ok || err != nil || got.Hour() != 9 {
		t.Errorf("перенесён: %v %v %v", got, ok, err)
	}
}

// Папки хозяина нет — тикет лиду в домашней папке с именем хозяина.
func TestIOTicketerUnknownOwnerToLead(t *testing.T) {
	a, _ := testIOApp(t)
	dir := t.TempDir()
	tk, calls := fakeTk(t, dir, `[]`)
	c := ioroute.Case{Start: time.Now(), End: time.Now(), DurMS: 3000, Owner: "nobody-master", Class: "go", Text: "x"}
	tkr := &ioTicketer{a: a}
	if err := tkr.apply(context.Background(), ioTicketJob{owner: "nobody-master", class: "go", cases: []ioroute.Case{c}, tk: tk, lead: "lead"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(calls)
	if !strings.Contains(string(b), a.home+"|new io-freeze-go") || !strings.Contains(string(b), "Папка хозяина не найдена — тикет лиду: lead (за nobody-master)") {
		t.Fatalf("лиду: %s", b)
	}
}

func TestTicketClassAndSlugN(t *testing.T) {
	for in, want := range map[string]string{"adream.service": "adream", "com.google.Chrome": "com-google-chrome", "tab(2164408).scope": "tab",
		"plocate-updatedb": "plocate-updatedb", "": "", "dream-evals-1525968.scope": "dream-evals", "docker-99b2d0ff9fc9": "docker",
		"worker-io-3346494.scope": "worker-io", "session-c836.scope": "session-c836", "go": "go", "sc2-replay-sort.service": "sc2-replay-sort"} {
		if got := ticketClass(in); got != want {
			t.Errorf("ticketClass(%q) = %q, want %q", in, got, want)
		}
	}
	for slug, want := range map[string]int{"io-freeze-go": 1, "io-freeze-go-2": 2, "io-freeze-gopls": 0, "io-freeze-go-x": 0, "io-freeze-go-1": 0} {
		n, ok := classSlugN(slug, "io-freeze-go")
		if ok != (want > 0) || ok && n != want {
			t.Errorf("classSlugN(%q) = %d %v, want %d", slug, n, ok, want)
		}
	}
}

func TestIOBlameMainDeterminesRouteOwnerAndWaitingIsNotAlertable(t *testing.T) {
	o := &ioOwnerFinder{units: map[string]string{"adream.service": "adream-master"}}
	for _, tc := range []struct {
		name, main, owner, proc string
		waiting                 bool
	}{
		{"writeback preserves normal agent session route", "/user.slice/user-1000.slice/user@1000.service/agents.slice/agent-dream-master-1.scope", "dream-master", "writer(1)", false},
		{"idle device follows waiting service", "/system.slice/adream.service", "adream-master", "", true},
		{"stored service name still resolves owner", "adream.service", "adream-master", "", false},
		{"desktop cgroup is not itself an owner", "/user.slice/user-1000.slice/user@1000.service/app.slice/app-com.google.Chrome@123.service", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := iowatch.Case{Start: time.Now().Add(-time.Minute), End: time.Now(), DurMS: 3000, Waiting: tc.waiting,
				Culprits: &iowatch.Culprit{Main: tc.main, MainProc: tc.proc, ProcessDetails: map[string]iowatch.ProcessDetails{"writer(1)": {Cmdline: "writer", CWD: "/home/u/.cache/claude-tmp/claude-1000/-home-u-git-dream/12345678-1234-1234-1234-123456789012/scratchpad/x"}}}}
			o.home = "/home/u"
			o.sessions = func(id string) (string, error) {
				if id == "12345678-1234-1234-1234-123456789012" {
					return "dream-master", nil
				}
				return "", nil
			}
			rc, err := o.routeCase(&c)
			if err != nil || rc.Owner != tc.owner {
				t.Fatalf("маршрут: %+v %v, want owner %s", rc, err, tc.owner)
			}
			if tc.waiting && alertable(&c, iowatch.Default()) {
				t.Fatal("локальное ожидание ушло в маршрут фриза")
			}
		})
	}
}

func TestIOToolOwnerOverridesSessionOwner(t *testing.T) {
	main := "/user.slice/user-1000.slice/user@1000.service/agents.slice/agent-shop-master-123.scope"
	c := iowatch.Case{Culprits: &iowatch.Culprit{Main: main, MainProc: "tl(474531)"}}
	for _, tc := range []struct {
		name, wantOwner, wantTicketOwner string
		tools                            map[string]string
	}{
		{"tool configured", "shop-master", "akit-master", map[string]string{"tl": "akit-master"}},
		{"tool absent", "shop-master", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := &ioOwnerFinder{tools: tc.tools}
			rc, err := o.routeCase(&c)
			if err != nil || rc.Owner != tc.wantOwner || rc.TicketOwner != tc.wantTicketOwner || rc.Class != "tl" || rc.Generic {
				t.Fatalf("route: %+v %v, want owner=%s ticket_owner=%s class=tl generic=false", rc, err, tc.wantOwner, tc.wantTicketOwner)
			}
		})
	}
}

func TestIOWorkerOwnerFallsBackToJobOwnerWithoutMasterSession(t *testing.T) {
	a, _ := testIOApp(t)
	project := filepath.Join(a.home, "git", "existing")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	writeIOFile(t, filepath.Join(a.home, ".config", "cl", "names.tsv"), project+"\texisting-master\n")
	cfg := iowatch.Default()
	cfg.Lead = "lead"
	cfg.Tools = map[string]string{"podman": "akit-master"}
	main := "/user.slice/user-1000.slice/user@1000.service/agents.slice/worker-job-J.service"
	for _, tc := range []struct{ master, want string }{
		{"measure-dir-master", "launcher-master"},
		{"existing-master", "existing-master"},
	} {
		jobDir := filepath.Join(a.home, ".local", "state", "worker")
		writeIOFile(t, filepath.Join(jobDir, "J", "job.json"), `{"id":"J","master":"`+tc.master+`","owner":{"name":"launcher-master"}}`)
		o := a.ioOwners(&cfg)
		if o.tools["podman"] != "akit-master" {
			t.Fatalf("ioOwners не взял cfg.Tools: %v", o.tools)
		}
		c := iowatch.Case{Culprits: &iowatch.Culprit{Main: main, MainProc: "codex(1)", MainLeafProc: "podman(2)"}}
		rc, err := o.routeCase(&c)
		if err != nil || rc.Owner != tc.want || rc.TicketOwner != "akit-master" || rc.Class != "podman" || rc.Generic {
			t.Fatalf("master %s: route %+v, err=%v, want owner=%s ticket_owner=akit-master class=podman generic=false", tc.master, rc, err, tc.want)
		}
	}
}

func TestIOToolOwnerInTemporaryScope(t *testing.T) {
	o := &ioOwnerFinder{home: "/home/u", table: map[string]string{}}
	o.tools = map[string]string{"tl": "akit-master"}
	c := iowatch.Case{Culprits: &iowatch.Culprit{
		Main:     "/user.slice/user-1000.slice/user@1000.service/app.slice/run-p123-i456.scope",
		MainProc: "tl(7)", ProcessDetails: map[string]iowatch.ProcessDetails{
			"tl(7)": {Cmdline: "tl", CWD: "/home/u/git/store"},
		},
	}}
	rc, err := o.routeCase(&c)
	if err != nil || rc.Owner != "store-master" || rc.TicketOwner != "akit-master" || rc.Class != "tl" || rc.Generic {
		t.Fatalf("временный scope: %+v %v", rc, err)
	}
}

func TestIOToolOwnerSessionScopeUsesOwnerTicketWording(t *testing.T) {
	c := ioroute.Case{Owner: "akit-master", TicketOwner: "akit-master"}
	act := ioroute.Action{Why: ioroute.WhyVictim, Cases: []ioroute.Case{c}}
	got := ioRouteLetter(act, nil, 0, nil)
	if !strings.Contains(got, "ему тикет") || strings.Contains(got, "тикет — владельцу инструмента akit-master") {
		t.Fatalf("письмо при Owner == TicketOwner: %q", got)
	}
}

func TestIORouteNoOwnerLetterNamesToolTicketRecipient(t *testing.T) {
	withTool := ioroute.Action{Why: ioroute.WhyNoOwner, Cases: []ioroute.Case{{TicketOwner: "akit-master"}}}
	got := ioRouteLetter(withTool, nil, 0, nil)
	want := "aguard io: вред сейчас, хозяина виновника нет; тикет — владельцу инструмента akit-master."
	if got != want {
		t.Fatalf("письмо без хозяина не назвало владельца тикета: %q", got)
	}
	withoutTool := ioroute.Action{Why: ioroute.WhyNoOwner, Cases: []ioroute.Case{{}}}
	if got, want := ioRouteLetter(withoutTool, nil, 0, nil), "aguard io: вред сейчас, хозяина виновника нет."; got != want {
		t.Fatalf("письмо без владельца инструмента изменилось: %q, want %q", got, want)
	}
}

func TestIOWorkerOwnerCachesSessionDirectoryCheck(t *testing.T) {
	dir := t.TempDir()
	jobDir := filepath.Join(dir, "J")
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobDir, "job.json"), []byte(`{"id":"J","master":"akit-master","owner":{"name":"launcher-master"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	checks := 0
	o := &ioOwnerFinder{jobs: &cpuwatch.Jobs{Dir: dir}, sessionExists: func(name string) bool {
		checks++
		return name == "akit-master"
	}}
	for i := range 2 {
		owner, err := o.workerOwner("worker-job-J.service")
		if err != nil || owner != "akit-master" {
			t.Fatalf("случай %d: owner=%q err=%v", i, owner, err)
		}
	}
	if checks != 1 {
		t.Fatalf("папка сессии проверена %d раз, want 1", checks)
	}
}

func TestIOClaudeVersionWithoutLeafUsesClaudeClass(t *testing.T) {
	main := "/user.slice/user-1000.slice/user@1000.service/agents.slice/agent-shop-master-123.scope"
	c := iowatch.Case{Culprits: &iowatch.Culprit{Main: main, MainProc: "2.1.328(485370)"}}
	rc, err := (&ioOwnerFinder{}).routeCase(&c)
	if err != nil || rc.Owner != "shop-master" || rc.Class != "claude" || rc.Generic {
		t.Fatalf("версия comm Claude Code: %+v %v", rc, err)
	}
}

func TestIORouteLetterNamesToolTicketOwner(t *testing.T) {
	for _, tc := range []struct {
		why  ioroute.Why
		want string
	}{
		{ioroute.WhyOffline, "хозяин shop-master не в сети — тебе; тикет — владельцу инструмента akit-master"},
		{ioroute.WhyVictim, "может быть жертвой вытеснения, тикет — владельцу инструмента akit-master, письмо тебе"},
	} {
		act := ioroute.Action{Why: tc.why, Cases: []ioroute.Case{{Owner: "shop-master", TicketOwner: "akit-master"}}}
		if got := ioRouteLetter(act, nil, 0, nil); !strings.Contains(got, tc.want) {
			t.Errorf("причина %s: письмо %q не содержит %q", tc.why, got, tc.want)
		}
	}
}

// Хозяин временного scope (проба через systemd-run) — по scratchpad сессии в cmdline главного процесса: живая сессия с
// этим SessionID, иначе мастер проекта по ключу; без scratchpad — по cwd.
func TestIOOwnerByProcess(t *testing.T) {
	home := "/home/u"
	o := &ioOwnerFinder{home: home, table: map[string]string{home: "lead"}, sessions: func(id string) (string, error) {
		if id == "381a85bb-1bde-458a-bed8-2cd4785b78a6" {
			return "dream-master", nil
		}
		return "", nil
	}}
	cmd := "fuse-overlayfs -o lowerdir=/home/u/.cache/claude-tmp/claude-1000/-home-u-git-dream/381a85bb-1bde-458a-bed8-2cd4785b78a6/scratchpad/x"
	for _, tc := range []struct {
		d    iowatch.ProcessDetails
		want string
	}{
		{iowatch.ProcessDetails{Cmdline: cmd, CWD: "/"}, "dream-master"},
		{iowatch.ProcessDetails{Cmdline: strings.ReplaceAll(cmd, "381a85bb", "00000000"), CWD: "/"}, "dream-master"},
		{iowatch.ProcessDetails{Cmdline: "/home/u/.cache/claude-tmp/claude-1000/-home-u-git-aiarena-web/00000000-1bde-458a-bed8-2cd4785b78a6/", CWD: "/"}, "aiarena-web-master"},
		{iowatch.ProcessDetails{Cmdline: "rg x", CWD: "/home/u/work/proj/sub"}, ""},
		{iowatch.ProcessDetails{Cmdline: "rg x", CWD: "/home/u/work/proj"}, "proj-master"},
		{iowatch.ProcessDetails{Cmdline: "chrome", CWD: "/home/u/"}, ""}, // ~ — программы рабочего стола, не лид
		{iowatch.ProcessDetails{}, ""},
	} {
		got, err := o.byProcess(tc.d)
		if err != nil || got != tc.want {
			t.Errorf("%+v: %q %v, want %q", tc.d, got, err, tc.want)
		}
	}
	// Фриз пробы 08.11 13:12:52: чтение 387 МБ при подкачке машины 864 + 721 страниц (~6 МБ) — чтение файлов,
	// главная — виновник: письмо хозяину, лиду ничего (пользователь 08.11, сужение правила 03.11).
	c := iowatch.Case{Start: time.Now().Add(-12600 * time.Millisecond), End: time.Now(), DurMS: 12600, SwapIn: 864, Majflt: 721,
		Culprits: &iowatch.Culprit{
			Main: "/user.slice/user-1000.slice/user@1000.service/app.slice/run-p2016228-i69065841.scope", MainRead: 387 << 20,
			MainProc: "fuse-overlayfs(2016008)", ProcessDetails: map[string]iowatch.ProcessDetails{"fuse-overlayfs(2016008)": {Cmdline: cmd, CWD: "/"}}}}
	rc, err := o.routeCase(&c)
	if err != nil || rc.Owner != "dream-master" || rc.Victim || rc.Class != "fuse-overlayfs" {
		t.Fatalf("run-p scope: %+v %v", rc, err)
	}
	def := iowatch.Default()
	acts := ioroute.New(def.Route()).Route(rc, func(s string) bool { return s == "dream-master" || s == "lead" })
	if len(acts) != 1 || acts[0].Kind != ioroute.Letter || acts[0].To != "dream-master" || acts[0].Why != ioroute.WhyUrgent {
		t.Fatalf("фриз пробы 13:12: %+v", acts)
	}
	// Подкачка — половина чтения и больше: возможная жертва; меньше половины — виновник.
	for _, tc := range []struct {
		pages  int64
		victim bool
	}{{(387 << 20) / 2 / 4096, true}, {(387<<20)/2/4096 - 1, false}} {
		c.SwapIn, c.Majflt = tc.pages, 0
		if rc, err := o.routeCase(&c); err != nil || rc.Victim != tc.victim {
			t.Errorf("подкачка %d стр: Victim %v %v, want %v", tc.pages, rc.Victim, err, tc.victim)
		}
	}
	c.SwapIn, c.Culprits.MainWrite = 1<<20, 388<<20 // пишет больше, чем читает — не жертва при любой подкачке
	if rc, _ := o.routeCase(&c); rc.Victim {
		t.Errorf("пишущая главная — жертва: %+v", rc)
	}
}

func TestIOOwnerGraftHeavyPriority(t *testing.T) {
	a, _ := testIOApp(t)
	repo := filepath.Join(a.home, "git", "dream")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(a.home, ".config", "cl"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.home, ".config", "cl", "names.tsv"), []byte("~/git\tgit-master\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := iowatch.Default()
	cfg.Lead = "lead"
	o := a.ioOwners(&cfg)
	const main = "/user.slice/user-1000.slice/user@1000.service/app.slice/app-graft.slice/graft-heavy-4018124.scope"
	parentSession := iowatch.Process{PID: 50, PPID: 60, Comm: "bash", Cgroup: "/user.slice/user-1000.slice/user@1000.service/agents.slice/agent-other-master-12.scope"}
	parentWorker := iowatch.Process{PID: 70, PPID: 1, Comm: "worker", Cgroup: "/user.slice/user-1000.slice/user@1000.service/agents.slice/worker-job-J.service"}
	jobDir := filepath.Join(a.home, ".local", "state", "worker", "J")
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeIOFile(t, filepath.Join(jobDir, "job.json"), `{"id":"J","master":"worker-master"}`)
	// Случай 21.11 10:05:40: graft heavy запущен из timeout 55m bash … в scope сессии, выше цепочка уходит в systemd --user.
	detached := []iowatch.Process{
		{PID: 3890701, PPID: 3890696, Comm: "bash", Cgroup: "/user.slice/user-1000.slice/user@1000.service/agents.slice/agent-akit-master-1332071.scope"},
		{PID: 3890696, PPID: 3431, Comm: "timeout", Cgroup: "/user.slice/user-1000.slice/user@1000.service/agents.slice/agent-akit-master-1332071.scope"},
		{PID: 3431, PPID: 1, Comm: "systemd", Cgroup: "/user.slice/user-1000.slice/user@1000.service/init.scope"},
	}
	worktree := filepath.Join(a.home, ".cache", "worktrees", "dream", "user-word-cache")
	if err := os.MkdirAll(filepath.Join(worktree, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeIOFile(t, filepath.Join(worktree, ".git"), "gitdir: "+filepath.Join(repo, ".git", "worktrees", "user-word-cache")+"\n")
	notWorktree := filepath.Join(a.home, ".cache", "worktrees", "dream", "plain")
	if err := os.MkdirAll(notWorktree, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, kind, cwd, want string
		chain                 []iowatch.Process
	}{
		{name: "repo from real graft kind without table row wins", kind: "dream:exec:user-word", chain: []iowatch.Process{parentSession}, want: "dream-master"},
		{name: "missing repo directory continues to parent", kind: "shop:test-unit", chain: []iowatch.Process{parentSession}, want: "other-master"},
		{name: "home-name repo without directory continues to parent", kind: "aisee:exec:user-word", chain: []iowatch.Process{parentSession}, want: "other-master"},
		{name: "slash kind falls back to Claude parent", kind: "/:exec:user-word", chain: []iowatch.Process{parentSession}, want: "other-master"},
		{name: "slash kind finds worker parent", kind: "/:exec:user-word", chain: []iowatch.Process{parentWorker}, want: "worker-master"},
		{name: "no kind or parent uses byProcess", cwd: repo, want: "dream-master"},
		{name: "detached parent chain up to systemd finds session scope", kind: "akit:exec:bash", chain: detached, want: "akit-master"},
		{name: "session scope above a non-session parent", kind: "/:exec:bash", chain: append([]iowatch.Process{
			{PID: 80, PPID: 3890701, Comm: "nohup", Cgroup: "/user.slice/user-1000.slice/user@1000.service/app.slice/run-u12.scope"}}, detached...), want: "akit-master"},
		{name: "no kind, cwd in project worktree", cwd: worktree, want: "dream-master"},
		{name: "no kind, cwd below project worktree root", cwd: filepath.Join(worktree, "sub"), want: "dream-master"},
		{name: "folder under worktrees without .git file", cwd: notWorktree, want: ""},
		{name: "worktrees repo folder itself", cwd: filepath.Dir(worktree), want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := iowatch.Case{Culprits: &iowatch.Culprit{Main: main, MainProc: "user-word(4018124)",
				GraftKind: tc.kind, ProcessDetails: map[string]iowatch.ProcessDetails{"user-word(4018124)": {Cmdline: "user-word", CWD: tc.cwd}}, ParentChain: tc.chain}}
			rc, err := o.routeCase(&c)
			if err != nil || rc.Owner != tc.want {
				t.Fatalf("route %+v, err=%v, want owner %s", rc, err, tc.want)
			}
		})
	}
}

func TestIOGraftHeavyScopeSuffixBoundary(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/user.slice/graft-heavy-12.scope", true},
		{"/user.slice/graft-heavy-12.scope.extra", false},
		{"/user.slice/graft-heavy-12", false},
	} {
		if got := isGraftHeavy(tc.path); got != tc.want {
			t.Errorf("isGraftHeavy(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestIOOwnerGraftHeavyBrokenNamesTablePropagates(t *testing.T) {
	a, _ := testIOApp(t)
	repo := filepath.Join(a.home, "git", "dream")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := iowatch.Default()
	cfg.Lead = "lead"
	o := a.ioOwners(&cfg)
	o.tableErr = errors.New("broken names table")
	c := iowatch.Case{Culprits: &iowatch.Culprit{Main: "/user.slice/graft-heavy-4018124.scope", GraftKind: "dream:exec:user-word"}}
	if _, err := o.routeCase(&c); err == nil || !strings.Contains(err.Error(), "broken names table") {
		t.Fatalf("broken names table error = %v", err)
	}
}

// Письмо хозяину не ушло (сессии нет в реестре) — его случаи в тикет хозяина; письмо лиду не ушло — тикета нет.
func TestIOLetterFailedToTicket(t *testing.T) {
	a, _ := testIOApp(t)
	s := newIOService(context.Background(), a)
	cfg := iowatch.Default()
	cfg.Mode, cfg.Lead = iowatch.ModeAlert, "lead"
	s.cfg = cfg
	s.setRoute(cfg)
	c := ioroute.Case{Start: time.Now().Add(-12 * time.Second), End: time.Now(), DurMS: 12000, Owner: "gone-master", Class: "go"}
	s.act([]ioroute.Action{{Kind: ioroute.Letter, To: "lead", Why: ioroute.WhyNoOwner, Cases: []ioroute.Case{{End: time.Now()}}},
		{Kind: ioroute.Letter, To: "gone-master", Why: ioroute.WhyUrgent, Cases: []ioroute.Case{c}}})
	select {
	case j := <-s.tickets.queue:
		if j.owner != "gone-master" || j.class != "go" || len(j.cases) != 1 {
			t.Fatalf("тикет %+v", j)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("тикета нет")
	}
	select {
	case j := <-s.tickets.queue:
		t.Fatalf("лишний тикет %+v", j)
	case <-time.After(200 * time.Millisecond):
	}
}

// Главная cgroup — unit бойца worker (и его повтор): хозяин — мастер из job.json, ему маршрут, а не лиду
// (io-alert-worker-master: 29 случаев от 2 с у worker-*.service за 29–30.10 доставались только лиду).
func TestIOOwnerWorkerMaster(t *testing.T) {
	dir := t.TempDir()
	writeIOFile(t, filepath.Join(dir, "J", "job.json"), `{"id":"J","master":"sc2go-master","unit":"worker-J"}`)
	o := &ioOwnerFinder{jobs: &cpuwatch.Jobs{Dir: dir, Units: []string{t.TempDir()}}}
	alive := func(s string) bool { return s == "sc2go-master" || s == "lead" }
	for _, unit := range []string{"worker-J.service", "worker-J-2.service"} {
		c := iowatch.Case{Start: time.Now().Add(-2500 * time.Millisecond), End: time.Now(), DurMS: 2500,
			Culprits: &iowatch.Culprit{Main: "/user.slice/user-1000.slice/user@1000.service/app.slice/" + unit,
				MainWrite: 400 << 20, MainProc: "go(4242)"}}
		rc, err := o.routeCase(&c)
		if err != nil || rc.Owner != "sc2go-master" || rc.Victim {
			t.Fatalf("%s: %+v %v", unit, rc, err)
		}
		def := iowatch.Default()
		acts := ioroute.New(def.Route()).Route(rc, alive)
		// 2,5 с — без вреда сейчас: тикет в проект мастера бойца (маршрут agent-mail-async), лиду ничего.
		if len(acts) != 1 || acts[0].Kind != ioroute.Ticket || acts[0].Owner != "sc2go-master" {
			t.Fatalf("%s 2,5 с: %+v", unit, acts)
		}
		c.Start, c.DurMS = time.Now().Add(-12600*time.Millisecond), 12600
		if rc, err = o.routeCase(&c); err != nil {
			t.Fatal(err)
		}
		acts = ioroute.New(def.Route()).Route(rc, alive)
		if len(acts) != 1 || acts[0].Kind != ioroute.Letter || acts[0].To != "sc2go-master" || acts[0].Why != ioroute.WhyUrgent {
			t.Fatalf("%s 12,6 с: письмо не мастеру бойца: %+v", unit, acts)
		}
	}
}

// assertOldSeriesAccounting — строка журнала «находки старой серии» с началами отсеянных и учёт: ticket_old_series —
// число отсеянных, ticket_new — случаев в новом тикете.
func assertOldSeriesAccounting(t *testing.T, a *app, log, name, wantLog string, oldN, newN int64) {
	t.Helper()
	line := ""
	for l := range strings.Lines(log) {
		if strings.Contains(l, "находки старой серии") {
			line = l
		}
	}
	if wantLog == "" && line != "" || wantLog != "" && !strings.Contains(line, wantLog) {
		t.Fatalf("%s: строка старой серии %q, ждали %q; журнал:\n%s", name, line, wantLog, log)
	}
	if err := a.act.Flush(time.Now(), activityKeep); err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	_, err := activity.NewStore(filepath.Join(a.home, "activity")).Read(time.Time{}, func(r *activity.Record) { // путь testIOApp
		if r.Watcher == "io" && (r.Kind == "ticket_old_series" || r.Kind == "ticket_new") {
			got[r.Kind] = r.Max
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["ticket_old_series"] != oldN || got["ticket_new"] != newN {
		t.Fatalf("%s: учёт %v, ждали ticket_old_series=%d ticket_new=%d", name, got, oldN, newN)
	}
}

// assertTkCalls — вызовы поддельного tk (fakeTk): want == nil — вызовов нет; иначе все want есть, ни одного not.
func assertTkCalls(t *testing.T, name, calls string, want, not []string) {
	t.Helper()
	b, err := os.ReadFile(calls)
	if want == nil {
		if err == nil {
			t.Fatalf("%s: лишний вызов tk:\n%s", name, b)
		}
		return
	}
	for _, w := range want {
		if !strings.Contains(string(b), w) {
			t.Fatalf("%s: нет %q в\n%s", name, w, b)
		}
	}
	for _, w := range not {
		if strings.Contains(string(b), w) {
			t.Fatalf("%s: лишнее %q в\n%s", name, w, b)
		}
	}
}

// Хозяин по дереву graft: имя сессии — из таблицы имён для основного репозитория; битая таблица и ошибка чтения
// .git дерева не проглатываются.
func TestIOOwnerWorktreeTableAndErrors(t *testing.T) {
	a, _ := testIOApp(t)
	repo := filepath.Join(a.home, "git", "dream")
	worktree := filepath.Join(a.home, ".cache", "worktrees", "dream", "cache")
	looped := filepath.Join(a.home, ".cache", "worktrees", "dream", "looped")
	for _, dir := range []string{repo, worktree, looped, filepath.Join(a.home, ".config", "cl")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeIOFile(t, filepath.Join(worktree, ".git"), "gitdir: "+filepath.Join(repo, ".git", "worktrees", "cache")+"\n")
	if err := os.Symlink(".git", filepath.Join(looped, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.home, ".config", "cl", "names.tsv"), []byte("~/git/dream\tdream-renamed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := iowatch.Default()
	cfg.Lead = "lead"
	o := a.ioOwners(&cfg)
	route := func(cwd string) (string, error) {
		c := iowatch.Case{Culprits: &iowatch.Culprit{Main: "/user.slice/graft-heavy-1.scope", MainProc: "bash(1)",
			ProcessDetails: map[string]iowatch.ProcessDetails{"bash(1)": {Cmdline: "bash", CWD: cwd}}}}
		rc, err := o.routeCase(&c)
		return rc.Owner, err
	}
	if owner, err := route(worktree); err != nil || owner != "dream-renamed" {
		t.Fatalf("worktree owner %q, err=%v, want dream-renamed from names table", owner, err)
	}
	workRepo := filepath.Join(a.home, "work", "shop")
	workTree := filepath.Join(a.home, ".cache", "worktrees", "shop-accept", "fix")
	for _, dir := range []string{workRepo, workTree} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeIOFile(t, filepath.Join(workTree, ".git"), "gitdir: "+filepath.Join(workRepo, ".git", "worktrees", "fix")+"\n")
	if owner, err := route(workTree); err != nil || owner != "shop-master" {
		t.Fatalf("~/work worktree owner %q, err=%v, want shop-master from .git, not folder name", owner, err)
	}
	if _, err := route(looped); err == nil {
		t.Fatal("unreadable worktree .git: error swallowed")
	}
	o.tableErr = errors.New("broken names table")
	if _, err := route(filepath.Join(a.home, "git", "other")); err == nil || !strings.Contains(err.Error(), "broken names table") {
		t.Fatalf("broken names table for plain cwd: err=%v", err)
	}
}

// Чужой инструмент в сессии dream-master — тикет хозяину проекта, где инструмент собран; свой инструмент dream,
// системная утилита, имя в двух проектах и неисполняемый файл — тикет сессии (тикет io-route-tool-owner).
func TestIOToolOwnerByBuildProject(t *testing.T) {
	home := t.TempDir()
	for path, mode := range map[string]os.FileMode{
		"git/graft/bin/graft":              0o755,
		"git/llmark/tool/bin/worker":       0o755,
		"git/dream/bin/adream":             0o755,
		"git/a/bin/dup":                    0o755,
		"git/b/bin/dup":                    0o755,
		"git/akit/bin/notes":               0o644,
		"git/p/.venv/bin/python3":          0o755,
		"git/akit/bin/claude-rules-export": 0o755,
		"work/shop/bin/shoptool":           0o755,
	} {
		full := filepath.Join(home, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, nil, mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(home, "git", "akit", "bin", "tldir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "git", "q", "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/bin/rg", filepath.Join(home, "git", "q", "bin", "rg")); err != nil {
		t.Fatal(err)
	}
	main := "/user.slice/user-1000.slice/user@1000.service/agents.slice/agent-dream-master-123.scope"
	for _, tc := range []struct {
		proc, want, letter string
		tools              map[string]string
	}{
		{"graft(1)", "graft-master", "тикет — владельцу инструмента graft-master", nil},
		{"worker(1)", "llmark-master", "тикет — владельцу инструмента llmark-master", nil},
		{"adream(1)", "dream-master", "ему тикет", nil},
		{"du(1)", "", "ему тикет", nil},
		{"dup(1)", "", "ему тикет", nil},
		{"notes(1)", "", "ему тикет", nil},
		{"tldir(1)", "", "ему тикет", nil},
		{"python3(1)", "", "ему тикет", nil},
		{"rg(1)", "", "ему тикет", nil},
		{"claude-rules-ex(1)", "akit-master", "тикет — владельцу инструмента akit-master", nil},
		{"claude-rules(1)", "", "ему тикет", nil},
		{"shoptool(1)", "shop-master", "тикет — владельцу инструмента shop-master", nil},
		{"graft(1)+дети", "", "ему тикет", nil},
		{"tl(1)+дети", "akit-master", "тикет — владельцу инструмента akit-master", map[string]string{"tl": "akit-master"}},
		{"graft(1)", "other-master", "тикет — владельцу инструмента other-master", map[string]string{"graft": "other-master"}},
	} {
		o := &ioOwnerFinder{home: home, table: map[string]string{}, tools: tc.tools}
		rc, err := o.routeCase(&iowatch.Case{Culprits: &iowatch.Culprit{Main: main, MainProc: tc.proc}})
		if err != nil || rc.Owner != "dream-master" || rc.TicketOwner != tc.want {
			t.Fatalf("%s: route %+v, err=%v, want owner=dream-master ticket_owner=%q", tc.proc, rc, err, tc.want)
		}
		if got := ioTicketRecipient(rc); got != tc.letter {
			t.Fatalf("%s: адрес тикета %q, want %q", tc.proc, got, tc.letter)
		}
	}
	broken := &ioOwnerFinder{home: home, tableErr: os.ErrNotExist}
	if got, err := broken.toolOwner("graft(1)"); got != "" || err == nil {
		t.Fatalf("битая таблица имён: владелец %q, err=%v, want пусто и ошибку", got, err)
	}
	if rc, err := broken.routeCase(&iowatch.Case{Culprits: &iowatch.Culprit{Main: main, MainProc: "graft(1)"}}); err == nil || rc.Owner != "dream-master" || rc.TicketOwner != "" {
		t.Fatalf("битая таблица имён в маршруте: %+v, err=%v — want хозяин сессии, без владельца инструмента, с ошибкой", rc, err)
	}
	if got, err := broken.toolOwner("du(1)"); got != "" || err != nil {
		t.Fatalf("системная утилита при битой таблице: %q, %v — want пусто без ошибки", got, err)
	}
}

// Запускатель с детьми в задании бойца и в graft-heavy — тикет хозяину задания и репозитория, а не владельцу
// запускателя: дети — тесты и сборка проекта (ревью io-route-tool-owner, 19.11 14:39:39 и 22.11 12:57:00); I/O самого
// worker — владельцу worker.
func TestIOBuiltToolOwnerKeepsJobAndRepoOwner(t *testing.T) {
	a, _ := testIOApp(t)
	for _, dir := range []string{"git/llmark/tool/bin", "git/graft/bin", "git/store", "git/existing"} {
		if err := os.MkdirAll(filepath.Join(a.home, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, bin := range []string{"git/llmark/tool/bin/worker", "git/graft/bin/graft"} {
		if err := os.WriteFile(filepath.Join(a.home, bin), nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeIOFile(t, filepath.Join(a.home, ".config", "cl", "names.tsv"), filepath.Join(a.home, "git", "existing")+"\texisting-master\n")
	writeIOFile(t, filepath.Join(a.home, ".local", "state", "worker", "J", "job.json"), `{"id":"J","master":"existing-master","owner":{"name":"launcher-master"}}`)
	cfg := iowatch.Default()
	cfg.Lead = "lead"
	o := a.ioOwners(&cfg)
	job := "/user.slice/user-1000.slice/user@1000.service/agents.slice/worker-job-J.service"
	heavy := "/user.slice/user-1000.slice/user@1000.service/app.slice/graft-heavy-170987.scope"
	for _, tc := range []struct {
		main, kind, proc, owner, ticketOwner string
	}{
		{job, "", "worker(1)+дети", "existing-master", ""},
		{job, "", "worker(1)", "existing-master", "llmark-master"},
		{heavy, "store:exec:bash", "graft(1)+дети", "store-master", ""},
		{heavy, "store:exec:bash", "graft(1)", "store-master", "graft-master"},
	} {
		c := iowatch.Case{Culprits: &iowatch.Culprit{Main: tc.main, GraftKind: tc.kind, MainProc: tc.proc}}
		rc, err := o.routeCase(&c)
		if err != nil || rc.Owner != tc.owner || rc.TicketOwner != tc.ticketOwner {
			t.Fatalf("%s %s: route %+v, err=%v, want owner=%s ticket_owner=%q", tc.main, tc.proc, rc, err, tc.owner, tc.ticketOwner)
		}
	}
}

// Служба без хозяина с собранным инструментом (graft в graft-gocache-trim.service) — класс по процессу, тикет
// владельцу инструмента; сбой чтения bin живёт toolIndexLife и не обходит папки заново на каждый случай.
func TestIOBuiltToolOwnerForServiceAndCachedFailure(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, "git", "graft", "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "graft"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	main := "/user.slice/user-1000.slice/user@1000.service/app.slice/graft-gocache-trim.service"
	c := iowatch.Case{Culprits: &iowatch.Culprit{Main: main, MainProc: "graft(1)"}}
	o := &ioOwnerFinder{home: home, table: map[string]string{}}
	rc, err := o.routeCase(&c)
	if err != nil || rc.Owner != "" || rc.TicketOwner != "graft-master" || rc.Class != "graft" || rc.Generic {
		t.Fatalf("служба: route %+v, err=%v, want owner=\"\" ticket_owner=graft-master class=graft generic=false", rc, err)
	}
	if err := os.Rename(filepath.Join(bin, "graft"), filepath.Join(bin, "graft.old")); err != nil {
		t.Fatal(err)
	}
	if got, err := o.toolOwner("graft(1)"); got != "graft-master" || err != nil {
		t.Fatalf("список не сохранён на toolIndexLife: %q, err=%v", got, err)
	}
	if err := os.Rename(filepath.Join(bin, "graft.old"), filepath.Join(bin, "graft")); err != nil {
		t.Fatal(err)
	}

	locked := &ioOwnerFinder{home: home, table: map[string]string{}}
	if err := os.Chmod(bin, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(bin, 0o700) })
	if got, err := locked.toolOwner("graft(1)"); got != "" || err == nil {
		t.Fatalf("нечитаемый bin: %q, err=%v — want пусто и ошибку", got, err)
	}
	if err := os.Chmod(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if got, err := locked.toolOwner("graft(1)"); got != "" || err == nil {
		t.Fatalf("сбой не сохранён на toolIndexLife: %q, err=%v", got, err)
	}
	locked.toolIndexAt = locked.toolIndexAt.Add(-toolIndexLife)
	if got, err := locked.toolOwner("graft(1)"); got != "graft-master" || err != nil {
		t.Fatalf("после toolIndexLife список не собран заново: %q, err=%v", got, err)
	}
}
