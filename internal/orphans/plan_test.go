package orphans

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
)

const (
	tabCg = "/user.slice/user-1000.slice/user@1000.service/app.slice/app-org.kde.konsole-1.scope/tab(10).scope"
	sid   = "sess-a"
	cid   = "5c914bc702065634fb6059efe8b0ad8b31561e893023d1eddbf3a9c161a80cce"
)

var (
	boot = time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	now  = boot.Add(100 * time.Hour)
)

// proc — процесс, стартовавший через startH часов после загрузки.
func proc(pid, ppid int, comm string, startH float64, session int) *Proc {
	start := uint64(startH * 3600 * clockTicks)
	return &Proc{
		Pid: pid, Ppid: ppid, Comm: comm, UID: 1000, Args: comm, Cgroup: tabCg, Cwd: "/home/u/work/x",
		Start: start, StartRaw: strconv.FormatUint(start, 10),
		StartAt: boot.Add(time.Duration(startH * float64(time.Hour))),
		Session: session, SessionID: sid,
	}
}

func snap(ps ...*Proc) *Snapshot {
	s := &Snapshot{Now: now, Procs: map[int]*Proc{}}
	for _, p := range ps {
		s.Procs[p.Pid] = p
	}
	return s
}

func testCfg(t *testing.T, ex ...Exclude) *Config {
	t.Helper()
	c := Default("/home/u")
	c.Exclude = ex
	if err := c.Compile("/home/u"); err != nil {
		t.Fatal(err)
	}
	return &c
}

// longDead — состояние, где сессия sid с pid 100 замечена мёртвой 5 ч назад.
func longDead() map[string]time.Time {
	return map[string]time.Time{SessionKey{100, sid}.String(): now.Add(-5 * time.Hour)}
}

func only(t *testing.T, trees []Tree) *Tree {
	t.Helper()
	if len(trees) != 1 {
		t.Fatalf("деревьев %d, ожидалось 1: %+v", len(trees), trees)
	}
	return &trees[0]
}

func TestLiveSessionNotOrphan(t *testing.T) {
	s := snap(
		proc(100, 10, "claude", 1, 0),
		proc(200, 1807, "ledger", 2, 100), // отвязан (nohup), но сессия жива
		proc(201, 100, "bash", 3, 100),
	)
	if trees := Plan(s, testCfg(t), longDead()); len(trees) != 0 {
		t.Fatalf("при живой сессии сирот быть не должно: %+v", trees)
	}
}

func TestReusedPidMeansDead(t *testing.T) {
	seen := map[string]time.Time{}
	s := snap(
		proc(100, 5, "claude", 90, 0), // pid сессии занят новой claude, она моложе сироты
		proc(200, 1807, "ledger", 2, 100),
	)
	tr := only(t, Plan(s, testCfg(t), seen))
	if tr.Verdict != Reap {
		t.Fatalf("verdict %v (%s), ожидалось снять: pid переиспользован 10 ч назад", tr.Verdict, tr.Reason)
	}
	if want := boot.Add(90 * time.Hour); !tr.DeadBy.Equal(want) {
		t.Fatalf("DeadBy %v, ожидалось момент переиспользования %v", tr.DeadBy, want)
	}
}

// Процесс, порождённый уже после смерти сессии (цикл под nohup), моложе
// переиспользовавшего pid: сравнивать надо с самым ранним процессом сессии.
func TestReusedPidLateSpawn(t *testing.T) {
	s := snap(
		proc(100, 5, "bash", 50, 0),
		proc(200, 1807, "sh", 2, 100),
		proc(300, 200, "tail", 80, 100),
	)
	tr := only(t, Plan(s, testCfg(t), longDead()))
	if tr.Verdict != Reap || len(tr.Members) != 2 {
		t.Fatalf("verdict %v (%s), процессов %d; ожидалось снять дерево из 2", tr.Verdict, tr.Reason, len(tr.Members))
	}
}

func TestThresholdWait(t *testing.T) {
	seen := map[string]time.Time{}
	s := snap(proc(200, 1807, "ledger", 2, 100)) // pid 100 нет вовсе: когда умерла — неизвестно
	tr := only(t, Plan(s, testCfg(t), seen))
	if tr.Verdict != Wait {
		t.Fatalf("verdict %v, ожидалось ждать: смерть замечена только сейчас", tr.Verdict)
	}
	if got := seen[SessionKey{100, sid}.String()]; !got.Equal(now) {
		t.Fatalf("seen = %v, ожидалось now", got)
	}
	seen[SessionKey{100, sid}.String()] = now.Add(-61 * time.Minute)
	if tr := only(t, Plan(s, testCfg(t), seen)); tr.Verdict != Reap {
		t.Fatalf("через 61 мин verdict %v (%s), ожидалось снять", tr.Verdict, tr.Reason)
	}
}

func TestServiceSkipped(t *testing.T) {
	p := proc(200, 1807, "payapp", 2, 100)
	p.Cgroup = "/user.slice/user-1000.slice/user@1000.service/app.slice/payapp.service"
	tr := only(t, Plan(snap(p), testCfg(t), longDead()))
	if tr.Verdict != Skip || !strings.Contains(tr.Reason, "payapp.service") {
		t.Fatalf("verdict %v (%s), ожидалось не трогать сервис", tr.Verdict, tr.Reason)
	}
}

func TestServiceInsideTreeSkipped(t *testing.T) {
	child := proc(201, 200, "worker", 3, 0)
	child.Cgroup = "/user.slice/user-1000.slice/user@1000.service/app.slice/run-u12.service"
	tr := only(t, Plan(snap(proc(200, 1807, "sh", 2, 100), child), testCfg(t), longDead()))
	if tr.Verdict != Skip {
		t.Fatalf("verdict %v, ожидалось не трогать дерево с сервисом внутри", tr.Verdict)
	}
}

func TestContainerStoppedByPodman(t *testing.T) {
	conmon := proc(300, 1807, "conmon", 2, 100)
	conmon.Args = "/usr/bin/conmon --api-version 1 -c " + cid + " -n ledger-sandbox --exit-command /usr/bin/podman" +
		" --exit-command-arg --root --exit-command-arg /home/u/.local/share/containers/storage" +
		" --exit-command-arg --runroot --exit-command-arg /run/user/1000/containers --exit-command-arg container"
	inner := proc(301, 300, "systemd-journal", 2, 0)
	inner.Cgroup = "/user.slice/user-1000.slice/user@1000.service/user.slice/libpod-" + cid +
		".scope/container/system.slice/systemd-journald.service"
	pasta := proc(299, 1807, "pasta.avx2", 2, 100)
	trees := Plan(snap(conmon, inner, pasta), testCfg(t), longDead())
	if len(trees) != 2 {
		t.Fatalf("деревьев %d, ожидалось 2 (pasta и conmon)", len(trees))
	}
	p, c := trees[0], trees[1]
	if want := "--root /home/u/.local/share/containers/storage --runroot /run/user/1000/containers"; strings.Join(c.Storage, " ") != want {
		t.Fatalf("хранилище %q, ожидалось %q", c.Storage, want)
	}
	if c.Verdict != Reap || c.Container != cid || len(c.Members) != 1 {
		t.Fatalf("conmon: verdict %v (%s), контейнер %q, процессов %d; ожидалось podman stop одного conmon",
			c.Verdict, c.Reason, c.Container, len(c.Members))
	}
	if p.Verdict != Skip || !strings.Contains(p.Reason, "podman stop") {
		t.Fatalf("pasta: verdict %v (%s), ожидалось оставить до podman stop", p.Verdict, p.Reason)
	}
}

func TestExcludedContainerKeepsNetwork(t *testing.T) {
	conmon := proc(300, 1807, "conmon", 2, 100)
	conmon.Args = "conmon -c " + cid
	cfg := testCfg(t, Exclude{Comm: "conmon", Reason: "песочница"})
	trees := Plan(snap(conmon, proc(299, 1807, "pasta", 2, 100)), cfg, longDead())
	for _, tr := range trees {
		if tr.Verdict != Skip {
			t.Fatalf("%s: verdict %v (%s), ожидалось не трогать", tr.Root.Comm, tr.Verdict, tr.Reason)
		}
	}
}

func TestClaudeInsideOrAbove(t *testing.T) {
	// мёртвая сессия запустила konsole, в ней живёт новая claude
	inside := snap(proc(200, 1807, "konsole", 2, 100), proc(201, 200, "bash", 3, 100), proc(202, 201, "claude", 4, 100))
	if tr := only(t, Plan(inside, testCfg(t), longDead())); tr.Verdict != Skip {
		t.Fatalf("verdict %v, ожидалось не трогать дерево с claude внутри", tr.Verdict)
	}
	// процесс с чужим CLAUDE_PID под живой claude
	above := snap(proc(150, 10, "claude", 3, 0), proc(200, 150, "make", 4, 100))
	if tr := only(t, Plan(above, testCfg(t), longDead())); tr.Verdict != Skip {
		t.Fatalf("verdict %v, ожидалось не трогать потомка живой claude", tr.Verdict)
	}
}

func TestTreeGroupsDescendants(t *testing.T) {
	s := snap(
		proc(200, 1807, "sh", 2, 100),
		proc(201, 200, "tail", 2, 100),
		proc(202, 200, "grep", 2, 0), // окружение очищено — всё равно часть дерева
		proc(203, 202, "cat", 2, 0),
	)
	tr := only(t, Plan(s, testCfg(t), longDead()))
	if tr.Root.Pid != 200 || len(tr.Members) != 4 || tr.Verdict != Reap {
		t.Fatalf("корень %d, процессов %d, verdict %v; ожидалось дерево 200 из 4, снять", tr.Root.Pid, len(tr.Members), tr.Verdict)
	}
}

func TestPartialMemberSkipped(t *testing.T) {
	child := proc(201, 200, "ssh-agent", 3, 0)
	child.Partial = "environ: permission denied"
	if tr := only(t, Plan(snap(proc(200, 1807, "sh", 2, 100), child), testCfg(t), longDead())); tr.Verdict != Skip {
		t.Fatalf("verdict %v, ожидалось не трогать дерево с непрочитанным процессом", tr.Verdict)
	}
}

func TestExcludeUntil(t *testing.T) {
	s := snap(proc(200, 1807, "ledger", 2, 100))
	active := testCfg(t, Exclude{Session: sid, Until: now.Add(time.Hour).Format(time.RFC3339), Reason: "до понедельника"})
	if tr := only(t, Plan(s, active, longDead())); tr.Verdict != Skip || !strings.Contains(tr.Reason, "до понедельника") {
		t.Fatalf("verdict %v (%s), ожидалось исключение", tr.Verdict, tr.Reason)
	}
	expired := testCfg(t, Exclude{Session: sid, Until: now.Add(-time.Hour).Format(time.RFC3339)})
	if tr := only(t, Plan(s, expired, longDead())); tr.Verdict != Reap {
		t.Fatalf("verdict %v, истёкшее исключение не должно действовать", tr.Verdict)
	}
	byArgs := testCfg(t, Exclude{Args: `serve$`, Cwd: "~/work"})
	p := proc(200, 1807, "ledger", 2, 100)
	p.Args, p.Cwd = "bin/ledger serve", "/home/u/work/ledger"
	if tr := only(t, Plan(snap(p), byArgs, longDead())); tr.Verdict != Skip {
		t.Fatalf("verdict %v, ожидалось исключение по args и cwd", tr.Verdict)
	}
}

func TestOtherUIDIgnored(t *testing.T) {
	p := proc(200, 1807, "sshd", 2, 0) // чужой: окружение не читали
	p.UID = 0
	if trees := Plan(snap(p), testCfg(t), longDead()); len(trees) != 0 {
		t.Fatalf("чужие процессы не судятся: %+v", trees)
	}
}

func TestServiceUnit(t *testing.T) {
	for cg, want := range map[string]string{
		tabCg: "",
		"/user.slice/user-1000.slice/user@1000.service/app.slice/tmpguard.service": "tmpguard.service",
		"/system.slice/sshd.service":                               "sshd.service",
		"/user.slice/user-1000.slice/user@1000.service/init.scope": "",
	} {
		if got := serviceUnit(cg); got != want {
			t.Errorf("serviceUnit(%q) = %q, want %q", cg, got, want)
		}
	}
}

func TestConfigValidate(t *testing.T) {
	c := Default("/home/u")
	c.Exclude = []Exclude{{Reason: "пусто"}}
	if err := c.Compile("/home/u"); err == nil {
		t.Fatal("пустое исключение должно быть ошибкой")
	}
	c = Default("/home/u")
	c.MinDead = policy.Duration(time.Second)
	if err := c.Compile("/home/u"); err == nil {
		t.Fatal("min_dead < 1m должно быть ошибкой")
	}
}

func TestPruneState(t *testing.T) {
	st := &State{
		Seen:   map[string]time.Time{"1/a": now.Add(-8 * 24 * time.Hour), "2/b": now.Add(-8 * 24 * time.Hour)},
		Logged: map[string]time.Time{"200@1": now, "999@1": now},
	}
	root := &Proc{Pid: 200, StartRaw: "1"}
	st.Prune([]Tree{{Root: root, Session: SessionKey{2, "b"}}}, now)
	if _, ok := st.Seen["1/a"]; ok {
		t.Error("пустая давняя сессия должна забыться")
	}
	if _, ok := st.Seen["2/b"]; !ok {
		t.Error("сессия с живыми сиротами должна остаться")
	}
	if _, ok := st.Logged["999@1"]; ok {
		t.Error("ушедший корень должен забыться")
	}
}

func TestFirstCwd(t *testing.T) {
	got, err := firstCwd(strings.NewReader("{\"type\":\"cost-state\"}\n{\"cwd\":\"/home/u/work/ledger\",\"x\":1}\n"))
	if err != nil || got != "/home/u/work/ledger" {
		t.Fatalf("firstCwd = %q, %v", got, err)
	}
}
