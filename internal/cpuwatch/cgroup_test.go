package cpuwatch

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func cpuStat(t *testing.T, root, rel string, n int64) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(p, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "cpu.stat"), fmt.Appendf(nil, "usage_usec %d\nuser_usec 0\nsystem_usec 0\n", n), 0600); err != nil {
		t.Fatal(err)
	}
}

func fakeCPUOwner(t *Task) Owner {
	switch filepath.Base(t.Cgroup) {
	case "worker-live.service":
		return Owner{Master: "live-master", Kind: KindWorker, Label: "live"}
	case "worker-x.service":
		return Owner{Master: "worker-master", Kind: KindWorker, Label: "x"}
	case "agent-a.scope":
		return Owner{Master: "alpha-master", Kind: KindSession}
	default:
		return Owner{Kind: KindOther, Label: filepath.Base(t.Cgroup)}
	}
}

func TestCgroupCPUShortWorkerStandAndThreshold(t *testing.T) {
	root := t.TempDir()
	cpuStat(t, root, "", 0)
	cpuStat(t, root, "app.slice", 0)
	cpuStat(t, root, "app.slice/worker-x.service", 0)
	cpuStat(t, root, "agents.slice", 0)
	cpuStat(t, root, "agents.slice/agent-a.scope", 0)
	var c CgroupCPU
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if loads, ready, err := c.Sample(root, base, fakeCPUOwner, nil); err != nil || ready || len(loads) != 0 {
		t.Fatalf("первый замер — база: %v %v %v", loads, ready, err)
	}
	// 200 коротких процессов/с с суммарными 400 CPU-секундами и четыре долгих процесса:
	// снимок даёт бойцу только 4 потока, но фактически это 6.67 ядра из 22.
	cpuStat(t, root, "", 400_000_000)
	cpuStat(t, root, "app.slice", 400_000_000)
	cpuStat(t, root, "app.slice/worker-x.service", 400_000_000)
	cpuStat(t, root, "agents.slice", 0)
	cpuStat(t, root, "agents.slice/agent-a.scope", 0)
	loads, ready, err := c.Sample(root, base.Add(time.Minute), fakeCPUOwner, nil)
	if err != nil || !ready {
		t.Fatalf("замер: %v %v", ready, err)
	}
	v := &Verdict{Threads: 22, UseCPU: true, Loads: mergeCPULoads(Owners([]Task{{N: 4, Cgroup: "/worker-x.service"}}, fakeCPUOwner), loads)}
	got := v.Recipients(.25)
	if len(got) != 1 || got[0].Name != "worker-master" || got[0].CPU < 5.5 {
		t.Fatalf("получатель должен пройти по 6.67 ядра, даже если старый снимок даёт 4: %+v loads=%+v", got, loads)
	}
}

func TestCgroupCPUDeletedScopeRemainderAndNoNestedDoubleCount(t *testing.T) {
	root := t.TempDir()
	cpuStat(t, root, "", 0)
	cpuStat(t, root, "agents.slice", 0)
	cpuStat(t, root, "agents.slice/agent-a.scope", 0)
	cpuStat(t, root, "agents.slice/graft-heavy-1.scope", 0)
	var c CgroupCPU
	at := time.Now()
	if _, _, e := c.Sample(root, at, fakeCPUOwner, nil); e != nil {
		t.Fatal(e)
	}
	// За минуту исчезнувший scope дал 120 CPU-секунд, соседняя сессия 60.
	// Родитель slice включает обе суммы, видимая сессия вычитается один раз.
	cpuStat(t, root, "", 180_000_000)
	cpuStat(t, root, "agents.slice", 180_000_000)
	cpuStat(t, root, "agents.slice/agent-a.scope", 60_000_000)
	if err := os.RemoveAll(filepath.Join(root, "agents.slice", "graft-heavy-1.scope")); err != nil {
		t.Fatal(err)
	}
	loads, _, e := c.Sample(root, at.Add(time.Minute), fakeCPUOwner, nil)
	if e != nil {
		t.Fatal(e)
	}
	var alpha, unknown float64
	unknownLabel := ""
	for _, l := range loads {
		if l.Owner.Master == "alpha-master" {
			alpha += l.CPU
		}
		if l.Owner.Master == "" {
			unknown += l.CPU
			unknownLabel = l.Owner.Label
		}
	}
	if alpha != 1 || unknown != 2 || !strings.Contains(unknownLabel, "хозяин неизвестен") {
		t.Fatalf("ожидали 1 ядро сессии + 2 неизвестного остатка без двойного счёта: %+v", loads)
	}
}

func TestCgroupEmptySliceRemainderHasUnknownOwnerLabel(t *testing.T) {
	root := t.TempDir()
	cpuStat(t, root, "", 0)
	cpuStat(t, root, "empty.slice", 0)
	var c CgroupCPU
	at := time.Now()
	if _, ready, err := c.Sample(root, at, fakeCPUOwner, nil); err != nil || ready {
		t.Fatalf("baseline: ready=%v err=%v", ready, err)
	}
	cpuStat(t, root, "", 60_000_000)
	cpuStat(t, root, "empty.slice", 60_000_000)
	loads, ready, err := c.Sample(root, at.Add(time.Minute), fakeCPUOwner, nil)
	if err != nil || !ready || len(loads) != 1 || loads[0].CPU != 1 || loads[0].Owner.Master != "" || !strings.Contains(loads[0].Owner.Label, "хозяин неизвестен") {
		t.Fatalf("остаток пустого slice должен быть помечен неизвестным: ready=%v loads=%+v err=%v", ready, loads, err)
	}
}

func TestCgroupCPUActualIntervalAndRestartBaseline(t *testing.T) {
	root := t.TempDir()
	cpuStat(t, root, "", 1_000_000)
	cpuStat(t, root, "agent-a.scope", 1_000_000)
	var c CgroupCPU
	at := time.Now()
	if _, ready, e := c.Sample(root, at, fakeCPUOwner, nil); e != nil || ready {
		t.Fatalf("первая база: ready=%v err=%v", ready, e)
	}
	cpuStat(t, root, "", 901_000_000)
	cpuStat(t, root, "agent-a.scope", 901_000_000)
	loads, _, e := c.Sample(root, at.Add(5*time.Minute), fakeCPUOwner, nil)
	if e != nil {
		t.Fatal(e)
	}
	if len(loads) != 1 || loads[0].CPU < 2.99 || loads[0].CPU > 3.01 {
		t.Fatalf("900 CPU-секунд за 300 с = 3 ядра, got %+v", loads)
	}
	var restarted CgroupCPU
	loads, ready, e := restarted.Sample(root, at.Add(6*time.Minute), fakeCPUOwner, nil)
	if e != nil || ready || len(loads) != 0 {
		t.Fatalf("после рестарта первая минута только база: ready=%v loads=%+v err=%v", ready, loads, e)
	}
	if got := (&Verdict{Threads: 22, UseCPU: true, Loads: loads}).Recipients(.25); len(got) != 0 {
		t.Fatalf("первая база после старта не должна выбирать получателя по снимку или накопленному CPU: %+v", got)
	}
}

func TestCgroupCPUDetailAttributesVanishedShortLeaf(t *testing.T) {
	mount := t.TempDir()
	root := filepath.Join(mount, "user.slice", "user-1000.slice", "user@1000.service")
	cpuStat(t, root, "", 0)
	cpuStat(t, root, "agents.slice", 0)
	c := CgroupCPU{MountRoot: mount}
	at := time.Now()
	if _, _, e := c.Sample(root, at, fakeCPUOwner, nil); e != nil {
		t.Fatal(e)
	}
	// Короткий scope успел исчезнуть до обхода CPU дерева; детальный iowatch сохранил его cpu.stat delta.
	cpuStat(t, root, "", 120_000_000)
	cpuStat(t, root, "agents.slice", 120_000_000)
	loads, _, e := c.Sample(root, at.Add(time.Minute), fakeCPUOwner, map[string]int64{"/user.slice/user-1000.slice/user@1000.service/agents.slice/worker-x.service": 120_000_000})
	if e != nil {
		t.Fatal(e)
	}
	if len(loads) != 1 || loads[0].Owner.Master != "worker-master" || loads[0].CPU != 2 {
		t.Fatalf("короткий лист должен получить 2 ядра из записи iowatch: %+v", loads)
	}
}

func TestCgroupOwnerPathUsesMountRelativeCgroupFormat(t *testing.T) {
	mount := t.TempDir()
	root := filepath.Join(mount, "user.slice", "user-1000.slice", "user@1000.service")
	cpuStat(t, root, "", 0)
	cpuStat(t, root, "agents.slice", 0)
	cpuStat(t, root, "agents.slice/agent-a.scope", 0)
	var got string
	owner := func(t *Task) Owner { got = t.Cgroup; return Owner{Master: "alpha-master", Kind: KindSession} }
	var c CgroupCPU
	c.MountRoot = mount
	at := time.Now()
	if _, _, e := c.Sample(root, at, owner, nil); e != nil {
		t.Fatal(e)
	}
	cpuStat(t, root, "", 1_000_000)
	cpuStat(t, root, "agents.slice", 1_000_000)
	cpuStat(t, root, "agents.slice/agent-a.scope", 1_000_000)
	if _, _, e := c.Sample(root, at.Add(time.Second), owner, nil); e != nil {
		t.Fatal(e)
	}
	want := "/user.slice/user-1000.slice/user@1000.service/agents.slice/agent-a.scope"
	if got != want {
		t.Fatalf("владелец получил filesystem path %q, хотели mount-relative cgroup %q", got, want)
	}
}

func TestCgroupDisappearingChildReadIsSkipped(t *testing.T) {
	root := t.TempDir()
	cpuStat(t, root, "", 0)
	cpuStat(t, root, "agents.slice", 0)
	cpuStat(t, root, "agents.slice/agent-a.scope", 0)
	var c CgroupCPU
	at := time.Now()
	if _, _, e := c.Sample(root, at, fakeCPUOwner, nil); e != nil {
		t.Fatal(e)
	}
	cpuStat(t, root, "", 120_000_000)
	cpuStat(t, root, "agents.slice", 120_000_000)
	if err := os.RemoveAll(filepath.Join(root, "agents.slice", "agent-a.scope")); err != nil {
		t.Fatal(err)
	}
	loads, ready, e := c.Sample(root, at.Add(time.Minute), fakeCPUOwner, nil)
	if e != nil || !ready {
		t.Fatalf("исчезнувшая cgroup не должна валить замер: ready=%v err=%v", ready, e)
	}
	if len(loads) != 1 || loads[0].Owner.Master != "" || loads[0].CPU != 2 {
		t.Fatalf("CPU удалённого ребёнка — остаток неизвестного slice: %+v", loads)
	}
}

func TestCgroupDisappearingDuringReadIsSkippedAndRemainderUnknown(t *testing.T) {
	for _, disappearAt := range []string{"cpu.stat", "ReadDir"} {
		t.Run(disappearAt, func(t *testing.T) {
			root := t.TempDir()
			cpuStat(t, root, "", 0)
			cpuStat(t, root, "agents.slice", 0)
			cpuStat(t, root, "agents.slice/agent-a.scope", 0)
			var c CgroupCPU
			at := time.Now()
			if _, ready, err := c.Sample(root, at, fakeCPUOwner, nil); err != nil || ready {
				t.Fatalf("baseline: ready=%v err=%v", ready, err)
			}
			cpuStat(t, root, "", 120_000_000)
			cpuStat(t, root, "agents.slice", 120_000_000)
			child := filepath.Join(root, "agents.slice", "agent-a.scope")
			readDir := os.ReadDir
			readFile := os.ReadFile
			injected := false
			if disappearAt == "ReadDir" {
				readDir = func(path string) ([]os.DirEntry, error) {
					if path == child && !injected {
						injected = true
						return nil, &os.PathError{Op: "readdir", Path: path, Err: fs.ErrNotExist}
					}
					return os.ReadDir(path)
				}
			} else {
				readFile = func(path string) ([]byte, error) {
					if path == filepath.Join(child, "cpu.stat") && !injected {
						injected = true
						return nil, &os.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
					}
					return os.ReadFile(path)
				}
			}
			loads, ready, err := c.sampleWith(root, at.Add(time.Minute), fakeCPUOwner, nil, readDir, readFile)
			if err != nil || !ready || !injected {
				t.Fatalf("узел исчез во время обхода: ready=%v injected=%v err=%v", ready, injected, err)
			}
			if len(loads) != 1 || loads[0].Owner.Master != "" || !strings.Contains(loads[0].Owner.Label, "хозяин неизвестен") || loads[0].CPU != 2 {
				t.Fatalf("доля исчезнувшего ребёнка остаётся неизвестным остатком slice: %+v", loads)
			}
		})
	}
}

func TestCgroupWalkRecognizesDisappearanceErrnos(t *testing.T) {
	for _, err := range []error{fs.ErrNotExist, syscall.ENODEV} {
		if !vanishedCgroup(err) {
			t.Errorf("%v must skip a cgroup that vanished during cpu.stat/ReadDir", err)
		}
	}
	if vanishedCgroup(errors.New("permission denied")) {
		t.Fatal("unrelated cgroup errors must still fail the sample")
	}
}

func TestCgroupDetailLiveLeafAndHiddenNeighborDoNotDoubleCount(t *testing.T) {
	mount := t.TempDir()
	root := filepath.Join(mount, "user.slice", "user-1000.slice", "user@1000.service")
	cpuStat(t, root, "", 0)
	cpuStat(t, root, "app.slice", 0)
	cpuStat(t, root, "app.slice/worker-live.service", 0)
	var c CgroupCPU
	c.MountRoot = mount
	at := time.Now()
	if _, _, e := c.Sample(root, at, fakeCPUOwner, nil); e != nil {
		t.Fatal(e)
	}
	// Live worker has 60 CPU seconds in cpu.stat; the vanished neighbor has no detail record.
	cpuStat(t, root, "", 120_000_000)
	cpuStat(t, root, "app.slice", 120_000_000)
	cpuStat(t, root, "app.slice/worker-live.service", 60_000_000)
	loads, _, e := c.Sample(root, at.Add(time.Minute), fakeCPUOwner, map[string]int64{"/user.slice/user-1000.slice/user@1000.service/app.slice/worker-live.service": 60_000_000})
	if e != nil {
		t.Fatal(e)
	}
	var worker, live, unknown float64
	for _, l := range loads {
		if l.Owner.Master == "worker-master" {
			worker += l.CPU
		}
		if l.Owner.Master == "live-master" {
			live += l.CPU
		}
		if l.Owner.Master == "" {
			unknown += l.CPU
		}
	}
	if worker != 0 || live != 1 || unknown != 1 {
		t.Fatalf("iowatch не должен повторно начислять CPU живому листу; исчезнувший сосед без detail остаётся неизвестным: %+v", loads)
	}
}

func TestProportionalCPUClampsAndDistributesDeterministically(t *testing.T) {
	a := Owner{Master: "a"}
	b := Owner{Master: "b"}
	req := map[Owner]int64{a: 60, b: 60}
	first := proportionalCPU(60, req)
	second := proportionalCPU(60, req)
	if first[a] != 30 || first[b] != 30 || first[a] != second[a] || first[b] != second[b] {
		t.Fatalf("60 usec остатка делятся поровну и стабильно: %v", first)
	}
	clamped := proportionalCPU(60, map[Owner]int64{a: 100})
	if clamped[a] != 60 {
		t.Fatalf("доля не превышает остаток cgroup: %v", clamped)
	}
}

func TestCPUDeltasIgnoreCounterResetAndCountRecreatedInode(t *testing.T) {
	values := map[string]int64{"/slice/leaf": 5}
	last := map[string]int64{"/slice/leaf": 100}
	same := map[string]uint64{"/slice/leaf": 7}
	if got := cpuDeltas(values, last, same, same); len(got) != 0 {
		t.Fatalf("сброс счётчика того же inode не даёт отрицательную дельту и не засчитывает накопленное: %v", got)
	}
	newInode := map[string]uint64{"/slice/leaf": 8}
	if got := cpuDeltas(values, last, newInode, same); got["/slice/leaf"] != 5 {
		t.Fatalf("пересозданная cgroup берёт новый usage целиком: %v", got)
	}
}

func TestPartsCPUOnlyDoesNotPrintEmptyCommList(t *testing.T) {
	got := parts([]Load{{Owner: Owner{Kind: KindWorker, Label: "job"}, CPU: 2.5}})
	if strings.Contains(got, "()") || !strings.Contains(got, "2.50 ядра CPU") {
		t.Fatalf("CPU-only detail has no empty process list: %q", got)
	}
}

func TestCPUSharesAggregateUnitsBeforeNotify(t *testing.T) {
	loads := []Load{
		{Owner: Owner{Master: "alpha-master", Kind: KindSession}, CPU: 3},
		{Owner: Owner{Master: "alpha-master", Kind: KindWorker, Label: "job"}, CPU: 3},
	}
	v := &Verdict{Threads: 22, UseCPU: true, Loads: loads}
	got := v.Recipients(.25)
	if len(got) != 1 || got[0].Name != "alpha-master" || got[0].CPU != 6 {
		t.Fatalf("сумма юнитов одного мастера — 6 ядер и проходит порог 5.5: %+v", got)
	}
}

func TestCgroupCPUNestedSlicesCountOnce(t *testing.T) {
	root := t.TempDir()
	cpuStat(t, root, "", 0)
	cpuStat(t, root, "agents.slice", 0)
	cpuStat(t, root, "agents.slice/agents-graft.slice", 0)
	cpuStat(t, root, "agents.slice/agents-graft.slice/graft-heavy-1.scope", 0)
	cpuStat(t, root, "agents.slice/agent-b.scope", 0)
	var c CgroupCPU
	at := time.Now()
	owner := func(t *Task) Owner {
		switch filepath.Base(t.Cgroup) {
		case "graft-heavy-1.scope":
			return Owner{Master: "alpha-master", Kind: KindSession}
		case "agent-b.scope":
			return Owner{Master: "beta-master", Kind: KindSession}
		default:
			return Owner{Kind: KindOther, Label: filepath.Base(t.Cgroup)}
		}
	}
	if _, _, e := c.Sample(root, at, owner, nil); e != nil {
		t.Fatal(e)
	}
	cpuStat(t, root, "", 900_000_000)
	cpuStat(t, root, "agents.slice", 900_000_000)
	cpuStat(t, root, "agents.slice/agents-graft.slice", 600_000_000)
	cpuStat(t, root, "agents.slice/agents-graft.slice/graft-heavy-1.scope", 360_000_000)
	cpuStat(t, root, "agents.slice/agent-b.scope", 300_000_000)
	loads, _, e := c.Sample(root, at.Add(time.Minute), owner, nil)
	if e != nil {
		t.Fatal(e)
	}
	var total, known, unknown float64
	for _, l := range loads {
		total += l.CPU
		if l.Owner.Master == "alpha-master" {
			known += l.CPU
		}
		if l.Owner.Master == "" {
			unknown += l.CPU
		}
	}
	if total != 15 || known != 6 || unknown != 4 {
		t.Fatalf("900 CPU-секунд = 15 ядро-секунд/сек: всего 15, известно 6, остаток graft 4, сосед 5; got total %.2f known %.2f unknown %.2f (%+v)", total, known, unknown, loads)
	}
}
