package iowatch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestParseDiskstatsFullAndShort(t *testing.T) {
	data := []byte("   7       0 loop0 1 2 3 4 5 6 7 8 9 10 11\n 259       0 nvme0n1 10 0 20 30 40 0 50 60 7 80 90 2 0 0 3 0 4\n")
	got, err := parseDiskstats(data, "nvme0n1")
	if err != nil {
		t.Fatal(err)
	}
	want := diskCounters{readBytes: 20 * 512, writeBytes: 50 * 512, rios: 10, rms: 30, wios: 40, wms: 60, inflight: 7, busyMS: 80, weighted: 90, dios: 2, dms: 3, fios: 0, fms: 4}
	if got != want {
		t.Fatalf("полные diskstats: %+v", got)
	}
	short, err := parseDiskstats([]byte("8 0 sda 1 0 2 3 4 0 5 6 7 8 9\n"), "sda")
	if err != nil {
		t.Fatal(err)
	}
	if short.rios != 1 || short.rms != 3 || short.wios != 4 || short.wms != 6 || short.inflight != 7 || short.busyMS != 8 || short.weighted != 9 || short.dios != 0 || short.fms != 0 {
		t.Fatalf("короткие diskstats: %+v", short)
	}
	if _, err := parseDiskstats(data, "sdz"); err == nil {
		t.Fatal("нет устройства — должна быть ошибка")
	}
}

func TestParsePressureVMStatMemInfo(t *testing.T) {
	some, full, err := parsePressureTotals([]byte("some avg10=0.00 avg60=0.00 avg300=0.00 total=8\nfull avg10=1.50 avg60=0.00 avg300=0.00 total=9\n"))
	if err != nil || some != 8 || full != 9 {
		t.Fatalf("PSI: %d %d %v", some, full, err)
	}
	in, out, maj, err := parseVMStat([]byte("nr_free_pages 1\npswpin 5\npswpout 6\npgmajfault 7\npgmajfault_s 99\n"))
	if err != nil || in != 5 || out != 6 || maj != 7 {
		t.Fatalf("vmstat: %d %d %d %v", in, out, maj, err)
	}
	mem, err := parseMemInfo([]byte("MemTotal:  10 kB\nMemAvailable:  6 kB\nSwapTotal:  9 kB\nSwapFree:  4 kB\nDirty:   2 kB\nWriteback:   3 kB\n"))
	if err != nil || mem != (memInfo{dirty: 2048, writeback: 3072, available: 6144, swapTotal: 9216, swapFree: 4096}) || mem.swapUsed() != 5120 {
		t.Fatalf("meminfo: %+v %v", mem, err)
	}
	if _, err := parseMemInfo([]byte("MemTotal:  10 kB\nDirty:   2 kB\nWriteback:   3 kB\n")); err == nil || !strings.Contains(err.Error(), "найдено 2") {
		t.Fatalf("meminfo без MemAvailable и свопа — ошибка: %v", err)
	}
}

// /proc/pressure/cpu: строки full на старых ядрах нет или она нулевая — только some; без some — ошибка.
func TestParsePressureCPUWithoutFullAndLoadavg(t *testing.T) {
	for name, data := range map[string]string{
		"без full":     "some avg10=12.00 avg60=9.00 avg300=8.00 total=37814016426\n",
		"нулевая full": "some avg10=12.00 avg60=9.00 avg300=8.00 total=37814016426\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n",
	} {
		some, full, err := parsePressureTotals([]byte(data))
		if err != nil || some != 37814016426 || full != 0 {
			t.Errorf("%s: %d %d %v", name, some, full, err)
		}
	}
	for name, data := range map[string]string{"пусто": "", "только full": "full avg10=0.00 total=5\n", "some без total": "some avg10=1.00\n"} {
		if _, _, err := parsePressureTotals([]byte(data)); err == nil {
			t.Errorf("%s: без some нужна ошибка", name)
		}
	}
	n, err := parseLoadavgRunnable([]byte("17.65 16.94 14.02 8/5201 3854786\n"))
	if err != nil || n != 7 {
		t.Fatalf("loadavg: %d %v", n, err)
	}
	for _, bad := range []string{"17.65 16.94 14.02\n", "1 2 3 x/5 9\n", "1 2 3 85201 9\n", "1 2 3 0/5 9\n"} {
		if _, err := parseLoadavgRunnable([]byte(bad)); err == nil {
			t.Errorf("loadavg %q без ошибки", bad)
		}
	}
}

func TestParseProcStatCommWithParensAndSpaces(t *testing.T) {
	st, err := parseProcStat([]byte("123 (odd ) command) S 77 1 1 1 0 0 0 0 17 0 30 40 0 0 0 0 0 0 99 5 6\n"))
	if err != nil {
		t.Fatal(err)
	}
	if string(st.comm) != "odd ) command" || st.ppid != 77 || st.start != 99 || st.maj != 17 {
		t.Fatalf("stat: %+v comm=%q", st, st.comm)
	}
	if _, err := parseProcStat([]byte("1 (x) S 1 2")); err == nil {
		t.Fatal("короткий stat — должна быть ошибка")
	}
}

func TestParseBlockDevicesFollowsHolders(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "nvme0n1", "dev"), "259:0\n")
	writeFile(t, filepath.Join(root, "nvme0n1", "nvme0n1p1", "partition"), "1\n")
	writeFile(t, filepath.Join(root, "nvme0n1", "nvme0n1p6", "partition"), "6\n")
	writeFile(t, filepath.Join(root, "nvme0n1", "nvme0n1p6", "holders", "dm-0"), "")
	writeFile(t, filepath.Join(root, "dm-0", "dev"), "252:0\n")
	writeFile(t, filepath.Join(root, "dm-0", "holders", "dm-1"), "")
	writeFile(t, filepath.Join(root, "dm-1", "dev"), "252:1\n")
	got, err := blockDevices(root, "nvme0n1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "259:0,252:0,252:1" {
		t.Fatalf("устройства: %v", got)
	}
	if _, err := blockDevices(root, "sda"); err == nil {
		t.Fatal("нет диска — должна быть ошибка")
	}
}

// Буферная запись на LUKS учитывается в io.stat на диске и на dm одинаково, O_DIRECT — почти только на dm:
// по каждому полю берётся наибольшая дельта, а не сумма и не один диск.
func TestParseIOStatMaxOverDiskAndDM(t *testing.T) {
	root := t.TempDir()
	leaf := filepath.Join(root, "user.slice", "agent-s-1.scope")
	writeFile(t, filepath.Join(leaf, "cgroup.procs"), "1\n")
	for _, name := range []string{"io.pressure", "memory.pressure"} {
		writeFile(t, filepath.Join(leaf, name), "some avg10=0 total=0\nfull avg10=0 total=0\n")
	}
	stat := func(disk, dm string) {
		writeFile(t, filepath.Join(leaf, "io.stat"), "7:0 rbytes=1 wbytes=999999 rios=1 wios=1\n259:0 "+disk+"\n252:0 "+dm+"\n")
	}
	stat("rbytes=0 wbytes=0 rios=0 wios=0", "rbytes=0 wbytes=0 rios=0 wios=0")
	r := newCgroupReader(root, []string{"259:0", "252:0"})
	defer r.close()
	now := time.Now()
	if skipped, err := r.read(&Step{At: now}); err != nil || skipped != nil {
		t.Fatal(err, skipped)
	}
	// буферно 100 на обоих, O_DIRECT 1500 через dm (на диске — 47), чтение только на диске
	stat("rbytes=30 wbytes=147 rios=3 wios=60", "rbytes=0 wbytes=1600 rios=0 wios=50")
	s := Step{At: now.Add(time.Second)}
	if skipped, err := r.read(&s); err != nil || skipped != nil {
		t.Fatal(err, skipped)
	}
	if len(s.Cgroups) != 1 {
		t.Fatalf("листья: %+v", s.Cgroups)
	}
	if g := s.Cgroups[0]; g.WriteBytes != 1600 || g.ReadBytes != 30 || g.WriteOps != 60 || g.ReadOps != 3 || g.Path != "/user.slice/agent-s-1.scope" {
		t.Fatalf("io.stat по устройствам: %+v", g)
	}
}

func TestParseSnapshotSkipsBrokenAndUnfinishedLines(t *testing.T) {
	var buf bytes.Buffer
	at := time.Date(2026, 10, 28, 15, 0, 0, 0, time.Local)
	if err := WriteRecord(&buf, Record{Kind: KindStep, Step: Step{At: at, IOFullUS: 100}}); err != nil {
		t.Fatal(err)
	}
	buf.WriteString("broken json\n{\"k\":\"x\",\"at\":\"2026-10-28T15:00:00Z\"}\n")
	if err := WriteRecord(&buf, Record{Kind: KindDetail, Step: Step{At: at, Processes: []Process{{PID: 1, Comm: "dd", WriteBytes: 5}}}}); err != nil {
		t.Fatal(err)
	}
	buf.WriteString(`{"k":"s","at":"2026-10-28T15:00:01`) // файл ещё пишется
	var got []Record
	warnings := 0
	if err := ParseSnapshot(&buf, func(r Record) { got = append(got, r) }, func(error) { warnings++ }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].IOFullUS != 100 || got[1].Kind != KindDetail || got[1].Processes[0].Comm != "dd" || warnings != 2 {
		t.Fatalf("записи %+v, предупреждений %d", got, warnings)
	}
}

func TestDeltaFastCounterReset(t *testing.T) {
	root := t.TempDir()
	write := func(ioFull, swapIn, written, cpuSome, runnable int) {
		writeFile(t, filepath.Join(root, "pressure", "io"), "some avg10=0 total=1\nfull avg10=0 total="+strconv.Itoa(ioFull)+"\n")
		writeFile(t, filepath.Join(root, "pressure", "memory"), "some avg10=0 total=1\nfull avg10=0 total=1\n")
		writeFile(t, filepath.Join(root, "pressure", "cpu"), "some avg10=0 total="+strconv.Itoa(cpuSome)+"\n")
		writeFile(t, filepath.Join(root, "loadavg"), "1.00 2.00 3.00 "+strconv.Itoa(runnable)+"/5201 77\n")
		writeFile(t, filepath.Join(root, "vmstat"), "pswpin "+strconv.Itoa(swapIn)+"\npswpout 0\npgmajfault 0\n")
		writeFile(t, filepath.Join(root, "diskstats"), "259 0 nvme0n1 0 0 0 0 0 0 "+strconv.Itoa(written)+" 0 0 0 0\n")
	}
	write(1000, 10, 8, 500, 3)
	r := NewFastReader(root, "nvme0n1")
	defer r.Close()
	now := time.Now()
	if s, err := r.Read(now); err != nil || s.IOFullUS != 0 {
		t.Fatalf("первое чтение — точка отсчёта: %+v %v", s, err)
	}
	write(61000, 4, 16, 12500, 18)
	s, err := r.Read(now.Add(100 * time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if s.IOFullUS != 60000 || s.SwapIn != 0 || s.WriteBytes != 8*512 || s.CPUSomeUS != 12000 || s.Runnable != 17 { // 18 в loadavg без читающего потока
		t.Fatalf("дельты (своп сброшен), CPU some — дельта, runnable — моментальное: %+v", s)
	}
}

// fakeProc — /proc с процессами: pid → stat и io.
type fakeProc struct {
	t    *testing.T
	root string
}

func (p fakeProc) set(pid, ppid int, start uint64, comm string, read, write, majflt int64) {
	dir := filepath.Join(p.root, strconv.Itoa(pid))
	writeFile(p.t, filepath.Join(dir, "stat"), strconv.Itoa(pid)+" ("+comm+") S "+strconv.Itoa(ppid)+" 1 1 1 0 0 0 0 "+strconv.FormatInt(majflt, 10)+" 0 0 0 0 0 0 0 0 0 "+strconv.FormatUint(start, 10)+" 0\n")
	writeFile(p.t, filepath.Join(dir, "io"), "rchar: 1\nread_bytes: "+strconv.FormatInt(read, 10)+"\nwrite_bytes: "+strconv.FormatInt(write, 10)+"\n")
	writeFile(p.t, filepath.Join(dir, "cgroup"), "0::/user.slice/agent-s-"+strconv.Itoa(pid)+".scope\n")
}

func (p fakeProc) remove(pid int) {
	if err := os.RemoveAll(filepath.Join(p.root, strconv.Itoa(pid))); err != nil {
		p.t.Fatal(err)
	}
}

func readPass(t *testing.T, r *processReader, at time.Time) map[int]Process {
	t.Helper()
	s := Step{At: at}
	if skipped, err := r.read(&s); err != nil || skipped != nil {
		t.Fatal(err, skipped)
	}
	out := map[int]Process{}
	for _, p := range s.Processes {
		out[p.PID] = p
	}
	return out
}

func TestDeltaProcessNewReusedAndGone(t *testing.T) {
	fp := fakeProc{t, t.TempDir()}
	fp.set(10, 1, 100, "bash", 0, 1000, 0)
	r := newProcessReader(fp.root)
	defer r.close()
	now := time.Now()
	if got := readPass(t, r, now); len(got) != 0 {
		t.Fatalf("первый проход — точка отсчёта: %+v", got)
	}
	fp.set(10, 1, 100, "bash", 0, 1500, 2)
	fp.set(20, 10, 150, "dd", 0, 700, 0) // родился после прошлого прохода: счётчики целиком его
	got := readPass(t, r, now.Add(time.Second))
	if got[10].WriteBytes != 500 || got[10].Majflt != 2 || got[20].WriteBytes != 700 || got[20].Comm != "dd" || got[20].Cgroup != "/user.slice/agent-s-20.scope" {
		t.Fatalf("дельты: %+v", got)
	}
	fp.set(20, 10, 160, "cc", 0, 30, 0) // pid переиспользован: другой starttime
	got = readPass(t, r, now.Add(2*time.Second))
	if got[20].Comm != "cc" || got[20].WriteBytes != 30 || got[20].Start != 160 {
		t.Fatalf("переиспользованный pid: %+v", got)
	}
	fp.remove(20)
	if got = readPass(t, r, now.Add(3*time.Second)); len(got) != 0 {
		t.Fatalf("исчезнувший процесс: %+v", got)
	}
}

// Два dd подряд из одного bash: у каждого свой I/O под своим pid; скачок родителя при wait() —
// родителю за вычетом уже учтённого у детей, с пометкой «+дети».
func TestDeltaChildExitParentKeepsJumpMarked(t *testing.T) {
	fp := fakeProc{t, t.TempDir()}
	fp.set(10, 1, 100, "bash", 0, 0, 0)
	fp.set(11, 10, 101, "dd", 0, 0, 0)
	r := newProcessReader(fp.root)
	defer r.close()
	now := time.Now()
	var details []Step
	pass := func(i int) map[int]Process {
		t.Helper()
		s := Step{At: now.Add(time.Duration(i) * time.Second)}
		if skipped, err := r.read(&s); err != nil || skipped != nil {
			t.Fatal(err, skipped)
		}
		details = append(details, s)
		out := map[int]Process{}
		for _, p := range s.Processes {
			out[p.PID] = p
		}
		return out
	}
	pass(0)
	fp.set(11, 10, 101, "dd", 0, 600<<20, 0)
	if got := pass(1); got[11].WriteBytes != 600<<20 || got[11].Comm != "dd" {
		t.Fatalf("первый dd (comm — после чтения io тем же буфером): %+v", got[11])
	}
	fp.remove(11)
	fp.set(10, 1, 100, "bash", 0, 1000<<20, 0) // 600 уже учтены у первого dd
	fp.set(12, 10, 102, "dd", 0, 300<<20, 0)   // второй dd родился после прохода: счётчики целиком
	got := pass(2)
	if p := got[10]; p.WriteBytes != 400<<20 || !p.Children || got[12].WriteBytes != 300<<20 || got[11].PID != 0 {
		t.Fatalf("скачок родителя и второй dd: %+v", got)
	}
	fp.remove(12)
	fp.set(10, 1, 100, "bash", 0, 1350<<20, 0)
	if p := pass(3)[10]; p.WriteBytes != 50<<20 || !p.Children {
		t.Fatalf("после второго dd: %+v", p)
	}
	fp.set(10, 1, 100, "bash", 0, 1351<<20, 0)
	if p := pass(4)[10]; p.WriteBytes != 1<<20 || p.Children {
		t.Fatalf("дальше I/O родителя — без пометки: %+v", p)
	}
	c := SumCulprits(details)
	if c.ProcWrite["dd(11)"] != 600<<20 || c.ProcWrite["dd(12)"] != 300<<20 || c.ProcWrite["bash(10)+дети"] != 451<<20 {
		t.Fatalf("итог: %v", c.ProcWrite)
	}
}

// Процесс с битым stat пропускается в этом проходе, остальные считаются.
func TestDeltaProcessErrorSkipsOnlyIt(t *testing.T) {
	fp := fakeProc{t, t.TempDir()}
	fp.set(10, 1, 100, "bash", 0, 0, 0)
	fp.set(11, 1, 101, "dd", 0, 0, 0)
	r := newProcessReader(fp.root)
	defer r.close()
	now := time.Now()
	readPass(t, r, now)
	fp.set(10, 1, 100, "bash", 0, 5, 0)
	writeFile(t, filepath.Join(fp.root, "11", "stat"), "мусор\n")
	s := Step{At: now.Add(time.Second)}
	skipped, err := r.read(&s)
	if err != nil || skipped == nil || !strings.Contains(skipped.Error(), "пропущено процессов: 1") {
		t.Fatalf("ошибка одного процесса: skipped=%v err=%v", skipped, err)
	}
	if len(s.Processes) != 1 || s.Processes[0].PID != 10 || s.Processes[0].WriteBytes != 5 {
		t.Fatalf("остальные должны считаться: %+v", s.Processes)
	}
	fp.set(11, 1, 101, "dd", 0, 70, 0)
	if p := readPass(t, r, now.Add(2*time.Second))[11]; p.WriteBytes != 70 {
		t.Fatalf("после починки — дельта от прежних счётчиков: %+v", p)
	}
}

// Имя процесса — по последнему comm: обёртка с exec (timeout → dd) видна командой.
func TestDeltaCulpritNameLastComm(t *testing.T) {
	details := []Step{{Processes: []Process{{PID: 5, Start: 1, Comm: "timeout", WriteBytes: 1}}},
		{Processes: []Process{{PID: 5, Start: 1, Comm: "dd", WriteBytes: 2}}}}
	if c := SumCulprits(details); c.ProcWrite["dd(5)"] != 3 || len(c.ProcWrite) != 1 {
		t.Fatalf("имя по последнему comm: %v", c.ProcWrite)
	}
}

func TestParseProcIOSubtractsCancelledWrites(t *testing.T) {
	read, write, err := parseProcIO([]byte("rchar: 1\nread_bytes: 10\nwrite_bytes: 100\ncancelled_write_bytes: 40\n"))
	if err != nil || read != 10 || write != 60 {
		t.Fatalf("io: %d %d %v", read, write, err)
	}
	if _, write, _ := parseProcIO([]byte("write_bytes: 10\ncancelled_write_bytes: 40\n")); write != 0 {
		t.Fatalf("отменённых больше записанных: %d", write)
	}
}

// Лист cgroup, созданный после прошлого обхода, виден со следующего прохода (не через 10 с) и
// со счётчиками целиком; старый лист, получивший процессы, — только с дельтой.
func TestDeltaCgroupNewLeafFullCounters(t *testing.T) {
	root := t.TempDir()
	leaf := func(rel, procs, wbytes string) {
		dir := filepath.Join(root, rel)
		writeFile(t, filepath.Join(dir, "cgroup.procs"), procs)
		for _, name := range []string{"io.pressure", "memory.pressure"} {
			writeFile(t, filepath.Join(dir, name), "some avg10=0 total=0\nfull avg10=0 total=0\n")
		}
		writeFile(t, filepath.Join(dir, "io.stat"), "252:0 rbytes=0 wbytes="+wbytes+" rios=0 wios=1\n")
	}
	leaf("user.slice/a.scope", "1\n", "1000")
	leaf("user.slice/idle.scope", "", "7000")
	r := newCgroupReader(root, []string{"252:0"})
	defer r.close()
	now := time.Now()
	if skipped, err := r.read(&Step{At: now}); err != nil || skipped != nil {
		t.Fatal(err, skipped)
	}
	time.Sleep(10 * time.Millisecond) // время создания новой папки — позже прошлого обхода
	leaf("user.slice/short.scope", "2\n", "500")
	leaf("user.slice/idle.scope", "3\n", "7100")
	s := Step{At: now.Add(time.Second)}
	if skipped, err := r.read(&s); err != nil || skipped != nil {
		t.Fatal(err, skipped)
	}
	got := map[string]int64{}
	for _, g := range s.Cgroups {
		got[g.Path] = g.WriteBytes
	}
	if got["/user.slice/short.scope"] != 500 || got["/user.slice/idle.scope"] != 0 || len(got) != 1 {
		t.Fatalf("новый лист: %v", got)
	}
	leaf("user.slice/idle.scope", "3\n", "7300")
	s = Step{At: now.Add(2 * time.Second)}
	if skipped, err := r.read(&s); err != nil || skipped != nil {
		t.Fatal(err, skipped)
	}
	if len(s.Cgroups) != 1 || s.Cgroups[0].Path != "/user.slice/idle.scope" || s.Cgroups[0].WriteBytes != 200 {
		t.Fatalf("старый лист с процессами — по дельте: %+v", s.Cgroups)
	}
}

// CPU листа — дельта usage_usec из cpu.stat, anon — моментальный из memory.stat. Лист только с
// anon (спит, CPU и IO нет) в проход не попадает; cpu.stat без usage_usec — ошибка этого листа.
func TestDeltaCgroupCPUAndAnon(t *testing.T) {
	root := t.TempDir()
	leaf := func(rel, usage, anon string) {
		dir := filepath.Join(root, rel)
		writeFile(t, filepath.Join(dir, "cgroup.procs"), "1\n")
		for _, name := range []string{"io.pressure", "memory.pressure"} {
			writeFile(t, filepath.Join(dir, name), "some avg10=0 total=0\nfull avg10=0 total=0\n")
		}
		writeFile(t, filepath.Join(dir, "cpu.stat"), "usage_usec "+usage+"\nuser_usec 1\nsystem_usec 1\n")
		writeFile(t, filepath.Join(dir, "memory.stat"), "anon "+anon+"\nanon_thp 4096\nfile 1\npgmajfault 0\n")
	}
	leaf("glint.service", "1000000", "1048576")
	leaf("sleep.scope", "500", "2097152")
	r := newCgroupReader(root, []string{"252:0"})
	defer r.close()
	now := time.Now()
	if skipped, err := r.read(&Step{At: now}); err != nil || skipped != nil {
		t.Fatal(err, skipped)
	}
	leaf("glint.service", "4000000", "3145728")
	leaf("sleep.scope", "500", "4194304")
	s := Step{At: now.Add(time.Second)}
	if skipped, err := r.read(&s); err != nil || skipped != nil {
		t.Fatal(err, skipped)
	}
	if len(s.Cgroups) != 1 || s.Cgroups[0].Path != "/glint.service" || s.Cgroups[0].CPUUS != 3_000_000 || s.Cgroups[0].AnonB != 3<<20 {
		t.Fatalf("CPU и anon: %+v", s.Cgroups)
	}
	writeFile(t, filepath.Join(root, "glint.service", "cpu.stat"), "user_usec 1\n")
	s = Step{At: now.Add(2 * time.Second)}
	if skipped, err := r.read(&s); err != nil || skipped == nil || !strings.Contains(skipped.Error(), "usage_usec") {
		t.Fatalf("cpu.stat без usage_usec: skipped=%v err=%v", skipped, err)
	}
}

func TestDeltaDeniedProcessSkipped(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root читает чужие файлы")
	}
	fp := fakeProc{t, t.TempDir()}
	fp.set(30, 1, 100, "sshd", 0, 0, 0)
	if err := os.Chmod(filepath.Join(fp.root, "30", "io"), 0); err != nil {
		t.Fatal(err)
	}
	r := newProcessReader(fp.root)
	defer r.close()
	readPass(t, r, time.Now())
	if _, ok := r.denied[30]; !ok || r.procs[30] != nil {
		t.Fatalf("чужой процесс должен пропускаться: denied=%v", r.denied)
	}
}

func TestDeltaTopAndZeroFields(t *testing.T) {
	r := newProcessReader(t.TempDir())
	defer r.close()
	for i := range 20 {
		p := Process{PID: i + 1, Start: 1}
		switch {
		case i < 7:
			p.WriteBytes = int64(20 - i)
		case i < 14:
			p.ReadBytes = int64(20 - i)
		default:
			p.Majflt = int64(20 - i)
		}
		r.collect(p)
	}
	r.collect(Process{PID: 99}) // нулевой — не попадает никуда
	s := Step{At: time.Now()}
	r.selectTop(&s, func(err error) { t.Fatal(err) })
	if len(s.Processes) != 3*topProcesses {
		t.Fatalf("объединение трёх топов: %d записей", len(s.Processes))
	}
	if s.Processes[0].PID != 1 || s.Processes[topProcesses].PID != 8 || s.Processes[2*topProcesses].PID != 15 {
		t.Fatalf("порядок по убыванию: %+v", s.Processes)
	}
	var buf bytes.Buffer
	if err := WriteRecord(&buf, Record{Kind: KindDetail, Step: Step{At: s.At, Processes: s.Processes[:1]}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "rd_b") || strings.Contains(buf.String(), "majflt") || strings.Contains(buf.String(), "swapin") {
		t.Fatalf("нулевые поля в записи: %s", buf.String())
	}
}

func TestRingOverflowOrderAndFiveMinutes(t *testing.T) {
	cfg := Default()
	r := NewRing(cfg)
	base := time.Date(2026, 10, 28, 15, 0, 0, 0, time.Local)
	for i := range 6000 { // 10 минут шагов по 100 мс и детальные раз в секунду
		at := base.Add(time.Duration(i) * 100 * time.Millisecond)
		r.Add(Record{Kind: KindStep, Step: Step{At: at}})
		if i%10 == 0 {
			r.Add(Record{Kind: KindDetail, Step: Step{At: at}})
		}
	}
	got := r.Records()
	last := got[len(got)-1].At
	if first := got[0].At; last.Sub(first) != 5*time.Minute || len(got) != 3001+300 {
		t.Fatalf("окно %s, записей %d", last.Sub(first), len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].At.Before(got[i-1].At) {
			t.Fatalf("порядок нарушен на %d", i)
		}
	}
	small := &Ring{records: make([]Record, 3), window: time.Hour}
	for i := range 5 {
		small.Add(Record{Kind: KindStep, Step: Step{At: base.Add(time.Duration(i) * time.Second)}})
	}
	if got := small.Records(); len(got) != 3 || !got[0].At.Equal(base.Add(2*time.Second)) {
		t.Fatalf("переполнение: %+v", got)
	}
}

// feed — шаги по 100 мс с простоем idle[i] от base; detail — детальная запись в начале каждой секунды.
func feed(r *Recorder, base time.Time, idle []int64, detail func(at time.Time) Step) []Case {
	var out []Case
	for i, v := range idle {
		at := base.Add(time.Duration(i) * 100 * time.Millisecond)
		if i%10 == 0 && detail != nil {
			r.Detail(detail(at))
		}
		out = append(out, r.Step(Step{At: at, IOFullUS: v, WriteBytes: 1 << 20, WIOS: 2, WMS: 60, InFlight: int64(i)})...)
	}
	return out
}

func TestCaseStallsMergeAcrossShortCalmAndGetCulprits(t *testing.T) {
	cfg := Default()
	cfg.CaseMin = policy.Duration(200 * time.Millisecond)
	r := NewRecorder(cfg)
	base := time.Date(2026, 10, 28, 15, 0, 0, 0, time.Local)
	idle := make([]int64, 20)
	idle[1], idle[2], idle[3], idle[4] = 10_000, 60_000, 20_000, 70_000 // 10 мс — не простой; 20 мс между простоями входит в случай
	got := feed(r, base, idle, func(at time.Time) Step {
		return Step{At: at, Dirty: 5 << 20, Processes: []Process{{PID: 1, Start: 3, Comm: "dd", WriteBytes: 7 << 20, Cgroup: "/user.slice/agent-sess7-1.scope"}},
			Cgroups: []Cgroup{{Path: "/user.slice/agent-sess7-1.scope", WriteBytes: 7 << 20, IOFullUS: 90_000}}}
	})
	if len(got) != 1 {
		t.Fatalf("случаев %d: %+v", len(got), got)
	}
	c := got[0]
	if !c.Start.Equal(base.Add(100*time.Millisecond)) || !c.End.Equal(base.Add(400*time.Millisecond)) || c.DurMS != 300 ||
		c.IdleMS != 150 || c.PeakMS != 70 || c.IOFullMS != 150 || c.WriteB != 3<<20 || c.WAwaitMS != 30 || c.InFlight != 4 || c.Dirty != 5<<20 {
		t.Fatalf("сводка: %+v", c)
	}
	k := c.Culprits
	if k == nil || k.Main != "/user.slice/agent-sess7-1.scope" || len(k.ProcWrite) != 1 || k.ProcWrite[0] != (Ranked{"dd(1)", 14 << 20}) ||
		k.CgroupWrite[0].Name != "sess7" || k.CgroupIdle[0].Value != 180 || !k.From.Equal(c.Start.Add(-5*time.Second)) {
		t.Fatalf("виновники (детальные 0 и 1 с в окне): %+v", k)
	}
}

// Коммит btrfs по чужому fsync: io.stat у postgres 870 МБ при своих 5 МБ, а тест, удаляющий тысячи файлов, сам пачкает
// 560 МБ метаданных и в io.stat почти не виден (случай 30.10 12:15) — главный тест; запись устройства сверх листьев cgroup —
// фон ядра.
func TestCulpritsMainByProcessWrite(t *testing.T) {
	cfg := Default()
	cfg.CaseMin = policy.Duration(200 * time.Millisecond)
	r := NewRecorder(cfg)
	base := time.Date(2026, 10, 30, 12, 15, 0, 0, time.Local)
	idle := make([]int64, 20)
	idle[1], idle[2], idle[3], idle[4] = 60_000, 60_000, 60_000, 60_000
	test := fmt.Sprintf("/user.slice/user-%d.slice/user@%d.service/agents.slice/agent-sess9-1.scope", os.Getuid(), os.Getuid())
	const pg = "/system.slice/postgresql.service"
	got := feed(r, base, idle, func(at time.Time) Step {
		return Step{At: at, Processes: []Process{
			{PID: 7, Start: 1, Comm: "dreamstats.test", WriteBytes: 280 << 20, Cgroup: test},
			{PID: 9, Start: 1, Comm: "postgres", WriteBytes: 2 << 20, Cgroup: pg}},
			Cgroups: []Cgroup{{Path: pg, WriteBytes: 435 << 20}, {Path: test, WriteBytes: 1 << 20}}}
	})
	if len(got) != 1 || got[0].Culprits == nil {
		t.Fatalf("случаи: %+v", got)
	}
	k := got[0].Culprits
	if k.Main != test || k.CgroupDirty[0] != (Ranked{"sess9", 560 << 20}) || k.CgroupOwn["sess9"] != 560<<20 {
		t.Fatalf("главный и своя запись: %+v", k)
	}
	if _, ok := k.CgroupOwn["postgresql.service"]; ok { // процессы чужого uid: своя запись не видна, ноль — не «не писал»
		t.Fatalf("своя запись системной службы: %+v", k.CgroupOwn)
	}
	// io.stat postgres 870 МиБ больше всей меры теста 560 МиБ: главная неточна
	if k.Foreign != (Ranked{"postgresql.service", 870 << 20}) {
		t.Fatalf("чужая cgroup сверх главной: %+v", k.Foreign)
	}
	var back Culprit
	if b, err := json.Marshal(Culprit{CgroupOwn: map[string]int64{}}); err != nil {
		t.Fatal(err)
	} else if err := json.Unmarshal(b, &back); err != nil || back.CgroupOwn == nil {
		t.Fatalf("пустая своя запись после чтения %s: %v %v", b, back.CgroupOwn, err)
	}
	// быстрые шаги по 1 МиБ: окно деталей 0 и 1 с — (−1 с, 1 с], в буфере шаги 0…1,0 с — 11 МиБ; cgroup — 872 МиБ
	if k.DeviceWrite != 11<<20 || k.KernelWrite != 0 {
		t.Fatalf("запись устройства и фон ядра: %d %d", k.DeviceWrite, k.KernelWrite)
	}
}

// Простои через промежуток больше gap — два случая; короче case_min — без виновников; случай
// отдаётся, только когда детальная запись за его конец уже пришла.
func TestCaseSplitByGapShortWithoutCulpritsWaitsDetail(t *testing.T) {
	r := NewRecorder(Default())
	base := time.Date(2026, 10, 28, 15, 0, 0, 0, time.Local)
	idle := make([]int64, 40)
	idle[5], idle[10] = 80_000, 80_000 // 10 − 5 = 500 мс > gap 300 + шаг
	var got []Case
	for i, v := range idle {
		at := base.Add(time.Duration(i) * 100 * time.Millisecond)
		cases := r.Step(Step{At: at, IOFullUS: v})
		if len(cases) > 0 && i < 16 {
			t.Fatalf("случай отдан на шаге %d, до детальной записи за его конец", i)
		}
		got = append(got, cases...)
	}
	if len(got) != 2 || got[0].DurMS != 100 || !got[1].End.Equal(base.Add(time.Second)) || got[0].Culprits != nil || got[1].Culprits != nil {
		t.Fatalf("случаи: %+v", got)
	}
}

func TestCaseCloseOnStopAndDump(t *testing.T) {
	r := NewRecorder(Default())
	base := time.Now()
	if got := r.Step(Step{At: base, MemFullUS: 90_000}); len(got) != 0 {
		t.Fatalf("случай закрылся сразу: %+v", got)
	}
	got := r.Close()
	if len(got) != 1 || got[0].MemFullMS != 90 || got[0].IdleMS != 90 || !got[0].Quiet {
		t.Fatalf("закрытие при остановке (без обмена с диском — тихий): %+v", got)
	}
	r.Step(Step{At: base.Add(time.Second), IOFullUS: 90_000, WriteBytes: 2 << 20})
	if got := r.Close(); len(got) != 1 || got[0].Quiet {
		t.Fatalf("с обменом 2 МиБ — не тихий: %+v", got)
	}
	dir := t.TempDir()
	snap := r.Dump(dir, base)
	if err := snap.Wait(); err != nil {
		t.Fatal(err)
	}
	if !IsManual(filepath.Base(snap.Path)) {
		t.Fatalf("имя ручного снимка %s", snap.Path)
	}
	if parts, err := filepath.Glob(filepath.Join(dir, "*.part")); err != nil || len(parts) != 0 {
		t.Fatalf("временный файл остался: %v %v", parts, err)
	}
	win, err := ReadSnapshots(dir, base.Add(-time.Minute), base.Add(time.Minute), time.Minute, func(err error) { t.Error(err) })
	if err != nil || len(win.Steps) != 2 {
		t.Fatalf("снимок: %d шагов, %v", len(win.Steps), err)
	}
}

func TestKeepRemovesOldSnapshotsOnly(t *testing.T) {
	dir := t.TempDir()
	old, fresh := filepath.Join(dir, "old.jsonl"), filepath.Join(dir, "fresh.jsonl")
	writeFile(t, old, "{}\n")
	writeFile(t, fresh, "{}\n")
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	if err := CleanupSnapshots(dir, time.Hour, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("старый снимок остался: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("свежий снимок удалён: %v", err)
	}
}

func TestCasesSaveLoadMergeAndBrokenFile(t *testing.T) {
	base := time.Date(2026, 10, 28, 15, 0, 0, 0, time.Local)
	path := filepath.Join(t.TempDir(), "io", "cases.jsonl")
	if got, err := LoadCases(path); err != nil || got != nil {
		t.Fatalf("нет файла — пусто: %+v %v", got, err)
	}
	a := []Case{{Start: base, DurMS: 100}, {Start: base.Add(time.Hour), DurMS: 2500, Culprits: &Culprit{Main: "/x.scope", ProcWrite: []Ranked{{"dd(1)", 5}}}}}
	b := []Case{{Start: base.Add(time.Hour), DurMS: 2500}, {Start: base.Add(-48 * time.Hour)}, {Start: base.Add(30 * time.Minute)}}
	merged := MergeCases(a, b, base.Add(-time.Hour))
	if len(merged) != 3 || !merged[1].Start.Equal(base.Add(30*time.Minute)) || merged[2].Culprits == nil {
		t.Fatalf("слияние: повтор по началу — один, старше since — нет, по времени: %+v", merged)
	}
	if err := SaveCases(path, merged); err != nil {
		t.Fatal(err)
	}
	got, err := LoadCases(path)
	if err != nil || len(got) != 3 || got[2].Culprits.ProcWrite[0].Name != "dd(1)" || !got[0].Start.Equal(base) {
		t.Fatalf("прочитано: %+v %v", got, err)
	}
	writeFile(t, path, `{"start":"2026-10-28T15:00:00+01:00"}`+"\n{битая\n")
	if _, err := LoadCases(path); err == nil || !strings.Contains(err.Error(), "запись 2") {
		t.Fatalf("битая запись — ошибка с номером: %v", err)
	}
}

func TestSummarizeBucketsHoursMainAndCulprits(t *testing.T) {
	base := time.Date(2026, 10, 29, 3, 10, 0, 0, time.Local)
	long := func(at time.Time, ms int64, main, proc string) Case {
		return Case{Start: at, DurMS: ms, IdleMS: ms / 2, Culprits: &Culprit{Main: main,
			ProcWrite: []Ranked{{proc + "(1)", 1 << 20}, {proc + "(2)", 1 << 20}}, CgroupSwapIn: []Ranked{{"sess7", 10}}}}
	}
	cases := []Case{
		{Start: base, DurMS: 100, IdleMS: 60},
		long(base.Add(time.Minute), 2500, "/user.slice/agent-sess7-1.scope", "chrome"),
		long(base.Add(2*time.Minute), 700, "/user.slice/agent-sess7-1.scope", "chrome"),
		long(base.Add(time.Hour), 6000, "/app.slice/app-com.google.Chrome@ab12.service", "rm"),
		{Start: base.Add(-time.Hour), DurMS: 9000}, // до since
	}
	s := Summarize(cases, base.Add(-time.Minute), base.Add(2*time.Hour), 500*time.Millisecond)
	if s.Total != 4 || s.IdleMS != 60+1250+350+3000 || s.Buckets[0].Count != 1 || s.Buckets[2].Count != 1 || s.Buckets[4].Count != 1 ||
		s.Buckets[5].Count != 1 || s.Buckets[5].Label != "≥5 с" || s.Buckets[5].ToMS != 0 {
		t.Fatalf("итог и корзины: %+v", s)
	}
	if len(s.Hours) != 2 || s.Hours[0].Count != 3 || s.Hours[0].Long != 2 || s.Hours[0].MaxDurMS != 2500 || !s.Hours[1].Hour.Equal(base.Add(time.Hour).Truncate(time.Hour)) {
		t.Fatalf("часы: %+v", s.Hours)
	}
	if len(s.Cases) != 3 || s.Cases[0].DurMS != 6000 || s.Main[0] != (RankedCases{"sess7", 3200, 2}) || s.Main[1].Name != "com.google.Chrome" {
		t.Fatalf("случаи и главные: %+v %+v", s.Cases, s.Main)
	}
	if w := s.Culprits.ProcWrite; len(w) != 2 || w[0] != (RankedCases{"chrome", 4 << 20, 2}) || s.Culprits.CgroupSwapIn[0] != (RankedCases{"sess7", 30, 3}) {
		t.Fatalf("виновники: процессы без pid, случай считается раз: %+v", s.Culprits)
	}
}

func TestCgroupNameSessionAppDocker(t *testing.T) {
	for path, want := range map[string]string{
		"/user.slice/user-1000.slice/user@1000.service/app.slice/app-com.google.Chrome@a1e2e36ed98.service": "com.google.Chrome",
		"/user.slice/user-1000.slice/user@1000.service/app.slice/app-com.google.Chrome-1020295.scope":       "com.google.Chrome",
		"/user.slice/app-flatpak-com.rtosta.zapzap-3652013825.scope":                                        "flatpak-com.rtosta.zapzap",
		"/system.slice/docker-ffa992139584eed83753dccdd082ae554395f6cc36ab18839a0eea1701d43e91.scope":       "docker-ffa992139584",
		"/system.slice/systemd-journald.service":                                                            "systemd-journald.service",
	} {
		if got := CgroupName(path); got != want {
			t.Errorf("%s → %q, хотели %q", path, got, want)
		}
	}
	if got := ProcBase("bash(123)+дети"); got != "bash" {
		t.Errorf("ProcBase: %q", got)
	}
}

// Сброс буфера 5 минут в файл (3000 шагов и 300 детальных) — ручной снимок.
func BenchmarkRecorderDump5m(b *testing.B) {
	r := NewRecorder(Default())
	base := time.Date(2026, 10, 28, 15, 0, 0, 0, time.Local)
	for i := range 3000 {
		at := base.Add(time.Duration(i) * 100 * time.Millisecond)
		r.Step(Step{At: at, IOFullUS: 1200, MemFullUS: 300, ReadBytes: 4096, WriteBytes: 8192, RIOS: 2, WIOS: 3, WMS: 8, InFlight: 1})
		if i%10 == 0 {
			r.Detail(Step{At: at, Processes: []Process{{PID: i + 1, Comm: "benchmark", ReadBytes: 4096, WriteBytes: 8192}, {PID: i + 2, Comm: "dd", WriteBytes: 1 << 20}},
				Cgroups: []Cgroup{{Path: "/user.slice/agent-s-1.scope", ReadBytes: 4096, WriteBytes: 8192, WriteOps: 3, IOFullUS: 1000}}})
		}
	}
	dir := b.TempDir()
	b.ReportAllocs()
	for b.Loop() {
		snap := r.Dump(dir, base.Add(5*time.Minute))
		if err := snap.Wait(); err != nil {
			b.Fatal(err)
		}
		if err := os.Remove(snap.Path); err != nil {
			b.Fatal(err)
		}
	}
}

// Ошибка чтения одного листа cgroup пропускает только его: остальные листья секунды считаются.
func TestDeltaCgroupErrorSkipsOnlyIt(t *testing.T) {
	root := t.TempDir()
	leaf := func(rel, pressure, wbytes string) {
		dir := filepath.Join(root, rel)
		writeFile(t, filepath.Join(dir, "cgroup.procs"), "1\n")
		for _, name := range []string{"io.pressure", "memory.pressure"} {
			writeFile(t, filepath.Join(dir, name), pressure)
		}
		writeFile(t, filepath.Join(dir, "io.stat"), "252:0 rbytes=0 wbytes="+wbytes+" rios=0 wios=1\n")
	}
	const ok = "some avg10=0 total=0\nfull avg10=0 total=0\n"
	leaf("a.scope", ok, "1000")
	leaf("b.scope", ok, "1000")
	r := newCgroupReader(root, []string{"252:0"})
	defer r.close()
	now := time.Now()
	if skipped, err := r.read(&Step{At: now}); err != nil || skipped != nil {
		t.Fatal(err, skipped)
	}
	leaf("a.scope", "full avg10=0 total=мусор\n", "3000")
	leaf("b.scope", ok, "1500")
	s := Step{At: now.Add(time.Second)}
	skipped, err := r.read(&s)
	if err != nil || skipped == nil || !strings.Contains(skipped.Error(), "пропущено cgroup: 1") {
		t.Fatalf("ошибка одного листа: skipped=%v err=%v", skipped, err)
	}
	if len(s.Cgroups) != 1 || s.Cgroups[0].Path != "/b.scope" || s.Cgroups[0].WriteBytes != 500 {
		t.Fatalf("остальные должны считаться: %+v", s.Cgroups)
	}
	leaf("a.scope", ok, "3000")
	s = Step{At: now.Add(2 * time.Second)}
	if skipped, err := r.read(&s); err != nil || skipped != nil {
		t.Fatal(err, skipped)
	}
	if len(s.Cgroups) != 1 || s.Cgroups[0].Path != "/a.scope" || s.Cgroups[0].WriteBytes != 2000 {
		t.Fatalf("пропущенная дельта — в следующий проход: %+v", s.Cgroups)
	}
}

// Быстрый шаг Recorder, как в цикле службы: шаг 100 мс, детальная запись раз в секунду, каждые
// 3 с — простой на 200 мс (случай открывается и закрывается); ряд, профили и живое состояние —
// как у службы (service) и без них (bare: `aguard io record`, прежний путь). Горячий путь: без аллокаций.
func BenchmarkRecorderStep(b *testing.B) {
	b.Run("bare", func(b *testing.B) { benchRecorderStep(b, NewRecorder(Default())) })
	b.Run("service", func(b *testing.B) {
		r := NewRecorder(Default())
		r.Attach(NewSeries(24*time.Hour), NewProfiles(), NewLive())
		benchRecorderStep(b, r)
	})
}

func benchRecorderStep(b *testing.B, r *Recorder) {
	base := time.Date(2026, 10, 28, 15, 0, 0, 0, time.Local)
	detail := Step{Dirty: 40 << 20, Writeback: 1 << 20, Processes: []Process{{PID: 7, Comm: "dd", WriteBytes: 1 << 20, Cgroup: "/user.slice/agent-s-1.scope"}},
		Cgroups: []Cgroup{{Path: "/user.slice/agent-s-1.scope", ReadBytes: 4096, WriteBytes: 1 << 20, WriteOps: 3, IOFullUS: 1000}}}
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		at := base.Add(time.Duration(i) * 100 * time.Millisecond)
		if i%10 == 0 {
			detail.At = at
			r.Detail(detail)
		}
		s := Step{At: at, IOFullUS: 1200, MemFullUS: 300, ReadBytes: 4096, WriteBytes: 8192, RIOS: 2, WIOS: 3, WMS: 8, InFlight: 1, FIOS: 1}
		if i%30 < 2 {
			s.IOFullUS = 80_000
		}
		r.Step(s)
		i++
	}
}

// Чтение быстрого шага и детального прохода с настоящих /proc и /sys машины: сколько стоит шаг
// службы (fast), проход целиком (detail) и одни листья cgroup (cgroups). Не Linux или нет диска —
// пропуск. Машина живая: цифры сравнивать прогонами подряд (benchstat), не с чужой машиной.
func BenchmarkReadLive(b *testing.B) {
	roots, device := SystemRoots(), Default().Device
	if _, err := os.Stat(filepath.Join(roots.Block, device)); err != nil {
		b.Skipf("нет диска %s: %v", device, err)
	}
	b.Run("fast", func(b *testing.B) {
		r := NewFastReader(roots.Proc, device)
		defer r.Close()
		b.ReportAllocs()
		for b.Loop() {
			if _, err := r.Read(time.Now()); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("detail", func(b *testing.B) {
		r, err := NewDetailReader(roots, device)
		if err != nil {
			b.Fatal(err)
		}
		defer r.Close()
		b.ReportAllocs()
		for b.Loop() {
			if _, _, err := r.Read(time.Now()); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("cgroups", func(b *testing.B) {
		devices, err := blockDevices(roots.Block, device)
		if err != nil {
			b.Fatal(err)
		}
		r := newCgroupReader(roots.Cgroup, devices)
		defer r.close()
		b.ReportAllocs()
		leaves, inPass, cpuOnly := 0, 0, 0
		for b.Loop() {
			s := Step{At: time.Now()}
			if _, err := r.read(&s); err != nil {
				b.Fatal(err)
			}
			leaves, inPass, cpuOnly = len(r.leaves), len(s.Cgroups), 0
			for _, g := range s.Cgroups {
				if g.ReadBytes|g.WriteBytes|g.ReadOps|g.WriteOps|g.SwapIn|g.SwapOut|g.Majflt|g.IOFullUS|g.MemFullUS == 0 {
					cpuOnly++ // в проходе только из-за CPU: столько добавила фаза 1б в буфер
				}
			}
		}
		b.ReportMetric(float64(leaves), "leaves")
		b.ReportMetric(float64(inPass), "in-pass")
		b.ReportMetric(float64(cpuOnly), "cpu-only")
	})
}

// Фон ядра: запись устройства 11 МиБ за окно, листья cgroup — 2 МиБ: 9 МиБ писали потоки ядра в корневую cgroup.
func TestCulpritsKernelWrite(t *testing.T) {
	cfg := Default()
	cfg.CaseMin = policy.Duration(200 * time.Millisecond)
	r := NewRecorder(cfg)
	idle := make([]int64, 20)
	idle[1], idle[2], idle[3], idle[4] = 60_000, 60_000, 60_000, 60_000
	got := feed(r, time.Date(2026, 10, 30, 12, 15, 0, 0, time.Local), idle, func(at time.Time) Step {
		return Step{At: at, Cgroups: []Cgroup{{Path: "/system.slice/postgresql.service", WriteBytes: 1 << 20}}}
	})
	if len(got) != 1 || got[0].Culprits == nil || got[0].Culprits.DeviceWrite != 11<<20 || got[0].Culprits.KernelWrite != 9<<20 {
		t.Fatalf("фон ядра: %+v", got)
	}
}

// Чужая cgroup сверх главной — по всему I/O io.stat (docker: чтение 45 меньше 50 главной, с записью 55 — больше); имя и в system.slice, и в
// user@<uid> — не своё: своя запись неизвестна; главной нет — пометки нет (проверка 30.10).
func TestCulpritsForeignOver(t *testing.T) {
	user := fmt.Sprintf("/user.slice/user-%d.slice/user@%d.service/", os.Getuid(), os.Getuid())
	sess := user + "agents.slice/agent-sess9-1.scope"
	c := SumCulprits([]Step{{Processes: []Process{{PID: 7, Start: 1, Comm: "go", WriteBytes: 50 << 20, Cgroup: sess}},
		Cgroups: []Cgroup{{Path: sess, WriteBytes: 40 << 20},
			{Path: "/system.slice/docker-0123456789abcdef0123.scope", WriteBytes: 10 << 20, ReadBytes: 45 << 20},
			{Path: "/system.slice/dbus-broker.service", WriteBytes: 1 << 20}, {Path: user + "dbus-broker.service", WriteBytes: 1 << 20},
			{Path: fmt.Sprintf("/user.slice/user-%d.slice/session-3.scope", os.Getuid()), WriteBytes: 1 << 20}}}})
	main := c.MainCgroup()
	if main != sess {
		t.Fatalf("главная %q", main)
	}
	if f, ok := c.ForeignOver(main); !ok || f != (Ranked{"docker-0123456789ab", 55 << 20}) {
		t.Fatalf("чужая сверх главной: %+v %v", f, ok)
	}
	if c.Readable["dbus-broker.service"] || !c.Readable["sess9"] || !c.Readable["session-3.scope"] {
		t.Fatalf("своё по имени: %+v", c.Readable)
	}
	if f, ok := c.ForeignOver(""); ok {
		t.Fatalf("без главной: %+v", f)
	}
}
