package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/agentwait"
	"github.com/aiseeq/aguard/internal/config"
	"github.com/aiseeq/aguard/internal/disk"
	"github.com/aiseeq/aguard/internal/iowatch"
	"github.com/aiseeq/aguard/internal/names"
	"github.com/aiseeq/aguard/internal/said"
)

// Тикет names-table-all-observers: прошлую таблицу имён при битой names.tsv держит не только очередь, а все места демона.

// namesPlaces — что каждое место демона назвало хозяином папки p (ошибка — "ERR"): agentwaits (только «прошёл ли»),
// тикеты, disk, io, said.
func namesPlaces(t *testing.T, b *queueBench, p string) map[string]string {
	t.Helper()
	got := map[string]string{}
	cfg := agentwait.Default()
	if err := b.a.agentwaitsRound(&cfg, true); err != nil {
		got["agentwaits"] = "ERR"
	} else {
		got["agentwaits"] = "ok"
	}
	if dir, err := b.a.sessionDir("io", "pm", b.c.Lead); err != nil {
		got["ticket"] = "ERR"
	} else {
		got["ticket"] = dir
	}
	if projects, err := b.a.diskProjects(); err != nil {
		got["disk"] = "ERR"
	} else {
		got["disk"] = projects[disk.ProjectKey(p)]
	}
	def := iowatch.Default()
	if name, err := b.a.ioOwners(&def).byProcess(iowatch.ProcessDetails{Cmdline: "rg x", CWD: p}); err != nil {
		got["io"] = "ERR"
	} else {
		got["io"] = name
	}
	scratch := filepath.Join(b.a.home, ".cache", "claude-tmp", "claude-1000", strings.ReplaceAll(p, "/", "-"),
		"381a85bb-1bde-458a-bed8-2cd4785b78a6") + "/"
	if name, err := b.a.ioOwners(&def).byProcess(iowatch.ProcessDetails{Cmdline: "rg " + scratch, CWD: b.a.home}); err != nil {
		got["io-scratch"] = "ERR"
	} else {
		got["io-scratch"] = name
	}
	evs := []said.Event{{Cwd: p, Session: "нет-в-реестре"}}
	if err := b.a.saidNames(evs); err != nil {
		got["said"] = "ERR"
	} else {
		got["said"] = evs[0].Name
	}
	return got
}

func wantPlaces(t *testing.T, what string, got, want map[string]string) {
	t.Helper()
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: %s = %q, want %q", what, k, got[k], w)
		}
	}
}

// Прогретый демон, таблица сломана: agentwaits, тикеты, disk, io, said идут на прошлой таблице; лиду одно письмо на все
// места; таблицу поправили — все места сразу берут новую.
func TestNamesBrokenWarmAllPlaces(t *testing.T) {
	b, got := namesBench(t)
	p := filepath.Join(b.dir, "git", "p")
	b.writeNames(t, p+"\tpm\n")
	warm := map[string]string{"agentwaits": "ok", "ticket": p, "disk": "pm", "io": "pm", "io-scratch": "pm", "said": "pm"}
	wantPlaces(t, "целая", namesPlaces(t, b, p), warm)

	b.writeNames(t, p+"\tpm\n"+brokenNamesLine)
	wantPlaces(t, "битая, прошлая", namesPlaces(t, b, p), warm)
	b.a.namesAlert(b.c, false, time.Now())
	b.a.namesAlert(b.c, false, time.Now())
	lead := drainLetters(got["lead"])
	if countWith(lead, "таблица имён") != 1 || countWith(lead, "на прошлой таблице") != 1 || countWith(lead, "agentwaits") != 1 ||
		countWith(lead, "said") != 1 || countWith(lead, "disk") != 1 || countWith(lead, "io") != 1 {
		t.Fatalf("письмо лиду о битой таблице: %q", lead)
	}

	b.writeNames(t, p+"\tpn\n")
	wantPlaces(t, "поправили", namesPlaces(t, b, p),
		map[string]string{"agentwaits": "ok", "ticket": "ERR", "disk": "pn", "io": "pn", "io-scratch": "pn", "said": "pn"})
	if dir, err := b.a.sessionDir("io", "pn", b.c.Lead); err != nil || dir != p {
		t.Fatalf("тикет по новой таблице: %q %v", dir, err)
	}
}

// Холодный старт с битой таблицей: выдумывать хозяев нельзя — каждое место с ошибкой, а не с пустой таблицей.
func TestNamesBrokenColdAllPlaces(t *testing.T) {
	b, got := namesBench(t)
	p := filepath.Join(b.dir, "git", "p")
	b.writeNames(t, p+"\tpm\n"+brokenNamesLine)
	wantPlaces(t, "холодный", namesPlaces(t, b, p),
		map[string]string{"agentwaits": "ERR", "ticket": "ERR", "disk": "ERR", "io": "ERR", "io-scratch": "ERR", "said": "ERR"})
	b.a.namesAlert(b.c, false, time.Now())
	if lead := drainLetters(got["lead"]); countWith(lead, "таблица имён") != 1 || countWith(lead, "стоят") != 1 {
		t.Fatalf("письмо лиду при холодном старте: %q", lead)
	}

	// said: имя из транскрипта или реестра таблицы не требует — такие реплики идут и при холодном старте.
	info := `{"pid":999999,"sessionId":"s-reg","name":"reg"}`
	if err := os.WriteFile(filepath.Join(b.a.reg.Dir, "999999.json"), []byte(info), 0o600); err != nil {
		t.Fatal(err)
	}
	evs := []said.Event{{Cwd: p, Name: "x-master"}, {Cwd: p, Session: "s-reg"}}
	if err := b.a.saidNames(evs); err != nil || evs[0].Name != "x-master" || evs[1].Name != "reg" {
		t.Fatalf("said с именами при холодном старте: %+v %v", evs, err)
	}
	evs = []said.Event{{Cwd: p, Session: "нет-в-реестре"}, {Cwd: p, Session: "s-reg"}} // безымянная не последней
	if err := b.a.saidNames(evs); err == nil {
		t.Fatalf("said с безымянной при холодном старте: %+v", evs)
	}
}

// Отметка о письме лиду не ставится, если таблицу поправили, пока письмо шло: та же поломка потом — снова письмо.
func TestNamesSentAfterFix(t *testing.T) {
	b, _ := namesBench(t)
	b.writeNames(t, brokenNamesLine)
	if _, err := b.a.namesTable("queue"); err == nil {
		t.Fatal("битая таблица без прошлой прочиталась")
	}
	m := &b.a.namesMem
	cur := m.err
	b.a.namesSent("x", time.Now()) // пока писали, ошибка сменилась
	if m.sentErr != "" {
		t.Fatalf("отметка о другой ошибке: %q", m.sentErr)
	}
	b.a.namesSent(cur, time.Now())
	if m.sentErr != cur {
		t.Fatalf("отметка о своей ошибке: %q", m.sentErr)
	}
}

// disk: холодный старт с битой таблицей — проход с ошибкой до обхода, починка — проход идёт.
func TestDiskRoundNamesCold(t *testing.T) {
	a, _ := testIOApp(t)
	cfg := config.Default(a.home)
	cfg.Disk.Roots, cfg.Disk.Containers, cfg.Disk.Track = nil, nil, nil
	// Сигнал о свободном месте от таблицы не зависит: идёт и при холодном старте с битой таблицей.
	got := liveLead(t, a)
	cfg.Lead, cfg.Disk.Lead, cfg.Disk.FreeMode, cfg.Disk.FreeMin = "lead", "lead", disk.FreeAlert, 1<<62
	if err := os.MkdirAll(a.dir("disk"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(a.home, names.File)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(brokenNamesLine), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.diskRound(t.Context(), &cfg); err == nil || !strings.Contains(err.Error(), "нужно «папка") {
		t.Fatalf("disk при холодном старте: %v", err)
	}
	select {
	case msg := <-got:
		if !strings.Contains(msg, "свободно") {
			t.Fatalf("письмо лиду: %q", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("сигнал о свободном месте не ушёл при холодном старте с битой таблицей")
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.diskRound(t.Context(), &cfg); err != nil {
		t.Fatal(err)
	}
}

// said: холодный старт с битой таблицей и безымянной репликой — проход с ошибкой, смещения не сохранены; починка —
// та же реплика размечается.
func TestSaidRoundNamesCold(t *testing.T) {
	b := newSaidBench(t)
	b.roundDirs(t)
	path := filepath.Join(b.a.home, names.File)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(brokenNamesLine), 0o644); err != nil {
		t.Fatal(err)
	}
	round := func() error { return b.a.saidRound(context.Background(), b.c, false) }
	if err := round(); err != nil { // реплик нет — таблица не нужна
		t.Fatal(err)
	}
	p := filepath.Join(b.a.home, "git", "p")
	dir := filepath.Join(b.a.projects, strings.ReplaceAll(p, "/", "-"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"user","origin":{"kind":"human"},"timestamp":"2026-10-26T13:01:33Z","cwd":"` + p + `","message":{"content":"реплика"}}`
	if err := os.WriteFile(filepath.Join(dir, "0123456789ab.jsonl"), []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := round(); err == nil || !strings.Contains(err.Error(), "нужно «папка") {
		t.Fatalf("said при холодном старте: %v", err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := round(); err != nil {
		t.Fatal(err)
	}
	if calls := b.calls(t); len(calls) == 0 {
		t.Fatal("после починки реплика не размечена")
	}
}

// WARN «иду на прошлой» — раз на наблюдателя и текст ошибки, повтор — через namesWarnRepeat; починка снимает отметки.
func TestNamesWarnRepeat(t *testing.T) {
	b, _ := namesBench(t)
	log := b.captureLog()
	warns := func() int {
		t.Helper()
		n := strings.Count(log.String(), "иду на прошлой")
		log.Reset()
		return n
	}
	read := func(watchers ...string) {
		t.Helper()
		for _, w := range watchers {
			if _, err := b.a.namesTable(w); err != nil {
				t.Fatal(err)
			}
		}
	}
	b.writeNames(t, "")
	read("disk")
	b.writeNames(t, brokenNamesLine)
	read("disk", "disk", "io", "io", "said")
	if n := warns(); n != 3 {
		t.Fatalf("WARN за первые чтения: %d, want 3", n)
	}
	m := &b.a.namesMem
	namesMu.Lock()
	m.warned["disk"] = namesWarned{err: m.warned["disk"].err, at: time.Now().Add(-namesWarnRepeat)}
	m.warned["io"] = namesWarned{err: m.warned["io"].err, at: time.Now().Add(-namesWarnRepeat + time.Minute)}
	namesMu.Unlock()
	read("disk", "io")
	if n := warns(); n != 1 {
		t.Fatalf("WARN после срока: %d, want 1 (disk)", n)
	}
	b.writeNames(t, "#\n"+brokenNamesLine) // другая строка — другой текст
	read("io")
	if n := warns(); n != 1 {
		t.Fatalf("WARN на другой текст: %d", n)
	}
	b.writeNames(t, "")
	read("io")
	b.writeNames(t, "#\n"+brokenNamesLine)
	read("io")
	if n := warns(); n != 1 {
		t.Fatalf("WARN после починки: %d", n)
	}
}

// Чтение таблицы и письмо лиду из разных горутин наблюдателей — под мьютексом (go test -race).
func TestNamesTableConcurrent(t *testing.T) {
	b, got := namesBench(t)
	b.a.log = slog.New(slog.DiscardHandler)
	p := filepath.Join(b.dir, "git", "p")
	b.writeNames(t, p+"\tpm\n")
	if _, err := b.a.namesTable("queue"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, w := range []string{"disk", "io"} {
		wg.Go(func() {
			for range 50 {
				table, err := b.a.namesTable(w)
				if err != nil || table[p] != "pm" {
					t.Errorf("%s: %v %v", w, table, err)
					return
				}
			}
		})
	}
	wg.Go(func() {
		for i := range 50 { // как проход очереди: чтение, затем письмо
			if _, err := b.a.namesTable("queue"); err != nil {
				t.Error(err)
			}
			b.a.namesAlert(b.c, false, time.Now())
			if i == 25 { // подмена целиком: чтение не видит полузаписанный файл
				path := filepath.Join(b.a.home, names.File)
				if err := os.WriteFile(path+".new", []byte(p+"\tpm\n"+brokenNamesLine), 0o644); err != nil {
					t.Error(err)
				}
				if err := os.Rename(path+".new", path); err != nil {
					t.Error(err)
				}
			}
		}
	})
	wg.Wait()
	if n := countWith(drainLetters(got["lead"]), "таблица имён"); n != 1 {
		t.Fatalf("писем лиду: %d", n)
	}
}
