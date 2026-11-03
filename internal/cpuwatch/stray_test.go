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
	otherCg   = "/user.slice/user-1000.slice/user@1000.service/app.slice/run-r1.scope"
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
		"curl --token=abc123 https://x":             "curl --token=*** https://x",
		"app API_KEY: qwe rest":                     "app API_KEY: *** rest",
		"x password hunter2 y":                      "x password *** y",
		"run sk-abcdefghijklmnopqrstuv":             "run ***",
		"bash -c 'while :; do :; done'":             "bash -c 'while :; do :; done'",
		"curl -H Authorization: Bearer secretvalue": "curl -H Authorization: Bearer ***",
		"curl -H 'authorization: basic dXNlcjpw'":   "curl -H 'authorization: basic ***",
		"tool bearer secretvalue x":                 "tool bearer *** x",
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
	s := Detail{Pid: 7, Start: 1}
	st.Told[s.Key("pc")] = time.Now()
	st.Told["ноут:9:9"] = time.Now()
	if st.Forget("pc", []Detail{s}) {
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
	cfg := StrayConfig{Every: DefaultStrays().Every, After: 0, Hot: 0.3, Exclude: DefaultStrays().Exclude}
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

// Сессия без места у logind зовётся session-c1.scope: это та же сессия, простой остаток считается по возрасту.
func TestStrayLogindSessionName(t *testing.T) {
	var w StrayWatch
	got := w.Judge(time.Now(), []Proc{{Pid: 7, PPid: 1, Age: time.Hour, Comm: "bash"}},
		[]Detail{{Pid: 7, Start: 1, Cgroup: "/user.slice/user-1000.slice/session-c1.scope"}}, strayCfg())
	if len(got) != 1 {
		t.Fatalf("session-c1.scope не остаток сессии: %+v", got)
	}
}

// Ровно порог — занято: проверка >=, а не >.
func TestStrayBusyExactlyHalf(t *testing.T) {
	if got := busyRun(t, 10, otherCg, func(i int) (float64, float64) { return float64(i) * 150, 0 }); got != 6 {
		t.Fatalf("письмо на проходе %d", got)
	}
}

// Пропуск замеров дольше двух every (сбой ssh, сон машины): средняя за пропуск не заменяет проходы подряд, отсчёт заново.
func TestStrayGapRestartsCount(t *testing.T) {
	var w StrayWatch
	at := time.Date(2026, 11, 3, 14, 0, 0, 0, time.UTC)
	cpu := 0.0
	pass := func(step time.Duration, grew float64) []Stray {
		at, cpu = at.Add(step), cpu+grew
		return w.Judge(at, []Proc{{Pid: 7, PPid: 1, Age: 3 * time.Hour, CPU: cpu, Comm: "x"}}, []Detail{{Pid: 7, Start: 1, Cgroup: otherCg}}, strayCfg())
	}
	pass(0, 0)
	pass(5*time.Minute, 300)
	if got := pass(35*time.Minute, 2100); len(got) != 0 {
		t.Fatalf("письмо сразу после пропуска: %+v", got)
	}
	for i := range 5 {
		if got := pass(5*time.Minute, 300); len(got) != 0 {
			t.Fatalf("письмо на %d-м проходе после пропуска: %+v", i+1, got)
		}
	}
	if got := pass(5*time.Minute, 300); len(got) != 1 {
		t.Fatalf("30 мин занятости после пропуска без письма: %+v", got)
	}
}

// Письмо об остатке сессии после пропуска замеров не выдаёт среднюю за пропуск за текущую занятость.
func TestStrayGapRateUnknown(t *testing.T) {
	var w StrayWatch
	at := time.Date(2026, 11, 3, 14, 0, 0, 0, time.UTC)
	pass := func(at time.Time, cpu float64) []Stray {
		return w.Judge(at, []Proc{{Pid: 7, PPid: 1, Age: time.Hour, CPU: cpu, Comm: "bash"}}, []Detail{{Pid: 7, Start: 1, Cgroup: sessionCg}}, strayCfg())
	}
	pass(at, 0)
	if got := pass(at.Add(35*time.Minute), 2100); len(got) != 1 || got[0].Rate != 0 {
		t.Fatalf("после пропуска: %+v", got)
	}
}

// Ключ различает машину, pid и starttime: pid переиспользуется, отметка чужому не достаётся.
func TestStrayKeyIdentity(t *testing.T) {
	a, b := Stray{Detail: Detail{Pid: 7, Start: 1}}, Stray{Detail: Detail{Pid: 7, Start: 2}}
	if a.Key("pc") != "pc:7:1" || a.Key("pc") == b.Key("pc") || a.Key("pc") == a.Key("laptop") {
		t.Fatalf("%s %s %s", a.Key("pc"), b.Key("pc"), a.Key("laptop"))
	}
}

// Битая строка замера — ошибка, не тишина: иначе сломанный ps выглядел бы как «процессов нет» и снимал отметки.
func TestParseStraysMalformed(t *testing.T) {
	for _, raw := range []string{
		"##procs\n 7 1 bash\n##strays\n##end\n",
		"##procs\n1 2 3\n##strays\n##end\n",
		"##procs\n x 1 7200 0 bash\n##strays\n##end\n",
		"##procs\n 7 1 7200 0 bash\n##strays\n7\t1\t/c\n##end\n",
		"##procs\n 7 1 7200 0 bash\n##strays\nx\t1\t/c\t/w\t\t\tcmd\n##end\n",
		"##procs\n 7 1 7200 0 bash\n##strays\n7\tx\t/c\t/w\t\t\tcmd\n##end\n",
		"##procs\n 7 1 7200 0 bash\n##strays\n",
	} {
		if _, _, err := ParseStrays([]byte(raw)); err == nil {
			t.Errorf("%q: нет ошибки", raw)
		}
	}
}

// Сам скрипт отсекает службы и init.scope по cgroup и пропускает остальное: поддельный ps даёт процесс с ppid 1,
// поддельный sed — его cgroup; среду машины проба не использует.
func TestStrayScriptCgroupFilter(t *testing.T) {
	real, err := exec.LookPath("sed")
	if err != nil {
		t.Fatal(err)
	}
	for cg, want := range map[string]int{
		"/user.slice/user-1000.slice/user@1000.service/app.slice/x.service": 0,
		"/init.scope": 0,
		"/user.slice/user-1000.slice/session-5.scope":                         1,
		"/user.slice/user-1000.slice/user@1000.service/app.slice/app-x.scope": 1,
	} {
		dir := t.TempDir()
		write(t, filepath.Join(dir, "ps"), fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' '%d 1 7200 0 bash'\n", os.Getpid()))
		write(t, filepath.Join(dir, "sed"), fmt.Sprintf("#!/bin/sh\ncase \"$3\" in /proc/*/cgroup) echo %s ;; *) exec %s \"$@\" ;; esac\n", cg, real))
		for _, f := range []string{"ps", "sed"} {
			if err := os.Chmod(filepath.Join(dir, f), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		c := exec.Command("sh", "-s")
		c.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
		c.Stdin = strings.NewReader(StrayScript)
		out, err := c.Output()
		if err != nil {
			t.Fatalf("%s: %v", cg, err)
		}
		_, ds, err := ParseStrays(out)
		if err != nil {
			t.Fatalf("%s: %v", cg, err)
		}
		if len(ds) != want {
			t.Errorf("%s: %+v, ждали %d", cg, ds, want)
		}
	}
}

// Главный процесс приложения сеанса — не сирота: его родитель init по замыслу (ложное срабатывание 03.11 на konsole со
// всеми мастерами в дереве). Орфан во вкладке терминала — другое дело: он в tab(N).scope, правило занятости его видит.
func TestStrayDesktopApp(t *testing.T) {
	const app = "/user.slice/user-1000.slice/user@1000.service/app.slice/"
	busy := func(i int) (float64, float64) { return float64(i) * 300, 0 }
	for _, cg := range []string{
		app + "app-org.kde.konsole-5028.scope/main.scope",
		app + "app-com.google.Chrome-2748777.scope",
		app + "app-flatpak-org.telegram.desktop-1378863977.scope",
	} {
		if got := busyRun(t, 10, cg, busy); got != -1 {
			t.Errorf("%s: письмо на проходе %d", cg, got)
		}
	}
	for _, cg := range []string{
		app + "app-org.kde.konsole-3681109.scope/tab(3681264).scope",
		app + "aguard-stray-probe.scope",
		"/user.slice/user-1000.slice/user@1000.service/agents.slice/agent-x-1-r3.scope",
	} {
		if got := busyRun(t, 10, cg, busy); got != 6 {
			t.Errorf("%s: письмо на проходе %d", cg, got)
		}
	}
}

// Живая сессия в дереве: занятость — её работа, снимать дерево нельзя; ни по занятости, ни как остаток сессии письма нет.
// Тот же корень без неё — письмо: проверка не отсекает всё подряд.
func TestStrayLiveSessionInTree(t *testing.T) {
	at := time.Date(2026, 11, 3, 14, 0, 0, 0, time.UTC)
	tree := func(comm string) []Proc {
		return []Proc{{Pid: 7, PPid: 1, Age: time.Hour, Comm: "bash"}, {Pid: 8, PPid: 7, Age: time.Hour, Comm: "bash"}, {Pid: 9, PPid: 8, Age: time.Hour, Comm: comm}}
	}
	d := []Detail{{Pid: 7, Start: 1, Cgroup: sessionCg}}
	var w StrayWatch
	if got := w.Judge(at, tree("claude"), d, strayCfg()); len(got) != 0 {
		t.Fatalf("дерево с claude: %+v", got)
	}
	if got := w.Judge(at, tree("sleep"), d, strayCfg()); len(got) != 1 {
		t.Fatalf("дерево без claude: %+v", got)
	}
}

// tmux потомком: живая сессия пользователя, совет снимать дерево недопустим.
func TestStrayTmuxInTree(t *testing.T) {
	ps := []Proc{{Pid: 7, PPid: 1, Age: 2 * time.Hour, Comm: "bash"}, {Pid: 9, PPid: 7, Age: 2 * time.Hour, Comm: "tmux: server"}}
	var w StrayWatch
	if got := w.Judge(time.Now(), ps, []Detail{{Pid: 7, Start: 1, Cgroup: sessionCg}}, strayCfg()); len(got) != 0 {
		t.Fatalf("дерево с tmux: %+v", got)
	}
}

// Живая сессия вне session scope: вкладка терминала с claude в дереве и сам корень-claude в agents.slice — не сироты.
func TestStrayLiveSessionOutsideSessionScope(t *testing.T) {
	const tab = "/user.slice/user-1000.slice/user@1000.service/app.slice/app-org.kde.konsole-3681109.scope/tab(3681264).scope"
	const ag = "/user.slice/user-1000.slice/user@1000.service/agents.slice/agent-x-1-r3.scope"
	at := time.Date(2026, 11, 3, 14, 0, 0, 0, time.UTC)
	var w, w2 StrayWatch
	for i := range 10 {
		now := at.Add(time.Duration(i) * 5 * time.Minute)
		ps := []Proc{{Pid: 7, PPid: 1, Age: time.Hour, CPU: float64(i) * 300, Comm: "bash"}, {Pid: 8, PPid: 7, Age: time.Hour, Comm: "bash"}, {Pid: 9, PPid: 8, Age: time.Hour, Comm: "claude"}}
		if got := w.Judge(now, ps, []Detail{{Pid: 7, Start: 1, Cgroup: tab}}, strayCfg()); len(got) != 0 {
			t.Fatalf("вкладка с claude на проходе %d: %+v", i, got)
		}
		root := []Proc{{Pid: 7, PPid: 1, Age: time.Hour, CPU: float64(i) * 300, Comm: "claude"}}
		if got := w2.Judge(now, root, []Detail{{Pid: 7, Start: 1, Cgroup: ag}}, strayCfg()); len(got) != 0 {
			t.Fatalf("корень claude на проходе %d: %+v", i, got)
		}
	}
}
