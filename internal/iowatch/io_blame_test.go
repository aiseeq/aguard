package iowatch

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

type ioBlameFixture struct {
	Class         string `json:"class"`
	Main          string `json:"main"`
	IOStatName    string `json:"io_stat_name"`
	IOStatPath    string `json:"io_stat_path"`
	DirtyName     string `json:"dirty_name"`
	DirtyPath     string `json:"dirty_path"`
	WaitingName   string `json:"waiting_name"`
	WaitingPath   string `json:"waiting_path"`
	DurMS         int64  `json:"dur_ms"`
	BusyMS        int64  `json:"busy_ms"`
	MainValue     int64  `json:"main_value"`
	MainRead      int64  `json:"main_read"`
	DeviceWrite   int64  `json:"device_wr"`
	KernelWrite   int64  `json:"kernel_wr"`
	DirtyValue    int64  `json:"dirty_value"`
	WaitingIdleMS int64  `json:"waiting_idle_ms"`
	IdleWindowMS  int64  `json:"idle_window_ms"`
	SecondDirty   int64  `json:"second_dirty"`
	OtherWrite    int64  `json:"other_write"`
	OtherOwn      int64  `json:"other_own"`
	MemFullMS     int64  `json:"mem_full_ms"`
	OutsideIdleMS int64  `json:"outside_idle_ms"`
	ProcName      string `json:"proc_name"`
}

func TestResolveMainFromJournalFixtures(t *testing.T) {
	f, err := os.Open("testdata/io-blame-cases.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var cases []Case
	var waitingCount int
	s := bufio.NewScanner(f)
	for s.Scan() {
		var in ioBlameFixture
		if err := json.Unmarshal(s.Bytes(), &in); err != nil {
			t.Fatal(err)
		}
		c := Case{DurMS: in.DurMS, BusyMS: in.BusyMS}
		c.MemFullMS = in.MemFullMS
		c.Culprits = &Culprit{Main: in.Main, MainValue: in.MainValue, MainRead: in.MainRead,
			DeviceWrite: in.DeviceWrite, KernelWrite: in.KernelWrite, IdleWindowMS: in.IdleWindowMS, ProcessDetails: map[string]ProcessDetails{},
			CgroupPaths: map[string]string{}}
		sum := Culprits{CgroupDirty: map[string]int64{}, CgroupIdle: map[string]int64{}, cgroupPaths: map[string]string{},
			cgroupTotal: map[string]int64{}, cgroupWrite: map[string]int64{}, cgroupRead: map[string]int64{},
			procCgroup: map[string]string{}, procIO: map[string]int64{}, ProcessDetails: map[string]ProcessDetails{}}
		if in.Class == "writeback" || in.Class == "weak-writeback" {
			sum.CgroupDirty[in.DirtyName] = in.DirtyValue
			if in.SecondDirty > 0 {
				sum.CgroupDirty["second"] = in.SecondDirty
			}
			if in.OtherWrite > 0 {
				c.Culprits.CgroupWrite = []Ranked{{Name: "competitor", Value: in.OtherWrite}}
				c.Culprits.CgroupOwn = map[string]int64{"competitor": in.OtherOwn}
			}
			sum.cgroupPaths[in.DirtyName] = in.DirtyPath
			sum.cgroupTotal[in.DirtyPath] = in.DirtyValue
			sum.cgroupWrite[in.DirtyPath] = in.DirtyValue
		} else if in.Class == "io-stat" {
			c.Culprits.CgroupWrite = []Ranked{{Name: in.IOStatName, Value: in.OtherWrite}}
			sum.cgroupPaths[in.IOStatName] = in.IOStatPath
		} else {
			sum.CgroupIdle[in.WaitingName] = in.WaitingIdleMS
			sum.cgroupPaths[in.WaitingName] = in.WaitingPath
		}
		resolveCaseMain(&c, &sum)
		switch in.Class {
		case "writeback":
			if !c.Culprits.KernelOver() || CgroupName(c.Culprits.Main) != in.DirtyName || !strings.Contains(c.Culprits.Main, "/") {
				t.Errorf("writeback %s: main=%q kernel_over=%v", in.Main, c.Culprits.Main, c.Culprits.KernelOver())
			}
		case "wait":
			waitingCount++
			if !c.Waiting || CgroupName(c.Culprits.Main) != in.WaitingName || c.Culprits.Main == in.Main {
				t.Errorf("wait %s: waiting=%v main=%q", in.Main, c.Waiting, c.Culprits.Main)
			}
		case "weak-writeback":
			if c.Culprits.Main != "" {
				t.Errorf("слабый грязный владелец должен остаться фоном ядра, main=%q", c.Culprits.Main)
			}
		case "io-stat":
			if c.Culprits.Main != in.IOStatPath || CgroupName(c.Culprits.Main) != in.IOStatName {
				t.Errorf("сильный писатель io.stat не выбран: main=%q", c.Culprits.Main)
			}
		default:
			t.Fatalf("unknown fixture class %q", in.Class)
		}
		cases = append(cases, c)
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	if waitingCount != 3 {
		t.Fatalf("ожиданий из фикстур %d, want 3", waitingCount)
	}
	summary := Summarize(cases, time.Time{}, time.Now().Add(time.Hour), 2*time.Second)
	var rankedCases int
	for _, item := range summary.Main {
		rankedCases += item.Cases
	}
	if summary.Total != 4 || summary.Waiting != 3 || len(summary.WaitingWho) != 1 || summary.WaitingWho[0].Name != "adream.service" ||
		len(summary.Main) != 2 || summary.Main[0].Name != "dream-master" || summary.Main[0].Cases != 3 || summary.Main[1].Name != "docker.service" || summary.Main[1].Cases != 1 || rankedCases != summary.Total {
		t.Fatalf("атрибуция/ожидание: total=%d main=%+v waiting=%d who=%+v", summary.Total, summary.Main, summary.Waiting, summary.WaitingWho)
	}
}

func TestReattributeStoredCasesFromJournalFixtures(t *testing.T) {
	f, err := os.Open("testdata/io-blame-cases.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var cases []Case
	wantMainProc := map[int64]string{}
	s := bufio.NewScanner(f)
	for s.Scan() {
		var in ioBlameFixture
		if err := json.Unmarshal(s.Bytes(), &in); err != nil {
			t.Fatal(err)
		}
		cul := &Culprit{Main: in.Main, MainValue: in.MainValue, MainRead: in.MainRead,
			DeviceWrite: in.DeviceWrite, KernelWrite: in.KernelWrite, IdleWindowMS: in.IdleWindowMS, CgroupPaths: map[string]string{}}
		if in.DirtyName != "" && in.DirtyPath != "" {
			cul.CgroupPaths[in.DirtyName] = in.DirtyPath
		}
		if in.WaitingName != "" && in.WaitingPath != "" {
			cul.CgroupPaths[in.WaitingName] = in.WaitingPath
		}
		if in.IOStatName != "" && in.IOStatPath != "" {
			cul.CgroupPaths[in.IOStatName] = in.IOStatPath
		}
		if in.ProcName != "" {
			wantMainProc[in.DurMS] = in.ProcName
			cul.ProcWrite = []Ranked{{Name: in.ProcName, Value: in.DirtyValue}}
			cul.ProcessDetails = map[string]ProcessDetails{in.ProcName: {Cmdline: "cp --reflink=always", CWD: "/home/aisee/worktrees/dream/project"}}
		}
		if in.DirtyName != "" {
			cul.CgroupDirty = []Ranked{{Name: in.DirtyName, Value: in.DirtyValue}}
		}
		if in.WaitingName != "" {
			cul.CgroupIdle = []Ranked{{Name: in.WaitingName, Value: in.WaitingIdleMS}}
		}
		if in.Class == "writeback" || in.Class == "weak-writeback" || in.Class == "io-stat" {
			competitor := "competitor"
			if in.IOStatName != "" {
				competitor = in.IOStatName
			}
			cul.CgroupWrite = []Ranked{{Name: competitor, Value: in.OtherWrite}}
			cul.CgroupOwn = map[string]int64{competitor: in.OtherOwn}
			if in.SecondDirty > 0 {
				cul.CgroupDirty = append(cul.CgroupDirty, Ranked{Name: "second", Value: in.SecondDirty})
			}
		}
		cases = append(cases, Case{DurMS: in.DurMS, BusyMS: in.BusyMS, MemFullMS: in.MemFullMS, OutsideIdleMS: in.OutsideIdleMS, Culprits: cul})
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	ReattributeCases(cases)
	for i, c := range cases {
		switch {
		case c.DurMS == 2799 || c.DurMS == 3199 || c.DurMS == 3499:
			if CgroupName(c.Culprits.Main) != "dream-master" || c.Culprits.Main != map[int64]string{2799: "/user.slice/user-1000.slice/user@1000.service/agents.slice/agent-dream-master-683403-r1.scope", 3199: "/user.slice/user-1000.slice/user@1000.service/agents.slice/agent-dream-master-683403-r1.scope", 3499: "/user.slice/user-1000.slice/user@1000.service/agents.slice/agent-dream-master-683403-r1.scope"}[c.DurMS] {
				t.Errorf("исторический writeback %d: main=%q", i, c.Culprits.Main)
			}
			if c.Culprits.MainProc != wantMainProc[c.DurMS] {
				t.Errorf("исторический MainProc=%q, want %q", c.Culprits.MainProc, wantMainProc[c.DurMS])
			}
		case c.DurMS == 3999 || c.DurMS == 6800 || c.DurMS == 20500:
			if !c.Waiting || CgroupName(c.Culprits.Main) != "adream.service" {
				t.Errorf("историческое ожидание %d: waiting=%v main=%q", i, c.Waiting, c.Culprits.Main)
			}
		case c.DurMS == 3000:
			if CgroupName(c.Culprits.Main) != "docker.service" || c.Culprits.Main != "/system.slice/docker.service" {
				t.Errorf("исторический сильный io.stat писатель: main=%q", c.Culprits.Main)
			}
		default:
			if c.Culprits.Main != "" {
				t.Errorf("слабый исторический writeback не фон ядра: main=%q", c.Culprits.Main)
			}
		}
	}
}

func TestStrongIOStatWriterWinsOverWeakDirtyOwner(t *testing.T) {
	const mb = int64(1 << 20)
	c := Case{DurMS: 3000, BusyMS: 2800}
	c.Culprits = &Culprit{Main: "/user.slice/agent-lead.scope", MainValue: 100 * mb, KernelWrite: 120 * mb}
	sum := Culprits{CgroupDirty: map[string]int64{"com.google.Chrome": 3_500_000, "aguard.service": 120_000},
		cgroupPaths: map[string]string{"com.google.Chrome": "/user.slice/user-1000.slice/user@1000.service/app.slice/com.google.Chrome"},
		cgroupTotal: map[string]int64{}, cgroupWrite: map[string]int64{}, cgroupRead: map[string]int64{},
		procCgroup: map[string]string{}, procIO: map[string]int64{}, ProcessDetails: map[string]ProcessDetails{}}
	c.Culprits.CgroupWrite = []Ranked{{Name: "docker.service", Value: 120 * mb}}
	resolveCaseMain(&c, &sum)
	if c.Culprits.Main != "docker.service" {
		t.Fatalf("слабые 3,5 МБ Chrome не должны скрыть писателя io.stat, main=%q", c.Culprits.Main)
	}
}

func TestStrongIOStatWriterReplacesWeakDirtyOwner(t *testing.T) {
	const mb = int64(1 << 20)
	c := Case{DurMS: 3000, BusyMS: 2800, SwapIn: 20_000}
	c.Culprits = &Culprit{Main: "/user.slice/user-1000.slice/user@1000.service/app.slice/app-com.google.Chrome-3212381.scope",
		MainValue: 3 * mb, KernelWrite: 120 * mb, CgroupDirty: []Ranked{{Name: "com.google.Chrome", Value: 3_500_000}},
		CgroupWrite: []Ranked{{Name: "docker.service", Value: 120 * mb}, {Name: "com.google.Chrome", Value: 2 * mb}},
		CgroupRead:  []Ranked{{Name: "docker.service", Value: 50 * mb}},
		CgroupOwn:   map[string]int64{"com.google.Chrome": 2 * mb},
		CgroupPaths: map[string]string{"docker.service": "/system.slice/docker.service"}}
	sum := Culprits{CgroupDirty: map[string]int64{"com.google.Chrome": 3_500_000}, cgroupPaths: map[string]string{"docker.service": "/system.slice/docker.service"},
		cgroupTotal: map[string]int64{"/system.slice/docker.service": 50 * mb}, cgroupWrite: map[string]int64{},
		cgroupRead: map[string]int64{"/system.slice/docker.service": 50 * mb}, procCgroup: map[string]string{}, procIO: map[string]int64{}}
	resolveCaseMain(&c, &sum)
	if c.Culprits.Main != "/system.slice/docker.service" || c.Culprits.MainWrite != 120*mb || c.Culprits.MainRead != 50*mb {
		t.Fatalf("писатель по io.stat не выбран с его read/write: main=%q write=%d read=%d", c.Culprits.Main, c.Culprits.MainWrite, c.Culprits.MainRead)
	}
	if c.Culprits.ReadMostly() || c.PagingVictim() {
		t.Fatalf("писатель io.stat ошибочно выглядит читающей жертвой: mostly=%v victim=%v", c.Culprits.ReadMostly(), c.PagingVictim())
	}
}

func TestIOStatWriterMustReachDirtyTop(t *testing.T) {
	const mb = int64(1 << 20)
	c := Case{DurMS: 3000, BusyMS: 2800, Culprits: &Culprit{Main: "/user.slice/agent-lead.scope", MainValue: 100 * mb, KernelWrite: 120 * mb,
		CgroupWrite: []Ranked{{Name: "io-owner", Value: 40 * mb}, {Name: "other", Value: 15 * mb}}}}
	sum := Culprits{CgroupDirty: map[string]int64{"dirty-owner": 50 * mb, "second": 30 * mb}, cgroupPaths: map[string]string{},
		cgroupTotal: map[string]int64{}, cgroupWrite: map[string]int64{}, cgroupRead: map[string]int64{}, procCgroup: map[string]string{}, procIO: map[string]int64{}}
	resolveCaseMain(&c, &sum)
	if c.Culprits.Main != "" {
		t.Fatalf("io.stat writer 40 MiB при dirty top 50 MiB принят: main=%q", c.Culprits.Main)
	}
}

func TestWritebackDirtyThresholdsAreIndependent(t *testing.T) {
	const mb = int64(1 << 20)
	for _, tc := range []struct {
		name       string
		dirty      int64
		second     int64
		ownWrite   int64
		otherWrite int64
	}{
		{name: "absolute volume", dirty: 19 * mb, second: 2 * mb, ownWrite: 19 * mb, otherWrite: 1 * mb},
		{name: "two to one margin", dirty: 30 * mb, second: 16 * mb, ownWrite: 30 * mb, otherWrite: 20 * mb},
		{name: "io.stat competitor", dirty: 50 * mb, second: 20 * mb, ownWrite: 40 * mb, otherWrite: 51 * mb},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Case{DurMS: 3000, BusyMS: 2800}
			c.Culprits = &Culprit{Main: "/user.slice/lead.scope", MainValue: 100 * mb, KernelWrite: 120 * mb,
				CgroupWrite: []Ranked{{Name: "owner", Value: tc.ownWrite}, {Name: "other", Value: tc.otherWrite}}}
			sum := Culprits{CgroupDirty: map[string]int64{"owner": tc.dirty, "second": tc.second}, cgroupPaths: map[string]string{},
				cgroupTotal: map[string]int64{}, cgroupWrite: map[string]int64{}, cgroupRead: map[string]int64{}, procCgroup: map[string]string{}, procIO: map[string]int64{}}
			resolveCaseMain(&c, &sum)
			if c.Culprits.Main != "" {
				t.Fatalf("тест должен отсечь только порог %q; main=%q", tc.name, c.Culprits.Main)
			}
		})
	}
}

func TestResolveMainClearsStaleWaiting(t *testing.T) {
	c := Case{DurMS: 3000, BusyMS: 2500, Waiting: true, Culprits: &Culprit{Main: "worker.service"}}
	sum := Culprits{CgroupDirty: map[string]int64{}, CgroupIdle: map[string]int64{}, cgroupPaths: map[string]string{},
		cgroupTotal: map[string]int64{}, cgroupWrite: map[string]int64{}, cgroupRead: map[string]int64{}, procCgroup: map[string]string{}, procIO: map[string]int64{}}
	resolveCaseMain(&c, &sum)
	if c.Waiting {
		t.Fatal("старый Waiting сохранился после пересчёта случая без признаков ожидания")
	}
}

func TestReattributeUsesOnlyStoredCgroupPaths(t *testing.T) {
	const mb = int64(1 << 20)
	oldMain := "/user.slice/user-1000.slice/user@1000.service/agents.slice/agent-lead-42.scope"
	cases := []Case{
		{DurMS: 3000, Culprits: &Culprit{Main: oldMain,
			MainValue: 100 * mb, KernelWrite: 120 * mb, CgroupDirty: []Ranked{{Name: "dream-master", Value: 30 * mb}},
			CgroupWrite: []Ranked{{Name: "docker.service", Value: 120 * mb}}, CgroupPaths: map[string]string{"docker.service": "/system.slice/docker.service"}}},
		{DurMS: 3000, Culprits: &Culprit{Main: oldMain,
			MainValue: 100 * mb, KernelWrite: 120 * mb, CgroupDirty: []Ranked{{Name: "dream-master", Value: 30 * mb}},
			CgroupWrite: []Ranked{{Name: "docker.service", Value: 120 * mb}}}},
	}
	ReattributeCases(cases)
	if got := cases[0].Culprits.Main; got != "/system.slice/docker.service" {
		t.Errorf("сохранённый путь не использован: main=%q", got)
	}
	if got := cases[1].Culprits.Main; got != "docker.service" {
		t.Errorf("для старой записи выдуман путь вместо имени: main=%q", got)
	}
}

func TestSnapshotDeletionStaysDistinctFromIOStatOwner(t *testing.T) {
	const mb = int64(1 << 20)
	c := Case{DurMS: 3000, BusyMS: 2800}
	c.Culprits = &Culprit{Main: "/user.slice/lead.scope", MainValue: 100 * mb, KernelWrite: 120 * mb, CleanerMS: cleanerBusyMS,
		CgroupDirty: []Ranked{{Name: "weak", Value: 2 * mb}}, CgroupWrite: []Ranked{{Name: "docker.service", Value: 120 * mb}}}
	sum := Culprits{CgroupDirty: map[string]int64{"weak": 2 * mb}, cgroupPaths: map[string]string{},
		cgroupTotal: map[string]int64{}, cgroupWrite: map[string]int64{}, cgroupRead: map[string]int64{}, procCgroup: map[string]string{}, procIO: map[string]int64{}}
	resolveCaseMain(&c, &sum)
	if c.Culprits.Main != "" || !c.Culprits.SnapshotDeletion() {
		t.Fatalf("снапшотное удаление переназначено писателю: main=%q snapshot=%v", c.Culprits.Main, c.Culprits.SnapshotDeletion())
	}
	s := Summarize([]Case{c}, time.Time{}, time.Now().Add(time.Hour), time.Second)
	if len(s.Main) != 1 || s.Main[0].Name != "btrfs-cleaner" {
		t.Fatalf("снапшот выпал из рейтинга: %+v", s.Main)
	}
}

func TestFinishResolvesKernelWritebackFromLiveDetails(t *testing.T) {
	r := NewRecorder(Default())
	at := time.Now()
	lead := "/user.slice/user-1000.slice/user@1000.service/agents.slice/agent-lead-1.scope"
	dirty := "/user.slice/user-1000.slice/user@1000.service/agents.slice/agent-dream-master-1.scope"
	other := "/system.slice/docker.service"
	r.ring.Add(Record{Kind: KindStep, Step: Step{At: at, WriteBytes: 200 << 20}})
	r.ring.Add(Record{Kind: KindDetail, Step: Step{At: at,
		Processes: []Process{{PID: 10, Start: 1, Comm: "reader", ReadBytes: 100 << 20, Cgroup: lead},
			{PID: 11, Start: 1, Comm: "writer", WriteBytes: 30 << 20, Cgroup: dirty}},
		Cgroups: []Cgroup{{Path: lead, ReadBytes: 100 << 20}, {Path: dirty, WriteBytes: 30 << 20}, {Path: other, WriteBytes: 1 << 20}}}})
	c := Case{Start: at, End: at, DurMS: 1000, BusyMS: 900}
	r.finish(&c)
	if c.Culprits == nil || CgroupName(c.Culprits.Main) != "dream-master" || c.Culprits.MainProc != "writer(11)" {
		t.Fatalf("finish не переназначил writeback и MainProc: %+v", c.Culprits)
	}
}

func TestWaitingRequiresSameWindowLowMemoryAndUncappedCase(t *testing.T) {
	for _, tc := range []struct {
		name    string
		memFull int64
		capped  bool
		idle    int64
		waiting bool
	}{
		{name: "same-window wait", idle: 9000, waiting: true},
		{name: "memory pressure", memFull: 700, idle: 9000},
		{name: "capped", capped: true, idle: 9000},
		{name: "idle only in case-sized comparison", idle: 5500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Case{DurMS: 2200, BusyMS: 100, MemFullMS: tc.memFull, Capped: tc.capped}
			c.Culprits = &Culprit{Main: "/user.slice/agent-lead.scope", MainValue: 1 << 20, IdleWindowMS: 10_000}
			sum := Culprits{CgroupIdle: map[string]int64{"adream.service": tc.idle},
				cgroupPaths: map[string]string{"adream.service": "/system.slice/adream.service"}, cgroupTotal: map[string]int64{},
				cgroupWrite: map[string]int64{}, cgroupRead: map[string]int64{}, procCgroup: map[string]string{}, procIO: map[string]int64{},
				ProcessDetails: map[string]ProcessDetails{}}
			resolveCaseMain(&c, &sum)
			if c.Waiting != tc.waiting {
				t.Fatalf("waiting=%v, want %v (main %q)", c.Waiting, tc.waiting, c.Culprits.Main)
			}
		})
	}
}

func TestCgroupPathUsesLargestValueForDuplicateDisplayName(t *testing.T) {
	low := "/user.slice/foo.service"
	high := "/system.slice/foo.service"
	sum := SumCulprits([]Step{{Processes: []Process{{PID: 1, Start: 1, Comm: "low", WriteBytes: 1, Cgroup: low},
		{PID: 2, Start: 1, Comm: "high", WriteBytes: 10, Cgroup: high}}}})
	if got := sum.cgroupPath("foo.service"); got != high {
		t.Fatalf("path for foo.service = %q, want largest-value path %q", got, high)
	}
}

func TestCaseVRateMinimumMergesCalmSteps(t *testing.T) {
	base := time.Unix(1_800_000_000, 0)
	open := openCase{start: base, last: base.Add(time.Second)}
	first, calm, later := 40.0, 25.0, 30.0
	open.sum.add(Step{At: base, VRate: &first})
	open.calm.add(Step{At: base.Add(500 * time.Millisecond), VRate: &calm})
	open.sum.merge(open.calm)
	open.sum.add(Step{At: base.Add(time.Second), VRate: &later})
	c := open.toCase()
	if c.VRateMin == nil || *c.VRateMin != 25 {
		t.Fatalf("минимум не включил спокойный шаг из merge: %+v", c)
	}
}

func TestCaseVRateBeforeStartIncludesFiveSecondsNotFivePointOne(t *testing.T) {
	base := time.Unix(1_800_000_000, 0)
	start := base.Add(10 * time.Second)
	r := NewRecorder(Default())
	for _, sample := range []struct {
		at time.Time
		v  float64
	}{{start.Add(-5100 * time.Millisecond), 20}, {start.Add(-5 * time.Second), 35}, {start.Add(-time.Second), 42}, {start, 10}} {
		value := sample.v
		r.ring.Add(Record{Kind: KindStep, Step: Step{At: sample.at, VRate: &value}})
	}
	c := Case{Start: start, End: start.Add(100 * time.Millisecond), DurMS: 100}
	r.finish(&c)
	if c.VRateBeforeCase == nil || *c.VRateBeforeCase != 35 {
		t.Fatalf("окно [start−5s,start): %+v", c.VRateBeforeCase)
	}
}
