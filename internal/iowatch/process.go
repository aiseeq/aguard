package iowatch

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/aiseeq/akit/proc"
)

const (
	topProcesses   = 5                // в запись — до стольких процессов по каждой мере
	deniedRetry    = 10               // чужой процесс (EACCES) перепроверяется раз в столько проходов: pid мог смениться
	exitedPasses   = 3                // столько проходов ждём скачка у родителя после ухода ребёнка
	cgroupRecheck  = 10 * time.Second // cgroup процесса из топа перечитывается не чаще
	direntNameSkip = 19               // смещение d_name в linux_dirent64
)

// processReader — дельты I/O (/proc/<pid>/io) и majflt (/proc/<pid>/stat) всех своих процессов за
// детальный интервал. Чужие (EACCES) пропускаются молча, исчезнувшие — тоже; процесс, чьи файлы не
// прочитались или не разобрались, пропускается в этом проходе, остальные считаются.
//
// Новый процесс, появившийся после прошлого прохода, получает свои счётчики целиком: они начинаются
// с нуля при fork, а за неполную секунду до первого прохода dd успевает записать сотни мегабайт.
//
// Ушедший ребёнок: при wait() ядро добавляет его итог родителю, и у родителя — скачок на весь I/O
// ребёнка. Уже приписанное детям (их последние счётчики) из скачка вычитается, остаток остаётся
// родителю с пометкой Children: это хвост детей после последнего прохода, I/O детей, которых проход
// не застал, и I/O самого родителя — разделить их нечем.
type processReader struct {
	root   string
	dir    *os.File
	dirBuf []byte
	pids   []int
	procs  map[int]*procEntry
	denied map[int]uint64
	exited map[int]*exitedChildren // по pid родителя
	pass   uint64
	buf    []byte
	all    []Process
}

type procEntry struct {
	stat, io         cachedFile
	comm             string
	ppid             int
	start            uint64
	read, write, maj int64 // накопительные
	pass             uint64
	cgroup           string
	cgroupAt         time.Time
}

// exitedChildren — ушедшие дети родителя, чей итог ядро добавит ему при wait().
type exitedChildren struct {
	read, write int64 // уже приписанное детям: из скачка родителя вычитается
	until       uint64
}

func newProcessReader(root string) *processReader {
	return &processReader{root: root, dirBuf: make([]byte, 32*1024), procs: map[int]*procEntry{}, denied: map[int]uint64{},
		exited: map[int]*exitedChildren{}, buf: make([]byte, 4096)}
}

func (r *processReader) close() {
	for pid, e := range r.procs {
		e.close()
		delete(r.procs, pid)
	}
	if r.dir != nil {
		_ = r.dir.Close()
		r.dir = nil
	}
}

func (e *procEntry) close() {
	e.stat.close()
	e.io.close()
}

// read добавляет в s процессы-лидеры по записи, чтению и majflt за интервал. skipped — процессы,
// пропущенные из-за ошибки (первая ошибка и их число); err — проход не удался целиком.
func (r *processReader) read(s *Step) (skipped, err error) {
	r.pass++
	pids, err := r.listPids()
	if err != nil {
		return nil, err
	}
	r.all = r.all[:0]
	var failed int
	var first error
	fail := func(err error) {
		if failed == 0 {
			first = err
		}
		failed++
	}
	for _, pid := range pids {
		if at, ok := r.denied[pid]; ok && r.pass-at < deniedRetry {
			continue
		}
		if e := r.procs[pid]; e != nil {
			p, gone, err := r.update(pid, e)
			if err != nil {
				e.pass = r.pass // счётчики прежние: следующий проход посчитает дельту от них
				fail(err)
				continue
			}
			if !gone {
				r.collect(p)
				continue
			}
			r.forget(pid, e) // pid переиспользован или процесс ушёл: читаем заново ниже
		}
		p, err := r.add(pid)
		if err != nil {
			fail(err)
			continue
		}
		r.collect(p)
	}
	for pid, e := range r.procs {
		if e.pass != r.pass {
			r.forget(pid, e)
		}
	}
	for pid, at := range r.denied {
		if r.pass-at >= deniedRetry {
			delete(r.denied, pid)
		}
	}
	r.applyExited()
	r.selectTop(s, fail)
	if failed > 0 {
		skipped = fmt.Errorf("пропущено процессов: %d, первая ошибка: %w", failed, first)
	}
	return skipped, nil
}

func (r *processReader) collect(p Process) {
	if p.ReadBytes|p.WriteBytes|p.Majflt != 0 {
		r.all = append(r.all, p)
	}
}

// listPids — числовые записи /proc одним getdents по открытому каталогу, без строк на каждую.
func (r *processReader) listPids() ([]int, error) {
	if r.dir == nil {
		dir, err := os.Open(r.root)
		if err != nil {
			return nil, fmt.Errorf("открыть %s: %w", r.root, err)
		}
		r.dir = dir
	}
	fd := int(r.dir.Fd())
	if _, err := syscall.Seek(fd, 0, 0); err != nil {
		return nil, fmt.Errorf("перемотать %s: %w", r.root, err)
	}
	r.pids = r.pids[:0]
	for {
		n, err := syscall.ReadDirent(fd, r.dirBuf)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("прочитать %s: %w", r.root, err)
		}
		if n <= 0 {
			return r.pids, nil
		}
		for off := 0; off+direntNameSkip < n; {
			reclen := int(binary.NativeEndian.Uint16(r.dirBuf[off+16:]))
			if reclen == 0 || off+reclen > n {
				break
			}
			if pid, ok := direntPID(r.dirBuf[off+direntNameSkip : off+reclen]); ok {
				r.pids = append(r.pids, pid)
			}
			off += reclen
		}
	}
}

// direntPID — pid из d_name (строка до NUL). Имена не с цифры (self, sys, …) отсекаются до разбора:
// ошибка разбора создаёт объект, а их в /proc около шестидесяти на проход.
func direntPID(name []byte) (int, bool) {
	if end := bytes.IndexByte(name, 0); end >= 0 {
		name = name[:end]
	}
	if len(name) == 0 || name[0] < '1' || name[0] > '9' {
		return 0, false
	}
	pid, err := parseIntBytes(name)
	return int(pid), err == nil
}

// update — дельты известного процесса; gone — процесс ушёл или pid уже у другого.
func (r *processReader) update(pid int, e *procEntry) (p Process, gone bool, err error) {
	st, err := r.readStat(e)
	if processGone(err) {
		return p, true, nil
	}
	if err != nil {
		return p, false, err
	}
	if st.start != e.start {
		return p, true, nil
	}
	if string(st.comm) != e.comm { // comm — срез r.buf: до следующего чтения
		e.comm = string(st.comm)
	}
	read, write, err := r.readIO(e)
	if processGone(err) || errors.Is(err, os.ErrPermission) {
		return p, true, nil // ушёл или сменил владельца (exec setuid): заново решит add
	}
	if err != nil {
		return p, false, err
	}
	e.ppid, e.pass = st.ppid, r.pass
	p = Process{PID: pid, PPID: st.ppid, Comm: e.comm, Start: e.start,
		ReadBytes: advance(&e.read, read), WriteBytes: advance(&e.write, write), Majflt: advance(&e.maj, st.maj)}
	return p, false, nil
}

// advance — прирост счётчика; меньшее значение (зомби отдаёт нули) не принимается.
func advance(prev *int64, cur int64) int64 {
	d := delta(cur, *prev)
	if cur > *prev {
		*prev = cur
	}
	return d
}

// add заводит процесс. Первый проход только запоминает счётчики, дальше новый процесс родился после
// прошлого прохода и его счётчики целиком — его I/O за этот интервал.
func (r *processReader) add(pid int) (Process, error) {
	base := filepath.Join(r.root, strconv.Itoa(pid))
	e := &procEntry{stat: cachedFile{path: filepath.Join(base, "stat")}, io: cachedFile{path: filepath.Join(base, "io")}, pass: r.pass}
	read, write, err := r.readIO(e)
	if errors.Is(err, os.ErrPermission) {
		e.close()
		r.denied[pid] = r.pass
		return Process{}, nil
	}
	if err == nil {
		var st procStat
		if st, err = r.readStat(e); err == nil {
			e.comm, e.ppid, e.start, e.maj = string(st.comm), st.ppid, st.start, st.maj
		}
	}
	if processGone(err) {
		e.close()
		return Process{}, nil
	}
	if err != nil {
		e.close()
		return Process{}, err
	}
	e.read, e.write = read, write
	r.procs[pid] = e
	if r.pass == 1 {
		return Process{}, nil
	}
	return Process{PID: pid, PPID: e.ppid, Comm: e.comm, Start: e.start, ReadBytes: read, WriteBytes: write, Majflt: e.maj}, nil
}

// forget убирает процесс и запоминает его у родителя: при wait() его итог придёт родителю.
func (r *processReader) forget(pid int, e *procEntry) {
	if e.ppid > 0 {
		c := r.exited[e.ppid]
		if c == nil {
			c = &exitedChildren{}
			r.exited[e.ppid] = c
		}
		c.read += e.read
		c.write += e.write
		c.until = r.pass + exitedPasses
	}
	e.close()
	delete(r.procs, pid)
}

// applyExited — скачок I/O родителя, у которого ушли дети: уже приписанное детям вычитается,
// остаток — родителю с пометкой Children. majflt родителя не трогается.
func (r *processReader) applyExited() {
	for i := range r.all {
		p := &r.all[i]
		c := r.exited[p.PID]
		if c == nil || p.ReadBytes|p.WriteBytes == 0 {
			continue
		}
		p.ReadBytes, p.WriteBytes = delta(p.ReadBytes, c.read), delta(p.WriteBytes, c.write)
		p.Children = true
		delete(r.exited, p.PID)
	}
	for pid, c := range r.exited {
		if r.pass > c.until {
			delete(r.exited, pid)
		}
	}
}

func (r *processReader) readStat(e *procEntry) (procStat, error) {
	var data []byte
	var err error
	if r.buf, data, err = e.stat.read(r.buf); err != nil {
		return procStat{}, err
	}
	st, err := parseProcStat(data)
	if err != nil {
		return procStat{}, fmt.Errorf("разобрать %s: %w", e.stat.path, err)
	}
	return st, nil
}

func (r *processReader) readIO(e *procEntry) (read, write int64, err error) {
	var data []byte
	if r.buf, data, err = e.io.read(r.buf); err != nil {
		return 0, 0, err
	}
	if read, write, err = parseProcIO(data); err != nil {
		return 0, 0, fmt.Errorf("разобрать %s: %w", e.io.path, err)
	}
	return read, write, nil
}

// processGone — процесс завершился: файла нет или задача уже убрана (ESRCH на открытом файле).
func processGone(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}

// selectTop оставляет в s до topProcesses процессов по записи, чтению и majflt (объединение) и
// подписывает им cgroup; cgroup, который не прочитался, — пустой, ошибка — в fail.
func (r *processReader) selectTop(s *Step, fail func(error)) {
	var top [3][topProcesses]int // индексы в r.all
	var counts [3]int
	for i := range r.all {
		for measure := range 3 {
			value := processMeasure(&r.all[i], measure)
			n := counts[measure]
			if value == 0 || n == topProcesses && value <= processMeasure(&r.all[top[measure][n-1]], measure) {
				continue
			}
			pos := min(n, topProcesses-1)
			if n < topProcesses {
				counts[measure]++
			}
			for pos > 0 && processMeasure(&r.all[top[measure][pos-1]], measure) < value {
				top[measure][pos] = top[measure][pos-1]
				pos--
			}
			top[measure][pos] = i
		}
	}
	first := len(s.Processes)
	for measure := range 3 {
	next:
		for _, index := range top[measure][:counts[measure]] {
			p := r.all[index]
			for _, have := range s.Processes[first:] {
				if have.PID == p.PID {
					continue next
				}
			}
			cgroup, err := r.processCgroup(p.PID, s.At)
			if err != nil {
				fail(err)
			}
			p.Cgroup = cgroup
			s.Processes = append(s.Processes, p)
		}
	}
}

func processMeasure(p *Process, measure int) int64 {
	switch measure {
	case 0:
		return p.WriteBytes
	case 1:
		return p.ReadBytes
	default:
		return p.Majflt
	}
}

// processCgroup — путь cgroup процесса из топа; перечитывается не чаще cgroupRecheck.
// Процесс ушёл между чтениями — прежний путь (у нового — пусто).
func (r *processReader) processCgroup(pid int, now time.Time) (string, error) {
	e := r.procs[pid]
	if e == nil {
		return "", nil
	}
	if e.cgroupAt.IsZero() || now.Sub(e.cgroupAt) >= cgroupRecheck {
		path := filepath.Join(r.root, strconv.Itoa(pid), "cgroup")
		b, err := os.ReadFile(path)
		if processGone(err) {
			return e.cgroup, nil
		}
		if err != nil {
			return "", fmt.Errorf("прочитать %s: %w", path, err)
		}
		e.cgroup, e.cgroupAt = proc.ParseCgroup(string(b)), now
	}
	return e.cgroup, nil
}
