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
// CPUSome — сумма, RunnableMax — наибольшее; Dirty, Writeback, MemAvail и SwapUsed — последние из
// детального прохода.
// Задержка цикла на 2 минуты с простоем: простой размазан по секундам, ни одна не показывает больше секунды.
func TestSeriesLongStallSpreads(t *testing.T) {
	r := NewRecorder(Default())
	series := NewSeries(time.Hour)
	r.Attach(series, nil, nil)
	base := time.Date(2026, 10, 29, 21, 0, 0, 0, time.Local)
	r.Step(Step{At: base})
	end := base.Add(2 * time.Minute)
	r.Step(Step{At: end, IOFullUS: 110_000_000})
	r.Step(Step{At: end.Add(2 * time.Second)})
	rows := series.Rows(base.Add(-time.Second)).Seconds()
	if len(rows) < 120 {
		t.Fatalf("простой за 2 минуты не размазан: %d секунд", len(rows))
	}
	var total int64
	for _, row := range rows {
		if row.StallMS > 1000 {
			t.Fatalf("секунда показывает простой %d мс", row.StallMS)
		}
		total += int64(row.StallMS)
	}
	if total < 119_000 || total > 120_000 {
		t.Fatalf("простой всего %d мс, хотим 120 с", total)
	}
}

// Сон машины в ряд пустыми секундами не ложится: дыра остаётся дырой, значения шага — в секунде At.
func TestSeriesLongGapStaysGap(t *testing.T) {
	r := NewRecorder(Default())
	series := NewSeries(time.Hour)
	r.Attach(series, nil, nil)
	base := time.Date(2026, 10, 29, 21, 0, 0, 0, time.Local)
	r.Step(Step{At: base.Add(100 * time.Millisecond)})
	wake := base.Add(10 * time.Minute)
	r.Step(Step{At: wake, CPUSomeUS: 5000})
	r.Step(Step{At: wake.Add(2 * time.Second)})
	rows := series.Rows(base.Add(-time.Second))
	if n := len(rows); n > 3 {
		t.Fatalf("сон лёг в ряд: %d секунд", n)
	}
}

// Длинный шаг ниже порога простоя: 50 с PSI за 120 с не ложатся в одну секунду, в ней — её доля.
func TestSeriesLongGapKeepsSecondShare(t *testing.T) {
	series := NewSeries(time.Hour)
	at := time.Date(2026, 10, 29, 21, 2, 0, 0, time.Local)
	series.step(&Step{At: at, IOFullUS: 50_000_000, IOSomeUS: 60_000_000, MemFullUS: 12_000_000, CPUSomeUS: 24_000_000}, 0, 2*time.Minute)
	series.step(&Step{At: at.Add(time.Second)}, 0, time.Second) // закрывает секунду
	rows := series.Rows(at.Add(-time.Hour)).Seconds()
	if len(rows) != 1 {
		t.Fatalf("пропуск лёг в ряд: %d секунд", len(rows))
	}
	r := rows[0]
	if r.IOFullMS != 416 || r.MemFullMS != 100 || r.CPUSomeMS != 200 {
		t.Fatalf("доля секунды: %+v", r)
	}
}

func TestSeriesFoldsStepsIntoSeconds(t *testing.T) {
	r := NewRecorder(Default())
	series := NewSeries(time.Hour)
	r.Attach(series, nil, nil)
	base := time.Date(2026, 10, 29, 21, 0, 0, 0, time.Local)
	r.Detail(Step{At: base, Dirty: 30 << 20, Writeback: 2 << 20, MemAvail: 3 << 30, SwapUsed: 1 << 30})
	for i := 1; i <= 10; i++ { // 21:00:00.1 … 21:00:01.0
		s := Step{At: base.Add(time.Duration(i) * 100 * time.Millisecond), IOFullUS: 10_000, WriteBytes: 1 << 20, WIOS: 2, WMS: 10, InFlight: int64(i), FIOS: 1,
			CPUSomeUS: 12_000, Runnable: int64(10 + i%4)}
		if i == 3 || i == 4 {
			s.IOFullUS = 70_000 // шаги с простоем
			s.WIOS, s.WMS = 1, 200
		}
		if i == 9 {
			r.Detail(Step{At: s.At, Dirty: 50 << 20, Writeback: 4 << 20, MemAvail: 1100 << 20, SwapUsed: 15 << 30})
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
		first.WAwaitMS != 470/16 || first.InFlightMax != 9 || first.Flushes != 9 || first.Dirty != 50<<20 || first.Writeback != 4<<20 ||
		first.CPUSomeMS != 9*12 || first.RunnableMax != 13 || first.MemAvail != 1100<<20 || first.SwapUsed != 15<<30 {
		t.Fatalf("секунда 0: %+v", first)
	}
	// 21:00:01: шаг ровно на границе (1.0) — эта секунда; шаг 2.0 закрыл её.
	if !second.At.Equal(base.Add(time.Second)) || second.IOFullMS != 10 || second.StallMS != 0 || second.InFlightMax != 10 || second.WAwaitMS != 5 ||
		second.CPUSomeMS != 12 || second.RunnableMax != 12 {
		t.Fatalf("секунда 1: %+v", second)
	}
	if size := unsafe.Sizeof(secondRow{}); size != 88 {
		t.Fatalf("строка ряда %d байт, в доке — 88", size)
	}
}

// series.jsonl фазы 1 (без CPU и памяти) загружается: новых полей нет — нули.
func TestLoadSeriesOldLineWithoutCPU(t *testing.T) {
	path := filepath.Join(t.TempDir(), "series.jsonl")
	writeFile(t, path, `{"at":"2026-10-29T22:56:30+01:00","io_full_ms":2,"mem_full_ms":2,"wr_b":1048576,"dirty":3145728,"swapin":5349,"majflt":4015}`+"\n")
	seconds, err := LoadSeries(path, time.Time{})
	if err != nil || len(seconds) != 1 {
		t.Fatalf("старая строка: %+v %v", seconds, err)
	}
	s := NewSeries(time.Hour)
	s.Add(seconds[0])
	got := s.Rows(time.Time{}).Seconds()[0]
	if got.SwapIn != 5349 || got.Majflt != 4015 || got.Dirty != 3<<20 || got.CPUSomeMS != 0 || got.RunnableMax != 0 || got.MemAvail != 0 || got.SwapUsed != 0 {
		t.Fatalf("строка в ряду: %+v", got)
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
		gone[path] = ProfileKind(path, KindSources{WorkerState: workerState}, func(err *KindUnknownError) { t.Fatal(err) })
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

// Профиль по CPU и памяти: лист живёт 3 прохода по 1 с, usage_usec растёт на 1, 3 и 2 с — CPU 6000 мс,
// пик 3000 тысячных ядра; anon — наибольший из трёх; majflt — сумма; swap-in машины — быстрые шаги
// от начала первого прохода до последнего, где лист был виден.
func TestProfileCPUAnonAndMachineSwapIn(t *testing.T) {
	r := NewRecorder(Default())
	profiles := NewProfiles()
	r.Attach(nil, profiles, nil)
	base := time.Date(2026, 10, 29, 22, 56, 0, 0, time.Local)
	at := base
	steps := func(n int, swapIn int64) {
		for range n {
			at = at.Add(100 * time.Millisecond)
			r.Step(Step{At: at, SwapIn: swapIn})
		}
	}
	const leaf = "/user.slice/app.slice/glint.service"
	steps(5, 7)
	r.Detail(Step{At: at}) // проход без листа: начало его первого прохода
	for i, cpuS := range []int64{1, 3, 2} {
		steps(10, 10)
		anon := []int64{100, 300, 200}[i] << 20
		r.Detail(Step{At: at, Cgroups: []Cgroup{{Path: leaf, WriteBytes: 4 << 20, CPUUS: cpuS * 1_000_000, AnonB: anon, Majflt: int64(4 + i)}}})
	}
	steps(10, 10) // после последнего прохода с листом — не его
	got := profiles.End(map[string]string{leaf: "service:glint.service"}, time.Second)
	if len(got) != 1 {
		t.Fatalf("профили: %+v", got)
	}
	p := got[0]
	if p.CPUMS != 6000 || p.PeakCPUMilli != 3000 || p.PeakAnonB != 300<<20 || p.Majflt != 15 || p.MachineSwapIn != 300 || p.SwapIn != 0 {
		t.Fatalf("профиль: %+v", p)
	}
}

// Профиль сохраняется и по одной минуте CPU, когда обмена мало и жизнь короче минуты: glint check
// и go test. 59 с CPU при тех же условиях — мелочь.
func TestProfileSavedByCPUOnly(t *testing.T) {
	p := NewProfiles()
	base := time.Date(2026, 10, 29, 22, 56, 0, 0, time.Local)
	const heavy, light = "/app.slice/go-test.scope", "/app.slice/light.scope"
	for i := range 3 { // проходы по 10 с: жизнь 30 с
		p.detail(&Step{At: base.Add(time.Duration(i) * 10 * time.Second), Cgroups: []Cgroup{
			{Path: heavy, WriteBytes: 4096, CPUUS: 30_000_000}, {Path: light, WriteBytes: 4096, CPUUS: 59_000_000 / 3}}}, 10*time.Second)
	}
	got := p.End(map[string]string{heavy: "service:go-test.scope", light: "service:light.scope"}, 10*time.Second)
	if len(got) != 1 || got[0].Path != heavy || got[0].CPUMS != 90_000 || got[0].PeakCPUMilli != 3000 || got[0].Dur() >= profileMinDur {
		t.Fatalf("по CPU: %+v", got)
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
		if got := ProfileKind(path, KindSources{WorkerState: state}, func(err *KindUnknownError) { t.Error(err) }); got != want {
			t.Errorf("%s: %q, ждали %q", path, got, want)
		}
	}
	for _, job := range []string{"noclass", "nofile"} {
		var noClass *KindUnknownError
		got := ProfileKind("/app.slice/worker-"+job+".service", KindSources{WorkerState: state}, func(err *KindUnknownError) { noClass = err })
		if got != KindWorkerUnknown || noClass == nil || noClass.Source != KindSourceWorker {
			t.Errorf("%s: %q %v", job, got, noClass)
		}
	}
}

// Повтор бойца — unit worker-<job>-<N>.service, а job.json лежит в <job>/: класс оттуда; имя задания с числом на конце
// (1110-1118-2026-11-10) — своя папка раньше обрезанной. worker-settle.service — служба worker, не боец.
func TestProfileKindWorkerRetryAndSettle(t *testing.T) {
	state := t.TempDir()
	writeFile(t, filepath.Join(state, "1107-1220-dream-page-dups", "job.json"), `{"class":"правка-по-ревью"}`)
	writeFile(t, filepath.Join(state, "1110-1118-2026-11-10", "job.json"), `{"class":"обход-сна"}`)
	writeFile(t, filepath.Join(state, "1110-1118-2026", "job.json"), `{"class":"чужой"}`)
	writeFile(t, filepath.Join(state, "1110-1200-7", "job.json"), `{"id":"x"}`)
	writeFile(t, filepath.Join(state, "1110-1200", "job.json"), `{"class":"чужой"}`)
	const app = "/user.slice/user-1000.slice/user@1000.service/app.slice/"
	for unit, want := range map[string]string{
		"worker-1107-1220-dream-page-dups-1.service": "worker:правка-по-ревью",
		"worker-1107-1220-dream-page-dups.service":   "worker:правка-по-ревью",
		"worker-1110-1118-2026-11-10.service":        "worker:обход-сна",
		"worker-1110-1118-2026-11-10-2.service":      "worker:обход-сна",
		"worker-settle.service":                      "service:worker-settle.service",
	} {
		if got := ProfileKind(app+unit, KindSources{WorkerState: state}, func(err *KindUnknownError) { t.Errorf("%s: %v", unit, err) }); got != want {
			t.Errorf("%s: %q, ждали %q", unit, got, want)
		}
	}
	// без класса в своём job.json — worker:?, к соседу с обрезанным хвостом не идём; хвост не число — не повтор
	for _, unit := range []string{"worker-1110-1200-7.service", "worker-1107-1220-dream-page-dups-x.service", "worker-1107-1220-dream-page-dups-0.service"} {
		var noClass *KindUnknownError
		if got := ProfileKind(app+unit, KindSources{WorkerState: state}, func(err *KindUnknownError) { noClass = err }); got != KindWorkerUnknown || noClass == nil {
			t.Errorf("%s: %q %v", unit, got, noClass)
		}
	}
}

// Scope тяжёлой работы graft (в agents-graft.slice и app-graft.slice) — вид задачи из файла graft по имени scope; файла
// нет или он пуст, многострочен — service:<scope> и причина с источником graft; чужой scope с тем же файлом не трогаем.
func TestProfileKindGraftHeavy(t *testing.T) {
	src := KindSources{WorkerState: t.TempDir(), GraftHeavy: GraftHeavyDir(t.TempDir())}
	writeFile(t, filepath.Join(src.GraftHeavy, "graft-heavy-101.scope"), "akit:exec:go\n")
	writeFile(t, filepath.Join(src.GraftHeavy, "graft-heavy-102.scope"), "aguard:test")
	writeFile(t, filepath.Join(src.GraftHeavy, "graft-heavy-103.scope"), "\n")
	writeFile(t, filepath.Join(src.GraftHeavy, "graft-heavy-104.scope"), "a:b\nc:d\n")
	writeFile(t, filepath.Join(src.GraftHeavy, "other-101.scope"), "akit:exec:go")
	const user = "/user.slice/user-1000.slice/user@1000.service/"
	for path, want := range map[string]string{
		user + "agents.slice/agents-graft.slice/graft-heavy-101.scope": "akit:exec:go",
		user + "app.slice/app-graft.slice/graft-heavy-102.scope":       "aguard:test",
		user + "app.slice/other-101.scope":                             "service:other-101.scope",
	} {
		if got := ProfileKind(path, src, func(err *KindUnknownError) { t.Errorf("%s: %v", path, err) }); got != want {
			t.Errorf("%s: %q, ждали %q", path, got, want)
		}
	}
	for _, unit := range []string{"graft-heavy-103.scope", "graft-heavy-104.scope", "graft-heavy-105.scope"} {
		var unknown *KindUnknownError
		got := ProfileKind(user+"app.slice/app-graft.slice/"+unit, src, func(err *KindUnknownError) { unknown = err })
		if got != "service:"+unit || unknown == nil || unknown.Source != KindSourceGraft {
			t.Errorf("%s: %q %v", unit, got, unknown)
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
	for i := range list {
		list[i].CPUMS, list[i].PeakCPUMilli, list[i].PeakAnonB, list[i].Majflt, list[i].MachineSwapIn = int64(i)*60_000, int64(i)*1000, int64(i)<<20, int64(i), int64(i)*100
	}
	list = append(list, Profile{Kind: KindAgent, WriteB: 7})
	st := SummarizeProfiles(list)
	w := st["worker:x"]
	if w.Count != 10 || w.PeakWriteBps != (Quantiles{5 << 20, 9 << 20}) || w.WriteB != (Quantiles{5 << 20, 9 << 20}) || w.DurMS != (Quantiles{4000, 8000}) ||
		w.CPUMS != (Quantiles{240_000, 480_000}) || w.PeakCPUMilli != (Quantiles{4000, 8000}) || w.PeakAnonB != (Quantiles{4 << 20, 8 << 20}) ||
		w.Majflt != (Quantiles{4, 8}) || w.MachineSwapIn != (Quantiles{400, 800}) ||
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
		r.Step(Step{At: base.Add(time.Duration(i) * 100 * time.Millisecond), IOFullUS: idle, WriteBytes: 1 << 20, ReadBytes: 1 << 19, WIOS: 4, WMS: 40, InFlight: int64(i % 7),
			CPUSomeUS: 11_000, Runnable: int64(i % 20), SwapIn: 130, Majflt: 100})
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
	chrome := "/user.slice/user-1000.slice/user@1000.service/app.slice/app-google\\x2dchrome-1234.scope"
	r.Detail(Step{At: base.Add(59 * time.Second), Dirty: 70 << 20, Writeback: 3 << 20, MemAvail: 1100 << 20, SwapUsed: 15 << 30,
		Cgroups: []Cgroup{{Path: "/a.slice/dd.scope", WriteBytes: 50 << 20}, {Path: "/a.slice/b.service", WriteBytes: 2 << 20, SwapIn: 40, Majflt: 30},
			{Path: "/a.slice/r.scope", ReadBytes: 1}, {Path: chrome, SwapIn: 5000, Majflt: 687}}})
	n, ok := live.Now("worker:x")
	if !ok || !n.At.Equal(base.Add(59900*time.Millisecond)) {
		t.Fatalf("at: %v %v", n.At, ok)
	}
	if n.W1s.Steps != 10 || n.W1s.WriteBps != 10<<20 || n.W1s.ReadBps != 5<<20 || n.W1s.WAwaitMS != 10 || n.W1s.StallMS != 300 ||
		n.W1s.IOFullMS != 270 || n.W1s.InFlightMax != 6 || n.W30s.Steps != 300 || n.W30s.StallMS != 300+300 {
		t.Fatalf("окна: 1с %+v, 30с %+v", n.W1s, n.W30s)
	}
	// 1 с — шаги 590–599: runnable 10…19; 5 с — 550–599, в них 559 и 579 дают 19.
	if n.W1s.CPUSomeMS != 110 || n.W1s.RunnableMax != 19 || n.W1s.Majflt != 1000 || n.W1s.SwapIn != 1300 ||
		n.W5s.CPUSomeMS != 550 || n.W5s.RunnableMax != 19 || n.W30s.CPUSomeMS != 3300 || n.W30s.Majflt != 30_000 {
		t.Fatalf("CPU и вытеснение в окнах: 1с %+v, 5с %+v, 30с %+v", n.W1s, n.W5s, n.W30s)
	}
	if n.NProc < 1 || n.MemAvailable != 1100<<20 || n.SwapUsed != 15<<30 {
		t.Fatalf("машина: nproc %d, avail %d, swap %d", n.NProc, n.MemAvailable, n.SwapUsed)
	}
	if len(n.Swappers) != 2 || n.Swappers[0].Name != "google\\x2dchrome" || n.Swappers[0].SwapInPS != 5000 || n.Swappers[0].MajfltPS != 687 ||
		n.Swappers[1].Name != "b.service" || n.Swappers[1].MajfltPS != 30 {
		t.Fatalf("жертвы вытеснения: %+v", n.Swappers)
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

// Лист без io.stat (контроллер io у слайса не включён: scope graft-heavy в agents-graft.slice) —
// чтение и запись не мерены: профиль помечен no_io, свод считает квантили записи только по
// замеренным, а IOCount говорит, по скольким.
func TestProfileNoIOAndSummaryOverMeasured(t *testing.T) {
	p := NewProfiles()
	base := time.Date(2026, 11, 11, 7, 0, 0, 0, time.Local)
	const blind, seen = "/agents.slice/agents-graft.slice/graft-heavy-1.scope", "/app.slice/go-test.scope"
	for i := range 3 {
		p.detail(&Step{At: base.Add(time.Duration(i) * 10 * time.Second), Cgroups: []Cgroup{
			{Path: blind, CPUUS: 30_000_000, NoIO: i == 1}, {Path: seen, WriteBytes: 40 << 20, CPUUS: 30_000_000}}}, 10*time.Second)
	}
	got := p.End(map[string]string{blind: "graft:gate", seen: "graft:gate"}, 10*time.Second)
	if len(got) != 2 {
		t.Fatalf("профили: %+v", got)
	}
	byPath := map[string]Profile{got[0].Path: got[0], got[1].Path: got[1]}
	if !byPath[blind].NoIO || byPath[seen].NoIO {
		t.Fatalf("no_io: %+v", got)
	}
	list := append(got, Profile{Kind: "graft:gate", NoIO: true}, Profile{Kind: "graft:gate", NoIO: true})
	st := SummarizeProfiles(list)["graft:gate"]
	if st.Count != 4 || st.IOCount != 1 || st.PeakWriteBps != (Quantiles{4 << 20, 4 << 20}) || st.WriteB != (Quantiles{120 << 20, 120 << 20}) ||
		st.CPUMS != (Quantiles{0, 90_000}) {
		t.Fatalf("свод по замеренным: %+v", st)
	}
	none := SummarizeProfiles([]Profile{{Kind: "x", NoIO: true, CPUMS: 5}})["x"]
	if none.Count != 1 || none.IOCount != 0 || none.PeakWriteBps != (Quantiles{}) || none.CPUMS != (Quantiles{5, 5}) {
		t.Fatalf("свод без замеренных: %+v", none)
	}
}
