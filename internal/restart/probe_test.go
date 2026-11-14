package restart

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/screen"
)

func write(t *testing.T, file, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadHookState(t *testing.T) {
	dir := t.TempDir()
	p := paths{hookStates: dir}
	if rec, err := readHookState(p, "s1"); err != nil || rec.state != "" {
		t.Errorf("нет записи — пустое состояние: %+v %v", rec, err)
	}
	write(t, filepath.Join(dir, "s1"), "paused\n")
	write(t, filepath.Join(dir, "s1.tasks"), `{"tasks":[{"id":"b1","type":"shell","description":"x"}],"crons":0}`)
	rec, err := readHookState(p, "s1")
	if err != nil || rec.state != "paused" || !rec.tasks || rec.written.IsZero() {
		t.Errorf("readHookState = %+v %v", rec, err)
	}
}

func TestDescendantsAndVersion(t *testing.T) {
	proc := t.TempDir()
	write(t, filepath.Join(proc, "10", "stat"), "10 (claude) S 1 10 10 0")
	write(t, filepath.Join(proc, "10", "task", "10", "children"), "11 14 99")
	write(t, filepath.Join(proc, "10", "task", "100", "children"), "15")
	write(t, filepath.Join(proc, "11", "stat"), "11 (bash) S 10 11 11 0")
	write(t, filepath.Join(proc, "11", "task", "11", "children"), "12")
	write(t, filepath.Join(proc, "12", "stat"), "12 (sleep) S 11 12 12 0")
	write(t, filepath.Join(proc, "12", "task", "12", "children"), "")
	write(t, filepath.Join(proc, "14", "stat"), "14 (python) S 10 14 14 0")
	write(t, filepath.Join(proc, "14", "task", "14", "children"), "")
	write(t, filepath.Join(proc, "15", "stat"), "15 (worker) S 10 15 15 0")
	write(t, filepath.Join(proc, "15", "task", "15", "children"), "")
	write(t, filepath.Join(proc, "13", "stat"), "13 (konsole) S 1 13 13 0")
	write(t, filepath.Join(proc, "self", "stat"), "x")
	got, err := descendants(proc, 10)
	if err != nil || !slices.Equal(got, []string{"11 bash", "12 sleep", "14 python", "15 worker"}) {
		t.Errorf("descendants = %q %v", got, err)
	}
	if err := os.Symlink("/home/u/.local/share/claude/versions/2.1.309 (deleted)", filepath.Join(proc, "10", "exe")); err != nil {
		t.Fatal(err)
	}
	if v, err := runningVersion(proc, 10); err != nil || v != "2.1.309" {
		t.Errorf("runningVersion = %q %v", v, err)
	}
}

func TestDescendantsFallsBackWithoutChildrenFiles(t *testing.T) {
	proc := t.TempDir()
	write(t, filepath.Join(proc, "20", "stat"), "20 (claude) S 1 20 20 0")
	if err := os.MkdirAll(filepath.Join(proc, "20", "task", "20"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(proc, "21", "stat"), "21 (bash) S 20 21 21 0")
	write(t, filepath.Join(proc, "22", "stat"), "22 (sleep) S 21 22 22 0")
	got, err := descendants(proc, 20)
	if err != nil || !slices.Equal(got, []string{"21 bash", "22 sleep"}) {
		t.Fatalf("fallback descendants = %q %v", got, err)
	}
}

func TestDescendantsSeenAcrossThreadsAndCycles(t *testing.T) {
	proc := t.TempDir()
	write(t, filepath.Join(proc, "40", "stat"), "40 (claude) S 1 40 40 0")
	write(t, filepath.Join(proc, "40", "task", "40", "children"), "41")
	write(t, filepath.Join(proc, "40", "task", "400", "children"), "41")
	write(t, filepath.Join(proc, "41", "stat"), "41 (bash) S 40 41 41 0")
	write(t, filepath.Join(proc, "41", "task", "41", "children"), "40")

	type result struct {
		items []string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		items, err := descendants(proc, 40)
		done <- result{items: items, err: err}
	}()
	select {
	case got := <-done:
		if got.err != nil || !slices.Equal(got.items, []string{"41 bash"}) {
			t.Fatalf("duplicate/cycle descendants = %q %v", got.items, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("цикл в children не завершил обход")
	}
}

func TestDescendantsChecksParentPID(t *testing.T) {
	proc := t.TempDir()
	write(t, filepath.Join(proc, "50", "stat"), "50 (claude) S 1 50 50 0")
	write(t, filepath.Join(proc, "50", "task", "50", "children"), "51 52")
	write(t, filepath.Join(proc, "51", "stat"), "51 (reused) S 99 51 51 0")
	write(t, filepath.Join(proc, "51", "task", "51", "children"), "")
	write(t, filepath.Join(proc, "52", "stat"), "52 (valid) S 50 52 52 0")
	write(t, filepath.Join(proc, "52", "task", "52", "children"), "")
	got, err := descendants(proc, 50)
	if err != nil || !slices.Equal(got, []string{"52 valid"}) {
		t.Fatalf("переиспользованный PID должен быть пропущен: %q %v", got, err)
	}
}

func TestDescendantsCacheReadsOnce(t *testing.T) {
	proc := t.TempDir()
	write(t, filepath.Join(proc, "30", "task", "30", "children"), "31")
	write(t, filepath.Join(proc, "31", "stat"), "31 (bash) S 30 31 31 0")
	write(t, filepath.Join(proc, "31", "task", "31", "children"), "")
	cache := &descendantsCache{}
	first, err := cache.get(proc, 30, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(proc, "31", "stat")); err != nil {
		t.Fatal(err)
	}
	second, err := cache.get(proc, 30, nil)
	if err != nil || !slices.Equal(first, second) || !slices.Equal(second, []string{"31 bash"}) {
		t.Fatalf("cache results = %q then %q, %v", first, second, err)
	}
}

func BenchmarkDescendantsFullLiveProc(b *testing.B) {
	for range b.N {
		if _, err := descendantsFull("/proc", os.Getpid()); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDescendantsSubtreeLiveProc(b *testing.B) {
	for range b.N {
		if _, err := descendants("/proc", os.Getpid()); err != nil {
			b.Fatal(err)
		}
	}
}

func TestParseStat(t *testing.T) {
	name, ppid, err := parseStat("4242 (tmux: server (x)) S 17 4242 4242 0")
	if err != nil || name != "tmux: server (x)" || ppid != 17 {
		t.Errorf("parseStat = %q %d %v", name, ppid, err)
	}
}

func TestTranscriptFile(t *testing.T) {
	dir := t.TempDir()
	p := paths{projects: dir}
	if _, _, err := transcriptFile(p, "s1"); err == nil {
		t.Error("нет транскрипта — ошибка")
	}
	write(t, filepath.Join(dir, "-home-u", "s1.jsonl"), "")
	if f, info, err := transcriptFile(p, "s1"); err != nil || f != filepath.Join(dir, "-home-u", "s1.jsonl") || info == nil || info.IsDir() {
		t.Errorf("transcriptFile = %q %v", f, err)
	}
}

// probe на подменённой сессии: сведения собраны из реестра, /proc, chooks, транскрипта и терминала, и решение
// по ним то же, что по готовым facts в decide_test.go.
func TestProbe(t *testing.T) {
	now := time.Now()
	idle := allowance{idle: time.Minute}
	w := newSessionWorld(t, worldTimes{now: now, turnAgo: 10 * time.Minute, ttyAgo: 20 * time.Minute})
	w.link(t, 4242)
	f := probe(w.p, &scanCache{}, 4242, now, false)
	if len(f.Errors) > 0 || f.Name != "x-master" || f.SessionID != "s1" || f.Running != "2.1.309" || f.Installed != "2.1.310" ||
		f.HookState != "done" || f.HookTasks || len(f.Children) != 0 {
		t.Fatalf("probe = %+v", f)
	}
	if f.Quiet != 10*time.Minute || f.TurnQuiet != 10*time.Minute || f.TTYQuiet != 20*time.Minute {
		t.Errorf("Quiet %s TurnQuiet %s TTYQuiet %s, want 10m 10m 20m", f.Quiet, f.TurnQuiet, f.TTYQuiet)
	}
	if v := decide(f, idle); !v.Restart {
		t.Errorf("простаивающая сессия: %+v", v)
	}
}

// Ввод в терминал после конца хода (d3516ca): TurnQuiet — от mtime транскрипта, иначе черновик в поле не виден.
func TestProbeDraftAfterTurn(t *testing.T) {
	now := time.Now()
	w := newSessionWorld(t, worldTimes{now: now, turnAgo: 10 * time.Minute, ttyAgo: 5 * time.Minute})
	w.link(t, 4242)
	f := probe(w.p, &scanCache{}, 4242, now, false)
	if v := decide(f, allowance{idle: time.Minute}); !v.Restart {
		t.Errorf("ввод 5 мин назад после хода 10 мин назад при пустом поле — фокус вкладки: %+v (facts %+v)", v, f)
	}
	f.Prompt = screen.Unknown
	if v := decide(f, allowance{idle: time.Minute}); v.Restart || !slices.Equal(codes(v), []string{"draft", "screen"}) {
		t.Errorf("ввод 5 мин назад после хода 10 мин назад, поля не видно — черновик: %+v (facts %+v)", v, f)
	}
}

// Идущий инструмент и потомок процесса держат перезапуск: probe передаёт их из транскрипта и /proc.
func TestProbeBusy(t *testing.T) {
	now := time.Now()
	w := newSessionWorld(t, worldTimes{now: now, turnAgo: 10 * time.Minute, ttyAgo: 20 * time.Minute},
		use(now.Add(-30*time.Minute).UTC().Format(time.RFC3339Nano), "t1", "AskUserQuestion", `{}`))
	w.link(t, 4242)
	write(t, filepath.Join(w.dir, "proc-tmpl", "task", "1", "children"), "4243")
	write(t, filepath.Join(w.p.proc, "4243", "stat"), "4243 (bash) S 4242 4243 4243 0")
	write(t, filepath.Join(w.p.proc, "4243", "task", "4243", "children"), "")
	f := probe(w.p, &scanCache{}, 4242, now, false)
	if !slices.Equal(f.Children, []string{"4243 bash"}) || !slices.Equal(f.Transcript.Pending, []string{"AskUserQuestion t1"}) {
		t.Fatalf("Children %q Pending %q", f.Children, f.Transcript.Pending)
	}
	if v := decide(f, allowance{idle: time.Minute}); v.Restart || !slices.Equal(codes(v), []string{"children", "pending"}) {
		t.Errorf("занятая сессия: %+v", v)
	}
}

// Свежая версия — дальше probe не смотрит: без транскрипта и chooks ошибки нет. Нет записи реестра — ошибка.
func TestProbeFreshAndMissing(t *testing.T) {
	now := time.Now()
	w := newSessionWorld(t, worldTimes{now: now, turnAgo: time.Minute, ttyAgo: time.Minute})
	w.link(t, 4242)
	w.setRunning(t, "2.1.310")
	if err := os.RemoveAll(w.p.projects); err != nil {
		t.Fatal(err)
	}
	if f := probe(w.p, &scanCache{}, 4242, now, false); len(f.Errors) > 0 || !decide(f, allowance{}).Fresh {
		t.Errorf("свежая версия: %+v", f)
	}
	if f := probe(w.p, &scanCache{}, 7, now, false); len(f.Errors) != 1 || decide(f, allowance{}).Restart {
		t.Errorf("нет записи реестра: %+v", f)
	}
}

// scanCache: тот же файл того же размера и времени не перечитывается, дописанный — перечитывается.
func TestScanCache(t *testing.T) {
	since, _ := time.Parse(time.RFC3339, "2026-10-24T09:00:00Z")
	file := filepath.Join(t.TempDir(), "s1.jsonl")
	write(t, file, use(tsNew, "t1", "AskUserQuestion", `{}`)+"\n")
	stat := func() os.FileInfo {
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		return info
	}
	cache := &scanCache{}
	if f, err := cache.read(file, stat(), since, since); err != nil || len(f.Pending) != 1 {
		t.Fatalf("первый разбор: %+v %v", f, err)
	}
	info := stat()
	if err := os.Rename(file, file+".gone"); err != nil {
		t.Fatal(err)
	}
	if f, err := cache.read(file, info, since, since); err != nil || len(f.Pending) != 1 {
		t.Errorf("файл не менялся — прошлый разбор без чтения: %+v %v", f, err)
	}
	if err := os.Rename(file+".gone", file); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(result(tsNew, "t1", "ok", `null`) + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := cache.read(file, stat(), since, since); err != nil || len(got.Pending) != 0 {
		t.Errorf("дописанный файл — новый разбор: %+v %v", got, err)
	}

	// недописанная строка ждёт перевода строки; после него разбор продолжается с её начала
	half := use(tsNew, "t2", "AskUserQuestion", `{}`)
	appendTo(t, file, half[:len(half)/2])
	if got, err := cache.read(file, stat(), since, since); err != nil || len(got.Pending) != 0 {
		t.Errorf("недописанная строка: %+v %v", got, err)
	}
	appendTo(t, file, half[len(half)/2:]+"\n")
	if got, err := cache.read(file, stat(), since, since); err != nil || !slices.Equal(got.Pending, []string{"AskUserQuestion t2"}) {
		t.Errorf("дописанная строка: %+v %v", got, err)
	}
	// файл стал короче разобранного — разбор заново
	write(t, file, use(tsNew, "t3", "AskUserQuestion", `{}`)+"\n")
	if got, err := cache.read(file, stat(), since, since); err != nil || !slices.Equal(got.Pending, []string{"AskUserQuestion t3"}) {
		t.Errorf("короткий файл: %+v %v", got, err)
	}
}

func appendTo(t *testing.T, file, s string) {
	t.Helper()
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// .tasks chooks: монитор артефакта — не задача, а объяснение паузы; таймеры по chooks — тоже; прочие задачи держат.
func TestReadHookTasks(t *testing.T) {
	dir := t.TempDir()
	p := paths{hookStates: dir}
	write(t, filepath.Join(dir, "s1"), "paused")
	artifact := `{"id":"sxtctgkwu","type":"monitor","description":"live updates for artifact https://claude.ai/artifact/HPY6 (auto-armed on publish)","since":"2026-10-27T05:01:26+01:00"}`
	shell := `{"id":"b7xi68io0","type":"shell","description":"Grep transcript for user quote","since":"2026-10-27T04:03:35+01:00"}`
	agent := `{"id":"a1","type":"subagent","description":"ревью","since":"2026-10-27T04:03:35+01:00"}`
	for name, c := range map[string]struct {
		file                 string
		tasks, waits, agents bool
	}{
		"нет файла":         {"", false, false, false},
		"монитор артефакта": {`{"tasks":[` + artifact + `],"crons":0}`, false, true, false},
		"монитор и команда": {`{"tasks":[` + artifact + `,` + shell + `],"crons":0}`, true, true, false},
		"только таймеры":    {`{"tasks":[],"crons":0,"schedules":2}`, false, true, false},
		"суб-агент":         {`{"tasks":[` + shell + `,` + agent + `],"crons":0}`, true, false, true},
	} {
		tasksFile := filepath.Join(dir, "s1.tasks")
		if c.file != "" {
			write(t, tasksFile, c.file)
		} else if err := os.Remove(tasksFile); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		rec, err := readHookState(p, "s1")
		if err != nil || rec.tasks != c.tasks || rec.waits != c.waits || rec.agents != c.agents {
			t.Errorf("%s: %+v %v", name, rec, err)
		}
	}
	write(t, filepath.Join(dir, "s1.tasks"), "{")
	if rec, err := readHookState(p, "s1"); err == nil || !rec.tasks {
		t.Errorf("нечитаемый .tasks — ошибка и сомнение в пользу задач: %+v %v", rec, err)
	}
}

// Папка не в репозитории — чистая: 27.10 «git status» в ~/git падал с кодом 128, и ошибка шла доводом против сброса.
func TestGitStatus(t *testing.T) {
	if got, err := gitStatus(t.TempDir()); err != nil || got != nil {
		t.Fatalf("не репозиторий: %v %v", got, err)
	}
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	write(t, filepath.Join(dir, "a"), "x")
	if got, err := gitStatus(dir); err != nil || !slices.Equal(got, []string{"?? a"}) {
		t.Fatalf("грязное дерево: %v %v", got, err)
	}
	if _, err := gitStatus(filepath.Join(dir, "нет")); err == nil {
		t.Error("нет папки — ошибка")
	}
}

// Серая подсказка Claude Code: в поле текст, но ввода в терминал после отправки последней реплики typed не было —
// набрать его было нечем. Ввод после реплики, реплика queued или от сессии — набранный текст, держит экран.
func TestProbeSuggestion(t *testing.T) {
	now := time.Now()
	human := func(src, kind string) string {
		return `{"type":"user","timestamp":"` + now.Add(-15*time.Minute).UTC().Format(time.RFC3339Nano) + `","promptSource":"` + src +
			`","origin":{"kind":"` + kind + `"},"message":{"role":"user","content":"сделай"}}`
	}
	for name, c := range map[string]struct {
		rec    string
		ttyAgo time.Duration
		want   bool
	}{
		"ввода после реплики нет":       {human("typed", "human"), 20 * time.Minute, true},
		"ввод после реплики":            {human("typed", "human"), 12 * time.Minute, false},
		"реплика набрана во время хода": {human("queued", "human"), 20 * time.Minute, false},
		"реплика от сессии":             {human("system", "peer"), 20 * time.Minute, false},
		"реплика от сессии, typed":      {human("typed", "peer"), 20 * time.Minute, false},
	} {
		w := newSessionWorld(t, worldTimes{now: now, turnAgo: 10 * time.Minute, ttyAgo: c.ttyAgo}, c.rec)
		w.p.prompt = func(int) (screen.Prompt, error) { return screen.Draft, nil }
		w.link(t, 4242)
		f := probe(w.p, &scanCache{}, 4242, now, false)
		if len(f.Errors) > 0 || f.Suggestion != c.want {
			t.Errorf("%s: Suggestion %v, want %v (errors %q)", name, f.Suggestion, c.want, f.Errors)
			continue
		}
		v := decide(f, allowance{idle: time.Minute})
		if held := slices.Contains(codes(v), "screen"); held == c.want {
			t.Errorf("%s: довод screen %v: %+v", name, held, v)
		}
	}
}
