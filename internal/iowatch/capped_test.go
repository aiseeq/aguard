package iowatch

import (
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
		capped, outside, who := cappedWait(capCase(base, 13, nil), base, end, time.Second, dur, busy)
		if !capped || outside != 26 || len(who) != 1 || who[0] != (Ranked{"run-p2016228-i69065841.scope", 9100}) {
			t.Fatalf("%v %d %+v", capped, outside, who)
		}
	})
	t.Run("простой под потолком до случая не его (сценарий 1)", func(t *testing.T) {
		details := append(capCase(base.Add(-5*time.Second), 4, nil), capCase(base, 2, func(_ int, s *Step) {
			s.Cgroups = []Cgroup{{Path: "/session.slice/plasmashell.service", IOFullUS: 600_000}}
		})...)
		if capped, outside, _ := cappedWait(details, base, base.Add(2*time.Second), time.Second, 2000, 100); capped || outside != 1200 {
			t.Fatalf("фриз после ожидания под потолком: %v %d", capped, outside)
		}
	})
	t.Run("потолок записи при простое чтения (сценарий 4)", func(t *testing.T) {
		details := capCase(base, 13, func(_ int, s *Step) { s.Cgroups[0].Cap = Cap{Path: s.Cgroups[0].Path, WBps: 30_000_000} })
		if capped, outside, _ := cappedWait(details, base, end, time.Second, dur, busy); capped || outside != 9100 {
			t.Fatalf("в потолок не упирались — простой машины: %v %d", capped, outside)
		}
	})
	t.Run("вне потолка ждали", func(t *testing.T) {
		details := capCase(base, 13, func(i int, s *Step) {
			if i == 5 {
				s.Cgroups[1].IOFullUS = 250_000
			}
		})
		if capped, outside, _ := cappedWait(details, base, end, time.Second, dur, busy); capped || outside != 274 {
			t.Fatalf("lead ждал 250 мс: %v %d", capped, outside)
		}
	})
	t.Run("последняя секунда чтения без упора (проба 08.11 13:43:06)", func(t *testing.T) {
		details := capCase(base, 2, func(i int, s *Step) {
			if i == 2 {
				s.Cgroups[0].ReadBytes, s.Cgroups[0].IOFullUS = 2_000_000, 40_000
			}
		})
		capped, outside, who := cappedWait(details, base.Add(700*time.Millisecond), base.Add(1700*time.Millisecond), time.Second, 999, 59)
		if !capped || outside != 40 || len(who) != 1 || who[0].Value != 700 {
			t.Fatalf("упор — по записи: %v %d %+v", capped, outside, who)
		}
	})
	t.Run("диск занят больше половины", func(t *testing.T) {
		if capped, _, _ := cappedWait(capCase(base, 13, nil), base, end, time.Second, dur, 6300); capped {
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
		capped, _, who := cappedWait(details, base, base.Add(5*time.Second), time.Second, 5000, 200)
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
	// детальные записи 1–4 с покрывают случай 0,9–3,9 с: вне потолка lead 4 × 3 мс
	if len(got) != 1 || !got[0].Capped || got[0].OutsideIdleMS != 12 || got[0].Culprits == nil || len(got[0].Culprits.CappedIdle) != 1 {
		t.Fatalf("случай под потолком: %+v", got)
	}
	s := Summarize(got, base, base.Add(time.Hour), 500*time.Millisecond)
	if s.Total != 0 || s.Capped != 1 || s.CappedIdleMS != got[0].IdleMS || len(s.CappedWho) != 1 || s.CappedWho[0].Name != "run-p1-i2.scope" || len(s.Cases) != 0 {
		t.Fatalf("сводка: %+v", s)
	}
}
