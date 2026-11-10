package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
	tk := write("tk", fmt.Sprintf("#!/bin/sh\nfor a in \"$@\"; do [ \"$a\" = --root ] && { cat %s; exit 0; }; done\ncat %s\n", git, docs))
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
			FreeAfter: policy.Duration(2 * time.Minute), GitRoots: []string{filepath.Join(dir, "git")},
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
