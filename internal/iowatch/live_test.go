package iowatch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"
)

// Шаги сворачиваются в секунду по At (как Window.Seconds): шаг ровно на границе — уже следующая
// секунда; идущая секунда в ряд не попадает. Stall — мс шагов с простоем, WAwait — WMS/WIOS секунды,
// Dirty и Writeback — последние из детального прохода.
func TestSeriesFoldsStepsIntoSeconds(t *testing.T) {
	r := NewRecorder(Default())
	series := NewSeries(time.Hour)
	r.Attach(series, nil, nil)
	base := time.Date(2026, 10, 29, 21, 0, 0, 0, time.Local)
	r.Detail(Step{At: base, Dirty: 30 << 20, Writeback: 2 << 20})
	for i := 1; i <= 10; i++ { // 21:00:00.1 … 21:00:01.0
		s := Step{At: base.Add(time.Duration(i) * 100 * time.Millisecond), IOFullUS: 10_000, WriteBytes: 1 << 20, WIOS: 2, WMS: 10, InFlight: int64(i), FIOS: 1}
		if i == 3 || i == 4 {
			s.IOFullUS = 70_000 // шаги с простоем
			s.WIOS, s.WMS = 1, 200
		}
		if i == 9 {
			r.Detail(Step{At: s.At, Dirty: 50 << 20, Writeback: 4 << 20})
		}
		r.Step(s)
	}
	if series.Len() != 1 { // 21:00:00 закрыл шаг 1.0, 21:00:01 идёт
		t.Fatalf("закрытых секунд %d, ждали 1", series.Len())
	}
	r.Step(Step{At: base.Add(2 * time.Second)})
	got := series.Rows(time.Time{}).Seconds()
	if len(got) != 2 {
		t.Fatalf("секунд %d: %+v", len(got), got)
	}
	first, second := got[0], got[1]
	// 21:00:00: шаги .1–.9 (9 шагов), из них 2 с простоем; WIOS 7·2+2·1 = 16, WMS 7·10+2·200 = 470 → 29 мс.
	if !first.At.Equal(base) || first.IOFullMS != 7*10+2*70 || first.StallMS != 200 || first.WriteB != 9<<20 ||
		first.WAwaitMS != 470/16 || first.InFlightMax != 9 || first.Flushes != 9 || first.Dirty != 50<<20 || first.Writeback != 4<<20 {
		t.Fatalf("секунда 0: %+v", first)
	}
	// 21:00:01: шаг ровно на границе (1.0) — эта секунда; шаг 2.0 закрыл её.
	if !second.At.Equal(base.Add(time.Second)) || second.IOFullMS != 10 || second.StallMS != 0 || second.InFlightMax != 10 || second.WAwaitMS != 5 {
		t.Fatalf("секунда 1: %+v", second)
	}
	if size := unsafe.Sizeof(secondRow{}); size != 72 {
		t.Fatalf("строка ряда %d байт, в доке — 72", size)
	}
}

// Кольцо ряда держит keep: старые секунды вытесняются, Rows(since) режет по времени, Resize
// переносит то, что влезает.
func TestSeriesKeepSinceAndResize(t *testing.T) {
	s := NewSeries(10 * time.Second)
	base := time.Date(2026, 10, 29, 21, 0, 0, 0, time.Local)
	for i := range 30 {
		s.Add(MachineSecond{At: base.Add(time.Duration(i) * time.Second), WriteB: int64(i)})
	}
	s.Add(MachineSecond{At: base}) // старше лежащих — не берётся
	rows := s.Rows(time.Time{}).Seconds()
	if len(rows) != 10 || rows[0].WriteB != 20 || rows[9].WriteB != 29 {
		t.Fatalf("кольцо: %d строк, %+v", len(rows), rows)
	}
	if got := s.Rows(base.Add(25 * time.Second)); len(got) != 5 {
		t.Fatalf("since: %d", len(got))
	}
	small := s.Resize(3 * time.Second)
	if got := small.Rows(time.Time{}).Seconds(); len(got) != 3 || got[0].WriteB != 27 || small.Keep() != 3*time.Second {
		t.Fatalf("resize: %+v", got)
	}
}

// Профиль: cgroup живёт 3 прохода с записью 10/30/20 МиБ — запись 60 МиБ, пик 30 МиБ/с; вид —
// класс из job.json бойца; профиль появляется, когда папки cgroup нет. Мелкая cgroup не хранится.
func TestProfileFromPassesKindAndEnd(t *testing.T) {
	cgroupRoot, workerState := t.TempDir(), t.TempDir()
	worker := "/user.slice/user-1000.slice/user@1000.service/app.slice/worker-1029-2100-glint-rule.service"
	small := "/user.slice/user-1000.slice/session-3.scope"
	writeFile(t, filepath.Join(workerState, "1029-2100-glint-rule", "job.json"), `{"id":"1029-2100-glint-rule","class":"glint-правило"}`)
	writeFile(t, filepath.Join(cgroupRoot, worker, "cgroup.procs"), "1\n")
	writeFile(t, filepath.Join(cgroupRoot, small, "cgroup.procs"), "2\n")
	r := NewRecorder(Default())
	profiles := NewProfiles()
	r.Attach(nil, profiles, nil)
	base := time.Date(2026, 10, 29, 21, 0, 0, 0, time.Local)
	for i, mib := range []int64{10, 30, 20} {
		r.Detail(Step{At: base.Add(time.Duration(i) * time.Second),
			Processes: []Process{{PID: 5, Comm: "codex", WriteBytes: mib << 19, Cgroup: worker}, {PID: 6, Comm: "accept-run.sh", WriteBytes: mib << 18, Cgroup: worker}},
			Cgroups:   []Cgroup{{Path: worker, WriteBytes: mib << 20, ReadBytes: 1 << 20, IOFullUS: 5000, SwapIn: 2}, {Path: small, WriteBytes: 4096}}})
	}
	if open := profiles.Open(); len(open) != 2 {
		t.Fatalf("копятся: %v", open)
	}
	gone := map[string]string{}
	for _, path := range profiles.Open() {
		ended, err := CgroupGone(cgroupRoot, path)
		if err != nil || ended {
			t.Fatalf("живая cgroup %s: %v %v", path, ended, err)
		}
	}
	for _, path := range []string{worker, small} {
		if err := os.RemoveAll(filepath.Join(cgroupRoot, path)); err != nil {
			t.Fatal(err)
		}
		ended, err := CgroupGone(cgroupRoot, path)
		if err != nil || !ended {
			t.Fatalf("кончившаяся cgroup %s: %v %v", path, ended, err)
		}
		gone[path] = ProfileKind(path, workerState, func(err error) { t.Fatal(err) })
	}
	got := profiles.End(gone, time.Second)
	if len(got) != 1 || len(profiles.Open()) != 0 {
		t.Fatalf("профили (мелкая не хранится): %+v, открыты %v", got, profiles.Open())
	}
	p := got[0]
	if p.Kind != "worker:glint-правило" || p.Name != "worker-1029-2100-glint-rule.service" || p.WriteB != 60<<20 || p.PeakWriteBps != 30<<20 ||
		p.ReadB != 3<<20 || p.PeakReadBps != 1<<20 || p.IOFullMS != 15 || p.SwapIn != 6 || p.DurMS != 3000 || !p.Start.Equal(base.Add(-time.Second)) {
		t.Fatalf("профиль: %+v", p)
	}
	if len(p.Procs) != 2 || p.Procs[0] != (Ranked{"codex", 30 << 20}) || p.Procs[1].Name != "accept-run.sh" {
		t.Fatalf("процессы бойца: %+v", p.Procs)
	}
}

func TestProfileKindAgentServiceAndWorkerWithoutClass(t *testing.T) {
	state := t.TempDir()
	writeFile(t, filepath.Join(state, "noclass", "job.json"), `{"id":"noclass"}`)
	for path, want := range map[string]string{
		"/user.slice/user-1000.slice/user@1000.service/agents.slice/agent-aguard-master-666231-r1.scope": KindAgent,
		"/user.slice/user-1000.slice/user@1000.service/app.slice/foo.service":                            "service:foo.service",
		"/user.slice/user-1000.slice/session-3.scope":                                                    "service:session-3.scope",
	} {
		if got := ProfileKind(path, state, func(err error) { t.Error(err) }); got != want {
			t.Errorf("%s: %q, ждали %q", path, got, want)
		}
	}
	for _, job := range []string{"noclass", "nofile"} {
		var noClass error
		got := ProfileKind("/app.slice/worker-"+job+".service", state, func(err error) { noClass = err })
		if got != KindWorkerUnknown || noClass == nil {
			t.Errorf("%s: %q %v", job, got, noClass)
		}
	}
}

// Свод профилей вида: медиана и p90 по ближайшему рангу, последний конец.
func TestSummarizeProfilesQuantiles(t *testing.T) {
	base := time.Date(2026, 10, 29, 21, 0, 0, 0, time.Local)
	var list []Profile
	for i := range 10 {
		list = append(list, Profile{Kind: "worker:x", PeakWriteBps: int64(i+1) << 20, WriteB: int64(10-i) << 20, DurMS: int64(i) * 1000, End: base.Add(time.Duration(i) * time.Minute)})
	}
	list = append(list, Profile{Kind: KindAgent, WriteB: 7})
	st := SummarizeProfiles(list)
	w := st["worker:x"]
	if w.Count != 10 || w.PeakWriteBps != (Quantiles{5 << 20, 9 << 20}) || w.WriteB != (Quantiles{5 << 20, 9 << 20}) || w.DurMS != (Quantiles{4000, 8000}) ||
		!w.Last.Equal(base.Add(9*time.Minute)) || st[KindAgent].WriteB != (Quantiles{7, 7}) {
		t.Fatalf("свод: %+v", st)
	}
}

// Живое состояние: окна по быстрым шагам, идущий случай, закрытые случаи за 30 и 60 с, писатели
// последнего прохода, свод профилей запрошенного вида.
func TestLiveNowWindowsCasesWriters(t *testing.T) {
	cfg := Default()
	r := NewRecorder(cfg)
	live := NewLive()
	r.Attach(nil, nil, live)
	if _, ok := live.Now(""); ok {
		t.Fatal("состояние до первого шага")
	}
	base := time.Date(2026, 10, 29, 21, 0, 0, 0, time.Local)
	step := func(i int, idle int64) {
		r.Step(Step{At: base.Add(time.Duration(i) * 100 * time.Millisecond), IOFullUS: idle, WriteBytes: 1 << 20, ReadBytes: 1 << 19, WIOS: 4, WMS: 40, InFlight: int64(i % 7)})
	}
	for i := range 600 { // 60 с
		var idle int64
		switch {
		case i == 100: // случай 100 мс, конец 10.0 с — старше 30 с к концу
			idle = 80_000
		case i >= 400 && i < 403: // случай 300 мс, конец 40.2 с
			idle = 80_000
		case i >= 597: // идущий случай с 59.6 с
			idle = 90_000
		}
		step(i, idle)
	}
	live.SetProfiles(map[string]ProfileStats{"worker:x": {Kind: "worker:x", Count: 3}})
	r.Detail(Step{At: base.Add(59 * time.Second), Dirty: 70 << 20, Writeback: 3 << 20,
		Cgroups: []Cgroup{{Path: "/a.slice/dd.scope", WriteBytes: 50 << 20}, {Path: "/a.slice/b.service", WriteBytes: 2 << 20}, {Path: "/a.slice/r.scope", ReadBytes: 1}}})
	n, ok := live.Now("worker:x")
	if !ok || !n.At.Equal(base.Add(59900*time.Millisecond)) {
		t.Fatalf("at: %v %v", n.At, ok)
	}
	if n.W1s.Steps != 10 || n.W1s.WriteBps != 10<<20 || n.W1s.ReadBps != 5<<20 || n.W1s.WAwaitMS != 10 || n.W1s.StallMS != 300 ||
		n.W1s.IOFullMS != 270 || n.W1s.InFlightMax != 6 || n.W30s.Steps != 300 || n.W30s.StallMS != 300+300 {
		t.Fatalf("окна: 1с %+v, 30с %+v", n.W1s, n.W30s)
	}
	if !n.Open.Open || n.Open.MS != 300 {
		t.Fatalf("идущий случай: %+v", n.Open)
	}
	c := n.Cases
	if c.From100ms30s != 1 || c.From200ms30s != 1 || c.From100ms60s != 2 || c.From200ms60s != 1 || c.LastEnd == nil || !c.LastEnd.Equal(base.Add(40200*time.Millisecond)) {
		t.Fatalf("случаи: %+v", c)
	}
	if n.Dirty != 70<<20 || len(n.Writers) != 2 || n.Writers[0].Name != "dd.scope" || n.Writers[0].WriteBps != 50<<20 || n.Writers[1].Name != "b.service" {
		t.Fatalf("писатели: %+v", n.Writers)
	}
	if n.Profile == nil || n.Profile.Count != 3 {
		t.Fatalf("свод вида: %+v", n.Profile)
	}
	if other, _ := live.Now("worker:нет"); other.Profile != nil {
		t.Fatalf("свод чужого вида: %+v", other.Profile)
	}
}

func TestCheckRequestVersionQueryAndSince(t *testing.T) {
	if _, err := CheckRequest(Request{V: 2, Q: QueryNow}); err == nil || !strings.Contains(err.Error(), "версия") {
		t.Errorf("другая версия: %v", err)
	}
	if _, err := CheckRequest(Request{V: 1, Q: "что-то"}); err == nil {
		t.Error("неизвестный запрос без ошибки")
	}
	if _, err := CheckRequest(Request{V: 1, Q: QuerySeries, Since: "1h"}); err == nil {
		t.Error("since не RFC3339 без ошибки")
	}
	since, err := CheckRequest(Request{V: 1, Q: QueryProfiles, Since: "2026-10-29T21:00:00+01:00"})
	if err != nil || since.IsZero() {
		t.Errorf("since: %v %v", since, err)
	}
}
