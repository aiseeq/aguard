package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/codexwatch"
	"github.com/aiseeq/aguard/internal/config"
	"github.com/aiseeq/aguard/internal/said"
)

// saidBench — said на подставном codex: настоящий akit/oneshot с CODEX_HOME на вызов. Учётка отвечает по файлу
// <home>/answer: текст отказа — turn.failed и код 1, как настоящий codex; файла нет — ответ модели.
type saidBench struct {
	a    *app
	c    *config.Config
	st   *said.State
	logs *bytes.Buffer
	dir  string
	seen string // файл: CODEX_HOME вызовов по порядку
}

const fakeCodex = `#!/bin/sh
cat >/dev/null
echo "$CODEX_HOME" >>"$FAKE_SEEN"
if [ -f "$CODEX_HOME/answer" ]; then
  printf '{"type":"turn.failed","error":{"message":"%s"}}\n' "$(cat "$CODEX_HOME/answer")"
  exit 1
fi
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"items\":[]}"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":0,"output_tokens":1}}'
`

func newSaidBench(t *testing.T) *saidBench {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(fakeCodex), 0o700); err != nil {
		t.Fatal(err)
	}
	approved := filepath.Join(dir, ".config", "models", "approved")
	if err := os.MkdirAll(filepath.Dir(approved), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(approved, []byte("luna gpt-6-luna\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	seen := filepath.Join(dir, "seen")
	t.Setenv("HOME", dir)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("FAKE_SEEN", seen)
	var logs bytes.Buffer
	a := &app{home: dir, stateDir: filepath.Join(dir, "state"), log: slog.New(slog.NewTextHandler(&logs, nil))}
	if err := os.MkdirAll(a.dir("codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	c := &config.Config{Said: said.Default(), Codex: codexwatch.Default(dir)}
	c.Said.File = filepath.Join(dir, "said.tsv")
	c.Codex.Mode, c.Codex.Lead = codexwatch.ModeLog, "lead"
	for _, acc := range c.Codex.Accounts {
		if err := os.MkdirAll(acc.Home, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return &saidBench{a: a, c: c, logs: &logs, dir: dir, seen: seen, st: &said.State{Pending: []said.Pending{
		{Event: said.Event{Session: "s1", Line: 1, Text: "реплика один"}},
		{Event: said.Event{Session: "s1", Line: 2, Text: "реплика два"}},
	}}}
}

// roundDirs — папки для целого saidRound: реестр, транскрипты, состояние.
func (b *saidBench) roundDirs(t *testing.T) {
	t.Helper()
	b.a.reg.Dir, b.a.projects = filepath.Join(b.dir, "sessions"), filepath.Join(b.dir, "projects")
	for _, d := range []string{b.a.projects, b.a.dir("said")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

// refuse — учётка отказывает этим текстом.
func (b *saidBench) refuse(t *testing.T, account, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(b.home(account), "answer"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (b *saidBench) home(account string) string {
	for _, acc := range b.c.Codex.Accounts {
		if acc.Name == account {
			return acc.Home
		}
	}
	panic("нет учётки " + account)
}

// measure — файл замера codexwatch у учётки.
func (b *saidBench) measure(t *testing.T, account string, at time.Time, used float64, denied bool) {
	t.Helper()
	st := codexwatch.State{ResetsAt: time.Now().Add(72 * time.Hour), Samples: []codexwatch.Sample{{At: at, Used: used}},
		Denied: denied}
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.a.dir("codex"), account+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// calls — вызовы codex с прошлого calls: имена учёток по порядку.
func (b *saidBench) calls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(b.seen)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(b.seen); err != nil {
		t.Fatal(err)
	}
	var out []string
	for line := range strings.Lines(string(data)) {
		for _, acc := range b.c.Codex.Accounts {
			if strings.TrimSpace(line) == acc.Home {
				out = append(out, acc.Name)
			}
		}
	}
	return out
}

func (b *saidBench) classify(t *testing.T) []said.Record {
	t.Helper()
	recs, err := b.a.saidClassify(context.Background(), b.c, b.st)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func wantCalls(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("вызовы %v, надо %v", got, want)
	}
}

// С1: настоящий отказ — код 1 и turn.failed на stdout; та же реплика тем же проходом идёт следующей учётке, попытка не
// тратится, вторая реплика сразу на живой учётке.
func TestSaidRefusalSwitchesAccountInPass(t *testing.T) {
	b := newSaidBench(t)
	b.refuse(t, "work", "Your workspace is out of credits. Ask your workspace owner to refill in order to continue.")
	recs := b.classify(t)
	wantCalls(t, b.calls(t), "work", "personal", "personal")
	if len(recs) != 3 || len(b.st.Pending) != 0 {
		t.Fatalf("записей %d, очередь %d", len(recs), len(b.st.Pending))
	}
	for _, rec := range recs[1:] {
		if rec.Account != "personal" || rec.Down || rec.Attempt != 1 {
			t.Fatalf("запись %+v", rec)
		}
	}
	if !b.st.Pause.IsZero() || !b.st.DownSince.IsZero() {
		t.Fatalf("пауза %v, простой %v при живой учётке", b.st.Pause, b.st.DownSince)
	}
	if !strings.Contains(b.logs.String(), "out of credits") {
		t.Fatalf("текст отказа не в журнале:\n%s", b.logs.String())
	}
	// метка: следующий проход не тратит вызов на отказавшую учётку
	b.st.Pending = []said.Pending{{Event: said.Event{Session: "s1", Line: 3, Text: "реплика три"}}}
	b.classify(t)
	wantCalls(t, b.calls(t), "personal")
}

// С3: замер старый и ниже запаса, а учётка отказывает — та же смена учётки, а не вечная пауза.
func TestSaidStaleMeasureStillSwitches(t *testing.T) {
	b := newSaidBench(t)
	b.measure(t, "work", time.Now().Add(-96*time.Hour), 80, false)
	b.refuse(t, "work", "Your workspace is out of credits.")
	b.classify(t)
	wantCalls(t, b.calls(t), "work", "personal", "personal")
}

// С4: текст про лимит при живом замере — метка на час, а не до сброса окна; после неё учётку снова пробуем.
func TestSaidRefusalMarkIsAnHour(t *testing.T) {
	b := newSaidBench(t)
	b.measure(t, "work", time.Now(), 23, false)
	b.refuse(t, "work", "You've reached your usage limit. Please try again later.")
	b.classify(t)
	until := b.st.Out["work"]
	if d := time.Until(until); d < 59*time.Minute || d > time.Hour {
		t.Fatalf("метка на %v, надо час", d)
	}
	b.calls(t)
	b.st.Out["work"] = time.Now().Add(-time.Second)
	if err := os.Remove(filepath.Join(b.home("work"), "answer")); err != nil {
		t.Fatal(err)
	}
	b.st.Pending = []said.Pending{{Event: said.Event{Session: "s1", Line: 3, Text: "реплика три"}}}
	b.classify(t)
	wantCalls(t, b.calls(t), "work")
}

// С5, С6: запрет ниже 100% и запас — учётку не зовём вовсе.
func TestSaidMeasureSkipsAccount(t *testing.T) {
	for _, tc := range []struct {
		name   string
		used   float64
		denied bool
	}{{"запрет", 60, true}, {"запас", 90, false}, {"100%", 100, false}} {
		t.Run(tc.name, func(t *testing.T) {
			b := newSaidBench(t)
			b.measure(t, "work", time.Now(), tc.used, tc.denied)
			b.classify(t)
			wantCalls(t, b.calls(t), "personal", "personal")
		})
	}
}

// С8: битый файл одной учётки убирает только её.
func TestSaidBrokenMeasureSkipsOnlyItsAccount(t *testing.T) {
	b := newSaidBench(t)
	if err := os.WriteFile(filepath.Join(b.a.dir("codex"), "work.json"), []byte("{битый"), 0o600); err != nil {
		t.Fatal(err)
	}
	b.classify(t)
	wantCalls(t, b.calls(t), "personal", "personal")
	if !strings.Contains(b.logs.String(), "замер учётки не прочитан") {
		t.Fatalf("журнал:\n%s", b.logs.String())
	}
}

// Все без лимита: вызова нет, очередь цела, пауза и начало простоя стоят, причина в журнале.
func TestSaidNoAccountWaits(t *testing.T) {
	b := newSaidBench(t)
	for _, acc := range b.c.Codex.Accounts {
		b.measure(t, acc.Name, time.Now(), 100, false)
	}
	if recs := b.classify(t); len(recs) != 0 {
		t.Fatalf("записи %+v", recs)
	}
	wantCalls(t, b.calls(t))
	if len(b.st.Pending) != 2 || b.st.Pending[0].Attempts != 0 {
		t.Fatalf("очередь %+v", b.st.Pending)
	}
	if b.st.Pause.IsZero() || b.st.DownSince.IsZero() || !strings.Contains(b.logs.String(), "нет учётки с лимитом") {
		t.Fatalf("пауза %v, простой %v, журнал:\n%s", b.st.Pause, b.st.DownSince, b.logs.String())
	}
	if b.st.DownErr != "" || !strings.Contains(b.st.Skip, "used 100") {
		t.Fatalf("модель не звали, а ошибка %q; причина %q", b.st.DownErr, b.st.Skip)
	}
}

// Отказали все: пауза, попытки целы, очередь цела, два вызова — две записи.
func TestSaidAllRefuse(t *testing.T) {
	b := newSaidBench(t)
	b.refuse(t, "work", "out of credits")
	b.refuse(t, "personal", "out of credits")
	recs := b.classify(t)
	wantCalls(t, b.calls(t), "work", "personal")
	if len(b.st.Pending) != 2 || b.st.Pending[0].Attempts != 0 || b.st.Pause.IsZero() {
		t.Fatalf("очередь %+v, пауза %v", b.st.Pending, b.st.Pause)
	}
	if len(recs) != 2 || recs[0].Account != "work" || recs[1].Account != "personal" {
		t.Fatalf("записи %+v", recs)
	}
}

// Сбой не по кредитам — как раньше: пауза без метки и без смены учётки.
func TestSaidOtherFailurePausesWithoutMark(t *testing.T) {
	b := newSaidBench(t)
	b.refuse(t, "work", "stream disconnected")
	b.classify(t)
	wantCalls(t, b.calls(t), "work")
	if b.st.Pause.IsZero() || len(b.st.Out) != 0 || b.st.DownErr == "" {
		t.Fatalf("пауза %v, метки %v, ошибка %q", b.st.Pause, b.st.Out, b.st.DownErr)
	}
}

// Подряд два замера: решает последний, а не первый из истории окна.
func TestSaidUsesLastSample(t *testing.T) {
	b := newSaidBench(t)
	st := codexwatch.State{ResetsAt: time.Now().Add(72 * time.Hour), Samples: []codexwatch.Sample{
		{At: time.Now().Add(-time.Hour), Used: 20}, {At: time.Now(), Used: 100}}}
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.a.dir("codex"), "work.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	b.classify(t)
	wantCalls(t, b.calls(t), "personal", "personal")
}

// Каждый вызов в журнале: отказ перед сменой учётки — тоже, с её именем.
func TestSaidRefusedCallIsJournaled(t *testing.T) {
	b := newSaidBench(t)
	b.refuse(t, "work", "out of credits")
	recs := b.classify(t)
	wantCalls(t, b.calls(t), "work", "personal", "personal")
	if len(recs) != 3 || recs[0].Account != "work" || !recs[0].Down || recs[1].Account != "personal" {
		t.Fatalf("записи %+v", recs)
	}
}

// Остановка демона посреди вызова: без паузы, метки и простоя, попытка цела.
func TestSaidStopMidCall(t *testing.T) {
	b := newSaidBench(t)
	slow := "#!/bin/sh\ncat >/dev/null\necho \"$CODEX_HOME\" >>\"$FAKE_SEEN\"\nexec sleep 5\n"
	if err := os.WriteFile(filepath.Join(b.dir, "bin", "codex"), []byte(slow), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	recs, err := b.a.saidClassify(ctx, b.c, b.st)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 0 || len(b.st.Pending) != 2 || b.st.Pending[0].Attempts != 0 || !b.st.Pause.IsZero() ||
		!b.st.DownSince.IsZero() || len(b.st.Out) != 0 {
		t.Fatalf("записи %+v, состояние %+v", recs, b.st)
	}
	wantCalls(t, b.calls(t), "work")
}

// Сквозь saidRound: простой от Stall даёт письмо с причиной один раз, пауза второго не даёт, ответ модели обнуляет простой.
func TestSaidRoundStall(t *testing.T) {
	b := newSaidBench(t)
	b.roundDirs(t)
	path := filepath.Join(b.a.dir("said"), "state.json")
	b.refuse(t, "work", "stream disconnected")
	b.st.DownSince, b.st.DownErr = time.Now().Add(-7*time.Hour), "старое"
	if err := b.st.Save(path); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	round := func() *said.State {
		t.Helper()
		if err := b.a.saidRound(ctx, b.c, false); err != nil {
			t.Fatal(err)
		}
		st, err := said.LoadState(path)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	letters := func() int { return strings.Count(b.logs.String(), "написал бы") }
	st := round()
	if letters() != 1 || !strings.Contains(letterLine(b.logs.String()), "stream disconnected") {
		t.Fatalf("письма с причиной нет:\n%s", b.logs.String())
	}
	if st.Signal.IsZero() || st.Pause.IsZero() || len(st.Pending) != 2 || st.Pending[0].Attempts != 0 {
		t.Fatalf("состояние %+v", st)
	}
	if round(); letters() != 1 {
		t.Fatalf("письмо на паузе: %d", letters())
	}
	st.Pause = time.Time{}
	if err := st.Save(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(b.home("work"), "answer")); err != nil {
		t.Fatal(err)
	}
	st = round()
	if !st.DownSince.IsZero() || !st.Signal.IsZero() || st.DownErr != "" || len(st.Pending) != 0 {
		t.Fatalf("после ответа %+v", st)
	}
}

// Свежий простой письма не даёт, даже если модель молчит: счёт идёт с первого сбоя, а не обнуляется на каждом проходе.
func TestSaidRoundFreshDownNoLetter(t *testing.T) {
	b := newSaidBench(t)
	b.roundDirs(t)
	b.refuse(t, "work", "stream disconnected")
	path := filepath.Join(b.a.dir("said"), "state.json")
	if err := b.st.Save(path); err != nil {
		t.Fatal(err)
	}
	if err := b.a.saidRound(context.Background(), b.c, false); err != nil {
		t.Fatal(err)
	}
	st, err := said.LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.DownSince.IsZero() || strings.Contains(b.logs.String(), "написал бы") {
		t.Fatalf("простой %v, журнал:\n%s", st.DownSince, b.logs.String())
	}
}

// letterLine — строка журнала с письмом о простое («написал бы»): причину искать в ней, а не во всём журнале, где та же
// ошибка есть из warn.
func letterLine(logs string) string {
	for line := range strings.Lines(logs) {
		if strings.Contains(line, "написал бы") {
			return line
		}
	}
	return ""
}

// Отказ всех учёток: на следующем проходе ошибка модели цела, а причина пропуска — метки.
func TestSaidDownReason(t *testing.T) {
	b := newSaidBench(t)
	b.refuse(t, "work", "out of credits")
	b.refuse(t, "personal", "out of credits")
	b.classify(t)
	if !strings.Contains(b.st.DownErr, "out of credits") || b.st.Skip != "" {
		t.Fatalf("ошибка %q, пропуск %q", b.st.DownErr, b.st.Skip)
	}
	b.st.Pause = time.Time{}
	b.classify(t)
	if !strings.Contains(b.st.DownErr, "out of credits") || !strings.Contains(b.st.Skip, "отказ, метка") {
		t.Fatalf("ошибка %q, пропуск %q", b.st.DownErr, b.st.Skip)
	}
}

// Сеть, потом замер: причина пропуска идёт за замером, ошибка сети остаётся, в письме обе. Вызов модели чистит пропуск.
func TestSaidDownReasonFollowsMeasure(t *testing.T) {
	b := newSaidBench(t)
	b.refuse(t, "work", "stream disconnected")
	b.classify(t)
	again := func(used float64) {
		t.Helper()
		for _, acc := range b.c.Codex.Accounts {
			b.measure(t, acc.Name, time.Now(), used, false)
		}
		b.st.Pause = time.Time{}
		b.classify(t)
	}
	again(100)
	if !strings.Contains(b.st.Skip, "used 100") || !strings.Contains(b.st.DownErr, "stream disconnected") {
		t.Fatalf("пропуск %q, ошибка %q", b.st.Skip, b.st.DownErr)
	}
	again(95)
	if !strings.Contains(b.st.Skip, "used 95") || strings.Contains(b.st.Skip, "used 100") {
		t.Fatalf("причина застыла: %q", b.st.Skip)
	}
	b.st.DownSince = time.Now().Add(-7 * time.Hour)
	b.a.saidStall(b.c, b.st, time.Now())
	line := letterLine(b.logs.String())
	if !strings.Contains(line, "used 95") || !strings.Contains(line, "stream disconnected") {
		t.Fatalf("в письме не обе причины: %s", line)
	}
	again(10) // замер снят, work снова молчит: зовём модель, пропуска больше нет
	if b.st.Skip != "" || !strings.Contains(b.st.DownErr, "stream disconnected") {
		t.Fatalf("пропуск %q, ошибка %q", b.st.Skip, b.st.DownErr)
	}
}

// Битые замеры всех учёток названы, ошибки модели нет: её не звали.
func TestSaidDownReasonBrokenMeasures(t *testing.T) {
	b := newSaidBench(t)
	for _, acc := range b.c.Codex.Accounts {
		if err := os.WriteFile(filepath.Join(b.a.dir("codex"), acc.Name+".json"), []byte("{битый"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	b.classify(t)
	if !strings.Contains(b.st.Skip, "work: замер не прочитан") || !strings.Contains(b.st.Skip, "personal: замер не прочитан") || b.st.DownErr != "" {
		t.Fatalf("пропуск %q, ошибка %q", b.st.Skip, b.st.DownErr)
	}
}

// Настоящая отправка через сокет живой сессии лида: письмо дошло с причиной, Signal сохранён, второго письма нет.
func TestSaidRoundLetterDelivered(t *testing.T) {
	b := newSaidBench(t)
	b.roundDirs(t)
	got := liveLead(t, b.a)
	b.c.Lead = "lead"
	b.c.Said.Mode = said.ModeWrite
	b.refuse(t, "work", "stream disconnected")
	path := filepath.Join(b.a.dir("said"), "state.json")
	b.st.DownSince = time.Now().Add(-7 * time.Hour)
	if err := b.st.Save(path); err != nil {
		t.Fatal(err)
	}
	if err := b.a.saidRound(context.Background(), b.c, false); err != nil {
		t.Fatal(err)
	}
	select {
	case letter := <-got:
		if !strings.Contains(letter, "stream disconnected") || !strings.Contains(letter, "aguard said") {
			t.Fatalf("письмо %q", letter)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("письмо не дошло:\n%s", b.logs.String())
	}
	st, err := said.LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Signal.IsZero() {
		t.Fatal("Signal не сохранён: письмо уйдёт снова")
	}
	st.Pause = time.Time{} // проход идёт дальше паузы: снова сбой и снова проверка письма
	if err := st.Save(path); err != nil {
		t.Fatal(err)
	}
	if err := b.a.saidRound(context.Background(), b.c, false); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(b.logs.String(), "aguard said: написал"); n != 1 {
		t.Fatalf("писем %d:\n%s", n, b.logs.String())
	}
}

// Остановка демона посреди первого вызова после простоя: письма нет (вызов отменён, а причина ещё прошлая), Signal пуст —
// настоящее письмо придёт, причина цела.
func TestSaidRoundStopNoLetter(t *testing.T) {
	b := newSaidBench(t)
	b.roundDirs(t)
	slow := "#!/bin/sh\ncat >/dev/null\nexec sleep 5\n"
	if err := os.WriteFile(filepath.Join(b.dir, "bin", "codex"), []byte(slow), 0o700); err != nil {
		t.Fatal(err)
	}
	b.st.Waiting(time.Now().Add(-7*time.Hour), "нет учётки")
	path := filepath.Join(b.a.dir("said"), "state.json")
	if err := b.st.Save(path); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	if err := b.a.saidRound(ctx, b.c, false); err != nil {
		t.Fatal(err)
	}
	if line := letterLine(b.logs.String()); line != "" {
		t.Fatalf("письмо при остановке: %s", line)
	}
	st, err := said.LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Signal.IsZero() || !strings.Contains(st.Skip, "нет учётки") || len(st.Pending) != 2 {
		t.Fatalf("состояние %+v", st)
	}
}

// Остановка после первого ответа: размеченное сохранено до выхода, иначе реплика размечалась бы заново, а в write — дубль
// в указателе. Отмена — когда второй вызов уже начался, а не по времени.
func TestSaidRoundStopKeepsAnswered(t *testing.T) {
	b := newSaidBench(t)
	b.roundDirs(t)
	script := "#!/bin/sh\ncat >/dev/null\nif [ -f \"$FAKE_SEEN\" ]; then echo second >>\"$FAKE_SEEN\"; exec sleep 5; fi\n" +
		"echo first >>\"$FAKE_SEEN\"\n" +
		`printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"items\":[]}"}}'` + "\n" +
		`printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":0,"output_tokens":1}}'` + "\n"
	if err := os.WriteFile(filepath.Join(b.dir, "bin", "codex"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(b.a.dir("said"), "state.json")
	if err := b.st.Save(path); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		defer cancel()
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if data, err := os.ReadFile(b.seen); err == nil && strings.Contains(string(data), "second") {
				return
			}
		}
		t.Error("второй вызов не начался за 30 с")
	}()
	if err := b.a.saidRound(ctx, b.c, false); err != nil {
		t.Fatal(err)
	}
	st, err := said.LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Pending) != 1 || st.Pending[0].Event.Line != 2 || st.Pending[0].Attempts != 0 {
		t.Fatalf("очередь на диске %+v", st.Pending)
	}
}

// Журнал не записан: прохода как бы не было, состояние не сохранено — письма нет, иначе оно уходит каждый проход.
func TestSaidRoundJournalFailNoLetter(t *testing.T) {
	b := newSaidBench(t)
	b.roundDirs(t)
	b.refuse(t, "work", "stream disconnected")
	b.st.DownSince = time.Now().Add(-7 * time.Hour)
	path := filepath.Join(b.a.dir("said"), "state.json")
	if err := b.st.Save(path); err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{time.Now(), time.Now().Add(time.Hour)} { // на переходе суток — оба дня
		day := filepath.Join(b.a.dir("said"), at.Format(time.DateOnly)+".jsonl")
		if err := os.Mkdir(day, 0o700); err != nil && !os.IsExist(err) {
			t.Fatal(err)
		}
	}
	if err := b.a.saidRound(context.Background(), b.c, false); err == nil {
		t.Fatal("сбой журнала не дошёл до прохода")
	}
	if line := letterLine(b.logs.String()); line != "" {
		t.Fatalf("письмо при сбое журнала: %s", line)
	}
}

// Состояние не сохранить: письма нет, иначе Signal теряется и письмо уходит каждый проход.
func TestSaidRoundStateFailNoLetter(t *testing.T) {
	b := newSaidBench(t)
	b.roundDirs(t)
	for _, acc := range b.c.Codex.Accounts { // замер: модель не зовём, журнала нет, падает только сохранение
		b.measure(t, acc.Name, time.Now(), 100, false)
	}
	b.st.DownSince = time.Now().Add(-7 * time.Hour)
	path := filepath.Join(b.a.dir("said"), "state.json")
	if err := b.st.Save(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".journal", nil, 0o400); err != nil { // Save открывает журнал на запись, Load — на чтение
		t.Fatal(err)
	}
	if err := b.a.saidRound(context.Background(), b.c, false); err == nil {
		t.Fatal("сбой сохранения не дошёл до прохода")
	}
	if line := letterLine(b.logs.String()); line != "" {
		t.Fatalf("письмо при несохранённом состоянии: %s", line)
	}
}

// Настоящее письмо — только в write: режим said решает сам, режим codex ни при чём; не ушло — Signal пуст, будет повтор.
func TestSaidStallFollowsSaidMode(t *testing.T) {
	b := newSaidBench(t)
	b.a.reg.Dir = filepath.Join(b.dir, "sessions")
	b.c.Lead = "lead"
	b.c.Codex.Mode = codexwatch.ModeAlert
	now := time.Now()
	b.st.Down(now.Add(-7*time.Hour), "сбой")
	b.a.saidStall(b.c, b.st, now)
	if !strings.Contains(b.logs.String(), "написал бы") || strings.Contains(b.logs.String(), "не отправлено") {
		t.Fatalf("режим log ушёл наружу:\n%s", b.logs.String())
	}
	b.st.Signal = time.Time{}
	b.logs.Reset()
	b.c.Said.Mode = said.ModeWrite
	b.c.Codex.Mode = codexwatch.ModeLog
	b.a.saidStall(b.c, b.st, now)
	if strings.Contains(b.logs.String(), "написал бы") || !strings.Contains(b.logs.String(), "письмо о простое не отправлено") {
		t.Fatalf("режим write не пытался отправить:\n%s", b.logs.String())
	}
	if !b.st.Signal.IsZero() {
		t.Fatal("Signal стоит, хотя письмо не ушло: повтора не будет")
	}
}

// Простой дольше Stall — письмо лиду один раз в сутки; короче — молчание; ответ модели обнуляет счёт.
func TestSaidStallSignal(t *testing.T) {
	b := newSaidBench(t)
	now := time.Now()
	count := func() int { return strings.Count(b.logs.String(), "написал бы") }
	b.st.Down(now.Add(-5*time.Hour), "out of credits")
	b.a.saidStall(b.c, b.st, now)
	if count() != 0 {
		t.Fatal("письмо раньше срока")
	}
	b.a.saidStall(b.c, b.st, now.Add(time.Hour))
	b.a.saidStall(b.c, b.st, now.Add(2*time.Hour))
	if count() != 1 {
		t.Fatalf("писем %d, журнал:\n%s", count(), b.logs.String())
	}
	b.a.saidStall(b.c, b.st, now.Add(26*time.Hour))
	if count() != 2 {
		t.Fatalf("письмо не повторилось через сутки: %d", count())
	}
	b.st.Answered()
	b.a.saidStall(b.c, b.st, now.Add(60*time.Hour))
	if count() != 2 {
		t.Fatal("письмо после ответа модели")
	}
}

// Модель не на codex: вызов без учётки, замеры OpenAI ни при чём.
func TestSaidNonCodexModelHasNoAccount(t *testing.T) {
	b := newSaidBench(t)
	b.c.Said.Model = "opus"
	b.measure(t, "work", time.Now(), 100, false)
	got, _ := b.a.saidAccounts(b.c, b.st, time.Now())
	if len(got) != 1 || got[0].Name != "" || got[0].Home != "" {
		t.Fatalf("учётки %+v", got)
	}
}
