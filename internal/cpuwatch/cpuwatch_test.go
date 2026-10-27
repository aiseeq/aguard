package cpuwatch

import (
	"errors"
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
	raw := "##stat\ncpu0 1\ncpu1 1\nprocs_running 9\n##pressure/io\nsome avg10=0.00 avg60=1.00 avg300=0.00 total=1\nfull avg10=0.00 avg60=0.50 avg300=0.00 total=1\n" +
		"##pressure/cpu\nsome avg10=3.00 avg60=0.00 avg300=0.00 total=1\n##vmstat\npswpin 0\npswpout 0\n##tasks\n" +
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
	v, err := pressure.NewMeter(root).Check(t.Context(), pressure.DefaultLimits, false)
	if err != nil {
		t.Fatal(err)
	}
	if v.Load.Runnable != 8 || v.Load.Threads != 2 || v.Measure != pressure.CPU {
		t.Errorf("правило по снятым файлам: %+v", v)
	}
	for _, bad := range []string{
		strings.Replace(raw, "##end\n", "", 1),
		strings.Replace(raw, "##vmstat\n", "", 1),
		strings.Replace(raw, "\t\t\n", "\n", 1),
	} {
		if _, err := ParseRemote([]byte(bad), t.TempDir()); err == nil {
			t.Errorf("оборванный вывод принят: %q", bad)
		}
	}
}

func testJobs(t *testing.T) *Jobs {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "1027-2233-r-t04", "job.json"),
		`{"id":"1027-2233-r-t04","dir":"/w/glint/r-t04","task":"r-t04","master":"glint-master","created":"2026-10-27T22:33:25+01:00"}`)
	write(t, filepath.Join(dir, "1027-2100-r-t04", "job.json"),
		`{"id":"1027-2100-r-t04","dir":"/w/glint/r-t04","task":"r-t04","master":"old-master","created":"2026-10-27T21:00:00+01:00"}`)
	write(t, filepath.Join(dir, "1027-2207-r-t07-1", "job.json"),
		`{"id":"1027-2207-r-t07-1","dir":"/w/glint/r-t07","task":"r-t07-1","master":"glint-master","created":"2026-10-27T22:07:00+01:00"}`)
	return &Jobs{Dir: dir}
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
	for _, c := range []struct {
		t    Task
		want Owner
	}{
		{Task{Cgroup: userSlice + "/app.slice/worker-r-t04.0123456789abcdef.scope"}, Owner{Master: "glint-master", Kind: KindWorker, Label: "r-t04"}},
		{Task{Cgroup: userSlice + "/app.slice/worker-nojob.0123456789abcdef.scope"}, Owner{Kind: KindWorker, Label: "worker-nojob.0123456789abcdef.scope"}},
		{Task{Cgroup: userSlice + "/app.slice/farm-w-chain.0123456789abcdef.scope"}, Owner{Master: farmMaster, Kind: KindFarm}},
		{Task{Cgroup: "/user.slice/user-1000.slice/session-9.scope", SSH: "10.0.0.1 40000 10.0.0.2 22"}, Owner{Master: "shop-master", Kind: KindSession}},
		{Task{Cgroup: "/user.slice/user-1000.slice/session-9.scope", SSH: "10.0.0.1 40001 10.0.0.2 22"}, Owner{Kind: KindOther, Label: "session-9.scope"}},
		{Task{Cgroup: "/system.slice/ollama.service"}, Owner{Kind: KindSystem}},
	} {
		if got := r.RemoteOwner(&c.t); got != c.want {
			t.Errorf("RemoteOwner(%s, %q) = %+v, хотим %+v", c.t.Cgroup, c.t.SSH, got, c.want)
		}
	}
}

var (
	glintWorker = Owner{Master: "glint-master", Kind: KindWorker, Label: "r-t04"}
	glintSess   = Owner{Master: "glint-master", Kind: KindSession}
	leadSess    = Owner{Master: "lead", Kind: KindSession}
	desktop     = Owner{Kind: KindOther, Label: "app-konsole.scope"}
)

// sample — замер: занятость по числу готовых задач и потоки по владельцам (владелец закодирован в cgroup).
func sample(at time.Time, running int, by map[Owner]int) *Sample {
	s := &Sample{At: at, Verdict: pressure.Rule(pressure.Load{Runnable: running, Threads: 22}, pressure.DefaultLimits, nil)}
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
	const window, share = 10 * time.Minute, 0.7
	start := time.Date(2026, 10, 27, 22, 0, 0, 0, time.UTC)
	var h Host
	heavy := map[Owner]int{glintWorker: 24, glintSess: 4, leadSess: 2, desktop: 2}
	var v Verdict
	at := start
	// 9 минут занятых замеров — окно ещё не полное
	for ; at.Sub(start) < 9*time.Minute; at = at.Add(15 * time.Second) {
		h.Add(sample(at, 32, heavy), byCgroup, window)
		if v = h.Judge(at, window, share); v.Over {
			t.Fatalf("перегрузка на неполном окне в %v", at.Sub(start))
		}
	}
	h.Add(sample(at, 32, heavy), byCgroup, window)
	if v = h.Judge(at, window, share); !v.Over || !v.Started || !v.Since.Equal(start) {
		t.Fatalf("окно полное и занято — ждём начало перегрузки с %v: %+v", start, v)
	}
	if v.Running != 32 || v.Threads != 22 {
		t.Errorf("среднее: %v при %d", v.Running, v.Threads)
	}
	rc := v.Recipients(0.25)
	if len(rc) != 1 || rc[0].Name != "glint-master" || rc[0].Avg != 28 {
		t.Errorf("адресаты: %+v", rc)
	}
	if !h.Due("glint-master", at, 30*time.Minute) || h.Due("glint-master", at.Add(29*time.Minute), 30*time.Minute) ||
		!h.Due("glint-master", at.Add(30*time.Minute), 30*time.Minute) || !h.Seen("glint-master") || h.Seen("lead") {
		t.Error("повтор письма одному адресату — не чаще repeat")
	}
	msg := MasterMessage(HostLocal, &v, &rc[0], at.Add(time.Minute), true)
	for _, part := range []string{"ноут перегружен 10 мин", "в среднем 32 при 22 потоках", "Твоих из них 28: боец r-t04 — 24 (go 24); сессия — 4 (go 4)", "worker run --pc"} {
		if !strings.Contains(msg, part) {
			t.Errorf("письмо мастеру без %q: %s", part, msg)
		}
	}
	lead := LeadMessage("pc", &v, []string{"glint-master"}, at)
	for _, part := range []string{"pc перегружен 9 мин", "(по процессам 32)", "Чьи: glint-master 28, lead 2, прочее 2", "Написал: glint-master."} {
		if !strings.Contains(lead, part) {
			t.Errorf("сводка лиду без %q: %s", part, lead)
		}
	}
	// перегрузка держится: Started не повторяется; редкие свободные замеры её не снимают
	at = at.Add(15 * time.Second)
	h.Add(sample(at, 10, heavy), byCgroup, window)
	if v = h.Judge(at, window, share); !v.Over || v.Started {
		t.Errorf("один свободный замер снял перегрузку: %+v", v)
	}
	// свободно больше 30 % окна — перегрузка кончилась, отметки писем сброшены
	var ended []time.Time
	for range 13 {
		at = at.Add(15 * time.Second)
		h.Add(sample(at, 5, nil), byCgroup, window)
		if v = h.Judge(at, window, share); !v.Ended.IsZero() {
			ended = append(ended, v.Ended)
		}
	}
	if v.Over || !slices.Equal(ended, []time.Time{start}) {
		t.Errorf("ждём один конец перегрузки, начатой %v: %v, %+v", start, ended, v)
	}
	if h.Due("glint-master", at, time.Minute) {
		t.Error("вне перегрузки писем нет")
	}
}

// Занятость по диску и свопу — не перегрузка процессора: её владельца готовые потоки не показывают.
func TestBusyCPUOnly(t *testing.T) {
	s := &Sample{Verdict: pressure.Rule(pressure.Load{Runnable: 3, Threads: 22, IOFull: 50}, pressure.DefaultLimits, nil)}
	if s.Verdict.Measure != pressure.IO || s.Busy() {
		t.Errorf("занятость по диску засчитана процессору: %+v", s.Verdict)
	}
}

func TestConfig(t *testing.T) {
	c := Default("/home/u")
	if err := c.Validate("/home/u"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []func(*Config){
		func(c *Config) { c.Mode = "act" },
		func(c *Config) { c.Window = c.Interval },
		func(c *Config) { c.Share = 0 },
		func(c *Config) { c.Notify = 2 },
	} {
		c := Default("/home/u")
		bad(&c)
		if c.Validate("/home/u") == nil {
			t.Errorf("принят неверный раздел %+v", c)
		}
	}
}
