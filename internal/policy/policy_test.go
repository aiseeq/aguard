package policy

import (
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/record"
	"github.com/aiseeq/akit/pressure"
)

const (
	me    = 1000
	home  = "/home/u"
	gib   = int64(1 << 30)
	mib   = int64(1 << 20)
	sLead = "lead"
)

var t0 = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

func cfg() Config { return Default(home) }

func sess(pid int, name, cwd string) *record.Session {
	return &record.Session{Pid: pid, Name: name, Cwd: cwd, ID: name + "-id", ProcStart: "100", Via: "ppid"}
}

// entry — запись размером bytes, прожившая age с постоянным размером.
func entry(path string, bytes int64, age time.Duration, o *record.Owner) record.Entry {
	return record.Entry{
		Path: path, Dir: true, UID: me, Born: t0.Add(-age), Owner: o,
		Bytes: bytes, MaxBytes: bytes, Files: 3, ByteSec: float64(bytes) * age.Seconds(), MeasuredAt: t0,
	}
}

func sessionOwner(s *record.Session, comm string, chain ...record.Link) *record.Owner {
	return &record.Owner{Proc: record.Proc{Pid: 5000, Comm: comm, Args: comm + " arg", UID: me, State: record.ProcOK},
		Chain: append(chain, record.Link{Pid: s.Pid, Comm: "claude"}), Session: s}
}

func plainOwner(comm string, uid int) *record.Owner {
	return &record.Owner{Proc: record.Proc{Pid: 7000, Comm: comm, UID: uid, State: record.ProcOK}}
}

func TestParseSize(t *testing.T) {
	cases := map[string]int64{"500K": 500 << 10, "5MB": 5 << 20, "1G": 1 << 30, "1.5GiB": 3 << 29, "42": 42}
	for in, want := range cases {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; хотим %d", in, got, err, want)
		}
	}
	if _, err := ParseSize("много"); err == nil {
		t.Error("ParseSize(много): ждали ошибку")
	}
}

func TestClassify(t *testing.T) {
	c := cfg()
	lead := sess(10, sLead, home)
	cases := []struct {
		name, path string
		o          *record.Owner
		fileUID    int
		want       Class
		fighter    string
	}{
		{"сессия сама", "/tmp/a", &record.Owner{Proc: record.Proc{Pid: 10, Comm: "claude", UID: me}, Session: lead}, me, ClassSession, ""},
		{"инструмент сессии", "/tmp/a", sessionOwner(lead, "mkdir", record.Link{Pid: 20, Comm: "bash"}), me, ClassSession, ""},
		{"боец opencode", "/tmp/a", sessionOwner(lead, "go", record.Link{Pid: 21, Comm: "opencode.exe"}, record.Link{Pid: 20, Comm: "bash"}), me, ClassFighter, "opencode.exe"},
		{"боец claude -p", "/tmp/a", sessionOwner(lead, "node", record.Link{Pid: 22, Comm: "claude"}), me, ClassFighter, "claude"},
		{"не агент", "/tmp/a", plainOwner("plasmashell", me), me, ClassNonAgent, ""},
		{"uid неизвестен, по файлу", "/tmp/a", plainOwner("x", -1), me, ClassNonAgent, ""},
		{"чужая учётка", "/tmp/a", plainOwner("systemd", 0), 0, ClassOtherUID, ""},
		{"кеш", "/tmp/playwright-transform-cache-1000", plainOwner("node", me), me, ClassCache, ""},
		{"без создателя", "/tmp/old", nil, me, ClassUnknown, ""},
	}
	for _, tc := range cases {
		got, f := c.Classify(tc.path, tc.o, tc.fileUID, me)
		if got != tc.want || f != tc.fighter {
			t.Errorf("%s: %s/%q; хотим %s/%q", tc.name, got, f, tc.want, tc.fighter)
		}
	}
	if !c.Ignored("/tmp/.X11-unix") || c.Ignored("/tmp/x") {
		t.Error("ignore работает неверно")
	}
}

func TestOverThresholds(t *testing.T) {
	c := cfg()
	lead := sess(10, sLead, home)
	shop := sess(11, "shop-master", home+"/work/shop")
	entries := []record.Entry{
		entry("/tmp/big-short", gib, 15*time.Minute, sessionOwner(shop, "go")),            // 15 ГБ·мин — сверх
		entry("/tmp/mid-long", 100*mib, 3*time.Hour, sessionOwner(lead, "go")),            // ≈17.6 — сверх
		entry("/tmp/small-day", 10*mib, 10*time.Hour, plainOwner("chrome", me)),           // ≈5.9 — ниже
		entry("/tmp/tiny-forever", 512<<10, 1000*time.Hour, plainOwner("chrome", me)),     // меньше min_entry
		entry("/tmp/.cache", 5*gib, 100*time.Hour, nil),                                   // кеш не в сводках
		entry("/tmp/two-small-a", 3*mib, 100*time.Hour, sessionOwner(lead, "touch")),      // по 17.6, но сумма
		entry("/tmp/claude-tmp/fresh", 2*gib, 2*time.Minute, sessionOwner(shop, "cargo")), // 4 ГБ·мин, тянет сумму shop
	}
	items := c.Items(entries, me)
	groups := c.Over(t0, items, 1)
	got := map[string]float64{}
	for _, g := range groups {
		got[g.Key] = g.GBMin
	}
	if _, ok := got["s:shop-master-id"]; !ok {
		t.Errorf("shop сверх порога, группы: %v", got)
	}
	if v := got["s:lead-id"]; v < 30 {
		t.Errorf("lead: сумма двух записей ≈35 ГБ·мин, получили %v", v)
	}
	if _, ok := got["c:chrome"]; ok {
		t.Error("chrome ниже порога, а попал в сводку")
	}
	if _, ok := got["e:/tmp/.cache"]; ok {
		t.Error("кеш попал в сводку")
	}
	for _, g := range groups {
		if g.Key == "s:shop-master-id" && g.Items[0].Entry.Path != "/tmp/big-short" {
			t.Errorf("записи не по убыванию меры: %s первой", g.Items[0].Entry.Path)
		}
	}
	// под давлением порог ×0.25: 10 МБ на 10 ч (≈5.9) уже сверх 3
	under := c.Over(t0, items, c.Pressure.Factor)
	found := false
	for _, g := range under {
		found = found || g.Key == "c:chrome"
	}
	if !found {
		t.Error("под давлением chrome должен попасть в сводку")
	}
	// min_owner: одна запись 2 МБ на сутки (≈2.8) с бюджетом 1 — отсекается суммой 5 МБ
	c.Policy.BudgetGBM = 1
	if g := c.Over(t0, c.Items([]record.Entry{entry("/tmp/x", 2*mib, 24*time.Hour, plainOwner("vim", me))}, me), 1); len(g) != 0 {
		t.Error("сумма меньше min_owner не должна давать сводку")
	}
}

func TestCurrentSizeAgeMeasure(t *testing.T) {
	c := cfg()
	c.Policy.BudgetGBM = 100
	c.Policy.MinOwner = Size(5 * mib)
	owner := plainOwner("worker", me)
	cases := []struct {
		name     string
		bytes    int64
		age      time.Duration
		maxBytes int64
		byteSec  float64
		want     bool
	}{
		{"убрана до 47 МиБ", 47 * mib, 24 * time.Hour, 14 * gib / 10, 1607 * GiBMin, false},
		{"690 МиБ после уборки", 690 * mib, 3 * time.Hour, 14 * gib / 10, 1200 * GiBMin, true},
		{"142 МиБ, 11 ч", 142 * mib, 11 * time.Hour, 142 * mib, 1, false},
		{"142 МиБ, 13 ч", 142 * mib, 13 * time.Hour, 142 * mib, 1, true},
		{"10 МиБ, 6 суток", 10 * mib, 6 * 24 * time.Hour, 10 * mib, 1, false},
		{"10 МиБ, 8 суток", 10 * mib, 8 * 24 * time.Hour, 10 * mib, 1, true},
		{"всплеск у старой записи", 50 * mib, 7 * 24 * time.Hour, 50 * mib, 0.01, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := entry("/tmp/measure", tc.bytes, tc.age, owner)
			e.MaxBytes, e.ByteSec = tc.maxBytes, tc.byteSec
			groups := c.Over(t0, c.Items([]record.Entry{e}, me), 1)
			if got := len(groups) != 0; got != tc.want {
				t.Fatalf("сводка = %v, ожидалось %v; мера %.1f ГБ·мин", got, tc.want, float64(tc.bytes)*tc.age.Seconds()/GiBMin)
			}
		})
	}
}

func TestPressureFactor(t *testing.T) {
	c := cfg()
	if f := c.Factor(pressure.Memory{PSISomeAvg60: 0.1, SwapUsedPct: 10}); f != 1 {
		t.Errorf("без давления factor=%v", f)
	}
	if f := c.Factor(pressure.Memory{SwapUsedPct: 60}); f != c.Pressure.Factor {
		t.Errorf("своп 60%%: factor=%v", f)
	}
	if f := c.Factor(pressure.Memory{PSISomeAvg60: 7}); f != c.Pressure.Factor {
		t.Errorf("PSI 7: factor=%v", f)
	}
}

// fakeLive — живые сессии по pid и имени.
type fakeLive struct {
	byPid  map[int]Target
	byName map[string]Target
	worker map[string]WorkerJob
}

func (f *fakeLive) WorkerByUnit(unit string) (WorkerJob, bool, error) {
	j, ok := f.worker[unit]
	return j, ok, nil
}

func (f *fakeLive) ByPid(pid int, _ string) (Target, bool) { t, ok := f.byPid[pid]; return t, ok }
func (f *fakeLive) ByName(n string) (Target, bool)         { t, ok := f.byName[n]; return t, ok }

func newLive(ts ...Target) *fakeLive {
	f := &fakeLive{byPid: map[int]Target{}, byName: map[string]Target{}, worker: map[string]WorkerJob{}}
	for _, t := range ts {
		f.byPid[t.Pid] = t
		f.byName[t.Name] = t
	}
	return f
}

var (
	tLead  = Target{Name: sLead, Pid: 10, Socket: "/s/10.sock"}
	tShop  = Target{Name: "shop-master", Pid: 11, Socket: "/s/11.sock"}
	tAdmin = Target{Name: "work-admin", Pid: 12, Socket: "/s/12.sock"}
)

func targets(sums []Summary) []string {
	var out []string
	for _, s := range sums {
		out = append(out, s.Target.Key())
	}
	return out
}

func TestPlanEscalation(t *testing.T) {
	c := cfg()
	shop := sess(11, "shop-master", home+"/work/shop")
	groups := c.Over(t0, c.Items([]record.Entry{entry("/tmp/claude-tmp/big", gib, time.Hour, sessionOwner(shop, "go"))}, me), 1)
	live := newLive(tLead, tShop, tAdmin)
	st := &UserState{}
	want := []string{"shop-master", "shop-master", "work-admin", "work-admin", "lead"}
	now := t0
	for round, w := range want {
		sums, _ := c.Plan(now, groups, live, st, 1)
		if got := targets(sums); len(got) != 1 || got[0] != w {
			t.Fatalf("сводка %d: адресаты %v, хотим %s", round+1, got, w)
		}
		rel := sums[0].Lines[0].Rel
		switch {
		case w == "shop-master" && rel != RelOwn, w != "shop-master" && rel != RelIgnored:
			t.Errorf("сводка %d: отношение %v", round+1, rel)
		}
		now = now.Add(5 * time.Minute)
	}
	// верхняя ступень уже знает — повтор не раньше top_repeat
	sums, deferred := c.Plan(now, groups, live, st, 1)
	if len(sums) != 0 {
		t.Errorf("лиду повтор через 5 мин: %v", targets(sums))
	}
	// отложенное видно: dry-run покажет, о чём и когда будет повтор
	if len(deferred) != 1 || deferred[0].Target.Name != sLead || Count(deferred) != 1 ||
		!deferred[0].NextAt.Equal(now.Add(-5*time.Minute).Add(30*time.Minute)) {
		t.Errorf("отложенная сводка: %+v", deferred)
	}
	if sums, _ := c.Plan(now.Add(31*time.Minute), groups, live, st, 1); len(sums) != 1 || sums[0].Target.Name != sLead {
		t.Errorf("через 30 мин лиду повтор: %v", targets(sums))
	}
	// запись ниже порога (давление спало), но не убрана — счётчики живы:
	// под давлением снова не новая, лиду — по top_repeat
	c.Plan(now, nil, live, st, 1)
	st.Forget(c.Items([]record.Entry{entry("/tmp/claude-tmp/big", gib, time.Hour, sessionOwner(shop, "go"))}, me))
	if sums, _ := c.Plan(now.Add(5*time.Minute), groups, live, st, 1); len(sums) != 0 {
		t.Errorf("запись ниже порога забыта, повтор: %v", targets(sums))
	}
	// запись убрали — состояние забыто
	st.Forget(nil)
	if len(st.Alerts) != 0 {
		t.Errorf("состояние убранной записи не забыто: %v", st.Alerts)
	}
}

func TestLiveWorkerJobTmpRouting(t *testing.T) {
	c := cfg()
	c.Policy.BudgetGBM = 1
	c.Policy.MinOwner = Size(1)
	c.Escalation.After = 2
	master := Target{Name: "alpha-master", Pid: 14, Socket: "/s/14.sock"}
	admin := Target{Name: "work-admin", Pid: 12, Socket: "/s/12.sock"}
	live := newLive(tLead, master, admin)
	live.worker["worker-J-2.service"] = WorkerJob{ID: "J", Master: master.Name, Cwd: home + "/work/project", Live: true}
	c.Escalation.Admins[home+"/work"] = admin.Name
	owner := plainOwner("go", me)
	owner.Proc.Cgroup = "/user.slice/user-1000.slice/user@1000.service/app.slice/worker-J-2.service"
	e := entry("/tmp/w-J-go", gib, 10*time.Minute, owner)
	items := c.Items([]record.Entry{e}, me)
	if err := ResolveWorkerJobs(items, live); err != nil {
		t.Fatal(err)
	}
	groups := c.Over(t0, items, 1)
	if len(groups) != 1 || groups[0].Key != "w:alpha-master" {
		t.Fatalf("группировка живого задания: %+v", groups)
	}
	st := &UserState{}
	sums, _ := c.Plan(t0, groups, live, st, 0.5)
	if got := targets(sums); len(got) != 1 || got[0] != "alpha-master" {
		t.Fatalf("живое задание адресовано %v", got)
	}
	msg := c.Message(&sums[0], t0, false)
	if !strings.Contains(msg, "задание J идёт, папку уберёт worker") ||
		!strings.Contains(msg, "1.0 ГБ") || !strings.Contains(msg, "возраст 10 мин") ||
		strings.Contains(msg, "rm -rf") || strings.Contains(msg, "work-admin") || strings.Contains(msg, "lead") {
		t.Fatalf("неверное письмо живого задания: %s", msg)
	}
	if sums, deferred := c.Plan(t0.Add(30*time.Minute), groups, live, st, 0.5); len(sums) != 0 || len(deferred) != 1 {
		t.Fatalf("повтор раньше срока: send=%v deferred=%v", targets(sums), targets(deferred))
	}
	if sums, _ := c.Plan(t0.Add(61*time.Minute), groups, live, st, 0.5); len(sums) != 1 {
		t.Fatalf("повтор под давлением через час отсутствует: %v", targets(sums))
	}
	if st.Alerts[EntryKey(&items[0])].Sent != 0 {
		t.Fatal("письма об идущем задании не должны расходовать ступени эскалации")
	}

	live.worker["worker-J-2.service"] = WorkerJob{ID: "J", Master: master.Name, Cwd: home + "/work/project"}
	if err := ResolveWorkerJobs(items, live); err != nil {
		t.Fatal(err)
	}
	endedAt := t0.Add(62 * time.Minute)
	groups = c.Over(endedAt, items, 1)
	sums, _ = c.Plan(endedAt, groups, live, st, 1)
	if got := targets(sums); len(got) != 1 || got[0] != master.Name ||
		!strings.Contains(c.Message(&sums[0], endedAt, false), "rm -rf") {
		t.Fatalf("завершённое задание не перешло на обычную лестницу: %v", targets(sums))
	}
	st = &UserState{}
	for i := range 3 {
		sums, _ = c.Plan(t0.Add(time.Duration(i)*c.Interval.D()), groups, live, st, 1)
	}
	if got := targets(sums); len(got) != 1 || got[0] != admin.Name {
		t.Fatalf("после двух сводок адресат %v, хотим админа", got)
	}

	// Разные заказчики не складывают меру друг с другом.
	other := plainOwner("go", me)
	other.Proc.Cgroup = "/app.slice/worker-K.service"
	live.worker["worker-K.service"] = WorkerJob{ID: "K", Master: "akit-master", Live: true}
	two := c.Items([]record.Entry{entry("/tmp/w-J-go", gib, 10*time.Minute, owner),
		entry("/tmp/w-K-go", gib, 10*time.Minute, other)}, me)
	if err := ResolveWorkerJobs(two, live); err != nil {
		t.Fatal(err)
	}
	if got := c.Over(t0, two, 1); len(got) != 2 {
		t.Fatalf("заказчики должны иметь отдельные пороги: %+v", got)
	}
}

func TestLiveWorkerRepeatPolicy(t *testing.T) {
	c := cfg()
	master := Target{Name: "alpha-master", Pid: 14, Socket: "/s/14.sock"}
	live := newLive(tLead, master)
	makeLive := func(bytes int64) Group {
		return Group{Key: "w:alpha-master", Items: []Item{{Entry: entry("/tmp/w-job", bytes, time.Hour, plainOwner("go", me)),
			WorkerLive: true, WorkerMaster: master.Name, WorkerJobID: "J"}}}
	}
	withAction := func() Group {
		owner := sessionOwner(sess(14, master.Name, home), "go")
		items := c.Items([]record.Entry{entry("/tmp/action", gib, time.Hour, owner)}, me)
		return Group{Key: "action", Items: items}
	}
	assertCounts := func(name string, send, deferred []Summary, wantSend, wantDeferred int) {
		t.Helper()
		if len(send) != wantSend || len(deferred) != wantDeferred {
			t.Fatalf("%s: send=%d deferred=%d; хотим %d/%d", name, len(send), len(deferred), wantSend, wantDeferred)
		}
	}

	t.Run("initial without action defers", func(t *testing.T) {
		st := &UserState{}
		for i := range 2 {
			send, deferred := c.Plan(t0.Add(time.Duration(i)*time.Minute), []Group{makeLive(gib)}, live, st, 1)
			assertCounts("pass", send, deferred, 0, 1)
			if !st.Alerts[EntryKey(&makeLive(gib).Items[0])].LastSent.IsZero() {
				t.Fatal("отложенная неназванная строка изменила LastSent")
			}
		}
	})
	t.Run("action summary carries live row once", func(t *testing.T) {
		st := &UserState{}
		send, _ := c.Plan(t0, []Group{makeLive(gib), withAction()}, live, st, 1)
		assertCounts("first", send, nil, 1, 0)
		if len(send[0].Lines) != 2 {
			t.Fatalf("first summary lines=%d, want action and worker", len(send[0].Lines))
		}
		if got := st.Alerts[EntryKey(&makeLive(gib).Items[0])].LastBytes; got != gib {
			t.Fatalf("первая отправка запомнила %d байт, хотим %d", got, gib)
		}
		send, deferred := c.Plan(t0.Add(5*time.Minute), []Group{makeLive(gib), withAction()}, live, st, 1)
		assertCounts("second", send, deferred, 1, 1)
		for _, line := range send[0].Lines {
			if line.Rel == RelWorkerLive {
				t.Fatal("named worker row repeated without growth or pressure")
			}
		}
	})
	t.Run("repeat remembers latest size", func(t *testing.T) {
		st := &UserState{Alerts: map[string]*AlertState{
			EntryKey(&makeLive(100 * mib).Items[0]): {LastSent: t0, LastTarget: master.Name, LastBytes: 100 * mib},
		}}
		send, _ := c.Plan(t0.Add(61*time.Minute), []Group{makeLive(400 * mib)}, live, st, 1)
		assertCounts("growth", send, nil, 1, 0)
		if got := st.Alerts[EntryKey(&makeLive(100 * mib).Items[0])].LastBytes; got != 400*mib {
			t.Fatalf("после повтора last_bytes=%d, want %d", got, 400*mib)
		}
		msg := c.Message(&send[0], t0.Add(61*time.Minute), false)
		if !strings.Contains(msg, "при прошлом письме 100 МБ") {
			t.Fatalf("повтор не объясняет прежний размер: %s", msg)
		}
		send, deferred := c.Plan(t0.Add(122*time.Minute), []Group{makeLive(400 * mib)}, live, st, 1)
		assertCounts("same size after hour", send, deferred, 0, 1)
	})
	t.Run("MaxListed controls whether row was named", func(t *testing.T) {
		for _, maxListed := range []int{1, 2} {
			name := "room available"
			if maxListed == 1 {
				name = "full summary"
			}
			t.Run(name, func(t *testing.T) {
				subCfg := c
				subCfg.Policy.MaxListed = maxListed
				st := &UserState{}
				send, deferred := subCfg.Plan(t0, []Group{withAction(), makeLive(gib)}, live, st, 1)
				key := EntryKey(&makeLive(gib).Items[0])
				if maxListed == 1 {
					assertCounts("full summary", send, deferred, 1, 1)
					if !st.Alerts[key].LastSent.IsZero() {
						t.Fatal("непоказанная строка обновила LastSent")
					}
				} else {
					assertCounts("space available", send, deferred, 1, 0)
					if st.Alerts[key].LastSent.IsZero() {
						t.Fatal("показанная строка не обновила LastSent")
					}
				}
			})
		}
	})
	t.Run("repeat-only summary does not name another live row", func(t *testing.T) {
		a := makeLive(400 * mib).Items[0]
		b := makeLive(100 * mib).Items[0]
		b.Entry.Path = "/tmp/w-job-B"
		st := &UserState{Alerts: map[string]*AlertState{
			EntryKey(&a): {LastSent: t0.Add(-2 * time.Hour), LastTarget: master.Name, LastBytes: 100 * mib},
		}}
		send, deferred := c.Plan(t0, []Group{{Items: []Item{a}}, {Items: []Item{b}}}, live, st, 1)
		assertCounts("repeat A and defer B", send, deferred, 1, 1)
		if len(send[0].Lines) != 1 || send[0].Lines[0].Item.Entry.Path != a.Entry.Path {
			t.Fatalf("неназванная B попала к повтору A: %+v", send)
		}
		if !st.Alerts[EntryKey(&b)].LastSent.IsZero() {
			t.Fatal("отложенная B изменила LastSent")
		}
	})
	t.Run("pressure interval", func(t *testing.T) {
		st := &UserState{}
		send, _ := c.Plan(t0, []Group{makeLive(gib)}, live, st, 0.5)
		assertCounts("first", send, nil, 1, 0)
		for _, d := range []time.Duration{30 * time.Minute} {
			send, deferred := c.Plan(t0.Add(d), []Group{makeLive(gib)}, live, st, 0.5)
			assertCounts("too soon", send, deferred, 0, 1)
		}
		send, _ = c.Plan(t0.Add(61*time.Minute), []Group{makeLive(gib)}, live, st, 0.5)
		assertCounts("after hour", send, nil, 1, 0)
	})
	t.Run("growth threshold and interval", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			after time.Duration
			bytes int64
			want  int
		}{{"400MB after 61m", 61 * time.Minute, 400 * mib, 1}, {"399MB", 61 * time.Minute, 399 * mib, 0}, {"400MB before hour", 59 * time.Minute, 400 * mib, 0}} {
			t.Run(tc.name, func(t *testing.T) {
				st := &UserState{Alerts: map[string]*AlertState{EntryKey(&makeLive(100 * mib).Items[0]): {LastSent: t0, LastTarget: master.Name, LastBytes: 100 * mib}}}
				send, _ := c.Plan(t0.Add(tc.after), []Group{makeLive(tc.bytes)}, live, st, 1)
				assertCounts(tc.name, send, nil, tc.want, 0)
			})
		}
	})
	t.Run("seed legacy state", func(t *testing.T) {
		item := makeLive(100 * mib).Items[0]
		st := &UserState{Alerts: map[string]*AlertState{EntryKey(&item): {LastSent: t0, LastTarget: master.Name}}}
		send, deferred := c.Plan(t0.Add(2*time.Hour), []Group{makeLive(400 * mib)}, live, st, 1)
		assertCounts("seed", send, deferred, 0, 1)
		if got := st.Alerts[EntryKey(&item)].LastBytes; got != 400*mib {
			t.Fatalf("seeded last_bytes=%d, want %d", got, 400*mib)
		}
	})
}

func TestWorkerJobWithoutMasterFallsBackToLead(t *testing.T) {
	c := cfg()
	c.Policy.MinEntry, c.Policy.MinOwner, c.Policy.BudgetGBM = 1, 1, 1
	owner := plainOwner("go", me)
	owner.Proc.Cgroup = "/app.slice/worker-J.service"
	items := c.Items([]record.Entry{entry("/tmp/w-J-go", gib, 10*time.Minute, owner)}, me)
	live := newLive(tLead)
	live.worker["worker-J.service"] = WorkerJob{ID: "J"}
	if err := ResolveWorkerJobs(items, live); err != nil {
		t.Fatal(err)
	}
	if items[0].OwnerKey != "c:go" || items[0].WorkerMaster != "" {
		t.Fatalf("нет заказчика — запись должна остаться обычной: %+v", items[0])
	}
	sums, _ := c.Plan(t0, c.Over(t0, items, 1), live, &UserState{}, 1)
	if got := targets(sums); len(got) != 1 || got[0] != sLead {
		t.Fatalf("без заказчика адресат %v, хотим лида", got)
	}
}

func TestPlanDeadCreatorAndLadderGaps(t *testing.T) {
	c := cfg()
	shop := sess(11, "shop-master", home+"/work/shop")
	gitS := sess(13, "llmark-master", home+"/git/llmark")
	lead := sess(10, sLead, home)
	admin := sess(12, "work-admin", home+"/work")
	entries := []record.Entry{
		entry("/tmp/dead", gib, time.Hour, sessionOwner(shop, "go")),
		entry("/tmp/git-dead", gib, time.Hour, sessionOwner(gitS, "go", record.Link{Pid: 30, Comm: "opencode"})),
		entry("/tmp/user", gib, time.Hour, plainOwner("chrome", me)),
		entry("/tmp/root", gib, time.Hour, plainOwner("systemd", 0)),
		entry("/tmp/legacy", gib, time.Hour, nil),
		entry("/tmp/lead-own", gib, time.Hour, sessionOwner(lead, "go")),
		entry("/tmp/admin-own", gib, time.Hour, sessionOwner(admin, "go")),
	}
	entries[3].UID = 0
	groups := c.Over(t0, c.Items(entries, me), 1)
	// shop и git-admin мертвы; work-admin и лид живы
	live := newLive(tLead, tAdmin)
	sums, _ := c.Plan(t0, groups, live, &UserState{}, 1)
	where := map[string]string{}
	rels := map[string]Relation{}
	for _, s := range sums {
		for _, l := range s.Lines {
			where[l.Item.Entry.Path] = s.Target.Key()
			rels[l.Item.Entry.Path] = l.Rel
		}
	}
	expect := map[string]struct {
		to  string
		rel Relation
	}{
		"/tmp/dead":      {"work-admin", RelDeadOwner}, // создатель мёртв → админ ~/work
		"/tmp/git-dead":  {"lead", RelDeadOwner},       // git-admin мёртв → лид
		"/tmp/user":      {"lead", RelNonAgent},
		"/tmp/root":      {"lead", RelOtherUID},
		"/tmp/legacy":    {"lead", RelNoCreator},
		"/tmp/lead-own":  {"lead", RelOwn},
		"/tmp/admin-own": {"work-admin", RelOwn},
	}
	for p, w := range expect {
		if where[p] != w.to || rels[p] != w.rel {
			t.Errorf("%s: %s/%v, хотим %s/%v", p, where[p], rels[p], w.to, w.rel)
		}
	}
	// админ сам себе создатель: следующая ступень сразу лид (без дубля)
	st := &UserState{}
	g := c.Over(t0, c.Items([]record.Entry{entry("/tmp/admin-own", gib, time.Hour, sessionOwner(admin, "go"))}, me), 1)
	var seq []string
	for i := range 4 {
		sums, _ := c.Plan(t0.Add(time.Duration(i)*5*time.Minute), g, live, st, 1)
		for _, s := range sums {
			seq = append(seq, s.Target.Key())
		}
	}
	if strings.Join(seq, ",") != "work-admin,work-admin,lead" {
		t.Errorf("лестница админа: %v", seq)
	}
	// лида нет — сводки не-агентам некому слать
	if s, d := c.Plan(t0, c.Over(t0, c.Items(entries[2:3], me), 1), newLive(), &UserState{}, 1); len(s)+len(d) != 0 {
		t.Errorf("без лида сводок быть не должно: %v", targets(s))
	}
}

func TestAdminLongestPrefix(t *testing.T) {
	c := cfg()
	c.Escalation.Admins[home+"/work/shop"] = "shop-admin"
	if a := c.adminFor(home + "/work/shop/sub"); a != "shop-admin" {
		t.Errorf("длинный префикс: %q", a)
	}
	if a := c.adminFor(home + "/workshop"); a != "" {
		t.Errorf("/workshop не под /work: %q", a)
	}
}

func TestMessage(t *testing.T) {
	c := cfg()
	shop := sess(11, "shop-master", home+"/work/shop")
	lead := sess(10, sLead, home)
	sum := Summary{Target: tLead, Factor: 1, Lines: []Line{
		{Item: Item{Entry: entry("/tmp/claude-tmp/own", 1536*mib, 25*time.Minute, sessionOwner(lead, "go")), Class: ClassSession}, Rel: RelOwn},
		{Item: Item{Entry: entry("/tmp/oc", 300*mib, 3*time.Hour, sessionOwner(lead, "node")), Class: ClassFighter, Fighter: "opencode"}, Rel: RelFighter},
		{Item: Item{Entry: entry("/tmp/x y", 20*mib, 50*time.Hour, sessionOwner(shop, "go")), Class: ClassSession}, Rel: RelDeadOwner, CreatorName: "shop-master"},
		{Item: Item{Entry: entry("/tmp/legacy", 2*gib, 72*time.Hour, nil), Class: ClassUnknown}, Rel: RelNoCreator},
	}}
	msg := c.Message(&sum, t0, false)
	for _, want := range []string{
		"tmpfs, то есть оперативка",
		"Ваши записи:\n1. /tmp/claude-tmp/own — 1.5 ГБ, файлов 3, возраст 25 мин, мера 38 ГБ·мин; создал: go arg (pid 5000), сессия lead",
		"rm -rf /tmp/claude-tmp/own , если не нужна; нужную — mv /tmp/claude-tmp/own в scratchpad на диске",
		"Записи ваших бойцов:\n2. /tmp/oc",
		", боец opencode",
		"Записи завершившейся сессии shop-master",
		"'/tmp/x y'",
		"возраст 2 д 2 ч",
		"Записи без известного создателя",
		"неизвестно (запись старше сторожа)",
		"Порог 12 ГБ·мин на владельца (1 ГБ на 12 мин ≈ 100 МБ на 2 ч 2 мин ≈ 10 МБ на 20 ч 28 мин)",
		"сессия-создатель, админ пространства, lead",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("в сводке нет %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "—\n") || strings.Contains(msg, "**") {
		t.Error("в сводке разметка")
	}
	if strings.HasPrefix(msg, TestMark) {
		t.Error("пометка теста в боевой сводке")
	}
	if !strings.HasPrefix(c.Message(&sum, t0, true), TestMark) {
		t.Error("нет пометки теста")
	}
	c.Policy.MaxListed = 2
	sum.Pressure, sum.Factor = true, 0.25
	short := c.Message(&sum, t0, false)
	if !strings.Contains(short, "И ещё записей: 2") || strings.Contains(short, "/tmp/legacy") {
		t.Errorf("обрезка списка:\n%s", short)
	}
	if !strings.Contains(short, "ужесточён до ×0.25") {
		t.Error("нет пометки давления")
	}
}

func TestHumanFormats(t *testing.T) {
	bytesCases := map[int64]string{512: "512 Б", 1536: "1.5 КБ", 350 * mib: "350 МБ", 12 * gib / 10: "1.2 ГБ"}
	for in, want := range bytesCases {
		if got := HumanBytes(in); got != want {
			t.Errorf("HumanBytes(%d) = %q, хотим %q", in, got, want)
		}
	}
	ageCases := map[time.Duration]string{40 * time.Second: "40 с", 25 * time.Minute: "25 мин", 3 * time.Hour: "3 ч",
		190 * time.Minute: "3 ч 10 мин", 52 * time.Hour: "2 д 4 ч", 48 * time.Hour: "2 д"}
	for in, want := range ageCases {
		if got := HumanAge(in); got != want {
			t.Errorf("HumanAge(%v) = %q, хотим %q", in, got, want)
		}
	}
}

// TestCreatorTextNamesSession: процесс сессии в «создал» — её имя, не командная строка claude
// (10.11: «claude --settings …/git-master.json» прочли как сессию git-master).
func TestCreatorTextNamesSession(t *testing.T) {
	s := sess(24573, "akit-master", home+"/git/akit")
	self := &record.Owner{Proc: record.Proc{Pid: 24573, Comm: "claude",
		Args: "/home/aisee/.local/bin/claude --settings /home/aisee/.config/cl/settings/git-master.json", UID: me}, Session: s}
	child := &record.Owner{Proc: record.Proc{Pid: 5, Comm: "mkdir", Args: "mkdir", UID: me}, Session: s}
	noName := &record.Owner{Proc: record.Proc{Pid: 5, Comm: "mkdir", Args: "mkdir", UID: me}, Session: sess(24573, "", "")}
	noSess := &record.Owner{Proc: record.Proc{Pid: 5, Comm: "mkdir", Args: "mkdir", UID: me}}
	for _, c := range []struct {
		o    *record.Owner
		want string
	}{
		{self, "сессия akit-master (pid 24573)"},
		{child, "mkdir (pid 5), сессия akit-master"},
		{noName, "mkdir (pid 5)"},
		{noSess, "mkdir (pid 5)"},
		{nil, "неизвестно (запись старше сторожа)"},
	} {
		if got := CreatorText(c.o); got != c.want {
			t.Errorf("CreatorText = %q, want %q", got, c.want)
		}
	}
}
