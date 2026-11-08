package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// Хозяин временного scope (проба через systemd-run) — по scratchpad сессии в cmdline главного процесса: живая сессия с
// этим SessionID, иначе мастер проекта по ключу; без scratchpad — по cwd.
func TestIOOwnerByProcess(t *testing.T) {
	home := "/home/u"
	o := &ioOwnerFinder{home: home, table: map[string]string{}, sessions: func(id string) (string, error) {
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
