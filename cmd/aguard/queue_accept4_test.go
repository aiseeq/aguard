package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/codexwatch"
	"github.com/aiseeq/aguard/internal/queue"
	"github.com/aiseeq/aguard/internal/restart"
)

// Приёмка прохода queue, четвёртый заход: находки независимой проверки. Написана мастером, боец её не правит.

const queueCritJSON = `[{"repo":"p","slug":"c","status":"открыт","prio":"CRIT","title":"t","path":"docs/tasks/20261103-1200-CRIT-c.md"}]`

const queueHighJSON = `[{"repo":"p","slug":"c","status":"открыт","prio":"HIGH","title":"t","path":"docs/tasks/20261103-1200-HIGH-c.md"}]`

func (b *queueBench) captureLog() *strings.Builder {
	var log strings.Builder
	b.a.log = slog.New(slog.NewTextHandler(&log, nil))
	return &log
}

func (b *queueBench) writeHistory(t *testing.T, rows map[string]time.Duration) {
	t.Helper()
	now := time.Now()
	var body strings.Builder
	for account, age := range rows {
		fmt.Fprintf(&body, `{"at":%q,"sevenDayResetsAt":%q,"sevenDayUsedPercentage":20,"account":%q}`+"\n",
			now.Add(-age).Format(time.RFC3339), now.Add(72*time.Hour).Format(time.RFC3339), account)
	}
	if err := os.WriteFile(b.c.Queue.History, []byte(body.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (b *queueBench) ageGate(t *testing.T) {
	t.Helper()
	stale := time.Now().Add(-2 * time.Hour) // без смены open и lane файл обновляется раз в час
	if err := os.Chtimes(b.c.Queue.Gate, stale, stale); err != nil {
		t.Fatal(err)
	}
}

func (b *queueBench) statePath() string { return filepath.Join(b.a.dir("queue"), "state.json") }

// История Claude молчит больше суток: это сбой входа, а не запас 99. gate.json остаётся прежним, журнал говорит о причине.
func TestQueueStaleHistoryKeepsGate(t *testing.T) {
	b := newQueueBench(t, "[]", "[]")
	b.round(t)
	before, err := os.ReadFile(b.c.Queue.Gate)
	if err != nil {
		t.Fatal(err)
	}
	b.writeHistory(t, map[string]time.Duration{"a": 25 * time.Hour, "b": 30 * time.Hour})
	b.ageGate(t)
	log := b.captureLog()
	b.round(t)
	after, err := os.ReadFile(b.c.Queue.Gate)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("устаревшая история переписала ворота:\n%s\n%s", before, after)
	}
	if !strings.Contains(log.String(), "история лимитов") {
		t.Fatalf("причины нет в журнале: %s", log)
	}
}

// Самая свежая строка моложе суток: история жива, даже если другая учётка молчит дольше. Наибольший разрыв живого файла 7 ч.
func TestQueueHistoryFreshEnough(t *testing.T) {
	for name, rows := range map[string]map[string]time.Duration{
		"23 ч":          {"a": 23 * time.Hour},
		"вторая молчит": {"a": time.Hour, "b": 30 * time.Hour},
	} {
		t.Run(name, func(t *testing.T) {
			b := newQueueBench(t, "[]", "[]")
			b.round(t)
			before, err := queue.ReadGate(b.c.Queue.Gate)
			if err != nil {
				t.Fatal(err)
			}
			b.writeHistory(t, rows)
			b.ageGate(t)
			time.Sleep(10 * time.Millisecond)
			b.round(t)
			after, err := queue.ReadGate(b.c.Queue.Gate)
			if err != nil {
				t.Fatal(err)
			}
			if !after.At.After(before.At) {
				t.Fatalf("ворота не обновлены: %v -> %v", before.At, after.At)
			}
		})
	}
}

// Битый state.json не останавливает CRIT: файл отложен для разбора, проход идёт с пустого состояния, предупреждение в журнале.
func TestQueueBrokenStateSetAside(t *testing.T) {
	b := newQueueBench(t, queueCritJSON, "[]")
	const garbage = "{не json"
	if err := os.WriteFile(b.statePath(), []byte(garbage), 0o600); err != nil {
		t.Fatal(err)
	}
	log := b.captureLog()
	b.round(t)
	if n := b.journal(t, queue.KindUnreachable); n != 1 {
		t.Fatalf("CRIT не прошёл через битое состояние, журнал: %d", n)
	}
	if !strings.Contains(log.String(), "состояние") {
		t.Fatalf("нет предупреждения: %s", log)
	}
	aside, err := filepath.Glob(b.statePath() + ".*")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, name := range aside {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		found = found || string(data) == garbage
	}
	if !found {
		t.Fatalf("битый файл не отложен: %v", aside)
	}
	loaded, err := queue.LoadState(b.statePath())
	if err != nil {
		t.Fatalf("новое состояние: %v", err)
	}
	if !loaded.Seeded {
		t.Fatal("новое состояние не записано")
	}
	b.round(t)
	if n := b.journal(t, queue.KindUnreachable); n != 1 {
		t.Fatalf("повтор после восстановления: %d", n)
	}
}

func (b *queueBench) breakTk(t *testing.T) {
	t.Helper()
	broken := filepath.Join(b.dir, "tk-broken")
	if err := os.WriteFile(broken, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	b.c.Queue.Tk = broken
}

// tk ls сломан, список устарел или папки изменились: письма идут по последнему удачному списку, а не останавливаются.
func TestQueueTkFailUsesLastList(t *testing.T) {
	for name, outdate := range map[string]func(*testing.T, *queueBench){
		"кеш старше 10 мин": func(_ *testing.T, b *queueBench) {
			b.a.queueCache.ScannedAt = time.Now().Add(-11 * time.Minute)
		},
		"папка изменилась": func(t *testing.T, b *queueBench) {
			name := filepath.Join(b.dir, "git", "p", "docs", "tasks", "20261104-1000-LOW-new.md")
			if err := os.WriteFile(name, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			b := newQueueBench(t, queueCritJSON, "[]")
			b.round(t)
			if n := b.journal(t, queue.KindUnreachable); n != 1 {
				t.Fatalf("первый проход: %d", n)
			}
			b.breakTk(t)
			outdate(t, b)
			if err := os.Remove(b.statePath()); err != nil { // CRIT снова считается: видно, что список тот же
				t.Fatal(err)
			}
			log := b.captureLog()
			b.round(t)
			if n := b.journal(t, queue.KindUnreachable); n != 2 {
				t.Fatalf("проход остановился без списка: %d", n)
			}
			if !strings.Contains(log.String(), "tk ls") {
				t.Fatalf("нет предупреждения: %s", log)
			}
		})
	}
}

// Удачного списка ещё не было: состояние не трогаем, пустой список не стирает отметки.
func TestQueueTkFailNoListKeepsState(t *testing.T) {
	b := newQueueBench(t, queueCritJSON, "[]")
	b.round(t)
	before, err := os.ReadFile(b.statePath())
	if err != nil {
		t.Fatal(err)
	}
	fresh := *b.a
	fresh.queueCache = queueTicketCache{}
	b.a = &fresh
	b.breakTk(t)
	b.round(t)
	after, err := os.ReadFile(b.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("состояние без списка изменилось:\n%s\n%s", before, after)
	}
}

// noteTk — tk со списком bench, команда note пишет строку в notes.log; пока есть файл note-fail, она падает.
func (b *queueBench) noteTk(t *testing.T) (notes, fail string) {
	t.Helper()
	notes, fail = filepath.Join(b.dir, "notes.log"), filepath.Join(b.dir, "note-fail")
	body := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = note ]; then\n [ -e %s ] && { echo занято >&2; exit 1; }\n echo \"$2 $3\" >> %s\n exit 0\nfi\nexec %s \"$@\"\n", fail, notes, b.c.Queue.Tk)
	wrapper := filepath.Join(b.dir, "tk-notes")
	if err := os.WriteFile(wrapper, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	b.c.Queue.Tk = wrapper
	return notes, fail
}

// sentHigh — состояние после письма: строка HIGH уже ушла, запись в журнал тикета ещё не сделана.
func (b *queueBench) sentHigh(t *testing.T, serious bool) {
	t.Helper()
	tickets, err := b.a.queueTickets(context.Background(), b.c)
	if err != nil {
		t.Fatal(err)
	}
	yes := func(string) bool { return true }
	view := queue.View{Now: time.Now(), Lead: "lead", Alive: yes, Free: yes, Serious: func(string) bool { return serious }}
	state := queue.State{Seeded: true}
	sent := 0
	for _, n := range state.Step(tickets, view) {
		if n.Kind == queue.KindHigh {
			state.Confirm(n, view)
			sent++
		}
	}
	if sent != 1 {
		t.Fatalf("строк HIGH: %d", sent)
	}
	if err := state.Save(b.statePath()); err != nil {
		t.Fatal(err)
	}
}

func noteCount(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

// Письмо ушло, tk note упал: запись в журнал тикета повторяется на следующих проходах, переживает рестарт и делается один раз.
func TestQueueTicketNoteRetried(t *testing.T) {
	b := newQueueBench(t, queueHighJSON, "[]")
	notes, fail := b.noteTk(t)
	if err := os.WriteFile(fail, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	b.sentHigh(t, true)
	log := b.captureLog()
	b.round(t)
	if n := noteCount(t, notes); n != 0 {
		t.Fatalf("записей при упавшем tk: %d", n)
	}
	if !strings.Contains(log.String(), "tk note") {
		t.Fatalf("нет предупреждения: %s", log)
	}
	fresh := *b.a
	fresh.queueCache = queueTicketCache{}
	b.a = &fresh
	if err := os.Remove(fail); err != nil {
		t.Fatal(err)
	}
	b.round(t)
	if n := noteCount(t, notes); n != 1 {
		t.Fatalf("после починки tk: %d", n)
	}
	b.round(t)
	if n := noteCount(t, notes); n != 1 {
		t.Fatalf("повтор записи: %d", n)
	}
}

// Режим журнала письма не шлёт, и в журнал тикета писать нечего.
func TestQueueLogModeNoTicketNote(t *testing.T) {
	b := newQueueBench(t, queueHighJSON, "[]")
	notes, _ := b.noteTk(t)
	b.sentHigh(t, false)
	b.round(t)
	if n := noteCount(t, notes); n != 0 {
		t.Fatalf("запись в журнал тикета без письма: %d", n)
	}
}

// Тикет исчез из списка: ждущая запись снимается, а не повторяется вечно.
func TestQueueTicketNoteGoneTicket(t *testing.T) {
	b := newQueueBench(t, queueHighJSON, "[]")
	notes, fail := b.noteTk(t)
	if err := os.WriteFile(fail, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	b.sentHigh(t, true)
	b.round(t)
	if err := os.WriteFile(filepath.Join(b.dir, "git.json"), []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	b.a.queueCache = queueTicketCache{}
	if err := os.Remove(fail); err != nil {
		t.Fatal(err)
	}
	b.round(t)
	if n := noteCount(t, notes); n != 0 {
		t.Fatalf("запись в исчезнувший тикет: %d", n)
	}
	state, err := queue.LoadState(b.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Tickets) != 0 {
		t.Fatalf("след тикета в состоянии: %+v", state.Tickets)
	}
}

// Сухой прогон ничего не пишет: битое состояние остаётся на месте, а CRIT он всё равно показывает.
func TestQueueDryRunBrokenStateUntouched(t *testing.T) {
	b := newQueueBench(t, queueCritJSON, "[]")
	const garbage = "{не json"
	if err := os.WriteFile(b.statePath(), []byte(garbage), 0o600); err != nil {
		t.Fatal(err)
	}
	out := &strings.Builder{}
	b.a.out = out
	if err := b.a.queueRound(context.Background(), b.c, restart.NewJudge(b.dir), true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(b.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != garbage {
		t.Fatalf("сухой прогон изменил состояние: %s", data)
	}
	aside, err := filepath.Glob(b.statePath() + ".*")
	if err != nil {
		t.Fatal(err)
	}
	if len(aside) != 0 {
		t.Fatalf("сухой прогон отложил файл: %v", aside)
	}
	if !strings.Contains(out.String(), queue.KindUnreachable) {
		t.Fatalf("CRIT не показан: %s", out)
	}
}

// tk ответил мусором вместо JSON: это тоже сбой списка, письма идут по последнему удачному.
func TestQueueTkGarbageUsesLastList(t *testing.T) {
	b := newQueueBench(t, queueCritJSON, "[]")
	b.round(t)
	garbage := filepath.Join(b.dir, "tk-garbage")
	if err := os.WriteFile(garbage, []byte("#!/bin/sh\necho не json\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	b.c.Queue.Tk = garbage
	b.a.queueCache.ScannedAt = time.Now().Add(-11 * time.Minute)
	if err := os.Remove(b.statePath()); err != nil {
		t.Fatal(err)
	}
	b.round(t)
	if n := b.journal(t, queue.KindUnreachable); n != 2 {
		t.Fatalf("проход остановился без списка: %d", n)
	}
}

// Поля, по которым tk next решает, виден ли тикет, доходят от tk ls.
func TestQueueTicketExecutorAndDue(t *testing.T) {
	b := newQueueBench(t, `[{"repo":"p","slug":"c","status":"ждёт","prio":"LOW","title":"t","executor":"p-master","overdue":true,"path":"docs/tasks/20261103-1205-LOW-c.md"}]`, "[]")
	ts, err := b.a.queueTickets(context.Background(), b.c)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 1 || ts[0].Executor != "p-master" || !ts[0].WaitDue {
		t.Fatalf("%+v", ts)
	}
}

func (b *queueBench) openAIAccount(t *testing.T) {
	t.Helper()
	b.c.Codex.Accounts = []codexwatch.Account{{Name: "w"}}
	if err := os.MkdirAll(b.a.dir("codex"), 0o700); err != nil {
		t.Fatal(err)
	}
}

// Вход OpenAI не прочитан: это не «данных нет». Открыть пары по одному Claude нельзя, gate.json остаётся прежним.
func TestQueueOpenAIBrokenKeepsGate(t *testing.T) {
	for name, spoil := range map[string]func(*testing.T, *queueBench){
		"битая история": func(t *testing.T, b *queueBench) {
			if err := os.WriteFile(filepath.Join(b.a.dir("queue"), "openai-history.jsonl"), []byte("{не json\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"битый замер учётки": func(t *testing.T, b *queueBench) {
			if err := os.WriteFile(filepath.Join(b.a.dir("codex"), "w.json"), []byte("{не json"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"замер не прочитать": func(t *testing.T, b *queueBench) {
			dir := b.a.dir("codex")
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dir, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			b := newQueueBench(t, "[]", "[]")
			b.openAIAccount(t)
			b.round(t)
			before, err := os.ReadFile(b.c.Queue.Gate)
			if err != nil {
				t.Fatal(err)
			}
			spoil(t, b)
			b.ageGate(t)
			log := b.captureLog()
			b.round(t)
			after, err := os.ReadFile(b.c.Queue.Gate)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatalf("сбой OpenAI переписал ворота:\n%s\n%s", before, after)
			}
			if !strings.Contains(log.String(), "история OpenAI") {
				t.Fatalf("нет предупреждения: %s", log)
			}
		})
	}
}

// Учётка без файла замера — данных нет, а не сбой: openai null, ворота считаются.
func TestQueueOpenAIMissingIsNull(t *testing.T) {
	b := newQueueBench(t, "[]", "[]")
	b.openAIAccount(t)
	b.round(t)
	gate, err := queue.ReadGate(b.c.Queue.Gate)
	if err != nil {
		t.Fatal(err)
	}
	if gate.OpenAI != nil {
		t.Fatalf("запас без замера: %v", *gate.OpenAI)
	}
}

// Письма считают open по самому gate.json, как tk next: файл не записан или устарел — MED и LOW нет, хоть расчёт и открыл пары.
func TestQueueLettersReadGateFile(t *testing.T) {
	t.Run("файл не записан", func(t *testing.T) {
		b := newQueueBench(t, "[]", "[]")
		block := filepath.Join(b.dir, "file")
		if err := os.WriteFile(block, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		b.c.Queue.Gate = filepath.Join(block, "gate.json")
		open, err := gateOpen(b.a.queueGatePass(b.c, time.Now(), false))
		if err != nil {
			t.Fatal(err)
		}
		if len(open) != 0 {
			t.Fatalf("письма видят пары мимо файла: %v", open)
		}
	})
	t.Run("файл свежий", func(t *testing.T) {
		b := newQueueBench(t, "[]", "[]")
		open, err := gateOpen(b.a.queueGatePass(b.c, time.Now(), false))
		if err != nil {
			t.Fatal(err)
		}
		gate, err := queue.ReadGate(b.c.Queue.Gate)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(open, ",") != strings.Join(gate.Open, ",") || len(open) == 0 {
			t.Fatalf("письма %v, файл %v", open, gate.Open)
		}
	})
	t.Run("вход сломан, файл старше 26 ч", func(t *testing.T) {
		b := newQueueBench(t, "[]", "[]")
		b.round(t)
		b.writeHistory(t, map[string]time.Duration{"a": 25 * time.Hour})
		open, err := gateOpen(b.a.queueGatePass(b.c, time.Now().Add(27*time.Hour), false))
		if err != nil {
			t.Fatal(err)
		}
		if len(open) != 0 {
			t.Fatalf("устаревший файл: %v", open)
		}
	})
	t.Run("сухой прогон показывает расчёт и не пишет", func(t *testing.T) {
		b := newQueueBench(t, "[]", "[]")
		open, err := gateOpen(b.a.queueGatePass(b.c, time.Now(), true))
		if err != nil {
			t.Fatal(err)
		}
		if len(open) == 0 || b.gateExists() {
			t.Fatalf("open %v, файл есть: %v", open, b.gateExists())
		}
	})
}

// Замер учётки без срока сброса — данных нет: в историю не пишем, иначе ReadHistory откажет на этой строке всегда.
func TestQueueOpenAINoResetNoRow(t *testing.T) {
	b := newQueueBench(t, "[]", "[]")
	b.openAIAccount(t)
	st := codexwatch.State{Samples: []codexwatch.Sample{{At: time.Now().Add(-time.Minute), Used: 40}}}
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.a.dir("codex"), "w.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	b.round(t)
	if _, err := os.Stat(filepath.Join(b.a.dir("queue"), "openai-history.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("строка без срока сброса в истории: %v", err)
	}
	gate, err := queue.ReadGate(b.c.Queue.Gate)
	if err != nil {
		t.Fatal(err)
	}
	if gate.OpenAI != nil {
		t.Fatalf("запас: %v", *gate.OpenAI)
	}
}

// state.json не пишется (диск полон, права): отметки живут в памяти демона, письма не идут потоком каждые 30 с.
func TestQueueStateWriteFailNoFlood(t *testing.T) {
	b := newQueueBench(t, queueCritJSON, "[]")
	dir := b.a.dir("queue")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Error(err)
		}
	})
	log := b.captureLog()
	ctx := context.Background()
	judge := restart.NewJudge(b.dir)
	if err := b.a.queueRound(ctx, b.c, judge, false); err == nil {
		t.Fatal("запись состояния в закрытую папку прошла: тест ничего не проверяет")
	}
	if err := b.a.queueRound(ctx, b.c, judge, false); err == nil {
		t.Fatal("второй проход: ошибка записи не показана")
	}
	if n := strings.Count(log.String(), "kind="+queue.KindUnreachable); n != 1 {
		t.Fatalf("строк за два прохода: %d\n%s", n, log)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := b.a.queueRound(ctx, b.c, judge, false); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(log.String(), "kind="+queue.KindUnreachable); n != 1 {
		t.Fatalf("после починки повтор: %d", n)
	}
	loaded, err := queue.LoadState(b.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Tickets) != 1 {
		t.Fatalf("отметки из памяти не записаны: %+v", loaded.Tickets)
	}
}
