package iowatch

import (
	"bytes"
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
	dirty, wb, err := parseMemInfo([]byte("MemTotal:  10 kB\nDirty:   2 kB\nWriteback:   3 kB\n"))
	if err != nil || dirty != 2048 || wb != 3072 {
		t.Fatalf("meminfo: %d %d %v", dirty, wb, err)
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
	write := func(ioFull, swapIn, written int) {
		writeFile(t, filepath.Join(root, "pressure", "io"), "some avg10=0 total=1\nfull avg10=0 total="+strconv.Itoa(ioFull)+"\n")
		writeFile(t, filepath.Join(root, "pressure", "memory"), "some avg10=0 total=1\nfull avg10=0 total=1\n")
		writeFile(t, filepath.Join(root, "vmstat"), "pswpin "+strconv.Itoa(swapIn)+"\npswpout 0\npgmajfault 0\n")
		writeFile(t, filepath.Join(root, "diskstats"), "259 0 nvme0n1 0 0 0 0 0 0 "+strconv.Itoa(written)+" 0 0 0 0\n")
	}
	write(1000, 10, 8)
	r := NewFastReader(root, "nvme0n1")
	defer r.Close()
	now := time.Now()
	if s, err := r.Read(now); err != nil || s.IOFullUS != 0 {
		t.Fatalf("первое чтение — точка отсчёта: %+v %v", s, err)
	}
	write(61000, 4, 16)
	s, err := r.Read(now.Add(100 * time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if s.IOFullUS != 60000 || s.SwapIn != 0 || s.WriteBytes != 8*512 {
		t.Fatalf("дельты (своп сброшен): %+v", s)
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

func eventConfig() Config {
	cfg := Default()
	cfg.Calm = policy.Duration(300 * time.Millisecond)
	return cfg
}

func TestEventOpensWithBufferExtendsAndClosesAfterCalm(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(eventConfig(), dir)
	base := time.Date(2026, 10, 28, 15, 0, 0, 0, time.Local)
	step := func(i int, idle int64) *Event {
		t.Helper()
		ev, err := r.Step(Step{At: base.Add(time.Duration(i) * 100 * time.Millisecond), IOFullUS: idle})
		if err != nil {
			t.Fatal(err)
		}
		return ev
	}
	step(0, 0)
	r.Detail(Step{At: base, Processes: []Process{{PID: 1, Comm: "dd", WriteBytes: 1}}})
	step(1, 10_000)
	if r.Active() {
		t.Fatal("шаг ниже доли открыл событие")
	}
	step(2, 60_000) // открывает: 50 мс из 100
	step(3, 20_000)
	step(4, 70_000) // продлевает: простой спокойного шага 3 входит в событие
	for i := 5; i < 8; i++ {
		if ev := step(i, 0); ev != nil {
			t.Fatalf("закрылось до calm на шаге %d", i)
		}
	}
	ev := step(8, 0)
	if ev == nil {
		t.Fatal("не закрылось после calm")
	}
	if !ev.Start.Equal(base.Add(200*time.Millisecond)) || !ev.End.Equal(base.Add(400*time.Millisecond)) || ev.IdleUS != 150_000 || ev.PeakUS != 70_000 {
		t.Fatalf("событие: %+v", ev)
	}
	if filepath.Base(ev.Path) != EventName(ev.Start) {
		t.Fatalf("имя файла %s", ev.Path)
	}
	if _, open, err := r.Close(); open || err != nil { // ждёт дописи файла закрытого события
		t.Fatalf("остановка без открытого события: open=%v err=%v", open, err)
	}
	win, err := ReadSnapshots(dir, base.Add(-time.Minute), base.Add(time.Minute), time.Minute, func(err error) { t.Error(err) })
	if err != nil {
		t.Fatal(err)
	}
	if len(win.Steps) != 9 || len(win.Details) != 1 || !win.Steps[0].At.Equal(base) {
		t.Fatalf("снимок: буфер до события и шаги до конца: шагов %d, детальных %d", len(win.Steps), len(win.Details))
	}
}

// Два события подряд: второе берёт из буфера только записи позже записанных первым.
func TestEventNextEventSkipsWrittenRecords(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(eventConfig(), dir)
	base := time.Date(2026, 10, 28, 15, 0, 0, 0, time.Local)
	closed := 0
	for i := range 40 {
		idle := int64(0)
		if i == 5 || i == 20 {
			idle = 80_000
		}
		at := base.Add(time.Duration(i) * 100 * time.Millisecond)
		if i%10 == 0 {
			r.Detail(Step{At: at})
		}
		ev, err := r.Step(Step{At: at, IOFullUS: idle})
		if err != nil {
			t.Fatal(err)
		}
		if ev != nil {
			closed++
		}
	}
	if _, _, err := r.Close(); err != nil || closed != 2 {
		t.Fatalf("событий %d, err %v", closed, err)
	}
	seen := map[string]int{}
	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := ParseSnapshot(f, func(rec Record) { seen[rec.Kind+rec.At.Format(time.StampMilli)]++ }, func(err error) { t.Error(err) }); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	for key, n := range seen {
		if n > 1 {
			t.Errorf("запись %s в снимках %d раз", key, n)
		}
	}
	if len(files) != 2 || len(seen) != 25+3 { // шаги 0…24 (второе событие закрылось на 24), детальные 0, 10, 20
		t.Fatalf("файлов %d, записей %d", len(files), len(seen))
	}
}

func TestEventCloseOnStopAndDump(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(eventConfig(), dir)
	base := time.Now()
	if _, err := r.Step(Step{At: base, MemFullUS: 90_000}); err != nil {
		t.Fatal(err)
	}
	if !r.Active() {
		t.Fatal("событие не открылось")
	}
	ev, open, err := r.Close()
	if err != nil || !open || ev.IdleUS != 90_000 {
		t.Fatalf("закрытие при остановке: %+v %v", ev, err)
	}
	snap := r.Dump(base)
	if err := snap.Wait(); err != nil {
		t.Fatal(err)
	}
	if !IsManual(filepath.Base(snap.Path)) {
		t.Fatalf("имя ручного снимка %s", snap.Path)
	}
	if _, err := os.Stat(snap.Path + ".part"); !os.IsNotExist(err) {
		t.Fatalf("временный файл остался: %v", err)
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

func TestFreezesStepsAboveMinAndCulprits(t *testing.T) {
	base := time.Date(2026, 10, 28, 15, 0, 0, 0, time.Local)
	var w Window
	for i, idle := range []int64{60_000, 70_000, 0, 0, 0, 60_000, 20_000, 20_000, 20_000} {
		w.Steps = append(w.Steps, Step{At: base.Add(time.Duration(i) * 100 * time.Millisecond), IOFullUS: idle})
	}
	w.Details = []Step{{At: base.Add(time.Second), Processes: []Process{{PID: 2, Comm: "dd", WriteBytes: 1 << 20}}}, {At: base.Add(5 * time.Second)}}
	got := w.Freezes(50 * time.Millisecond)
	if len(got) != 2 {
		t.Fatalf("фризы: %+v", got)
	}
	if got[0].IdleUS != 130_000 || got[0].PeakUS != 70_000 || !got[1].Start.Equal(base.Add(500*time.Millisecond)) || got[1].IdleUS != 60_000 {
		t.Fatalf("фон ниже min склеен или простой неверен: %+v", got)
	}
	if len(got[0].Details) != 1 || SumCulprits(got[0].Details).ProcWrite["dd(2)"] != 1<<20 {
		t.Fatalf("виновники фриза: %+v", got[0].Details)
	}
}

// Сброс буфера 5 минут в файл (3000 шагов и 300 детальных): столько пишет горутина снимка при
// открытии события и при ручном снимке.
func BenchmarkRecorderDump5m(b *testing.B) {
	r := NewRecorder(Default(), b.TempDir())
	base := time.Date(2026, 10, 28, 15, 0, 0, 0, time.Local)
	for i := range 3000 {
		at := base.Add(time.Duration(i) * 100 * time.Millisecond)
		if _, err := r.Step(Step{At: at, IOFullUS: 1200, MemFullUS: 300, ReadBytes: 4096, WriteBytes: 8192, RIOS: 2, WIOS: 3, WMS: 8, InFlight: 1}); err != nil {
			b.Fatal(err)
		}
		if i%10 == 0 {
			r.Detail(Step{At: at, Processes: []Process{{PID: i + 1, Comm: "benchmark", ReadBytes: 4096, WriteBytes: 8192}, {PID: i + 2, Comm: "dd", WriteBytes: 1 << 20}},
				Cgroups: []Cgroup{{Path: "/user.slice/agent-s-1.scope", ReadBytes: 4096, WriteBytes: 8192, WriteOps: 3, IOFullUS: 1000}}})
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		snap := r.Dump(base.Add(5 * time.Minute))
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
