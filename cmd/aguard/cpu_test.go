package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/cpuwatch"
	"github.com/aiseeq/aguard/internal/iowatch"
	"github.com/aiseeq/aguard/internal/policy"
	"github.com/aiseeq/akit/pressure"
	"github.com/aiseeq/akit/session"
)

// cpuApp — app наблюдателя cpu на временной папке: реестр сессий пуст, журнал — в logs.
func cpuApp(t *testing.T, logs *strings.Builder) *app {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	return &app{home: dir, stateDir: filepath.Join(dir, "state"), uid: 1000,
		reg: session.Registry{Dir: filepath.Join(dir, "sessions"), ProcRoot: dir}, log: slog.New(slog.NewTextHandler(logs, nil))}
}

func TestCpuRoundCgroupDetailOwnerLetter(t *testing.T) {
	var logs strings.Builder
	a := cpuApp(t, &logs)
	cfg := cpuwatch.Default(a.home)
	cfg.Mode, cfg.Hosts, cfg.Lead = cpuwatch.ModeLog, nil, "lead"
	cfg.Workers = filepath.Join(a.home, "worker")
	for id, master := range map[string]string{"short": "worker-master", "stale": "stale-master"} {
		job := fmt.Sprintf(`{"id":%q,"dir":"/tmp","task":%q,"master":%q,"unit":%q}`, id, id, master, "worker-"+id)
		cpuWrite(t, filepath.Join(cfg.Workers, id, "job.json"), job)
	}
	fs := filepath.Join(a.home, "cgroupfs")
	a.cgroupDir = fs
	h := a.cpuHosts(&cfg)[0]
	wantRoot := filepath.Join(fs, "user.slice", "user-1000.slice", "user@1000.service")
	if h.cgroup != wantRoot {
		t.Fatalf("локальный cgroup root = %q, want %q", h.cgroup, wantRoot)
	}
	proc := filepath.Join(a.home, "proc")
	a.procDir = proc
	h.root = proc
	writeCPUProc(t, proc, "/user.slice/user-1000.slice/user@1000.service/app.slice/worker-short.service")
	cpuStatWrite(t, h.cgroup, "", 0)
	cpuStatWrite(t, h.cgroup, "app.slice", 0)
	base := time.Now().Add(-time.Minute).Truncate(time.Second)
	owner := func(*cpuwatch.Task) cpuwatch.Owner { return cpuwatch.Owner{} }
	if _, ready, err := h.cpuUsage.Sample(h.cgroup, base, owner, nil); err != nil || ready {
		t.Fatalf("CPU base: ready=%v err=%v", ready, err)
	}
	h.cpuDetailAt = base
	cpuStatWrite(t, h.cgroup, "", 400_000_000)
	cpuStatWrite(t, h.cgroup, "app.slice", 400_000_000)
	io := newIOService(context.Background(), a)
	io.rec = iowatch.NewRecorder(iowatch.Default())
	io.detail(iowatch.Step{At: base.Add(-time.Second), Cgroups: []iowatch.Cgroup{{Path: "/user.slice/user-1000.slice/user@1000.service/app.slice/worker-stale.service", CPUUS: 400_000_000}}})
	io.detail(iowatch.Step{At: base.Add(30 * time.Second), Cgroups: []iowatch.Cgroup{{Path: "/user.slice/user-1000.slice/user@1000.service/app.slice/worker-short.service", CPUUS: 400_000_000}}})
	now := time.Now()
	h.win.Restore(cpuwatch.State{Since: now.Add(-11 * time.Minute), Last: now.Add(-time.Minute)})
	h.strayAt = now
	a.strayRead = func(context.Context, *cpuHost) ([]byte, error) { return []byte("##procs\n##strays\n##end\n"), nil }
	if err := a.cpuRound(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "session=worker-master") || !strings.Contains(logs.String(), "по CPU за интервал замера 6.") || !strings.Contains(logs.String(), "interval_s=60.") || !strings.Contains(logs.String(), "worker-master/short=6.") {
		t.Fatalf("10 мин перегрузки по коротким процессам должны написать мастеру, снимок из 4 потоков — пояснение:\n%s", logs.String())
	}
}

func cpuWrite(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func cpuStatWrite(t *testing.T, root, rel string, n int64) {
	t.Helper()
	cpuWrite(t, filepath.Join(root, rel, "cpu.stat"), fmt.Sprintf("usage_usec %d\nuser_usec 0\nsystem_usec 0\n", n))
}

func writeCPUProc(t *testing.T, root, cgroup string) {
	writeCPUProcN(t, root, cgroup, 4)
}

func writeCPUProcN(t *testing.T, root, cgroup string, count int) {
	t.Helper()
	var stat strings.Builder
	stat.WriteString("cpu 1 1 1 1\nbtime 1000\nprocs_running 30\n")
	for i := range 22 {
		fmt.Fprintf(&stat, "cpu%d 1 1 1 1\n", i)
	}
	cpuWrite(t, filepath.Join(root, "stat"), stat.String())
	cpuWrite(t, filepath.Join(root, "loadavg"), "30.00 30.00 30.00 30/100 1\n")
	for i := range count {
		pid := 123 + i
		dir := filepath.Join(root, "123")
		tid := fmt.Sprint(pid)
		cpuWrite(t, filepath.Join(dir, "task", tid, "stat"), tid+" (sh) R 1 2 3\n")
	}
	cpuWrite(t, filepath.Join(root, "123", "comm"), "sh\n")
	cpuWrite(t, filepath.Join(root, "123", "cgroup"), "0::"+cgroup+"\n")
	cpuWrite(t, filepath.Join(root, "123", "environ"), "")
}

func TestCPUShareLogTopEightAndRemainder(t *testing.T) {
	loads := make([]cpuwatch.Load, 10)
	for i := range loads {
		loads[i] = cpuwatch.Load{Owner: cpuwatch.Owner{Master: fmt.Sprintf("m%d", i), Kind: cpuwatch.KindSession, Label: fmt.Sprintf("scope-%d", i)}, CPU: float64(10 - i)}
	}
	got := cpuwatch.CPUShareSummary(loads)
	if strings.Count(got, "=") != 9 || !strings.Contains(got, "m0/scope-0=10.00") || !strings.Contains(got, "прочие=3.00") {
		t.Fatalf("top 8 с метками и сумма остальных: %s", got)
	}
}

func TestCPUTrackingBaseAndReadErrorFallBackToSnapshot(t *testing.T) {
	for _, mode := range []string{"baseline", "error"} {
		t.Run(mode, func(t *testing.T) {
			var logs strings.Builder
			a := cpuApp(t, &logs)
			cfg := cpuwatch.Default(a.home)
			cfg.Mode, cfg.Hosts, cfg.Lead = cpuwatch.ModeLog, nil, "lead"
			cfg.Workers = filepath.Join(a.home, "worker")
			cpuWrite(t, filepath.Join(cfg.Workers, "short", "job.json"), `{"id":"short","dir":"/tmp","task":"short","master":"worker-master","unit":"worker-short"}`)
			mount := filepath.Join(a.home, "cgroupfs")
			a.cgroupDir = mount
			h := a.cpuHosts(&cfg)[0]
			proc := filepath.Join(a.home, "proc")
			h.root = proc
			writeCPUProcN(t, proc, "/user.slice/user-1000.slice/user@1000.service/app.slice/worker-short.service", 6)
			cpuStatWrite(t, h.cgroup, "", 0)
			cpuStatWrite(t, h.cgroup, "app.slice", 0)
			if mode == "error" {
				cpuWrite(t, filepath.Join(h.cgroup, "cpu.stat"), "usage_usec broken\n")
			}
			now := time.Now()
			h.win.Restore(cpuwatch.State{Since: now.Add(-11 * time.Minute), Last: now.Add(-time.Minute)})
			h.strayAt = now
			a.strayRead = func(context.Context, *cpuHost) ([]byte, error) { return []byte("##procs\n##strays\n##end\n"), nil }
			if err := a.cpuRound(context.Background(), &cfg); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(logs.String(), "session=worker-master") {
				t.Fatalf("%s: без CPU дельты применяется снимок из 6 готовых задач:\n%s", mode, logs.String())
			}
			if !strings.Contains(logs.String(), `owners="по снимку" snapshot_threads=6 runnable=29`) || strings.Contains(logs.String(), `owners="worker-master/short=0.00"`) {
				t.Fatalf("%s: журнал без готовой CPU-дельты должен явно указывать снимок и число потоков:\n%s", mode, logs.String())
			}
		})
	}
}

func TestCPUDetailCursorReadsLateTimestampOnce(t *testing.T) {
	var logs strings.Builder
	a := cpuApp(t, &logs)
	base := time.Now().Truncate(time.Second)
	cursor := base.Add(-time.Minute)
	a.saveCPUDetail(iowatch.Step{At: base.Add(-10 * time.Second), Cgroups: []iowatch.Cgroup{{Path: "/cg/old", CPUUS: 11}}})
	first := a.cpuDetailDeltas(&cursor, base)
	if first["/cg/old"] != 11 {
		t.Fatalf("первый проход: %v", first)
	}
	// Поступила после прошлого замера, хотя её время раньше его; она лежит после последней прочитанной записи.
	a.saveCPUDetail(iowatch.Step{At: base.Add(-5 * time.Second), Cgroups: []iowatch.Cgroup{{Path: "/cg/late", CPUUS: 23}}})
	second := a.cpuDetailDeltas(&cursor, base.Add(time.Minute))
	if second["/cg/late"] != 23 || second["/cg/old"] != 0 {
		t.Fatalf("поздняя запись должна читаться отдельно от уже выданной: %v", second)
	}
	third := a.cpuDetailDeltas(&cursor, base.Add(2*time.Minute))
	if len(third) != 0 {
		t.Fatalf("прочитанные записи не должны учитываться повторно: %v", third)
	}
}

func TestCPUActSnapshotJournalForBaselineAndPC(t *testing.T) {
	for _, host := range []string{"ноут", "pc"} {
		t.Run(host, func(t *testing.T) {
			var logs strings.Builder
			a := cpuApp(t, &logs)
			cfg := cpuwatch.Default(a.home)
			cfg.After = policy.Duration(0)
			now := time.Now()
			v := cpuwatch.Verdict{Over: true, Started: true, Since: now.Add(-time.Minute), Runnable: 29, Threads: 22,
				Loads: []cpuwatch.Load{{Owner: cpuwatch.Owner{Master: "master", Kind: cpuwatch.KindSession}, N: 7}}}
			a.cpuAct(&cfg, &cpuwatch.Host{Name: host}, &v, now)
			if !strings.Contains(logs.String(), `owners="по снимку" snapshot_threads=7 runnable=29`) || strings.Contains(logs.String(), "interval_s=") {
				t.Fatalf("снимок без CPU дельты должен журналироваться явно: %s", logs.String())
			}
		})
	}
}

// cpuSample — замер при 22 потоках: load average за минуту, готовые сейчас и готовые потоки по мастерам.
func cpuSample(at time.Time, avg1 float64, runnable int, by map[string]int) *cpuwatch.Sample {
	s := &cpuwatch.Sample{At: at, Load: pressure.LoadAvg{Avg1: avg1}, Runnable: runnable, Threads: 22}
	for m, n := range by {
		s.Tasks = append(s.Tasks, cpuwatch.Task{N: n, Comm: "go", Cgroup: m})
	}
	return s
}

func cpuOwner(t *cpuwatch.Task) cpuwatch.Owner {
	return cpuwatch.Owner{Master: t.Cgroup, Kind: cpuwatch.KindSession}
}

// Письма cpuAct: не раньше after подряд; сводка лиду и письмо ему же как мастеру — порознь; в сводке — все, кому
// писали за перегрузку; длительность кончившейся — без пропуска замеров.
func TestCpuAct(t *testing.T) {
	var logs strings.Builder
	a := cpuApp(t, &logs)
	cfg := cpuwatch.Default(a.home)
	cfg.Mode, cfg.Lead = cpuwatch.ModeLog, "lead"
	h := cpuwatch.Host{Name: cpuwatch.HostLocal} // замеры реже, чем раз в минуту: серию рвёт только Break
	start := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	act := func(at time.Time, s *cpuwatch.Sample) {
		v := h.Judge(s, cpuOwner)
		a.cpuAct(&cfg, &h, &v, at)
	}
	heavy := map[string]int{"lead": 20, "glint-master": 10}

	act(start, cpuSample(start, 30, 30, heavy))
	if strings.Contains(logs.String(), "написал бы") {
		t.Fatalf("перегрузка только началась — писем до after нет:\n%s", logs.String())
	}

	at := start.Add(10 * time.Minute)
	logs.Reset()
	act(at, cpuSample(at, 30, 30, heavy))
	if n := strings.Count(logs.String(), "написал бы\" session=lead "); n != 2 {
		t.Errorf("лиду — письмо как мастеру и сводка, а не %d:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "session=glint-master") {
		t.Errorf("мастеру с 10 задачами из 22 — письмо:\n%s", logs.String())
	}

	// через repeat у glint-master задач мало: ему не пишем, но в сводке он — среди тех, кому писали за перегрузку
	at = start.Add(40 * time.Minute)
	logs.Reset()
	act(at, cpuSample(at, 30, 30, map[string]int{"lead": 20, "glint-master": 1}))
	if strings.Contains(logs.String(), "session=glint-master") ||
		!strings.Contains(logs.String(), "Написал за перегрузку: glint-master, lead.") {
		t.Errorf("сводка — обо всех письмах за перегрузку:\n%s", logs.String())
	}

	// замер не удался, следующий — через 2 минуты: прошлая перегрузка длилась до последнего удачного замера
	h.Break()
	at = start.Add(42 * time.Minute)
	logs.Reset()
	act(at, cpuSample(at, 30, 30, heavy))
	if !strings.Contains(logs.String(), "перегрузка кончилась") || !strings.Contains(logs.String(), "lasted=40m0s") ||
		!strings.Contains(logs.String(), "load1=30 runnable=30 threads=22") {
		t.Errorf("длительность без пропуска — 40 минут:\n%s", logs.String())
	}
}

// Сбой замера в проходе рвёт перегрузку «подряд», а интервал проход отдаёт окну машины.
func TestCpuRoundBreak(t *testing.T) {
	var logs strings.Builder
	a := cpuApp(t, &logs)
	cfg := cpuwatch.Default(a.home)
	cfg.Mode, cfg.Hosts, cfg.Lead = cpuwatch.ModeLog, nil, "lead"
	h := &cpuHost{root: filepath.Join(a.home, "absent"), state: filepath.Join(a.dir("cpu"), "local.json"),
		win: cpuwatch.Host{Name: cpuwatch.HostLocal}}
	a.cpu = map[string]*cpuHost{"": h}
	start := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	h.win.Judge(cpuSample(start, 30, 30, nil), cpuOwner)
	if err := a.cpuRound(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	if !h.win.Down || !strings.Contains(logs.String(), "не удался") {
		t.Fatalf("сбой замера не замечен:\n%s", logs.String())
	}
	if h.win.Every != cfg.Interval.D() {
		t.Errorf("интервал окна %v, хотим %v", h.win.Every, cfg.Interval.D())
	}
	at := start.Add(time.Minute)
	if v := h.win.Judge(cpuSample(at, 30, 30, nil), cpuOwner); !v.Started || !v.Ended.Equal(start) {
		t.Errorf("после сбоя замера перегрузка — новая серия: %+v", v)
	}
}

// Пробник показывает сводку лиду такой, какой она уйдёт: с теми, кому написано строкой выше (cpu-dry-told).
func TestCpuDryLettersLeadNamesRecipients(t *testing.T) {
	cfg := cpuwatch.Default(t.TempDir())
	cfg.Lead = "lead"
	now := time.Date(2026, 10, 31, 15, 3, 0, 0, time.UTC)
	h := cpuwatch.Host{Name: "pc"}
	v := h.Judge(cpuSample(now, 33.9, 43, map[string]int{"sc2go-master": 20, "demu-master": 10, "chooks-master": 1}), cpuOwner)
	var out strings.Builder
	cpuDryLetters(&out, &cfg, &h, &v, now)
	if !strings.Contains(out.String(), "Написал за перегрузку: demu-master, sc2go-master") {
		t.Fatalf("сводка лиду без адресатов:\n%s", out.String())
	}
	if strings.Contains(out.String(), "никому") {
		t.Fatalf("в сводке «никому» при письмах строкой выше:\n%s", out.String())
	}
}

// Проход демона сам сохраняет окно, удачный и неудачный, а новый процесс читает его при старте.
func TestCpuRoundPersistsWindow(t *testing.T) {
	var logs strings.Builder
	a := cpuApp(t, &logs)
	cfg := cpuwatch.Default(a.home)
	cfg.Mode, cfg.Hosts, cfg.Lead = cpuwatch.ModeLog, nil, "lead"
	state := filepath.Join(a.dir("cpu"), "local.json")
	h := &cpuHost{root: filepath.Join(a.home, "absent"), state: state, win: cpuwatch.Host{Name: cpuwatch.HostLocal}}
	a.cpu = map[string]*cpuHost{"": h}
	start := time.Now().Add(-time.Minute).Truncate(time.Second)
	h.win.Judge(cpuSample(start, 30, 30, nil), cpuOwner)
	if err := a.cpuRound(context.Background(), &cfg); err != nil { // замер не удался: сохранён и сбой
		t.Fatal(err)
	}

	next := *a // новый процесс: окна в памяти нет, файл тот же
	next.cpu = map[string]*cpuHost{}
	loaded := next.cpuHosts(&cfg)
	if len(loaded) != 1 || loaded[0].state != state {
		t.Fatalf("машины нового процесса: %+v", loaded)
	}
	if got := loaded[0].win.State(); !got.Since.Equal(start) || !got.Broken {
		t.Fatalf("новый процесс не прочитал окно: %+v", got)
	}

	root := filepath.Join(a.home, "proc") // замер удался, load average ниже потоков: серия кончилась и это сохранено
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"loadavg": "1.00 1.00 1.00 1/100 1\n",
		"stat":    "cpu  1 1 1 1\ncpu0 1 1 1 1\ncpu1 1 1 1 1\nprocs_running 2\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h.root = root
	if err := a.cpuRound(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	var after cpuwatch.Host
	if err := loadCPUState(state, &after); err != nil {
		t.Fatal(err)
	}
	if got := after.State(); !got.Since.IsZero() || got.Broken || got.Last.IsZero() {
		t.Fatalf("удачный проход не сохранил окно: %+v", got)
	}

	if err := os.WriteFile(state, []byte("{"), 0o600); err != nil { // битый файл: окно с нуля и запись в журнал
		t.Fatal(err)
	}
	logs.Reset()
	broken := *a
	broken.cpu = map[string]*cpuHost{}
	if got := broken.cpuHosts(&cfg)[0].win.State(); !got.Since.IsZero() || !strings.Contains(logs.String(), "не прочитано") {
		t.Fatalf("битое состояние: окно %+v, журнал:\n%s", got, logs.String())
	}

	cfg.Hosts = []string{"pc"} // у машины по ssh своё окно: файлы не перемешиваются
	remote := next.cpuHosts(&cfg)
	if want := filepath.Join(a.dir("cpu"), "host-pc.json"); remote[len(remote)-1].state != want {
		t.Errorf("файл окна pc: %q, хотим %q", remote[len(remote)-1].state, want)
	}
}

// Активная серия сквозь настоящие проходы: cpuRound сохраняет её, новый процесс читает через cpuHosts, продолжает и
// пишет письмо, а третий процесс видит отметку письма и раньше repeat не пишет.
func TestCpuRoundContinuesActiveSeriesAfterRestart(t *testing.T) {
	var logs strings.Builder
	a := cpuApp(t, &logs)
	cfg := cpuwatch.Default(a.home)
	cfg.Mode, cfg.Hosts, cfg.Lead = cpuwatch.ModeLog, nil, "lead"
	root := filepath.Join(a.home, "proc")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"loadavg": "30.00 1.00 1.00 30/100 1\n",
		"stat":    "cpu  1 1 1 1\ncpu0 1 1 1 1\ncpu1 1 1 1 1\nprocs_running 30\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	state := filepath.Join(a.dir("cpu"), "local.json")
	now := time.Now().Truncate(time.Second)
	since := now.Add(-11 * time.Minute)
	first := &cpuHost{root: root, state: state, win: cpuwatch.Host{Name: cpuwatch.HostLocal}}
	first.win.Restore(cpuwatch.State{Since: since, Last: now.Add(-time.Minute)})
	a.cpu = map[string]*cpuHost{"": first}
	want := cfg.After
	cfg.After = policy.Duration(time.Hour) // до рестарта серия короче after: писем ещё нет
	if err := a.cpuRound(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	if got := first.win.State(); !got.Since.Equal(since) || strings.Contains(logs.String(), "написал бы") {
		t.Fatalf("проход оборвал серию или написал раньше after: %+v\n%s", got, logs.String())
	}
	cfg.After = want

	restart := func() *cpuHost {
		next := *a
		next.cpu = map[string]*cpuHost{}
		hosts := next.cpuHosts(&cfg)
		if len(hosts) != 1 {
			t.Fatalf("машины нового процесса: %+v", hosts)
		}
		hosts[0].root = root
		next.cpu[""] = hosts[0]
		logs.Reset()
		if err := next.cpuRound(context.Background(), &cfg); err != nil {
			t.Fatal(err)
		}
		return hosts[0]
	}
	saved := restart()
	if got := saved.win.State(); !got.Since.Equal(since) {
		t.Fatalf("новый процесс продолжает активную серию с её начала: %+v", got)
	}
	var onDisk cpuwatch.Host
	if err := loadCPUState(state, &onDisk); err != nil {
		t.Fatal(err)
	}
	if got := onDisk.State(); !got.Since.Equal(since) || got.Sent[leadKey+"lead"].IsZero() {
		t.Fatalf("активная серия и отметка сводки лиду не на диске: %+v", got)
	}
	if !strings.Contains(logs.String(), "написал бы") {
		t.Fatalf("первый проход после after сверх 10 минут пишет лиду:\n%s", logs.String())
	}
	restart()
	if strings.Contains(logs.String(), "написал бы") {
		t.Fatalf("после второго рестарта письмо раньше repeat:\n%s", logs.String())
	}
}

// Рестарт на 9-й минуте: письмо мастеру и сводка лиду уходят на 10-й от начала, следующие — не раньше repeat.
func TestCpuLetterAfterRestart(t *testing.T) {
	var logs strings.Builder
	a := cpuApp(t, &logs)
	cfg := cpuwatch.Default(a.home)
	cfg.Mode, cfg.Lead = cpuwatch.ModeLog, "lead"
	path := filepath.Join(a.dir("cpu"), "local.json")
	start := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	heavy := map[string]int{"glint-master": 10, "akit-master": 10} // двое мастеров — и сводка лиду
	h := cpuwatch.Host{Name: cpuwatch.HostLocal, Every: time.Minute}
	round := func(i int) string {
		at := start.Add(time.Duration(i) * time.Minute)
		logs.Reset()
		v := h.Judge(cpuSample(at, 30, 30, heavy), cpuOwner)
		a.cpuAct(&cfg, &h, &v, at)
		return logs.String()
	}
	for i := range 10 { // минуты 0–9: серия ещё короче after
		if out := round(i); strings.Contains(out, "написал бы") {
			t.Fatalf("минута %d: письмо раньше after:\n%s", i, out)
		}
	}
	if err := saveCPUState(path, &h); err != nil {
		t.Fatal(err)
	}
	h = cpuwatch.Host{Name: cpuwatch.HostLocal, Every: time.Minute} // новый процесс
	if err := loadCPUState(path, &h); err != nil {
		t.Fatal(err)
	}
	if out := round(10); !strings.Contains(out, "session=glint-master") || !strings.Contains(out, "session=lead") {
		t.Fatalf("после рестарта отсчёт идёт от начала серии, письма на 10-й минуте:\n%s", out)
	}
	if err := saveCPUState(path, &h); err != nil {
		t.Fatal(err)
	}
	h = cpuwatch.Host{Name: cpuwatch.HostLocal, Every: time.Minute} // второй рестарт: отметки писем тоже с диска
	if err := loadCPUState(path, &h); err != nil {
		t.Fatal(err)
	}
	if out := round(11); strings.Contains(out, "написал бы") {
		t.Fatalf("письмо раньше repeat после рестарта:\n%s", out)
	}
}

// Перегрузка одного мастера — письмо ему, сводки лиду нет; второй мастер в той же перегрузке — сводка лиду.
func TestCpuLeadOnlyWithoutSingleOwner(t *testing.T) {
	var logs strings.Builder
	a := cpuApp(t, &logs)
	cfg := cpuwatch.Default(a.home)
	cfg.Mode, cfg.Lead = cpuwatch.ModeLog, "lead"
	h := cpuwatch.Host{Name: cpuwatch.HostLocal}
	start := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	act := func(at time.Time, heavy map[string]int) string {
		logs.Reset()
		v := h.Judge(cpuSample(at, 30, 30, heavy), cpuOwner)
		a.cpuAct(&cfg, &h, &v, at)
		return logs.String()
	}
	act(start, map[string]int{"glint-master": 20})
	out := act(start.Add(10*time.Minute), map[string]int{"glint-master": 20})
	if !strings.Contains(out, "session=glint-master") || strings.Contains(out, "session=lead") {
		t.Fatalf("один мастер — письмо ему, лиду нет:\n%s", out)
	}
	out = act(start.Add(11*time.Minute), map[string]int{"glint-master": 10, "akit-master": 10})
	if !strings.Contains(out, "session=akit-master") || !strings.Contains(out, "session=lead") {
		t.Fatalf("второй мастер — письмо ему и сводка лиду:\n%s", out)
	}
	h = cpuwatch.Host{Name: cpuwatch.HostLocal}
	act(start, nil)
	if out := act(start.Add(10*time.Minute), nil); !strings.Contains(out, "session=lead") {
		t.Fatalf("без мастеров — сводка лиду:\n%s", out)
	}
}

func TestCPUWindowSurvivesRestartAndKeepsRepeat(t *testing.T) {
	start := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "cpu", "local.json")
	h := cpuwatch.Host{Name: cpuwatch.HostLocal, Every: time.Minute}
	for i := range 10 {
		at := start.Add(time.Duration(i) * time.Minute)
		if v := h.Judge(cpuSample(at, 40, 30, nil), cpuOwner); !v.Over {
			t.Fatalf("замер %d: LA должен держать серию: %+v", i, v)
		}
	}
	h.Sent("lead", start.Add(5*time.Minute))
	if err := saveCPUState(path, &h); err != nil {
		t.Fatal(err)
	}
	var restored cpuwatch.Host
	if err := loadCPUState(path, &restored); err != nil {
		t.Fatal(err)
	}
	restored.Every = time.Minute
	at := start.Add(10 * time.Minute)
	v := restored.Judge(cpuSample(at, 40, 30, nil), cpuOwner)
	if !v.Long(at, 10*time.Minute) || !v.Since.Equal(start) {
		t.Fatalf("после рестарта серия продолжается от исходного времени: %+v", v)
	}
	if restored.Due("lead", at, 30*time.Minute) {
		t.Fatal("рестарт не должен сбрасывать интервал повторного письма")
	}
	if !restored.Due("lead", start.Add(35*time.Minute), 30*time.Minute) {
		t.Fatal("письмо снова доступно по истечении repeat")
	}
}

func TestCPUStateGapAfterRestartAndCorruptState(t *testing.T) {
	start := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "cpu", "local.json")
	h := cpuwatch.Host{Name: cpuwatch.HostLocal, Every: time.Minute}
	h.Judge(cpuSample(start, 40, 30, nil), cpuOwner)
	if err := saveCPUState(path, &h); err != nil {
		t.Fatal(err)
	}
	var restored cpuwatch.Host
	if err := loadCPUState(path, &restored); err != nil {
		t.Fatal(err)
	}
	restored.Every = time.Minute
	at := start.Add(4 * time.Minute)
	v := restored.Judge(cpuSample(at, 40, 30, nil), cpuOwner)
	if !v.Started || !v.Ended.Equal(start) || !v.Since.Equal(at) {
		t.Fatalf("пропуск больше двух интервалов после рестарта рвёт старую серию: %+v", v)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	var empty cpuwatch.Host
	if err := loadCPUState(path, &empty); err == nil {
		t.Fatal("битое состояние должно вернуть ошибку для журнала")
	}
	if v := empty.Judge(cpuSample(at, 40, 30, nil), cpuOwner); !v.Started || !v.Since.Equal(at) {
		t.Fatalf("после битого состояния окно начинается с нуля: %+v", v)
	}
}
