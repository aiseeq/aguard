package iowatch

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestParseIOMaxOwnDevicesMinAndErrors(t *testing.T) {
	data := []byte("259:0 rbps=80000000 wbps=max riops=max wiops=max\n252:0 rbps=30000000 wbps=max riops=max wiops=500\n8:0 rbps=1 wbps=1 riops=1 wiops=1\n")
	got, err := parseIOMax(data, []string{"259:0", "252:0"})
	if err != nil || got != (Cap{RBps: 30_000_000, WIOps: 500}) {
		t.Fatalf("пределы своих устройств, наименьший: %+v %v", got, err)
	}
	if got, err := parseIOMax([]byte("259:0 rbps=max wbps=max riops=max wiops=max\n"), []string{"259:0"}); err != nil || got.limited() {
		t.Fatalf("всё max — без потолка: %+v %v", got, err)
	}
	if got, err := parseIOMax(nil, []string{"259:0"}); err != nil || got.limited() {
		t.Fatalf("пустой io.max — без потолка: %+v %v", got, err)
	}
	for _, bad := range []string{"259:0 rbps=x\n", "259:0 rbps=0\n", "259:0 foo=1\n", "259:0 rbps\n"} {
		if _, err := parseIOMax([]byte(bad), []string{"259:0"}); err == nil {
			t.Errorf("%q — ошибка, а не тихий ноль", bad)
		}
	}
}

// Потолок предка (slice) и свой; io.max без числа — не потолок; снятый и поставленный потолок виден со
// следующего прохода, без нового обхода дерева (противник, сценарий 3).
func TestCgroupReaderCapFromAncestorAndLiveChange(t *testing.T) {
	root := t.TempDir()
	leaf := func(rel string) {
		dir := filepath.Join(root, rel)
		writeFile(t, filepath.Join(dir, "cgroup.procs"), "1\n")
		for _, name := range []string{"io.pressure", "memory.pressure"} {
			writeFile(t, filepath.Join(dir, name), "some avg10=0 total=0\nfull avg10=0 total=0\n")
		}
		writeFile(t, filepath.Join(dir, "io.stat"), "259:0 rbytes=0 wbytes=0 rios=0 wios=0\n")
	}
	pressure := func(rel string, fullUS int) {
		writeFile(t, filepath.Join(root, rel, "io.pressure"), "some avg10=0 total=0\nfull avg10=0 total="+strconv.Itoa(fullUS)+"\n")
	}
	const evals, own, free = "u.slice/evals.slice/a.scope", "u.slice/run-1.scope", "u.slice/lead.scope"
	for _, rel := range []string{evals, own, free} {
		leaf(rel)
	}
	writeFile(t, filepath.Join(root, "u.slice/evals.slice/io.max"), "259:0 rbps=80000000 wbps=max riops=max wiops=max\n")
	writeFile(t, filepath.Join(root, "u.slice/io.max"), "259:0 rbps=max wbps=max riops=max wiops=max\n")
	writeFile(t, filepath.Join(root, own, "io.max"), "259:0 rbps=30000000 wbps=max riops=max wiops=max\n")
	r := newCgroupReader(root, []string{"259:0"})
	defer r.close()
	now := time.Now()
	if _, err := r.read(&Step{At: now}); err != nil {
		t.Fatal(err)
	}
	pass := func(n int) map[string]Cap {
		t.Helper()
		for _, rel := range []string{evals, own, free} {
			pressure(rel, 1000*n)
		}
		s := Step{At: now.Add(time.Duration(n) * time.Second)}
		skipped, err := r.read(&s)
		if err != nil || skipped != nil {
			t.Fatal(err, skipped)
		}
		got := map[string]Cap{}
		for _, g := range s.Cgroups {
			got[g.Path] = g.Cap
		}
		return got
	}
	got := pass(1)
	if got["/"+evals] != (Cap{Path: "/u.slice/evals.slice", RBps: 80_000_000}) || got["/"+own] != (Cap{Path: "/" + own, RBps: 30_000_000}) ||
		got["/"+free].limited() || len(got) != 3 {
		t.Fatalf("потолки: %+v", got)
	}
	writeFile(t, filepath.Join(root, "u.slice/evals.slice/io.max"), "259:0 rbps=max wbps=max riops=max wiops=max\n")
	writeFile(t, filepath.Join(root, free, "io.max"), "259:0 rbps=max wbps=5000000 riops=max wiops=max\n")
	got = pass(2)
	if got["/"+evals].limited() || got["/"+free] != (Cap{Path: "/" + free, WBps: 5_000_000}) {
		t.Fatalf("смена потолка со следующего прохода: %+v", got)
	}
	writeFile(t, filepath.Join(root, own, "io.max"), "259:0 rbps=oops\n")
	for _, rel := range []string{evals, own, free} {
		pressure(rel, 3000)
	}
	s := Step{At: now.Add(3 * time.Second)}
	skipped, err := r.read(&s)
	if err != nil || skipped == nil || len(s.Cgroups) != 3 {
		t.Fatalf("битый io.max — ошибка прохода, лист учтён без потолка: %v %v %+v", err, skipped, s.Cgroups)
	}
	for _, g := range s.Cgroups {
		if g.Path == "/"+own && g.Cap.limited() {
			t.Fatalf("лист с битым io.max — вне потолка: %+v", g)
		}
	}
}

// capCase — случай 08.11 13:12:52 по секундам: проба под 30M читает 30 МБ/с и ждёт 700 мс в секунду; lead и
// dream-master ждут по 2 мс; диск занят 612 мс из 12 600.
func capCase(base time.Time, n int, step func(i int, s *Step)) []Step {
	probe := Cap{Path: "/app.slice/run-p2016228-i69065841.scope", RBps: 30_000_000}
	var out []Step
	for i := 1; i <= n; i++ {
		s := Step{At: base.Add(time.Duration(i) * time.Second), Cgroups: []Cgroup{
			{Path: probe.Path, ReadBytes: 30_000_000, ReadOps: 240, IOFullUS: 700_000, Cap: probe},
			{Path: "/agents.slice/agent-lead-1.scope", ReadBytes: 1 << 20, IOFullUS: 2000},
			{Path: "/agents.slice/agent-dream-master-2.scope", IOFullUS: 2000},
		}}
		if step != nil {
			step(i, &s)
		}
		out = append(out, s)
	}
	return out
}

func TestCappedWaitScenarios(t *testing.T) {
	base := time.Date(2026, 11, 8, 13, 12, 52, 0, time.Local)
	end := base.Add(12600 * time.Millisecond)
	const busy, dur = 612, 12600
	t.Run("случай 13:12:52 — под потолком", func(t *testing.T) {
		capped, outside, who := cw(capCase(base, 13, nil), base, end, time.Second, dur, busy)
		// 13 записей, последняя (12, 13 с] внутри случая на 0,6: 12,6 × 2 мс и 12,6 × 700 мс
		if !capped || outside != 25 || len(who) != 1 || who[0] != (Ranked{"run-p2016228-i69065841.scope", 8820}) {
			t.Fatalf("%v %d %+v", capped, outside, who)
		}
	})
	t.Run("простой под потолком до случая не его (сценарий 1)", func(t *testing.T) {
		details := append(capCase(base.Add(-5*time.Second), 4, nil), capCase(base, 2, func(_ int, s *Step) {
			s.Cgroups = []Cgroup{{Path: "/session.slice/plasmashell.service", IOFullUS: 600_000}}
		})...)
		// записи в 0 с нет: первая запись случая покрывает (−1, 1 с] и внутри него на половину — 300 + 600 мс
		if capped, outside, _ := cw(details, base, base.Add(2*time.Second), time.Second, 2000, 100); capped || outside != 900 {
			t.Fatalf("фриз после ожидания под потолком: %v %d", capped, outside)
		}
	})
	t.Run("потолок записи при простое чтения (сценарий 4)", func(t *testing.T) {
		details := capCase(base, 13, func(_ int, s *Step) { s.Cgroups[0].Cap = Cap{Path: s.Cgroups[0].Path, WBps: 30_000_000} })
		if capped, outside, _ := cw(details, base, end, time.Second, dur, busy); capped || outside != 8820 {
			t.Fatalf("в потолок не упирались — простой машины: %v %d", capped, outside)
		}
	})
	t.Run("вне потолка ждали", func(t *testing.T) {
		details := capCase(base, 13, func(i int, s *Step) {
			if i == 5 {
				s.Cgroups[1].IOFullUS = 250_000
			}
		})
		if capped, outside, _ := cw(details, base, end, time.Second, dur, busy); capped || outside != 273 {
			t.Fatalf("lead ждал 250 мс: %v %d", capped, outside)
		}
	})
	t.Run("последняя секунда чтения без упора (проба 08.11 13:43:06)", func(t *testing.T) {
		details := capCase(base, 2, func(i int, s *Step) {
			if i == 2 {
				s.Cgroups[0].ReadBytes, s.Cgroups[0].IOFullUS = 2_000_000, 40_000
			}
		})
		capped, outside, who := cw(details, base.Add(700*time.Millisecond), base.Add(1700*time.Millisecond), time.Second, 999, 59)
		// случай 0,7–1,7 с: от первой записи 0,3 (210 мс под потолком), от второй 0,7 (28 мс вне)
		if !capped || outside != 28 || len(who) != 1 || who[0].Value != 210 {
			t.Fatalf("упор — по записи: %v %d %+v", capped, outside, who)
		}
	})
	t.Run("диск занят больше половины", func(t *testing.T) {
		if capped, _, _ := cw(capCase(base, 13, nil), base, end, time.Second, dur, 6300); capped {
			t.Fatal("занятый диск — не оправдание")
		}
	})
	t.Run("общий потолок slice на двух листьях", func(t *testing.T) {
		slice := Cap{Path: "/agents.slice/agents-evals.slice", RBps: 80_000_000}
		details := capCase(base, 5, func(_ int, s *Step) {
			s.Cgroups = []Cgroup{
				{Path: slice.Path + "/a.scope", ReadBytes: 40_000_000, IOFullUS: 400_000, Cap: slice},
				{Path: slice.Path + "/b.scope", ReadBytes: 39_000_000, IOFullUS: 300_000, Cap: slice},
				{Path: "/agents.slice/agent-lead-1.scope", IOFullUS: 1000},
			}
		})
		capped, _, who := cw(details, base, base.Add(5*time.Second), time.Second, 5000, 200)
		if !capped || len(who) != 1 || who[0] != (Ranked{"agents-evals.slice", 3500}) {
			t.Fatalf("сумма листьев упирается в потолок slice: %v %+v", capped, who)
		}
	})
}

// Recorder: случай от case_min с ожиданием под потолком помечен, сводка его в фризы не считает.
func TestRecorderCappedCaseAndSummary(t *testing.T) {
	r := NewRecorder(Default())
	base := time.Date(2026, 11, 8, 13, 12, 50, 0, time.Local)
	idle := make([]int64, 60)
	for i := 10; i < 40; i++ {
		idle[i] = 70_000
	}
	probe := Cap{Path: "/app.slice/run-p1-i2.scope", RBps: 30_000_000}
	var got []Case
	for i, v := range idle {
		at := base.Add(time.Duration(i) * 100 * time.Millisecond)
		if i%10 == 0 {
			d := Step{At: at}
			if i >= 10 && i <= 40 {
				d.Cgroups = []Cgroup{{Path: probe.Path, ReadBytes: 30_000_000, IOFullUS: 700_000, Cap: probe},
					{Path: "/agents.slice/agent-lead-1.scope", IOFullUS: 3000}}
			}
			r.Detail(d)
		}
		got = append(got, r.Step(Step{At: at, IOFullUS: v, ReadBytes: 3_000_000, RIOS: 24, IOTicks: 5})...)
	}
	// детальные записи 1–4 с внутри случая 0,9–3,9 с на 0,1 + 1 + 1 + 0,9: вне потолка lead 3 × 3 мс
	if len(got) != 1 || !got[0].Capped || got[0].OutsideIdleMS != 9 || got[0].Culprits == nil || len(got[0].Culprits.CappedIdle) != 1 {
		t.Fatalf("случай под потолком: %+v", got)
	}
	s := Summarize(got, base, base.Add(time.Hour), 500*time.Millisecond)
	if s.Total != 0 || s.Capped != 1 || s.CappedIdleMS != got[0].IdleMS || len(s.CappedWho) != 1 || s.CappedWho[0].Name != "run-p1-i2.scope" || len(s.Cases) != 0 {
		t.Fatalf("сводка: %+v", s)
	}
}

// Окно случая и граница записи (проверка 13:59, сценарии 1 и 2 противника): простой под потолком в записях до
// случая не оправдывает его, даже когда вне потолка набралось меньше порога; запись с At ровно в начале случая
// покрывает секунду до него.
func TestCappedWaitWindowAndRecordBoundary(t *testing.T) {
	base := time.Date(2026, 11, 8, 14, 0, 0, 0, time.Local)
	evals := Cap{Path: "/agents.slice/agents-evals.slice", RBps: 80_000_000}
	rec := func(at time.Duration, g ...Cgroup) Step { return Step{At: base.Add(at), Cgroups: g} }
	eval := Cgroup{Path: evals.Path + "/a.scope", ReadBytes: 80_000_000, IOFullUS: 700_000, Cap: evals}
	ui := func(us int64) Cgroup { return Cgroup{Path: "/session.slice/plasmashell.service", IOFullUS: us} }
	t.Run("простой до случая, вне потолка 189 мс", func(t *testing.T) {
		details := []Step{rec(-3*time.Second, eval), rec(-2*time.Second, eval), rec(-time.Second, eval), rec(0, eval),
			rec(time.Second, ui(63_000)), rec(2*time.Second, ui(63_000)), rec(3*time.Second, ui(63_000))}
		if capped, outside, _ := cw(details, base, base.Add(2500*time.Millisecond), time.Second, 2500, 100); capped || outside != 157 {
			t.Fatalf("простой под потолком до случая — не его: %v %d", capped, outside)
		}
	})
	t.Run("запись за секунду до начала", func(t *testing.T) {
		big := eval
		big.IOFullUS = 900_000
		details := []Step{rec(0, big), rec(time.Second, big), rec(2*time.Second, ui(150_000))}
		if capped, outside, _ := cw(details, base.Add(1100*time.Millisecond), base.Add(1700*time.Millisecond), time.Second, 600, 50); capped || outside != 90 {
			t.Fatalf("запись (0, 1 с] со случаем 1,1–1,7 с не пересекается: %v %d", capped, outside)
		}
	})
}

// Запоздавшая детальная запись (проверка 13:59): проход стоял 3 с, одна запись — дельта за 3 с. 90 МБ за 3 с при
// потолке 80M — 30 МБ/с, в потолок не упирались, простой — машины.
func TestCappedWaitLateRecordInterval(t *testing.T) {
	base := time.Date(2026, 11, 8, 14, 0, 0, 0, time.Local)
	evals := Cap{Path: "/agents.slice/agents-evals.slice", RBps: 80_000_000}
	details := []Step{{At: base.Add(time.Second)}, {At: base.Add(4 * time.Second), Cgroups: []Cgroup{
		{Path: evals.Path + "/a.scope", ReadBytes: 90_000_000, IOFullUS: 2_500_000, Cap: evals},
		{Path: "/app.slice/app-com.google.Chrome-1.scope", IOFullUS: 150_000}}}}
	if capped, outside, _ := cw(details, base.Add(1200*time.Millisecond), base.Add(4*time.Second), time.Second, 2800, 900); capped || outside != 2333 {
		t.Fatalf("скорость — за весь промежуток записи: %v %d", capped, outside)
	}
	// те же 3 с по секунде — по 80 МБ: упор, случай под потолком
	details = []Step{{At: base.Add(time.Second)}}
	for i := 2; i <= 4; i++ {
		details = append(details, Step{At: base.Add(time.Duration(i) * time.Second), Cgroups: []Cgroup{
			{Path: evals.Path + "/a.scope", ReadBytes: 80_000_000, IOFullUS: 800_000, Cap: evals}}})
	}
	if capped, _, _ := cw(details, base.Add(1200*time.Millisecond), base.Add(4*time.Second), time.Second, 2800, 900); !capped {
		t.Fatal("посекундные записи с упором — под потолком")
	}
}

// Файлы io.max ушедших листьев закрываются при следующем обходе дерева.
func TestCgroupReaderDropsCapFilesOfGoneLeaves(t *testing.T) {
	root := t.TempDir()
	leaves := []string{"u.slice/a.scope", "u.slice/b.scope", "u.slice/c.scope"}
	full := func(rel string, us int) {
		writeFile(t, filepath.Join(root, rel, "io.pressure"), "some avg10=0 total=0\nfull avg10=0 total="+strconv.Itoa(us)+"\n")
	}
	for _, rel := range leaves {
		writeFile(t, filepath.Join(root, rel, "cgroup.procs"), "1\n")
		writeFile(t, filepath.Join(root, rel, "memory.pressure"), "some avg10=0 total=0\nfull avg10=0 total=0\n")
		writeFile(t, filepath.Join(root, rel, "io.max"), "259:0 rbps=30000000 wbps=max riops=max wiops=max\n")
		full(rel, 0)
	}
	r := newCgroupReader(root, []string{"259:0"})
	defer r.close()
	now := time.Now()
	if _, err := r.read(&Step{At: now}); err != nil {
		t.Fatal(err)
	}
	for _, rel := range leaves {
		full(rel, 1000)
	}
	if _, err := r.read(&Step{At: now.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if len(r.capFiles) != 3 {
		t.Fatalf("открыты io.max листьев: %v", r.capFiles)
	}
	time.Sleep(10 * time.Millisecond) // время папки u.slice сменится
	for _, rel := range leaves[1:] {
		if err := os.RemoveAll(filepath.Join(root, rel)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.read(&Step{At: now.Add(2 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	if len(r.capFiles) != 1 || r.capFiles[leaves[0]] == nil {
		t.Fatalf("после ухода двух листьев: %v", r.capFiles)
	}
}

// cw — признание случая тремя значениями: пометка, наибольший простой вне потолка, простой по потолкам.
func cw(details []Step, start, end time.Time, detail time.Duration, durMS, busyMS int64) (bool, int64, []Ranked) {
	v := cappedWait(details, start, end, detail, durMS, busyMS)
	return v.Capped, v.OutsideMS, v.Who
}
