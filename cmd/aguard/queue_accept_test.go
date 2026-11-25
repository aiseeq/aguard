package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/config"
	"github.com/aiseeq/aguard/internal/policy"
	"github.com/aiseeq/aguard/internal/queue"
	"github.com/aiseeq/aguard/internal/restart"
	"github.com/aiseeq/akit/session"
)

// Приёмка прохода queue, второй заход: написана мастером, боец её не правит. Живых сессий в этих тестах нет, письма не идут.

type queueBench struct {
	a   *app
	c   *config.Config
	dir string
}

func newQueueBench(t *testing.T, gitJSON, docsJSON string) *queueBench {
	t.Helper()
	dir := t.TempDir()
	a := &app{home: dir, stateDir: filepath.Join(dir, "state"), uid: 1000,
		reg: session.Registry{Dir: filepath.Join(dir, "sessions"), ProcRoot: dir}, log: slog.New(slog.DiscardHandler), out: &strings.Builder{}}
	if err := os.MkdirAll(a.dir("queue"), 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	git := write("git.json", gitJSON)
	docs := write("docs.json", docsJSON)
	tk := write("tk", fmt.Sprintf("#!/bin/sh\nfor a in \"$@\"; do [ \"$a\" = --root ] && { cat %s; exit 0; }; done\ncase \"$PWD\" in %s/*) cat %s ;; *) cat %s ;; esac\n", git, filepath.Join(dir, "git"), git, docs))
	now := time.Now()
	hist := write("limits.jsonl", fmt.Sprintf(`{"at":%q,"sevenDayResetsAt":%q,"sevenDayUsedPercentage":10,"account":"a"}
{"at":%q,"sevenDayResetsAt":%q,"sevenDayUsedPercentage":20,"account":"a"}
`, now.Add(-24*time.Hour).Format(time.RFC3339), now.Add(72*time.Hour).Format(time.RFC3339), now.Add(-time.Hour).Format(time.RFC3339), now.Add(72*time.Hour).Format(time.RFC3339)))
	for _, d := range []string{"git/p/docs/tasks", "docs/tasks", "docs/huge"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	c := &config.Config{Lead: "lead", Admins: map[string]string{filepath.Join(dir, "git"): "git-admin"},
		Queue: queue.Config{Mode: queue.ModeOn, Live: []string{"glint", "graft"}, Interval: policy.Duration(30 * time.Second),
			FreeAfter: policy.Duration(2 * time.Minute), CritCopyAfter: policy.Duration(15 * time.Minute), GitRoots: []string{filepath.Join(dir, "git")},
			Dirs: []string{filepath.Join(dir, "docs")}, Level: queue.LevelHigh,
			Gate: filepath.Join(dir, "gate.json"), History: hist, Tk: tk}}
	return &queueBench{a, c, dir}
}

func (b *queueBench) round(t *testing.T) {
	t.Helper()
	if err := b.a.queueRound(context.Background(), b.c, restart.NewJudge(b.dir), false); err != nil {
		t.Fatal(err)
	}
}

func (b *queueBench) gateExists() bool {
	_, err := os.Stat(b.c.Queue.Gate)
	return err == nil
}

func (b *queueBench) journal(t *testing.T, kind string) int {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(b.a.dir("queue"), "20*.jsonl*"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, name := range names {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		n += strings.Count(string(data), `"kind":"`+kind+`"`)
	}
	return n
}

func TestQueueRoundWritesGate(t *testing.T) {
	b := newQueueBench(t, "[]", "[]")
	b.round(t)
	data, err := os.ReadFile(b.c.Queue.Gate)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"live":["glint","graft"]`) {
		t.Fatalf("%s", data)
	}
	if b.journal(t, "gate") != 1 {
		t.Fatalf("первая запись ворот в журнале: %d", b.journal(t, "gate"))
	}
	// Тот же расчёт: ещё проход не переписывает файл, а обновление раз в час не считается сменой ворот.
	old := time.Now().Add(-2 * time.Hour)
	b.round(t)
	if b.journal(t, "gate") != 1 {
		t.Fatalf("смена без смены: %d", b.journal(t, "gate"))
	}
	if err := os.Chtimes(b.c.Queue.Gate, old, old); err != nil {
		t.Fatal(err)
	}
	b.round(t)
	fi, err := os.Stat(b.c.Queue.Gate)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(fi.ModTime()) > time.Hour {
		t.Fatal("файл старше часа не обновлён")
	}
	if b.journal(t, "gate") != 1 {
		t.Fatalf("обновление по сроку не смена: %d", b.journal(t, "gate"))
	}
}

// Обход только docs/tasks: в ~/docs лежат видео и чужие проекты, и файл там не повод звать tk.
func TestQueueFingerprintOnlyTasks(t *testing.T) {
	b := newQueueBench(t, "[]", "[]")
	dirs, err := queueFingerprintDirs(&b.c.Queue)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(dirs, filepath.Join(b.dir, "docs", "tasks")) || slices.Contains(dirs, filepath.Join(b.dir, "docs")) {
		t.Fatalf("%v", dirs)
	}
	if !slices.Contains(dirs, filepath.Join(b.dir, "git", "p", "docs", "tasks")) {
		t.Fatalf("%v", dirs)
	}
}

func ticketsJSON(n int) string {
	var s strings.Builder
	s.WriteString("[")
	for i := range n {
		if i > 0 {
			s.WriteString(",")
		}
		fmt.Fprintf(&s, `{"repo":"p","slug":"t%d","status":"готово","prio":"LOW","title":"заголовок тикета подлиннее","created":"2026-11-03T12:00:00+01:00","path":"docs/tasks/20261103-1200-LOW-t%d.md"}`, i, i)
	}
	s.WriteString("]")
	return s.String()
}

// Файл состояния мал и не переписывается без нужды: запись aguard уже разбирается отдельно (tmpguard-track-swap, small-writers).
func TestQueueStateCompact(t *testing.T) {
	b := newQueueBench(t, ticketsJSON(2000), "[]")
	b.round(t)
	p := filepath.Join(b.a.dir("queue"), "state.json")
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > 4096 {
		t.Fatalf("state.json %d байт: кэш тикетов и отпечаток держать в памяти", fi.Size())
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	b.round(t)
	again, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !again.ModTime().Equal(old) {
		t.Fatal("state.json переписан без изменений")
	}
}

func TestQueueProjectCacheRefreshesOnlyChangedProject(t *testing.T) {
	b := newQueueBench(t, "[]", "[]")
	p, q := filepath.Join(b.dir, "git", "p"), filepath.Join(b.dir, "git", "q")
	for _, project := range []string{p, q} {
		if err := os.MkdirAll(filepath.Join(project, "docs", "tasks"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pOne := `{"repo":"p","slug":"one","status":"открыт","prio":"HIGH","title":"p one","back":"lead","path":"docs/tasks/20261121-1000-HIGH-one.md"}`
	qOne := `{"repo":"q","slug":"two","status":"готово","prio":"HIGH","title":"q two","back":"lead","path":"docs/tasks/20261121-1001-HIGH-two.md"}`
	writeTicket := func(project, name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(project, "docs", "tasks", name), []byte("ticket\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeTicket(p, "20261121-1000-HIGH-one.md")
	writeTicket(q, "20261121-1001-HIGH-two.md")
	calls := filepath.Join(b.dir, "calls.log")
	writeJSON := func(name, data string) string {
		t.Helper()
		path := filepath.Join(b.dir, name)
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	writeScript := func(pJSON, secondProject, secondJSON string) {
		t.Helper()
		pFile := writeJSON("p.json", "["+pJSON+"]")
		secondFile, allJSON := writeJSON("second.json", "[]"), "["+pJSON+"]"
		if secondJSON != "" {
			secondFile = writeJSON("second.json", "["+secondJSON+"]")
			allJSON = "[" + pJSON + "," + secondJSON + "]"
		}
		caseProject := secondProject
		if caseProject == "" {
			caseProject = filepath.Join(b.dir, "no-project")
		}
		allFile := writeJSON("all.json", allJSON)
		body := fmt.Sprintf("#!/bin/sh\nfor a in \"$@\"; do [ \"$a\" = --root ] && { cat %s; exit 0; }; done\nprintf '%%s\\n' \"$PWD\" >> %s\ncase \"$PWD\" in %s) cat %s ;; %s) cat %s ;; *) cat %s ;; esac\n", allFile, calls, p, pFile, caseProject, secondFile, writeJSON("empty.json", "[]"))
		if err := os.WriteFile(b.c.Queue.Tk, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeScript(pOne, q, qOne)
	first, err := b.a.cachedQueueTickets(context.Background(), b.c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 {
		t.Fatalf("начальный список: %+v", first)
	}
	if err := os.WriteFile(filepath.Join(p, "docs", "tasks", "20261121-1002-HIGH-three.md"), []byte("edited ticket\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pTwo := `{"repo":"p","slug":"three","status":"открыт","prio":"CRIT","title":"p three","back":"lead","path":"docs/tasks/20261121-1002-HIGH-three.md"}`
	writeScript(pOne+","+pTwo, q, qOne)
	if err := os.WriteFile(calls, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cached, err := b.a.cachedQueueTickets(context.Background(), b.c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	called, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(called)); got != p {
		t.Fatalf("tk ls вызовы при правке p: %q; ожидался только %q", got, p)
	}
	full, err := b.a.queueTickets(context.Background(), b.c)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cached, full) {
		t.Fatalf("кешированный вывод не совпал с полным:\nкеш: %+v\nполный: %+v", cached, full)
	}
	wake := queue.WakeTicket("p", p, "p/first")
	view := queue.View{Now: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC), Lead: "lead", Alive: func(string) bool { return true }, Free: func(string) bool { return true }, WakeFree: func(string) bool { return true }, Serious: func(string) bool { return true }, Next: func(_, _ string) (queue.Ticket, bool, error) { return wake, true, nil }, Session: func(string) string { return "session" }}
	gotNotices := (&queue.State{Seeded: true}).Step(append(slices.Clone(cached), wake), view)
	wantNotices := (&queue.State{Seeded: true}).Step(append(slices.Clone(full), wake), view)
	if !reflect.DeepEqual(gotNotices, wantNotices) {
		t.Fatalf("письма/wake/back различаются:\nкеш: %+v\nполный: %+v", gotNotices, wantNotices)
	}
	kinds := map[string]bool{}
	for _, notice := range gotNotices {
		kinds[notice.Kind] = true
	}
	if !kinds[queue.KindCrit] || !kinds[queue.KindBack] || !kinds[queue.KindWake] {
		t.Fatalf("тест должен покрыть обычное письмо, wake и back, получены: %+v", gotNotices)
	}

	// Переименование исключает старый ключ и запрашивает список только у нового пути.
	r := filepath.Join(b.dir, "git", "renamed-q")
	if err := os.Rename(q, r); err != nil {
		t.Fatal(err)
	}
	rOne := strings.ReplaceAll(qOne, `"repo":"q"`, `"repo":"renamed-q"`)
	writeScript(pOne+","+pTwo, r, rOne)
	if err := os.WriteFile(calls, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	renameResult, err := b.a.cachedQueueTickets(context.Background(), b.c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	called, err = os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(called)); got != r {
		t.Fatalf("tk ls вызовы после переименования: %q; ожидался только %q", got, r)
	}
	full, err = b.a.queueTickets(context.Background(), b.c)
	if err != nil || !reflect.DeepEqual(renameResult, full) {
		t.Fatalf("вывод после переименования не совпал с полным: err=%v кеш=%+v полный=%+v", err, renameResult, full)
	}

	// Удалённый проект исчезает из собранного вывода без повторного tk ls для оставшихся проектов.
	if err := os.RemoveAll(r); err != nil {
		t.Fatal(err)
	}
	writeScript(pOne+","+pTwo, "", "")
	if err := os.WriteFile(calls, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	removedResult, err := b.a.cachedQueueTickets(context.Background(), b.c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(callsBytes(t, calls)) != 0 {
		t.Fatalf("удаление проекта неожиданно запустило tk ls: %s", callsBytes(t, calls))
	}
	full, err = b.a.queueTickets(context.Background(), b.c)
	if err != nil || !reflect.DeepEqual(removedResult, full) {
		t.Fatalf("вывод после удаления не совпал с полным: err=%v кеш=%+v полный=%+v", err, removedResult, full)
	}

	// Новый проект добавляется в вывод, а остальные остаются в кеше.
	s := filepath.Join(b.dir, "git", "new-project")
	if err := os.MkdirAll(filepath.Join(s, "docs", "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTicket(s, "20261121-1003-HIGH-four.md")
	sOne := `{"repo":"new-project","slug":"four","status":"открыт","prio":"HIGH","title":"new project","back":"lead","path":"docs/tasks/20261121-1003-HIGH-four.md"}`
	writeScript(pOne+","+pTwo, s, sOne)
	if err := os.WriteFile(calls, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	newResult, err := b.a.cachedQueueTickets(context.Background(), b.c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	called, err = os.ReadFile(calls)
	if err != nil || strings.TrimSpace(string(called)) != s {
		t.Fatalf("tk ls для нового проекта: calls=%q err=%v", called, err)
	}
	full, err = b.a.queueTickets(context.Background(), b.c)
	if err != nil || !reflect.DeepEqual(newResult, full) {
		t.Fatalf("вывод после добавления проекта не совпал с полным: err=%v кеш=%+v полный=%+v", err, newResult, full)
	}
}

const revTicket = "---\nstatus: открыт\nback: lead\n---\n# t\n"

func revWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Папка без .git и docs/tasks не является проектом: иначе tk ищет задачи выше и дублирует корневой тикет.
func TestRevNonGitDirWithoutTasks(t *testing.T) {
	b := newQueueBench(t, "[]", "[]")
	tk := "/home/aisee/bin/tk"
	if _, err := os.Stat(tk); err != nil {
		t.Skipf("живой tk недоступен: %v", err)
	}
	t.Setenv("HOME", b.dir)
	b.c.Queue.Tk = tk
	b.c.Queue.Dirs = nil
	gitRoot := filepath.Join(b.dir, "git")
	revWrite(t, filepath.Join(gitRoot, "docs", "tasks", "20261121-1000-HIGH-rootone.md"), revTicket)
	revWrite(t, filepath.Join(gitRoot, "proj", "docs", "tasks", "20261121-1001-HIGH-projone.md"), revTicket)
	if out, err := exec.Command("git", "init", "-q", filepath.Join(gitRoot, "proj")).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	if err := os.MkdirAll(filepath.Join(gitRoot, "zhdanov"), 0o755); err != nil {
		t.Fatal(err)
	}
	full, err := b.a.queueTickets(context.Background(), b.c)
	if err != nil {
		t.Fatal(err)
	}
	cached, err := b.a.cachedQueueTickets(context.Background(), b.c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(full, cached) {
		t.Fatalf("списки различаются байт-в-байт:\nполный: %+v\nкеш:    %+v", full, cached)
	}
	fullBytes, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	cachedBytes, err := json.Marshal(cached)
	if err != nil {
		t.Fatal(err)
	}
	if string(fullBytes) != string(cachedBytes) {
		t.Fatalf("JSON списков не совпал байт-в-байт:\nполный: %s\nкеш:    %s", fullBytes, cachedBytes)
	}
	if len(cached) != 2 {
		t.Fatalf("ожидались корневой и проектный тикеты без дубля: %+v", cached)
	}
	revWrite(t, filepath.Join(gitRoot, "proj", "docs", "tasks", "20261121-1002-HIGH-projtwo.md"), revTicket)
	cached, err = b.a.cachedQueueTickets(context.Background(), b.c, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	full, err = b.a.queueTickets(context.Background(), b.c)
	if err != nil || !reflect.DeepEqual(full, cached) || len(cached) != 3 {
		t.Fatalf("частичное обновление не совпало с --root: err=%v полный=%+v кеш=%+v", err, full, cached)
	}
}

func TestRevOwnersStaleOnPartialRefresh(t *testing.T) {
	b := newQueueBench(t, "[]", "[]")
	tk := "/home/aisee/bin/tk"
	if _, err := os.Stat(tk); err != nil {
		t.Skipf("живой tk недоступен: %v", err)
	}
	t.Setenv("HOME", b.dir)
	b.c.Queue.Tk = tk
	b.c.Queue.Dirs = nil
	gitRoot := filepath.Join(b.dir, "git")
	for _, project := range []string{"p", "q"} {
		dir := filepath.Join(gitRoot, project)
		revWrite(t, filepath.Join(dir, "docs", "tasks", "20261121-1000-HIGH-"+project+"one.md"), revTicket)
		if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
			t.Fatal(err, string(out))
		}
	}
	now := time.Now()
	if _, err := b.a.cachedQueueTickets(context.Background(), b.c, now); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(b.dir, "owner-calls.log")
	wrapper := filepath.Join(b.dir, "tk-count")
	if err := os.WriteFile(wrapper, fmt.Appendf(nil, "#!/bin/sh\nprintf '%%s|%%s\\n' \"$PWD\" \"$*\" >> %s\nexec %s \"$@\"\n", calls, tk), 0o755); err != nil {
		t.Fatal(err)
	}
	b.c.Queue.Tk = wrapper
	b.c.Admins = map[string]string{filepath.Join(gitRoot, "q"): "q-admin"}
	revWrite(t, filepath.Join(gitRoot, "p", "docs", "tasks", "20261121-1001-HIGH-ptwo.md"), revTicket)
	cached, err := b.a.cachedQueueTickets(context.Background(), b.c, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	called, err := os.ReadFile(calls)
	if err != nil || !strings.Contains(string(called), "--root "+gitRoot) || strings.Count(string(called), "\n") != 1 {
		t.Fatalf("смена Admins должна дать один полный root проход: calls=%q err=%v", called, err)
	}
	if _, err := b.a.cachedQueueTickets(context.Background(), b.c, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	called, err = os.ReadFile(calls)
	if err != nil || strings.Count(string(called), "\n") != 1 {
		t.Fatalf("после обновления Admins кеш должен быть прогрет: calls=%q err=%v", called, err)
	}
	full, err := b.a.queueTickets(context.Background(), b.c)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cached, full) {
		t.Fatalf("кеш не сбросился после смены Admins:\nкеш: %+v\nполный: %+v", cached, full)
	}
}

func TestRevTransferSkipDir(t *testing.T) {
	b := newQueueBench(t, "[]", "[]")
	line := `{"slug":"x","stem":"20261104-1100-HIGH-x","back":"lead","key":"TRK-7"}` + "\n"
	tasks := filepath.Join(b.dir, "docs", "tasks")
	if err := os.WriteFile(filepath.Join(tasks, ".transferred.jsonl"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	b.c.Queue.Skip = []string{filepath.Join(b.dir, "docs")}
	full, err := b.a.queueTickets(context.Background(), b.c)
	if err != nil {
		t.Fatal(err)
	}
	dirs := []string{tasks}
	cached, err := b.a.queueTicketsProject(context.Background(), b.c, filepath.Join(b.dir, "docs"), nil, dirs)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(full, cached) {
		t.Fatalf("перенос из Skip попал в очередь: полный=%+v кеш=%+v", full, cached)
	}
}

func TestQueueProjectChangesDeduplicatesDirectory(t *testing.T) {
	b := newQueueBench(t, "[]", "[]")
	tasks := filepath.Join(b.dir, "git", "p", "docs", "tasks")
	projects, changed, _, err := queueProjectChanges([]string{tasks, tasks}, &b.c.Queue, &queueTicketCache{})
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || len(changed) != 1 {
		t.Fatalf("одна папка учтена повторно: projects=%v changed=%v", projects, changed)
	}
}

func TestQueueProjectChangesSortsProjectsAndTickets(t *testing.T) {
	b := newQueueBench(t, "[]", "[]")
	q, p := filepath.Join(b.dir, "git", "q", "docs", "tasks"), filepath.Join(b.dir, "git", "p", "docs", "tasks")
	for _, tasks := range []string{q, p} {
		if err := os.MkdirAll(tasks, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_, changed, _, err := queueProjectChanges([]string{q, p}, &b.c.Queue, &queueTicketCache{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(changed, []string{filepath.Join(b.dir, "git", "p"), filepath.Join(b.dir, "git", "q")}) {
		t.Fatalf("порядок проектов неустойчивый: %v", changed)
	}
	projects := map[string]queueProjectCache{
		"p": {Tickets: []queue.Ticket{{Key: "p/2"}, {Key: "p/1"}}},
		"q": {Tickets: []queue.Ticket{{Key: "q/1"}}},
	}
	for range 20 {
		tickets := queueProjectTickets(projects)
		keys := make([]string, len(tickets))
		for i := range tickets {
			keys[i] = tickets[i].Key
		}
		if !slices.Equal(keys, []string{"p/1", "p/2", "q/1"}) {
			t.Fatalf("общий список не отсортирован: %v", keys)
		}
	}
}

func TestQueueProjectTTLUsesOldestProjectAndScansRoot(t *testing.T) {
	b := newQueueBench(t, "[]", "[]")
	root := b.c.Queue.GitRoots[0]
	q := filepath.Join(root, "q")
	if err := os.MkdirAll(filepath.Join(q, "docs", "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	b.c.Queue.Dirs = nil
	first := time.Now()
	if _, err := b.a.cachedQueueTickets(context.Background(), b.c, first); err != nil {
		t.Fatal(err)
	}
	for project, entry := range b.a.queueCache.Projects {
		entry.ScannedAt = first
		if project == filepath.Join(root, "p") {
			entry.ScannedAt = first.Add(-11 * time.Minute)
		}
		b.a.queueCache.Projects[project] = entry
	}
	oldTk, calls := b.c.Queue.Tk, filepath.Join(b.dir, "root-calls.log")
	wrapper := filepath.Join(b.dir, "tk-count")
	body := fmt.Sprintf("#!/bin/sh\nprintf '%%s|%%s\\n' \"$PWD\" \"$*\" >> %s\nexec %s \"$@\"\n", calls, oldTk)
	if err := os.WriteFile(wrapper, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	b.c.Queue.Tk = wrapper
	if _, err := b.a.cachedQueueTickets(context.Background(), b.c, first.Add(12*time.Minute)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], "--root "+root) {
		t.Fatalf("истечение срока должно дать один полный --root список, вызовы: %q", data)
	}
	for project, entry := range b.a.queueCache.Projects {
		if !entry.ScannedAt.Equal(first.Add(12 * time.Minute)) {
			t.Fatalf("после полного прохода %s отсканирован в %s", project, entry.ScannedAt)
		}
	}
}

// Новые папки в корне учитываются только по правилам tk taskRepos: у проекта есть docs/tasks,
// а worktree с .git-файлом и обычная папка не становятся отдельными проектами.
func TestQueueNewRootDirsFilteredBetweenScans(t *testing.T) {
	b := newQueueBench(t, "[]", "[]")
	b.c.Queue.Dirs = nil
	root := b.c.Queue.GitRoots[0]
	if err := os.RemoveAll(filepath.Join(root, "p", "docs", "tasks")); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(root, "base")
	if err := os.MkdirAll(filepath.Join(base, "docs", "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := b.a.cachedQueueTickets(context.Background(), b.c, time.Now()); err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(root, "worktree")
	if err := os.MkdirAll(filepath.Join(worktree, "docs", "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: ../base/.git/worktrees/wt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "not-a-project"), 0o755); err != nil {
		t.Fatal(err)
	}
	dirs, err := queueFingerprintDirs(&b.c.Queue)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(base, "docs", "tasks")}
	if !slices.Equal(dirs, want) {
		t.Fatalf("после появления worktree и обычной папки: dirs=%v want=%v", dirs, want)
	}
	projects, changed, topologyChanged, err := queueProjectChanges(dirs, &b.c.Queue, &b.a.queueCache)
	if err != nil || len(changed) != 0 || topologyChanged || len(projects) != 1 {
		t.Fatalf("неизменившийся набор проектов: projects=%v changed=%v topology=%v err=%v", projects, changed, topologyChanged, err)
	}
}

// Папка из Dirs с layout tasks/ остаётся проектом самой папки; частичный tk ls не должен
// трактовать её как --root и переписывать путь тикета в docs/tasks/.
func TestQueueNewDirsTaskLayoutUsesProjectMode(t *testing.T) {
	b := newQueueBench(t, "[]", "[]")
	if _, err := b.a.cachedQueueTickets(context.Background(), b.c, time.Now()); err != nil {
		t.Fatal(err)
	}
	ticketPath := "tasks/20261121-1010-HIGH-new.md"
	if err := os.WriteFile(filepath.Join(b.c.Queue.Dirs[0], ticketPath), []byte("ticket\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	docsJSON := filepath.Join(b.dir, "docs.json")
	data := `[{"slug":"new","status":"открыт","prio":"HIGH","title":"new task","path":"` + ticketPath + `"}]`
	if err := os.WriteFile(docsJSON, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	tickets, err := b.a.cachedQueueTickets(context.Background(), b.c, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(tickets) != 1 || tickets[0].Path != ticketPath || tickets[0].Dir != b.c.Queue.Dirs[0] {
		t.Fatalf("новый тикет из Dirs с layout tasks/: %+v", tickets)
	}
}

// Проверка ревьюера: полный проход по TTL не накапливает дубли в проектных срезах,
// последующее частичное обновление совпадает с текущим полным выводом.
func TestRev2TTLThenPartialNoDuplicates(t *testing.T) {
	b := newQueueBench(t, "[]", "[]")
	b.c.Queue.Dirs = nil
	root := b.c.Queue.GitRoots[0]
	p, q := filepath.Join(root, "p"), filepath.Join(root, "q")
	for _, project := range []string{p, q} {
		if err := os.MkdirAll(filepath.Join(project, "docs", "tasks"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	rootJSON := filepath.Join(b.dir, "root.json")
	pJSON := filepath.Join(b.dir, "p.json")
	rootList := `[{"repo":"p","slug":"one","status":"открыт","prio":"HIGH","title":"p one","path":"docs/tasks/20261121-1000-HIGH-one.md"},{"repo":"q","slug":"two","status":"готово","prio":"HIGH","title":"q two","path":"docs/tasks/20261121-1001-HIGH-two.md"}]`
	if err := os.WriteFile(rootJSON, []byte(rootList), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pJSON, []byte(rootList[:strings.LastIndex(rootList, ",")]+`]`), 0o600); err != nil {
		t.Fatal(err)
	}
	// В режиме проекта отдаём p.json, в режиме --root — root.json.
	tk := filepath.Join(b.dir, "tk-mode")
	script := fmt.Sprintf("#!/bin/sh\nfor a in \"$@\"; do [ \"$a\" = --root ] && { cat %s; exit; }; done\ncase \"$PWD\" in %s) cat %s ;; *) cat %s ;; esac\n", rootJSON, p, pJSON, rootJSON)
	if err := os.WriteFile(tk, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	b.c.Queue.Tk = tk
	now := time.Now()
	if _, err := b.a.cachedQueueTickets(context.Background(), b.c, now); err != nil {
		t.Fatal(err)
	}
	if _, err := b.a.cachedQueueTickets(context.Background(), b.c, now.Add(11*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(b.a.queueCache.Projects[p].Tickets) != 1 || len(b.a.queueCache.Projects[q].Tickets) != 1 {
		t.Fatalf("full --root refresh duplicated project tickets: p=%d q=%d", len(b.a.queueCache.Projects[p].Tickets), len(b.a.queueCache.Projects[q].Tickets))
	}
	if err := os.WriteFile(filepath.Join(p, "docs", "tasks", "20261121-1002-HIGH-three.md"), []byte("ticket\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pList := `[{"slug":"one","status":"открыт","prio":"HIGH","title":"p one","path":"docs/tasks/20261121-1000-HIGH-one.md"},{"slug":"three","status":"открыт","prio":"HIGH","title":"p three","path":"docs/tasks/20261121-1002-HIGH-three.md"}]`
	if err := os.WriteFile(pJSON, []byte(pList), 0o600); err != nil {
		t.Fatal(err)
	}
	fullList := `[{"repo":"p","slug":"one","status":"открыт","prio":"HIGH","title":"p one","path":"docs/tasks/20261121-1000-HIGH-one.md"},{"repo":"p","slug":"three","status":"открыт","prio":"HIGH","title":"p three","path":"docs/tasks/20261121-1002-HIGH-three.md"},{"repo":"q","slug":"two","status":"готово","prio":"HIGH","title":"q two","path":"docs/tasks/20261121-1001-HIGH-two.md"}]`
	if err := os.WriteFile(rootJSON, []byte(fullList), 0o600); err != nil {
		t.Fatal(err)
	}
	cached, err := b.a.cachedQueueTickets(context.Background(), b.c, now.Add(12*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	full, err := b.a.queueTickets(context.Background(), b.c)
	if err != nil || !reflect.DeepEqual(cached, full) {
		t.Fatalf("после полного прохода и частичного обновления: err=%v cached=%+v full=%+v", err, cached, full)
	}
}

func callsBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Отметка переживает рестарт демона: новый app, тот же файл, письма и журнал не повторяются.
func TestQueueStateSurvivesRestart(t *testing.T) {
	crit := `[{"repo":"p","slug":"c","status":"открыт","prio":"CRIT","title":"t","created":"2026-11-03T12:00:00+01:00","path":"docs/tasks/20261103-1200-CRIT-c.md"}]`
	b := newQueueBench(t, crit, "[]")
	b.round(t)
	if b.journal(t, queue.KindUnreachable) != 1 {
		t.Fatalf("журнал: %d", b.journal(t, queue.KindUnreachable))
	}
	fresh := *b.a
	b.a = &fresh
	b.round(t)
	if b.journal(t, queue.KindUnreachable) != 1 {
		t.Fatalf("после рестарта повтор: %d", b.journal(t, queue.KindUnreachable))
	}
	data, err := os.ReadFile(filepath.Join(b.a.dir("queue"), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "p/20261103-1200-c") {
		t.Fatalf("ключ по имени файла: %s", data)
	}
}

func TestQueueViewCritCopyAfter(t *testing.T) {
	b := newQueueBench(t, "[]", "[]")
	start := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	alive := map[string]session.Info{"glint-master": {}, "lead": {}}
	v := queueView(t.Context(), b.c, alive, fakeJudge{}, start, b.a, nil, nil)
	s := &queue.State{}
	ticket := queue.Ticket{Key: "glint/crit", Project: "glint", Slug: "crit", Prio: "CRIT", Status: "открыт", Master: "glint-master", Admin: "git-admin"}
	confirm := func(ns []queue.Notice) {
		for _, n := range ns {
			s.Confirm(n, v)
		}
	}
	if ns := s.Step([]queue.Ticket{ticket}, v); !queueHas(ns, queue.KindCrit, "glint-master") || queueHas(ns, queue.KindCritCopy, "lead") {
		t.Fatalf("первый проход через queueView: %+v", ns)
	} else {
		confirm(ns)
	}
	v.Now = start.Add(14 * time.Minute)
	if ns := s.Step([]queue.Ticket{ticket}, v); queueHas(ns, queue.KindCritCopy, "lead") {
		t.Fatalf("до срока из queueView: %+v", ns)
	}
	v.Now = start.Add(15 * time.Minute)
	ns := s.Step([]queue.Ticket{ticket}, v)
	if !queueHas(ns, queue.KindCritCopy, "lead") {
		t.Fatalf("срок из queueView: %+v", ns)
	}
}

func queueHas(ns []queue.Notice, kind, to string) bool {
	for _, n := range ns {
		if n.Kind == kind && n.To == to {
			return true
		}
	}
	return false
}

// Ключ — из имени файла, без PRIO и DIFF: смена приоритета не заводит новый тикет.
func TestQueueKeyFromPath(t *testing.T) {
	for _, path := range []string{"docs/tasks/20261103-1200-HIGH-x-y.md", "docs/tasks/20261103-1200-CRIT-DO-x-y.md"} {
		if got := queueKey("p", "x-y", path); got != "p/20261103-1200-x-y" {
			t.Errorf("%s: %s", path, got)
		}
	}
}

func TestQueueOwners(t *testing.T) {
	home := "/home/u"
	table := map[string]string{"/home/u": "lead", "/home/u/git": "git-admin", "/home/u/git/sc2demu": "demu-master"}
	admins := map[string]string{"/home/u/git": "git-admin", "/home/u/work": "work-admin"}
	cases := []struct{ dir, master, admin string }{
		{"/home/u/git/glint", "glint-master", "git-admin"},
		{"/home/u/git/sc2demu", "demu-master", "git-admin"},
		{"/home/u/git", "git-admin", "git-admin"},
		{"/home/u/work/reddy", "reddy-master", "work-admin"},
		{"/home/u/docs", "lead", "lead"}, // папка лида: имени в таблице нет
	}
	for _, c := range cases {
		m, ad := queueOwners(c.dir, home, "lead", table, admins)
		if m != c.master || ad != c.admin {
			t.Errorf("%s: %s, %s", c.dir, m, ad)
		}
	}
}

// Отчёт видит только дневные журналы: история OpenAI рядом не считается письмами.
func TestQueueReportIgnoresHistory(t *testing.T) {
	b := newQueueBench(t, "[]", "[]")
	var out strings.Builder
	b.a.out = &out
	hist := filepath.Join(b.a.dir("queue"), "openai-history.jsonl")
	if err := os.WriteFile(hist, fmt.Appendf(nil, `{"at":%q,"sevenDayResetsAt":%q,"sevenDayUsedPercentage":10,"account":"work"}`+"\n", time.Now().Format(time.RFC3339), time.Now().Format(time.RFC3339)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.a.queueReport(24*time.Hour, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"kinds":{}`) {
		t.Fatalf("%s", out.String())
	}
}
