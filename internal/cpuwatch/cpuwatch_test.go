package cpuwatch

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/pressure"
)

const userSlice = "/user.slice/user-1000.slice/user@1000.service"

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeProc — /proc с процессами: pid → состояния потоков, comm, cgroup, окружение.
type fakeProc struct {
	states string
	comm   string
	cgroup string
	env    string
}

func makeProc(t *testing.T, procs map[string]fakeProc) string {
	root := t.TempDir()
	write(t, filepath.Join(root, "stat"), "cpu  1 2 3\ncpu0 1 2 3\ncpu1 1 2 3\nprocs_running 5\n")
	for pid, p := range procs {
		for i, st := range p.states {
			tid := pid + "0" + string(rune('0'+i))
			write(t, filepath.Join(root, pid, "task", tid, "stat"), tid+" ("+p.comm+") "+string(st)+" 1 2 3")
		}
		write(t, filepath.Join(root, pid, "comm"), p.comm+"\n")
		write(t, filepath.Join(root, pid, "cgroup"), "0::"+p.cgroup+"\n")
		write(t, filepath.Join(root, pid, "environ"), p.env)
	}
	return root
}

func TestReadTasks(t *testing.T) {
	root := makeProc(t, map[string]fakeProc{
		"10": {states: "RRS", comm: "go) R x", cgroup: userSlice + "/app.slice/worker-a.service", env: "A=1\x00CLAUDE_PID=77\x00"},
		"11": {states: "SS", comm: "idle", cgroup: "/system.slice/x.service"},
		"12": {states: "R", comm: "ssh-run", cgroup: "/user.slice/user-1000.slice/session-3.scope", env: "SSH_CONNECTION=1.2.3.4 50123 5.6.7.8 22\x00"},
		"13": {states: "R", comm: "odd", cgroup: "/x", env: "CLAUDE_PID=abc\x00"},
	})
	got, err := ReadTasks(root)
	if err == nil || !strings.Contains(err.Error(), `CLAUDE_PID "abc"`) {
		t.Errorf("кривой CLAUDE_PID не замечен: %v", err)
	}
	if len(got) != 3 || got[2].Pid != 13 || got[2].Cgroup != "/x" {
		t.Fatalf("процесс с кривым окружением выпал из замера: %+v", got)
	}
	got = got[:2]
	slices.SortFunc(got, func(a, b Task) int { return a.Pid - b.Pid })
	want := []Task{
		{Pid: 10, N: 2, Comm: "go) R x", Cgroup: userSlice + "/app.slice/worker-a.service", Claude: 77},
		{Pid: 12, N: 1, Comm: "ssh-run", Cgroup: "/user.slice/user-1000.slice/session-3.scope", SSH: "1.2.3.4 50123 5.6.7.8 22"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("ReadTasks:\n got %+v\nwant %+v", got, want)
	}
}

func TestParseRemote(t *testing.T) {
	raw := "##stat\ncpu  1\ncpu0 1\ncpu1 1\nprocs_running 9\n##loadavg\n2.50 2.25 0.75 9/700 4242\n##tasks\n" +
		"4\t200\tcompile\t" + userSlice + "/app.slice/farm-x.0123456789abcdef.scope\t\t\n" +
		"1\t100\tsh\t/user.slice/user-1000.slice/session-9.scope\t\t10.0.0.1 40000 10.0.0.2 22\n##end\n"
	root := t.TempDir()
	tasks, err := ParseRemote([]byte(raw), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 || tasks[0].Pid != 100 || tasks[0].SSH != "10.0.0.1 40000 10.0.0.2 22" || tasks[1].N != 4 || tasks[1].Comm != "compile" {
		t.Errorf("задачи: %+v", tasks)
	}
	var s Sample
	if err := s.ReadLoad(root); err != nil {
		t.Fatal(err)
	}
	if s.Load.Avg1 != 2.5 || s.Load.Avg5 != 2.25 || s.Runnable != 8 || s.Threads != 2 || !s.Over() {
		t.Errorf("2.5 за минуту и 8 готовых при 2 потоках — перегрузка: %+v", s)
	}
	// без аргумента tasks раздел пуст
	if tasks, err := ParseRemote([]byte(strings.SplitAfter(raw, "##tasks\n")[0]+"##end\n"), t.TempDir()); err != nil || tasks != nil {
		t.Errorf("пустой раздел задач: %+v, %v", tasks, err)
	}
	for _, bad := range []string{
		strings.Replace(raw, "##end\n", "", 1),
		strings.Replace(raw, "##loadavg\n", "", 1),
		strings.Replace(raw, "\t\t\n", "\n", 1),
	} {
		if _, err := ParseRemote([]byte(bad), t.TempDir()); err == nil {
			t.Errorf("оборванный вывод принят: %q", bad)
		}
	}
}

func testJobs(t *testing.T) *Jobs {
	dir, units := t.TempDir(), t.TempDir()
	// живой unit — папка cgroup <unit>.service, повтор — <unit>-<n>.service
	for _, u := range []string{"worker-1027-2233-r-t04-1", "worker-1027-2330-tk-triage", "worker-1027-2338-tk-triage", "worker-1027-2207-r-t07-1"} {
		if err := os.MkdirAll(filepath.Join(units, u+".service"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(dir, "1027-2233-r-t04", "job.json"),
		`{"id":"1027-2233-r-t04","dir":"/w/glint/r-t04","task":"r-t04","master":"glint-master","unit":"worker-1027-2233-r-t04"}`)
	// то же дерево у другого мастера, unit не работает — на PC работает не он (в status.json фаза без конца — не довод)
	write(t, filepath.Join(dir, "1027-2300-r-t04", "job.json"),
		`{"id":"1027-2300-r-t04","dir":"/w/other/r-t04","task":"r-t04","master":"old-master","unit":"worker-1027-2300-r-t04"}`)
	write(t, filepath.Join(dir, "1027-2300-r-t04", "status.json"), `{"phase":"запуск"}`)
	// одно дерево у двух мастеров, оба живы — чей поток, не узнать
	write(t, filepath.Join(dir, "1027-2330-tk-triage", "job.json"),
		`{"id":"1027-2330-tk-triage","dir":"/w/sc2go/tk-triage","task":"tk-triage","master":"sc2go-master","unit":"worker-1027-2330-tk-triage"}`)
	write(t, filepath.Join(dir, "1027-2338-tk-triage", "job.json"),
		`{"id":"1027-2338-tk-triage","dir":"/w/sc2demu/tk-triage","task":"tk-triage","master":"sc2demu-master","unit":"worker-1027-2338-tk-triage"}`)
	write(t, filepath.Join(dir, "1027-2207-r-t07-1", "job.json"),
		`{"id":"1027-2207-r-t07-1","dir":"/w/glint/r-t07","task":"r-t07-1","master":"glint-master","unit":"worker-1027-2207-r-t07-1"}`)
	return &Jobs{Dir: dir, Units: units}
}

func TestOwner(t *testing.T) {
	r := &Resolver{Jobs: testJobs(t), Sessions: func(pid int) (string, error) {
		switch pid {
		case 77:
			return "graft-master", nil
		case 79:
			return "", errors.New("реестр не читается")
		}
		return "", nil
	}}
	write(t, filepath.Join(r.Jobs.Dir, "1027-0000-broken", "job.json"), "{")
	for _, c := range []struct {
		t    Task
		want Owner
	}{
		{Task{Cgroup: userSlice + "/agents.slice/agent-lead-666971.scope"}, Owner{Master: "lead", Kind: KindSession}},
		{Task{Cgroup: userSlice + "/agents.slice/agent-lead-666971-r1.scope"}, Owner{Master: "lead", Kind: KindSession}},
		{Task{Cgroup: userSlice + "/app.slice/worker-1027-2233-r-t04.service"}, Owner{Master: "glint-master", Kind: KindWorker, Label: "r-t04"}},
		{Task{Cgroup: userSlice + "/app.slice/worker-1027-2233-r-t04-2.service"}, Owner{Master: "glint-master", Kind: KindWorker, Label: "r-t04"}},
		{Task{Cgroup: userSlice + "/app.slice/worker-1027-2207-r-t07-1.service"}, Owner{Master: "glint-master", Kind: KindWorker, Label: "r-t07-1"}},
		{Task{Cgroup: userSlice + "/app.slice/worker-gone.service"}, Owner{Kind: KindWorker, Label: "worker-gone"}},
		{Task{Cgroup: userSlice + "/app.slice/farm-w-chain-attempt-1.c38ddf753936.scope"}, Owner{Master: farmMaster, Kind: KindFarm}},
		{Task{Cgroup: userSlice + "/app.slice/app-konsole.scope", Claude: 77}, Owner{Master: "graft-master", Kind: KindSession}},
		{Task{Cgroup: userSlice + "/app.slice/app-konsole.scope", Claude: 78}, Owner{Kind: KindOther, Label: "app-konsole.scope"}},
		{Task{Cgroup: "/system.slice/tmpguard-track.service"}, Owner{Kind: KindSystem}},
		{Task{Cgroup: "/"}, Owner{Kind: KindSystem}},
	} {
		if got := r.Owner(&c.t); got != c.want {
			t.Errorf("Owner(%s, CLAUDE_PID=%d) = %+v, хотим %+v", c.t.Cgroup, c.t.Claude, got, c.want)
		}
	}
	if err := r.Err(); err != nil {
		t.Fatalf("сбоев не было: %v", err)
	}
	// сбой поиска: поток без мастера, сбой — в Err
	for _, c := range []Task{
		{Cgroup: userSlice + "/app.slice/app-konsole.scope", Claude: 79},
		{Cgroup: userSlice + "/app.slice/worker-1027-0000-broken.service"},
	} {
		if got := r.Owner(&c); got.Master != "" {
			t.Errorf("при сбое поиска мастер %+v", got)
		}
	}
	if err := r.Err(); err == nil || !strings.Contains(err.Error(), "реестр не читается") || !strings.Contains(err.Error(), "job.json") {
		t.Errorf("Err: %v", err)
	}
}

func TestRemoteOwner(t *testing.T) {
	// ноут: ssh pid 500 держит сокет с локальным портом 40000 (0x9C40), его scope — сессия shop-master
	laptop := t.TempDir()
	write(t, filepath.Join(laptop, "net", "tcp"), "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"+
		"   0: 0100000A:9C40 0200000A:0016 01 00000000:00000000 00:00000000 00000000  1000        0 4242 1 0000000000000000 20 4 30 10 -1\n")
	write(t, filepath.Join(laptop, "500", "comm"), "ssh\n")
	write(t, filepath.Join(laptop, "500", "cgroup"), "0::"+userSlice+"/agents.slice/agent-shop-master-123.scope\n")
	write(t, filepath.Join(laptop, "500", "environ"), "")
	if err := os.MkdirAll(filepath.Join(laptop, "500", "fd"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("socket:[4242]", filepath.Join(laptop, "500", "fd", "3")); err != nil {
		t.Fatal(err)
	}
	r := &Resolver{Jobs: testJobs(t), ProcRoot: laptop}
	// битое задание не лишает владельцев остальных: они в ответе, сбой — в Err; файлы worker в папке заданий
	// (.prune.stamp) — не задания и не сбой
	write(t, filepath.Join(r.Jobs.Dir, "1027-0000-broken", "job.json"), "{")
	write(t, filepath.Join(r.Jobs.Dir, ".prune.stamp"), "")
	for _, c := range []struct {
		t    Task
		want Owner
	}{
		{Task{Cgroup: userSlice + "/app.slice/worker-r-t04.0123456789abcdef.scope"}, Owner{Master: "glint-master", Kind: KindWorker, Label: "r-t04"}},
		{Task{Cgroup: userSlice + "/app.slice/worker-nojob.0123456789abcdef.scope"}, Owner{Kind: KindWorker, Label: "worker-nojob.0123456789abcdef.scope"}},
		{Task{Cgroup: userSlice + "/app.slice/worker-tk-triage.0123456789abcdef.scope"},
			Owner{Kind: KindWorker, Label: "worker-tk-triage.0123456789abcdef.scope (sc2demu-master или sc2go-master)"}},
		{Task{Cgroup: userSlice + "/app.slice/farm-w-chain.0123456789abcdef.scope"}, Owner{Master: farmMaster, Kind: KindFarm}},
		{Task{Cgroup: "/user.slice/user-1000.slice/session-9.scope", SSH: "10.0.0.1 40000 10.0.0.2 22"}, Owner{Master: "shop-master", Kind: KindSession}},
		{Task{Cgroup: "/user.slice/user-1000.slice/session-9.scope", SSH: "10.0.0.1 40001 10.0.0.2 22"}, Owner{Kind: KindOther, Label: "session-9.scope"}},
		{Task{Cgroup: "/system.slice/ollama.service"}, Owner{Kind: KindSystem}},
	} {
		if got := r.RemoteOwner(&c.t); got != c.want {
			t.Errorf("RemoteOwner(%s, %q) = %+v, хотим %+v", c.t.Cgroup, c.t.SSH, got, c.want)
		}
	}
	if err := r.Err(); err == nil || !strings.Contains(err.Error(), "job.json") || strings.Contains(err.Error(), "stamp") {
		t.Errorf("в Err — только сбой битого задания: %v", err)
	}
}

var (
	glintWorker = Owner{Master: "glint-master", Kind: KindWorker, Label: "r-t04"}
	glintSess   = Owner{Master: "glint-master", Kind: KindSession}
	leadSess    = Owner{Master: "lead", Kind: KindSession}
	desktop     = Owner{Kind: KindOther, Label: "app-konsole.scope"}
)

// sample — замер: load average за минуту и готовые к работе задачи в момент снимка при 22 потоках, потоки по
// владельцам (владелец закодирован в cgroup).
func sample(at time.Time, avg1 float64, runnable int, by map[Owner]int) *Sample {
	s := &Sample{At: at, Load: pressure.LoadAvg{Avg1: avg1}, Runnable: runnable, Threads: 22}
	for o, n := range by {
		s.Tasks = append(s.Tasks, Task{N: n, Comm: "go", Cgroup: o.Master + "|" + o.Kind + "|" + o.Label})
	}
	return s
}

func byCgroup(t *Task) Owner {
	f := strings.Split(t.Cgroup, "|")
	return Owner{Master: f[0], Kind: f[1], Label: f[2]}
}

func TestJudge(t *testing.T) {
	start := time.Date(2026, 10, 27, 22, 0, 0, 0, time.UTC)
	var h Host
	heavy := map[Owner]int{glintWorker: 24, glintSess: 4, leadSess: 2, desktop: 2}
	if v := h.Judge(sample(start.Add(-2*time.Minute), 21.9, 30, heavy), byCgroup); v.Over || v.Loads != nil {
		t.Fatalf("load average за минуту ниже потоков — не перегрузка и владельцы не разбираются: %+v", v)
	}
	// Мгновенные готовые задачи сами не начинают серию при низком LA.
	if v := h.Judge(sample(start.Add(-time.Minute), 21, 30, map[Owner]int{glintSess: 1, desktop: 2}), byCgroup); v.Over {
		t.Fatalf("низкий load average — не перегрузка: %+v", v)
	}
	v := h.Judge(sample(start, 22, 30, heavy), byCgroup)
	if !v.Over || !v.Started || !v.Since.Equal(start) || v.Avg1 != 22 || v.Runnable != 30 || v.Threads != 22 {
		t.Fatalf("load average за минуту дошёл до потоков — начало перегрузки: %+v", v)
	}
	if v.Long(start.Add(9*time.Minute), 10*time.Minute) || !v.Long(start, 0) {
		t.Error("письма — когда перегрузка держится подряд не меньше after")
	}
	at := start.Add(10 * time.Minute)
	v = h.Judge(sample(at, 30, 36, map[Owner]int{glintWorker: 32, glintSess: 4}), byCgroup)
	if !v.Over || v.Started || !v.Since.Equal(start) || !v.Long(at, 10*time.Minute) {
		t.Fatalf("перегрузка держится 10 минут подряд: %+v", v)
	}
	rc := v.Recipients(0.25)
	if len(rc) != 1 || rc[0].Name != "glint-master" || rc[0].N != 36 {
		t.Fatalf("адресаты — по снимку: %+v", rc)
	}
	if !h.Due("glint-master", at, 30*time.Minute) || !h.First("glint-master") {
		t.Fatal("первое письмо мастеру в перегрузку")
	}
	h.Sent("glint-master", at)
	h.Sent("сводка:lead", at)
	if h.Due("glint-master", at.Add(29*time.Minute), 30*time.Minute) || !h.Due("glint-master", at.Add(30*time.Minute), 30*time.Minute) ||
		h.First("glint-master") || !h.Due("lead", at, 30*time.Minute) {
		t.Error("повтор письма одному адресату — не чаще repeat, сводка лиду и письмо ему как мастеру — порознь")
	}
	if told := h.Told(); !slices.Equal(told, []string{"glint-master"}) {
		t.Errorf("написал за перегрузку — без служебных ключей: %v", told)
	}
	msg := MasterMessage(HostLocal, &v, &rc[0], at, true)
	for _, part := range []string{"ноут перегружен 10 мин подряд", "load average за минуту 30.0, сейчас готовых к работе задач 36 при 22 потоках",
		"Твоих сейчас 36: боец r-t04 — 32 (go 32); сессия — 4 (go 4)", "worker run --pc"} {
		if !strings.Contains(msg, part) {
			t.Errorf("письмо мастеру без %q: %s", part, msg)
		}
	}
	if msg := MasterMessage("pc", &v, &rc[0], at, false); !strings.Contains(msg, "pc всё ещё перегружен") || strings.Contains(msg, "worker run --pc") {
		t.Errorf("повторное письмо о PC: %s", msg)
	}
	lead := LeadMessage("pc", &v, h.Told(), at)
	for _, part := range []string{"pc перегружен 10 мин подряд", "сейчас готовых к работе задач 36 при 22 потоках (по процессам 36)",
		"Чьи: glint-master 36.", "Написал за перегрузку: glint-master."} {
		if !strings.Contains(lead, part) {
			t.Errorf("сводка лиду без %q: %s", part, lead)
		}
	}
	// Высокий LA продолжает серию при низком runnable; низкий LA завершает её при высоком runnable.
	at = at.Add(time.Minute)
	if v = h.Judge(sample(at, 30, 3, heavy), byCgroup); !v.Over || !v.Since.Equal(start) {
		t.Errorf("низкий runnable не рвёт серию: %+v", v)
	}
	at = at.Add(time.Minute)
	if v = h.Judge(sample(at, 21, 30, heavy), byCgroup); v.Over || !v.Ended.Equal(start) || v.Lasted != 12*time.Minute || v.Loads != nil {
		t.Errorf("низкий LA завершает серию, даже если runnable высок: %+v", v)
	}
	if h.Due("glint-master", at, time.Minute) {
		t.Error("вне перегрузки писем нет")
	}
	// новая перегрузка сразу после конца: длительность с нуля, repeat — от прошлого письма
	at = at.Add(time.Minute)
	if v = h.Judge(sample(at, 25, 30, heavy), byCgroup); !v.Started || !v.Since.Equal(at) || v.Long(at, 10*time.Minute) {
		t.Fatalf("новая перегрузка — с этого замера: %+v", v)
	}
	if !h.First("glint-master") || len(h.Told()) != 0 || h.Due("glint-master", at, 30*time.Minute) {
		t.Error("письмо 2 минуты назад в прошлую перегрузку: в этой ещё не писали, но повтор раньше repeat")
	}
}

func TestJudgeLAOverloadStreakWithRunnableFluctuations(t *testing.T) {
	start := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	var h Host
	var firstLong time.Time
	for i := range 60 {
		runnable := 30
		if i%10 < 7 {
			runnable = 40
		}
		at := start.Add(time.Duration(i) * time.Minute)
		s := sample(at, 40, runnable, nil)
		s.Threads = 32
		v := h.Judge(s, byCgroup)
		if v.Long(at, 10*time.Minute) && firstLong.IsZero() {
			firstLong = at
		}
	}
	if firstLong.IsZero() || firstLong.After(start.Add(15*time.Minute)) {
		t.Fatalf("60 минут высокого LA с меняющимся runnable должны достичь порога к 15-й минуте, получили %v", firstLong)
	}
}

// Подряд — это без пропусков: сбой замера или пауза демона (сон машины) обрывают серию.
func TestJudgeGap(t *testing.T) {
	start := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	h := Host{Every: time.Minute}
	h.Judge(sample(start, 30, 30, nil), byCgroup)
	h.Judge(sample(start.Add(time.Minute), 30, 30, nil), byCgroup)
	h.Break() // ssh упал
	at := start.Add(2 * time.Minute)
	if v := h.Judge(sample(at, 30, 30, nil), byCgroup); !v.Ended.Equal(start) || v.Lasted != time.Minute || !v.Started || !v.Since.Equal(at) {
		t.Errorf("сбой замера — новая серия с этого замера, прошлая длилась до последнего замера: %+v", v)
	}
	at = at.Add(30 * time.Minute) // демон не замерял полчаса
	if v := h.Judge(sample(at, 30, 30, nil), byCgroup); !v.Started || !v.Since.Equal(at) || v.Long(at, 10*time.Minute) || v.Lasted != 0 {
		t.Errorf("пропуск дольше двух интервалов — новая серия, длительность прошлой без пропуска: %+v", v)
	}
	at = at.Add(2 * time.Minute) // замер запоздал, но не дольше двух интервалов
	if v := h.Judge(sample(at, 30, 30, nil), byCgroup); v.Started || !v.Since.Equal(at.Add(-2*time.Minute)) {
		t.Errorf("запоздавший замер серию не рвёт: %+v", v)
	}
}

// Мастеру пишется по числу его готовых задач в снимке против потоков машины, а не по доле от всех готовых.
func TestRecipientsShare(t *testing.T) {
	for _, c := range []struct {
		by   map[Owner]int
		want int
	}{
		{map[Owner]int{glintSess: 1, desktop: 2}, 0},  // треть готовых, но одна задача
		{map[Owner]int{glintSess: 1, desktop: 20}, 0}, // перегрузку дают не мастера
		{map[Owner]int{glintSess: 5, desktop: 20}, 0}, // 5 < 0.25 × 22
		{map[Owner]int{glintSess: 6, desktop: 20}, 1},
	} {
		var h Host
		v := h.Judge(sample(time.Now(), 30, 30, c.by), byCgroup)
		if rc := v.Recipients(0.25); len(rc) != c.want {
			t.Errorf("%v: адресаты %+v, хотим %d", c.by, rc, c.want)
		}
	}
}

func TestConfig(t *testing.T) {
	c := Default("/home/u")
	if err := c.Validate("/home/u"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []func(*Config){
		func(c *Config) { c.Mode = "act" },
		func(c *Config) { c.Interval = 0 },
		func(c *Config) { c.Repeat = c.Interval / 2 },
		func(c *Config) { c.Notify = 0 },
		func(c *Config) { c.After = -1 },
		func(c *Config) { c.Notify = 2 },
		func(c *Config) { c.SpinnerAfter = 0 },
		func(c *Config) { c.SpinnerCPU = 0 },
		func(c *Config) { c.SpinnerCPU = 1.1 },
		func(c *Config) { c.SpinnerIO = 0 },
	} {
		c := Default("/home/u")
		bad(&c)
		if c.Validate("/home/u") == nil {
			t.Errorf("принят неверный раздел %+v", c)
		}
	}
}

// Готовые кончились между чтением числа и обходом потоков: сводка говорит об этом, а не «Чьи: .».
func TestLeadMessageEmpty(t *testing.T) {
	var h Host
	at := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	v := h.Judge(sample(at, 30, 30, nil), byCgroup)
	if msg := LeadMessage(HostLocal, &v, nil, at); !strings.Contains(msg, "Чьи: в снимке готовых потоков нет.") {
		t.Errorf("сводка при пустом снимке: %s", msg)
	}
}

func TestLeadMessageUsesTopEightCPUOwnersAndRemainder(t *testing.T) {
	loads := []Load{
		{Owner: Owner{Master: "alpha", Kind: KindSession, Label: "session"}, CPU: .58},
		{Owner: Owner{Master: "alpha", Kind: KindWorker, Label: "worker"}, CPU: .45},
		{Owner: Owner{Master: "beta", Kind: KindWorker, Label: "job"}, CPU: .9},
		{Owner: Owner{Kind: KindOther, Label: "review.scope"}, CPU: .8},
		{Owner: Owner{Kind: KindSystem}, CPU: .7},
		{Owner: Owner{Kind: KindOther, Label: "graft-heavy.scope"}, CPU: .6},
		{Owner: Owner{Kind: KindOther, Label: "lex.scope"}, CPU: .5},
		{Owner: Owner{Kind: KindOther, Label: "build.scope"}, CPU: .4},
		{Owner: Owner{Kind: KindOther, Label: "tail.scope"}, CPU: .3},
		{Owner: Owner{Kind: KindOther, Label: "omitted.scope"}, CPU: .2},
	}
	v := Verdict{UseCPU: true, Loads: loads}
	msg := LeadMessage("host", &v, nil, time.Now())
	want := "Чьи: alpha 1.03 ядра, beta 0.90 ядра, прочее review.scope 0.80 ядра, система 0.70 ядра, прочее graft-heavy.scope 0.60 ядра, прочее lex.scope 0.50 ядра, прочее build.scope 0.40 ядра, прочее tail.scope 0.30 ядра, остальные 0.20 ядра."
	if !strings.Contains(msg, want) {
		t.Fatalf("сводка лиду должна объединять CPU мастера и показывать человекочитаемый топ-8 с остатком:\nwant: %s\ngot:  %s", want, msg)
	}
}

func TestLeadMessageSortsMasterAndUnownedCPUTogether(t *testing.T) {
	loads := make([]Load, 0, 10)
	for i := range 9 {
		loads = append(loads, Load{Owner: Owner{Master: fmt.Sprintf("master-%02d", i), Kind: KindSession}, CPU: .05})
	}
	loads = append(loads, Load{Owner: Owner{Kind: KindOther, Label: "heavy.scope"}, CPU: 1.9})
	v := Verdict{UseCPU: true, Loads: loads}
	msg := LeadMessage("host", &v, nil, time.Now())
	want := "Чьи: прочее heavy.scope 1.90 ядра, master-00 0.05 ядра, master-01 0.05 ядра, master-02 0.05 ядра, master-03 0.05 ядра, master-04 0.05 ядра, master-05 0.05 ядра, master-06 0.05 ядра, остальные 0.10 ядра."
	if !strings.Contains(msg, want) {
		t.Fatalf("крупный неизвестный владелец должен сортироваться вместе с мастерами и попасть в топ-8:\nwant: %s\ngot:  %s", want, msg)
	}
}

// id задания по unit бойца: повтор — сначала имя целиком, потом без хвоста -<n>; хвост не число или с нуля — не
// повтор; служба worker-settle и не worker-* — не боец.
func TestJobIDs(t *testing.T) {
	for unit, want := range map[string][]string{
		"worker-1107-1220-dream-page-dups.service":   {"1107-1220-dream-page-dups"},
		"worker-1107-1220-dream-page-dups-1.service": {"1107-1220-dream-page-dups-1", "1107-1220-dream-page-dups"},
		"worker-1107-1220-dups-x.service":            {"1107-1220-dups-x"},
		"worker-1107-1220-dups-0.service":            {"1107-1220-dups-0"},
		"worker-12":                                  {"12"},
		"worker-settle.service":                      nil,
		// имена llmark worker с 12.11 (3ab3552): jobUnit и roundUnit
		"worker-job-1113-0946-confirm-sudo-cached.service": {"1113-0946-confirm-sudo-cached"},
		"worker-round-1111-2150-runsh-g4-7.service":        {"1111-2150-runsh-g4"},
		"worker-round-1110-1118-2026-11-10-3.service":      {"1110-1118-2026-11-10"},
		"foo.service": nil,
	} {
		if got := JobIDs(unit); !slices.Equal(got, want) {
			t.Errorf("%s: %q, ждали %q", unit, got, want)
		}
	}
}

func TestUnescapeUnit(t *testing.T) {
	for in, want := range map[string]string{
		"worker-1111-2248-symlink-guard-tails.service":                        "worker-1111-2248-symlink-guard-tails.service",
		`worker-1112-0123-\xd0\xbf\xd1\x80\xd0\xbe\xd0\xb1\xd0\xb0-x.service`: "worker-1112-0123-проба-x.service",
		`worker-1112-0123-\xd0\xbf\xd1\x80\xd1\`:                              "worker-1112-0123-пр",
		`worker-1112-0123-\xd0\xbf\xd1\x80\xd1\x`:                             "worker-1112-0123-пр",
		`worker-1112-0123-\xd0\xbf\xd1\x80\xd1\x8`:                            "worker-1112-0123-пр",
		`worker-1112-0123-\xd0\xbf\xd1`:                                       "worker-1112-0123-п",
		`worker-1112-0123-\xd0\xbf\x2`:                                        "worker-1112-0123-п",
		`worker-1112-0123-ab\`:                                                "worker-1112-0123-ab",
		`a\zz`:                                                                `a\zz`,
	} {
		if got := UnescapeUnit(in); got != want {
			t.Errorf("UnescapeUnit(%q) = %q, want %q", in, got, want)
		}
	}
	if ids := JobIDs(`worker-1112-0123-\xd0\xbf\xd1\x80\xd0\xbe\xd0\xb1\xd0\xb0-3.service`); !slices.Equal(ids, []string{"1112-0123-проба-3", "1112-0123-проба"}) {
		t.Errorf("JobIDs escaped = %q", ids)
	}
}

func TestByUnitPrefix(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeUnitJob(t, dir, "1112-0123-проба-kill-пустышки", now, "alpha-master")
	writeUnitJob(t, dir, "1112-0124-другое", now, "beta-master")
	ask := "1112-0123-проба-kill-пустышки-ask" // вопрос worker ask с тем же началом: без unit, не боец
	if err := os.MkdirAll(filepath.Join(dir, ask), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ask, "job.json"), []byte(`{"id":"`+ask+`","master":"gamma-master"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	jobs := &Jobs{Dir: dir}
	j, err := jobs.ByUnitPrefix(`worker-1112-0123-\xd0\xbf\xd1\x80\xd0\xbe\xd0\xb1\xd0\xb0-kill-\xd0\xbf\xd1\x83\xd1\`)
	if err != nil || j == nil || j.ID != "1112-0123-проба-kill-пустышки" {
		t.Fatalf("ByUnitPrefix = %+v, %v", j, err)
	}
	for _, unit := range []string{"worker-1112-01", "worker-1112-0125-x", "agent-1112-0123-проба", "worker-1112-0123-проба-kill-пустышки.scope"} {
		if j, err := jobs.ByUnitPrefix(unit); err != nil || j != nil {
			t.Errorf("ByUnitPrefix(%q) = %+v, %v, want nil", unit, j, err)
		}
	}
}

func TestPartsShowsProcessRemainder(t *testing.T) {
	three := Load{Owner: Owner{Kind: KindSession}, N: 3, Comms: []Comm{{Name: "rg", N: 1, Procs: 1}, {Name: "graft", N: 1, Procs: 1}, {Name: "claude", N: 1, Procs: 1}}}
	four := Load{Owner: three.Owner, N: 5, Comms: append(append([]Comm{}, three.Comms...), Comm{Name: "grep", N: 2, Procs: 2})}
	if got := parts([]Load{three}); strings.Contains(got, "ещё") {
		t.Fatalf("exactly three commands have no remainder: %q", got)
	}
	got := parts([]Load{four})
	if !strings.Contains(got, "rg 1, graft 1, claude 1, ещё 2 процесса") {
		t.Fatalf("parts = %q", got)
	}
}

func TestPartsRemainderCountsProcessesNotThreads(t *testing.T) {
	owner := func(*Task) Owner { return Owner{Master: "alpha-master", Kind: KindSession} }
	var tasks []Task
	for _, n := range []int{20, 19, 18} {
		tasks = append(tasks, Task{N: n, Comm: fmt.Sprintf("top-%d", n)})
	}
	tasks = append(tasks, Task{N: 3, Comm: "scan"}, Task{N: 3, Comm: "scan"})
	loads := Owners(tasks, owner)
	if len(loads) != 1 {
		t.Fatalf("Owners = %+v", loads)
	}
	var scan Comm
	for _, c := range loads[0].Comms {
		if c.Name == "scan" {
			scan = c
		}
	}
	if scan.Procs != 2 || scan.N != 6 {
		t.Fatalf("scan process/thread counts = %+v", scan)
	}
	got := parts(loads)
	if !strings.Contains(got, "ещё 2 процесса") || strings.Contains(got, "ещё 6") {
		t.Fatalf("parts counts threads instead of processes: %s", got)
	}
}

func TestRussianCountForms(t *testing.T) {
	for n, want := range map[int]string{1: "процесс", 2: "процесса", 4: "процесса", 5: "процессов", 11: "процессов", 21: "процесс", 22: "процесса"} {
		if got := russianCount(n, "процесс", "процесса", "процессов"); got != want {
			t.Errorf("russianCount(%d)=%q, want %q", n, got, want)
		}
	}
}

func TestSpinnerRemainderUsesRussianPlural(t *testing.T) {
	for n, want := range map[int]string{1: "ещё 1 долгий процесс", 2: "ещё 2 долгих процесса", 5: "ещё 5 долгих процессов"} {
		spinners := make([]Spinner, 5+n)
		for i := range spinners {
			spinners[i] = Spinner{Pid: i + 1, Owner: Owner{Master: "alpha-master"}}
		}
		msg := MasterMessage(HostLocal, &Verdict{Spinners: spinners}, &Master{Name: "alpha-master"}, time.Now(), true)
		if !strings.Contains(msg, want) {
			t.Errorf("remainder %d missing %q: %s", n, want, msg)
		}
	}
}

func TestHumanBytesThresholds(t *testing.T) {
	for n, want := range map[int64]string{1023: "1023 Б", 1024: "1 КБ", 1 << 20: "1.0 МБ"} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d)=%q, want %q", n, got, want)
		}
	}
}

func TestReadProcessesUsesProcRoot(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "stat"), "btime 1000\n")
	dir := filepath.Join(root, "321")
	fields := make([]string, 20)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0], fields[11], fields[12], fields[19] = "S", "10", "20", "9876"
	write(t, filepath.Join(dir, "stat"), "321 (pipe-reader) "+strings.Join(fields, " ")+"\n")
	write(t, filepath.Join(dir, "comm"), "pipe-reader\n")
	write(t, filepath.Join(dir, "cmdline"), "pipe-reader\x00--input\x00")
	write(t, filepath.Join(dir, "environ"), "CLAUDE_PID=0\x00")
	write(t, filepath.Join(dir, "io"), "rchar: 12\nwchar: 34\nread_bytes: 0\nwrite_bytes: 0\n")
	write(t, filepath.Join(dir, "cgroup"), "0::/user.slice/alpha-master\n")
	ps, err := ReadProcessStats(root, time.Unix(1200, 0))
	if err != nil || len(ps) != 1 {
		t.Fatalf("ReadProcessStats = %+v, %v", ps, err)
	}
	if ps[0].Pid != 321 || ps[0].Start != 9876 || ps[0].CPU != 0.3 || ps[0].Age < 101*time.Second || ps[0].Age > 102*time.Second {
		t.Fatalf("process = %+v", ps[0])
	}
	if err := ReadProcessIO(root, &ps[0]); err != nil {
		t.Fatal(err)
	}
	if err := ReadProcessOwner(root, &ps[0]); err != nil {
		t.Fatal(err)
	}
	if !ps[0].IOKnown || ps[0].Rchar != 12 || ps[0].Wchar != 34 || ps[0].Cmdline != "pipe-reader --input" {
		t.Fatalf("details = %+v", ps[0])
	}
}

func TestReadProcessStatsSkipsMalformedProcessAndKeepsOthers(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "stat"), "btime 1000\n")
	write(t, filepath.Join(root, "111", "stat"), "111 (broken) S 1\n")
	fields := make([]string, 20)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0], fields[19] = "S", "123"
	write(t, filepath.Join(root, "222", "stat"), "222 (reader) "+strings.Join(fields, " ")+"\n")
	got, err := ReadProcessStats(root, time.Unix(1100, 0))
	if err == nil || len(got) != 1 || got[0].Pid != 222 {
		t.Fatalf("one bad proc entry stopped scan: processes=%+v err=%v", got, err)
	}
}

func TestReadProcessIOStarttimeAndCounters(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "123")
	fields := make([]string, 20)
	for i := range fields {
		fields[i] = "0"
	}
	fields[19] = "77"
	write(t, filepath.Join(dir, "stat"), "123 (reader) "+strings.Join(fields, " ")+"\n")
	write(t, filepath.Join(dir, "io"), "rchar: 15\nwchar: 0\n")
	p := Process{Task: Task{Pid: 123}, Start: 76}
	if err := ReadProcessIO(root, &p); !IsTransientProcessError(err) {
		t.Fatalf("pid reuse error=%v", err)
	}
	p.Start = 77
	if err := ReadProcessIO(root, &p); err != nil || !p.IOKnown || p.Rchar != 15 || p.Wchar != 0 {
		t.Fatalf("valid IO=%+v err=%v", p, err)
	}
	for _, io := range [][]byte{[]byte("rchar: 15\n"), []byte("wchar: 0\n")} {
		if _, _, err := parseProcIO(io); err == nil {
			t.Errorf("accepted incomplete io %q", io)
		}
	}
}

func TestProcessStatExpectedReadErrorsAreSkipped(t *testing.T) {
	now := time.Now()
	boot := now.Add(-time.Hour)
	root := t.TempDir()
	if _, ok, err := readProcessStat(filepath.Join(root, "404"), boot, now); err != nil || ok {
		t.Fatalf("gone stat: ok=%v err=%v", ok, err)
	}
	if !skipUnreadableStat(fs.ErrPermission) || !skipUnreadableStat(os.ErrNotExist) || skipUnreadableStat(errors.New("bad stat")) {
		t.Fatal("stat read error classification changed")
	}
	if !IsTransientProcessError(fs.ErrPermission) || !IsTransientProcessError(os.ErrNotExist) || IsTransientProcessError(errors.New("malformed CLAUDE_PID")) {
		t.Fatal("candidate error classification changed")
	}
}

// TestByUnitNewNames — задание по unit настоящего вида llmark worker (worker-job-<id>, worker-round-<id>-<n>), целым
// именем и обрезком agent-signal-guard.
func TestByUnitNewNames(t *testing.T) {
	dir := t.TempDir()
	write := func(id, unit, master string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, id), 0o700); err != nil {
			t.Fatal(err)
		}
		body := `{"id":"` + id + `","master":"` + master + `","unit":"` + unit + `"}`
		if err := os.WriteFile(filepath.Join(dir, id, "job.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("1113-0946-confirm-sudo-cached", "worker-job-1113-0946-confirm-sudo-cached", "graft-master")
	write("1111-2150-runsh-g4", "worker-round-1111-2150-runsh-g4-7", "dream-master")
	for unit, want := range map[string]string{
		"worker-job-1113-0946-confirm-sudo-cached.service": "graft-master",
		"worker-round-1111-2150-runsh-g4-7.service":        "dream-master",
	} {
		j, err := (&Jobs{Dir: dir}).ByUnit(unit)
		if err != nil || j == nil || j.Master != want {
			t.Errorf("ByUnit(%q) = %+v, %v, ждали %s", unit, j, err, want)
		}
	}
	for unit, want := range map[string]string{
		"worker-job-1113-0946-confirm-sud":   "graft-master",
		"worker-round-1111-2150-runsh-g4-7.": "dream-master",
	} {
		j, err := (&Jobs{Dir: dir}).ByUnitPrefix(unit)
		if err != nil || j == nil || j.Master != want {
			t.Errorf("ByUnitPrefix(%q) = %+v, %v, ждали %s", unit, j, err, want)
		}
	}
}
