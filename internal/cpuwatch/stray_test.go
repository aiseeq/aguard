package cpuwatch

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	sessionCg = "/user.slice/user-1000.slice/session-24104.scope"
	otherCg   = "/user.slice/user-1000.slice/user@1000.service/app.slice/app-x.scope"
)

func strayCfg() *StrayConfig {
	c := DefaultStrays()
	if err := c.Validate(); err != nil {
		panic(err)
	}
	return &c
}

func TestParseStrays(t *testing.T) {
	raw := "##procs\n  400697       1  113760  113000 bash\n 12 400697 10 5 Web Content\n##strays\n" +
		"400697\t555\t" + sessionCg + "\t/home/u/run/t\t\t10.0.0.1 1 10.0.0.2 22\tbash -c x\n##end\n"
	ps, ds, err := ParseStrays([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 || ps[0].Age != 113760*time.Second || ps[0].CPU != 113000 || ps[1].Comm != "Web Content" || ps[1].PPid != 400697 {
		t.Fatalf("процессы: %+v", ps)
	}
	if len(ds) != 1 || ds[0].Start != 555 || ds[0].SSH != "10.0.0.1 1 10.0.0.2 22" || ds[0].Cmd != "bash -c x" {
		t.Fatalf("подробности: %+v", ds)
	}
	for _, bad := range []string{"", "##procs\n", "##procs\n##strays\n", "##procs\nx y\n##strays\n##end\n", "##procs\n##strays\n1\t2\n##end\n"} {
		if _, _, err := ParseStrays([]byte(bad)); err == nil {
			t.Errorf("%q: нет ошибки", bad)
		}
	}
}

// Образец от git-admin: остаток сессии ssh с нулевым CPU — письмо по возрасту; моложе срока — нет.
func TestStrayIdleSessionLeftover(t *testing.T) {
	now := time.Date(2026, 11, 3, 14, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		age  time.Duration
		want int
	}{{29 * time.Minute, 0}, {30 * time.Minute, 1}, {33 * time.Hour, 1}} {
		var w StrayWatch
		got := w.Judge(now, []Proc{{Pid: 7, PPid: 1, Age: c.age, Comm: "bash"}}, []Detail{{Pid: 7, Start: 1, Cgroup: sessionCg}}, strayCfg())
		if len(got) != c.want {
			t.Errorf("возраст %v: %+v", c.age, got)
		}
	}
}

// Законный фон сессии: tmux, screen — не пишем.
func TestStrayExcluded(t *testing.T) {
	for _, comm := range []string{"tmux: server", "screen", "catatonit"} {
		var w StrayWatch
		got := w.Judge(time.Now(), []Proc{{Pid: 7, PPid: 1, Age: time.Hour, Comm: comm}}, []Detail{{Pid: 7, Cgroup: sessionCg}}, strayCfg())
		if len(got) != 0 {
			t.Errorf("%s: %+v", comm, got)
		}
	}
}

// busyRun — проходы раз в 5 мин: used(i) — прирост процессора дерева за проход, сек; ответ — номер первого прохода с письмом.
func busyRun(t *testing.T, passes int, cg string, cpu func(i int) (root, child float64)) int {
	t.Helper()
	var w StrayWatch
	at := time.Date(2026, 11, 3, 14, 0, 0, 0, time.UTC)
	for i := range passes {
		root, child := cpu(i)
		ps := []Proc{{Pid: 7, PPid: 1, Age: time.Second, CPU: root, Comm: "x"}, {Pid: 8, PPid: 7, Age: time.Second, CPU: child, Comm: "y"}}
		if got := w.Judge(at, ps, []Detail{{Pid: 7, Start: 1, Cgroup: cg}}, strayCfg()); len(got) > 0 {
			return i
		}
		at = at.Add(5 * time.Minute)
	}
	return -1
}

// Другой scope: занят подряд 30 мин — письмо на проходе 7 (первый проход без прироста, с 1-го занят, 1-й считается с 0-го).
func TestStrayBusyOtherScope(t *testing.T) {
	if got := busyRun(t, 10, otherCg, func(i int) (float64, float64) { return float64(i) * 300, 0 }); got != 6 {
		t.Fatalf("письмо на проходе %d", got)
	}
	// занятый потомок при спящем корне (го-тест): дерево занято
	if got := busyRun(t, 10, otherCg, func(i int) (float64, float64) { return 0, float64(i) * 300 }); got != 6 {
		t.Fatalf("потомок: письмо на проходе %d", got)
	}
	// порог — на каждом проходе: один спокойный проход сбрасывает отсчёт
	dip := func(i int) (float64, float64) {
		if i >= 3 {
			return float64(i-1) * 300, 0
		}
		return float64(i) * 300, 0
	}
	if got := busyRun(t, 9, otherCg, dip); got != -1 {
		t.Fatalf("после спада письмо на проходе %d", got)
	}
	// 49 % ядра — нет письма; в scope окна и не должно быть
	if got := busyRun(t, 20, otherCg, func(i int) (float64, float64) { return float64(i) * 147, 0 }); got != -1 {
		t.Fatalf("ниже порога: письмо на проходе %d", got)
	}
}

// Потомок, родившийся и живущий между проходами: весь его процессор — занятость; первый проход занятость не считает.
func TestStrayRateFirstPassAndNewChild(t *testing.T) {
	var w StrayWatch
	at := time.Date(2026, 11, 3, 14, 0, 0, 0, time.UTC)
	d := []Detail{{Pid: 7, Start: 1, Cgroup: sessionCg}}
	old := time.Hour
	got := w.Judge(at, []Proc{{Pid: 7, PPid: 1, Age: old, CPU: 999, Comm: "x"}}, d, strayCfg())
	if len(got) != 1 || got[0].Rate != 0 {
		t.Fatalf("первый проход: %+v", got)
	}
	got = w.Judge(at.Add(5*time.Minute), []Proc{{Pid: 7, PPid: 1, Age: old, CPU: 999, Comm: "x"}, {Pid: 8, PPid: 7, Age: time.Minute, CPU: 150, Comm: "y"}}, d, strayCfg())
	if len(got) != 1 || got[0].Rate != 0.5 || got[0].Descend != 1 || got[0].CPU != 1149 {
		t.Fatalf("второй проход: %+v", got)
	}
}

func TestCleanCmd(t *testing.T) {
	for in, want := range map[string]string{
		"curl --token=abc123 https://x": "curl --token=*** https://x",
		"app API_KEY: qwe rest":         "app API_KEY: *** rest",
		"x password hunter2 y":          "x password *** y",
		"run sk-abcdefghijklmnopqrstuv": "run ***",
		"bash -c 'while :; do :; done'": "bash -c 'while :; do :; done'",
	} {
		if got := CleanCmd(in); got != want {
			t.Errorf("%q → %q, ждали %q", in, got, want)
		}
	}
	if got := CleanCmd(strings.Repeat("я", 300)); len([]rune(got)) != cmdMax+1 {
		t.Errorf("длина %d", len([]rune(got)))
	}
}

// Шторм: одно письмо, названы самые дорогие, остальные — числом.
func TestStrayMessageStorm(t *testing.T) {
	var list []Stray
	for i := range 500 {
		list = append(list, Stray{Detail: Detail{Pid: i + 1, Cgroup: sessionCg}, Comm: "x", CPU: float64(i)})
	}
	msg := StrayMessage("pc", list, false, "sc2demu-master")
	if !strings.Contains(msg, "(500)") || !strings.Contains(msg, "pid 500 ") || strings.Contains(msg, "pid 1 ") ||
		!strings.Contains(msg, "… и ещё 495") || !strings.Contains(msg, "sc2demu-master нет в сессиях") {
		t.Fatalf("%s", msg)
	}
}

func TestStrayStateForgetAndDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x", "strays.json")
	st, err := LoadStrayState(path)
	if err != nil {
		t.Fatal(err)
	}
	s := Stray{Detail: Detail{Pid: 7, Start: 1}}
	st.Told[s.Key("pc")] = time.Now()
	st.Told["ноут:9:9"] = time.Now()
	if st.Forget("pc", []Stray{s}) {
		t.Fatal("живой забыт")
	}
	if err := st.Save(path); err != nil {
		t.Fatal(err)
	}
	st, err = LoadStrayState(path)
	if err != nil || len(st.Told) != 2 {
		t.Fatalf("%+v, %v", st, err)
	}
	if !st.Forget("pc", nil) || len(st.Told) != 1 || st.Told["ноут:9:9"].IsZero() {
		t.Fatalf("после снятия: %+v", st)
	}
}

func writeJob(t *testing.T, dir, id, tree, master string, at time.Time) {
	t.Helper()
	body := fmt.Sprintf(`{"id":%q,"dir":"/w/%s","master":%q,"task":%q,"created":%q}`, id, tree, master, tree, at.Format(time.RFC3339))
	write(t, filepath.Join(dir, id, "job.json"), body)
}

// Дерево в пути cwd: завершённое задание находится, из нескольких — самое новое, совпадение только суффикса имени — нет.
func TestJobsByPathAndStrayOwner(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 11, 2, 5, 0, 0, 0, time.UTC)
	writeJob(t, dir, "1102-0516-w-lease", "w-lease", "old-master", at)
	writeJob(t, dir, "1102-0616-w-lease", "w-lease", "sc2demu-master", at.Add(time.Hour))
	writeJob(t, dir, "1102-0716-x-tools", "x-tools", "wrong-master", at)
	r := &Resolver{Jobs: &Jobs{Dir: dir}, ProcRoot: t.TempDir(), Sessions: func(int) (string, error) { return "", nil }}
	for _, c := range []struct {
		cwd, want string
	}{
		{"/home/u/run/w-lease/sc2demu/tools", "sc2demu-master"},
		{"/home/u/git/graft/sub", "graft-master"},
		{"/home/u/work/widget", "widget-master"},
		{"/tmp/tools", ""},
	} {
		o := r.StrayOwner(true, &Detail{Pid: 7, Cwd: c.cwd, Cgroup: sessionCg}, "bash")
		if o.Master != c.want {
			t.Errorf("%s: %+v", c.cwd, o)
		}
	}
	if err := r.Err(); err != nil {
		t.Fatal(err)
	}
	if v, err := (&Jobs{Dir: filepath.Join(dir, "нет")}).ByPath("/x"); v != nil || err != nil {
		t.Fatalf("нет папки: %v %v", v, err)
	}
}

// Настоящий скрипт и настоящий процесс без родителя, занятый 3 с: виден, занятость считается, строка не рвётся. Сам
// тест под службой или подреапером такого процесса не даст — тогда пропуск с причиной.
func TestStrayScriptLive(t *testing.T) {
	cg, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		t.Skip("нет /proc: ", err)
	}
	if strings.HasSuffix(strings.TrimSpace(string(cg)), ".service") {
		t.Skip("тест под службой: её процессы скрипт пропускает по замыслу")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	// внук крутит цикл и пишет pid; его родитель выходит сразу, внука усыновляет init или systemd --user
	spin := exec.Command("sh", "-c", `sh -c 'echo $$ >"$0"; while :; do :; done' "$0" & exit 0`, pidFile)
	if err := spin.Run(); err != nil {
		t.Fatal(err)
	}
	var pid int
	deadline := time.Now().Add(3 * time.Second)
	for pid == 0 && time.Now().Before(deadline) {
		if b, err := os.ReadFile(pidFile); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("процесс не стартовал")
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	read := func() ([]Proc, []Detail) {
		c := exec.Command("sh", "-s")
		c.Stdin = strings.NewReader(StrayScript)
		out, err := c.Output()
		if err != nil {
			t.Fatal(err)
		}
		ps, ds, err := ParseStrays(out)
		if err != nil {
			t.Fatal(err)
		}
		return ps, ds
	}
	if !adopted(t, pid) { // среда, а не код: родитель проверен в /proc, мимо скрипта
		t.Skipf("pid %d усыновил подреапер, а не init или systemd --user", pid)
	}
	var d *Detail
	var w StrayWatch
	cfg := StrayConfig{After: 0, Hot: 0.3, Exclude: DefaultStrays().Exclude}
	ps, ds := read()
	for i := range ds {
		if ds[i].Pid == pid {
			d = &ds[i]
		}
	}
	if d == nil {
		t.Fatalf("скрипт не показал pid %d: %+v", pid, ds)
	}
	if d.Start == 0 || d.Cgroup == "" || !strings.Contains(d.Cmd, "while") {
		t.Fatalf("подробности: %+v", d)
	}
	w.Judge(time.Now(), ps, ds, &cfg)
	// занятость растёт секундами cputimes: проходы раз в секунду, пока не видна; под нагрузкой — до срока
	var rate float64
	for end := time.Now().Add(20 * time.Second); rate < cfg.Hot; {
		if time.Now().After(end) {
			t.Fatalf("занятость не посчитана: %.2f, %+v", rate, d)
		}
		time.Sleep(time.Second)
		ps, ds = read()
		for _, s := range w.Judge(time.Now(), ps, ds, &cfg) {
			if s.Pid == pid {
				rate = s.Rate
			}
		}
	}
}

// adopted — родитель pid: init или systemd --user.
func adopted(t *testing.T, pid int) bool {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	f := strings.Fields(s[strings.LastIndexByte(s, ')')+2:])
	if f[1] == "1" {
		return true
	}
	comm, err := os.ReadFile("/proc/" + f[1] + "/comm")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(comm)) == "systemd"
}
