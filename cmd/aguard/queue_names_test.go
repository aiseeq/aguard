package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/names"
	"github.com/aiseeq/aguard/internal/queue"
)

// Тикет queue-names-table-stops: строка без табуляции в names.tsv не останавливает письма очереди.

const brokenNamesLine = "строка без табуляции\n"

func (b *queueBench) writeNames(t *testing.T, body string) {
	t.Helper()
	p := filepath.Join(b.a.home, names.File)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newHigh — новый открытый HIGH h в проекте p; файл в папке задач меняет отпечаток, кэш tk ls не держит старый список.
func (b *queueBench) newHigh(t *testing.T) {
	t.Helper()
	name := time.Now().Format("20060102-1504") + "-HIGH-h.md"
	body := `[{"repo":"p","slug":"h","status":"открыт","prio":"HIGH","title":"срочное","path":"docs/tasks/` + name + `"}]`
	if err := os.WriteFile(filepath.Join(b.dir, "git.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.dir, "git", "p", "docs", "tasks", name), []byte("# h\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// namesBench — проект p в live, живы лид и админ git; мастер p по таблице — pm, его сессии нет, HIGH идёт админу с его именем.
func namesBench(t *testing.T) (*queueBench, map[string]<-chan string) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	b := newQueueBench(t, "[]", "[]")
	b.c.Queue.Live = []string{"p"}
	b.a.queueWorkers = func() ([]string, error) { return nil, nil }
	return b, liveNamed(t, b.a, "lead", "git-admin")
}

// drainLetters — письма, пришедшие до 300 мс тишины.
func drainLetters(ch <-chan string) []string {
	var out []string
	for {
		select {
		case m := <-ch:
			out = append(out, m)
		case <-time.After(300 * time.Millisecond):
			return out
		}
	}
}

func countWith(msgs []string, sub string) int {
	n := 0
	for _, m := range msgs {
		if strings.Contains(m, sub) {
			n++
		}
	}
	return n
}

// Прогретый демон, таблица сломана: HIGH идёт по прошлой таблице, лиду одно письмо о причине; второй проход — второго нет.
func TestQueueNamesBrokenWarm(t *testing.T) {
	b, got := namesBench(t)
	b.writeNames(t, filepath.Join(b.dir, "git", "p")+"\tpm\n")
	b.round(t)
	drainLetters(got["lead"])
	drainLetters(got["git-admin"])

	b.writeNames(t, filepath.Join(b.dir, "git", "p")+"\tpm\n"+brokenNamesLine)
	b.newHigh(t)
	b.round(t)
	admin := drainLetters(got["git-admin"])
	if countWith(admin, "HIGH p/h — нет живой сессии pm") != 1 {
		t.Fatalf("HIGH по прошлой таблице не ушёл: %q", admin)
	}
	lead := drainLetters(got["lead"])
	if countWith(lead, "таблица имён") != 1 || countWith(lead, filepath.Join(b.a.home, names.File)) != 1 ||
		countWith(lead, "нужно «папка") != 1 || countWith(lead, "на прошлой таблице") != 1 {
		t.Fatalf("письмо лиду о битой таблице: %q", lead)
	}

	b.round(t)
	if lead := drainLetters(got["lead"]); countWith(lead, "таблица имён") != 0 {
		t.Fatalf("второе письмо лиду: %q", lead)
	}
	if admin := drainLetters(got["git-admin"]); countWith(admin, "HIGH p/h") != 0 {
		t.Fatalf("повтор HIGH: %q", admin)
	}
}

// Холодный старт с битой таблицей: выдумывать таблицу нельзя — писем по тикетам нет, лиду письмо, что очередь стоит.
// Таблицу поправили — письмо по тикету уходит.
func TestQueueNamesBrokenCold(t *testing.T) {
	b, got := namesBench(t)
	b.writeNames(t, filepath.Join(b.dir, "git", "p")+"\tpm\n"+brokenNamesLine)
	b.newHigh(t)
	b.round(t)
	if admin := drainLetters(got["git-admin"]); len(admin) != 0 {
		t.Fatalf("письма по тикетам при холодном старте: %q", admin)
	}
	lead := drainLetters(got["lead"])
	if countWith(lead, "таблица имён") != 1 || countWith(lead, "стоят") != 1 || countWith(lead, "HIGH") != 0 {
		t.Fatalf("письмо лиду при холодном старте: %q", lead)
	}
	b.round(t)
	if lead := drainLetters(got["lead"]); len(lead) != 0 {
		t.Fatalf("второе письмо лиду: %q", lead)
	}

	b.writeNames(t, filepath.Join(b.dir, "git", "p")+"\tpm\n")
	b.round(t)
	if admin := drainLetters(got["git-admin"]); countWith(admin, "HIGH p/h — нет живой сессии pm") != 1 {
		t.Fatalf("после починки HIGH не ушёл: %q", admin)
	}
	if lead := drainLetters(got["lead"]); countWith(lead, "таблица имён") != 0 { // починка снимает и ошибку: старой не шлём
		t.Fatalf("после починки письмо лиду о таблице: %q", lead)
	}
}

// Повтор письма лиду: тот же текст — раз в сутки; другой текст — сразу; починка снимает отметку.
func TestQueueNamesAlertRepeat(t *testing.T) {
	b, got := namesBench(t)
	now := time.Now()
	read := func() {
		t.Helper()
		if _, err := b.a.queueNamesTable(); err != nil && b.a.queueNames.loaded {
			t.Fatal(err)
		}
	}
	letters := func(at time.Time) int {
		t.Helper()
		b.a.queueNamesAlert(b.c, false, at)
		return countWith(drainLetters(got["lead"]), "таблица имён")
	}
	b.writeNames(t, "")
	read()
	if n := letters(now); n != 0 {
		t.Fatalf("целая таблица: %d писем", n)
	}
	b.writeNames(t, brokenNamesLine)
	read()
	if n := letters(now); n != 1 {
		t.Fatalf("первое письмо: %d", n)
	}
	if n := letters(now.Add(24*time.Hour - time.Second)); n != 0 {
		t.Fatalf("тот же текст до суток: %d", n)
	}
	if n := letters(now.Add(24 * time.Hour)); n != 1 {
		t.Fatalf("тот же текст через сутки: %d", n)
	}
	b.writeNames(t, "#\n"+brokenNamesLine) // ошибка в другой строке — другой текст
	read()
	if n := letters(now.Add(24*time.Hour + time.Minute)); n != 1 {
		t.Fatalf("другой текст: %d", n)
	}
	b.writeNames(t, "#\n")
	read()
	b.writeNames(t, "#\n"+brokenNamesLine)
	read()
	if n := letters(now.Add(24*time.Hour + 2*time.Minute)); n != 1 {
		t.Fatalf("после починки та же поломка: %d", n)
	}
}

// Лида нет в живых: письмо ждёт без WARN каждые 30 с, отметки нет; лид появился — письмо уходит.
func TestQueueNamesAlertWaitsLead(t *testing.T) {
	b, got := namesBench(t)
	log := b.captureLog()
	lead := b.c.Lead
	b.c.Lead = "нет-такого"
	b.writeNames(t, brokenNamesLine)
	if _, err := b.a.queueNamesTable(); err == nil {
		t.Fatal("битая таблица без прошлой прочиталась")
	}
	now := time.Now()
	b.a.queueNamesAlert(b.c, false, now)
	if strings.Contains(log.String(), "level=WARN") {
		t.Fatalf("WARN без живого лида: %s", log.String())
	}
	b.c.Lead = lead
	b.a.queueNamesAlert(b.c, false, now.Add(time.Minute))
	if n := countWith(drainLetters(got["lead"]), "таблица имён"); n != 1 {
		t.Fatalf("письмо лиду после появления: %d", n)
	}
}

// Сухой прогон: письмо о таблице в вывод, лиду не уходит и отметки не ставит.
func TestQueueNamesAlertDry(t *testing.T) {
	b, got := namesBench(t)
	b.writeNames(t, brokenNamesLine)
	if _, err := b.a.queueNamesTable(); err == nil {
		t.Fatal("битая таблица без прошлой прочиталась")
	}
	b.a.queueNamesAlert(b.c, true, time.Now())
	if out := b.a.out.(*strings.Builder).String(); !strings.Contains(out, "names lead: aguard queue: таблица имён") {
		t.Fatalf("вывод: %q", out)
	}
	if lead := drainLetters(got["lead"]); len(lead) != 0 || b.a.queueNames.sentErr != "" {
		t.Fatalf("сухой прогон написал лиду: %q", lead)
	}
}

// Будильник проектов и внешние задачи берут ту же прошлую таблицу; без прошлой внешние — ошибка: без хозяев Step стёр бы отметки.
func TestQueueNamesWakeAndExternal(t *testing.T) {
	b, _ := namesBench(t)
	b.externalTk(t, extHigh)
	p := filepath.Join(b.dir, "git", "p")
	b.writeNames(t, p+"\tpm\n"+brokenNamesLine)
	var state queue.State
	if _, err := b.a.queueExternal(context.Background(), b.c, &state, time.Now()); err == nil {
		t.Fatal("внешние без прошлой таблицы")
	}
	b.writeNames(t, p+"\tpm\n")
	b.a.queueExternalAt = time.Time{}
	b.external(t, &state, time.Now())
	b.writeNames(t, p+"\tpm\n"+brokenNamesLine)
	if ts := b.external(t, &state, time.Now()); len(ts) != 1 || ts[0].Master != "pm" {
		t.Fatalf("внешние на прошлой таблице: %+v", ts)
	}
	var master string
	for _, w := range b.a.queueWakeProjects(b.c) {
		if w.Dir == p {
			master = w.Master
		}
	}
	if master != "pm" {
		t.Fatalf("будильник проекта p: мастер %q", master)
	}
}

// Тихий режим: письмо о таблице не отмечается отправленным и уходит после снятия режима.
func TestQueueNamesAlertQuiet(t *testing.T) {
	b, got := namesBench(t)
	b.writeNames(t, brokenNamesLine)
	if _, err := b.a.queueNamesTable(); err == nil {
		t.Fatal("битая таблица без прошлой прочиталась")
	}
	writeQuietState(t, b.a, time.Now().Add(time.Hour))
	now := time.Now()
	b.a.queueNamesAlert(b.c, false, now)
	if lead := drainLetters(got["lead"]); len(lead) != 0 {
		t.Fatalf("письмо в тихом режиме: %q", lead)
	}
	if err := os.Remove(b.a.quietPath()); err != nil {
		t.Fatal(err)
	}
	b.a.queueNamesAlert(b.c, false, now.Add(time.Minute))
	if n := countWith(drainLetters(got["lead"]), "таблица имён"); n != 1 {
		t.Fatalf("после тихого режима: %d", n)
	}
}
