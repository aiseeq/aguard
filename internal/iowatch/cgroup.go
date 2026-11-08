package iowatch

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/aiseeq/aguard/internal/cgroupstats"
)

// cgroupLeavesEvery — как часто дерево cgroup обходится заново без повода: лист мог получить
// процессы, а время папки от этого не меняется.
const cgroupLeavesEvery = 10 * time.Second

// cgroupReader — дельты листьев cgroup v2 с процессами: io.stat по диску и dm над ним, memory.stat
// (своп, majflt и моментальный anon), PSI full io и memory, CPU из cpu.stat.
//
// Новая cgroup видна со следующего прохода: каждый проход сверяет время изменения внутренних папок
// дерева (их около двадцати; mkdir и rmdir потомка его меняют) и при расхождении обходит дерево.
// Лист, созданный после прошлого обхода, получает счётчики целиком: они начинаются с нуля, а
// короткий scope иначе не попал бы в итог совсем.
type cgroupReader struct {
	root     string
	devices  []string // major:minor: диск и dm поверх него, см. blockDevices
	leaves   []string
	leavesAt time.Time
	inner    map[string]time.Time // внутренние папки (и корень) → время изменения при обходе
	newEmpty map[string]bool      // новые пустые листья прошлого обхода: процессы в них вот-вот перенесут
	groups   map[string]*cgroupEntry
	buf      []byte
	ioScr    [][ioFields]int64
	capFiles map[string]*cachedFile // io.max листьев и предков по пути от root
	capPass  map[string]Cap         // потолок папки в текущем проходе
}

type cgroupEntry struct {
	ioStat, memStat, ioPSI, memPSI, cpuStat cachedFile
	io                                      [][ioFields]int64 // накопительные по устройствам
	swapIn, swapOut, majflt                 int64
	ioFull, memFull, cpuUS                  int64
	primed                                  bool // false — первое чтение только запоминает; новый лист — с нулей
}

func newCgroupReader(root string, devices []string) *cgroupReader {
	return &cgroupReader{root: root, devices: devices, groups: map[string]*cgroupEntry{}, inner: map[string]time.Time{},
		buf: make([]byte, 8192), ioScr: make([][ioFields]int64, len(devices)), capFiles: map[string]*cachedFile{}, capPass: map[string]Cap{}}
}

func (r *cgroupReader) close() {
	for rel, g := range r.groups {
		g.close()
		delete(r.groups, rel)
	}
	r.dropCapFiles(nil)
}

func (g *cgroupEntry) close() {
	for _, f := range []*cachedFile{&g.ioStat, &g.memStat, &g.ioPSI, &g.memPSI, &g.cpuStat} {
		f.close()
	}
}

// read добавляет в s ненулевые дельты листьев; первое чтение листа только запоминает счётчики.
// Ошибка одного листа пропускает только его (skipped), err — сбой обхода дерева.
func (r *cgroupReader) read(s *Step) (skipped, err error) {
	if r.leavesAt.IsZero() || len(r.newEmpty) > 0 || s.At.Sub(r.leavesAt) >= cgroupLeavesEvery || r.treeChanged() {
		if err := r.refresh(s.At); err != nil {
			return nil, err
		}
	}
	failed := 0
	var first error
	clear(r.capPass)
	for _, rel := range r.leaves {
		g := r.groups[rel]
		if g == nil {
			continue // пропал в прошлом проходе: до обновления списка не читается
		}
		item, err := r.readGroup(rel, g)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENODEV) {
			g.close() // лист удалён между обходами
			delete(r.groups, rel)
			continue
		}
		if err != nil {
			if failed++; first == nil {
				first = err // счётчики листа не сдвинулись: дельта уйдёт в следующий проход
			}
			continue
		}
		// AnonB — моментальное, лист в проход не выводит: иначе каждый спящий лист с памятью.
		if item.ReadBytes|item.WriteBytes|item.ReadOps|item.WriteOps|item.SwapIn|item.SwapOut|item.Majflt|item.IOFullUS|item.MemFullUS|item.CPUUS != 0 {
			// Потолок не прочитан — лист идёт без него (вне потолка): его простой остаётся простоем машины.
			if item.Cap, err = r.capOf(rel); err != nil {
				if failed++; first == nil {
					first = fmt.Errorf("потолок %s (лист учтён без него): %w", rel, err)
				}
			}
			s.Cgroups = append(s.Cgroups, item)
		}
	}
	if failed > 0 {
		return fmt.Errorf("пропущено cgroup: %d, первая ошибка: %w", failed, first), nil
	}
	return nil, nil
}

// treeChanged — у какой-то внутренней папки сменилось время изменения (создан или удалён потомок).
func (r *cgroupReader) treeChanged() bool {
	for dir, mtime := range r.inner {
		info, err := os.Stat(dir)
		if err != nil || !info.ModTime().Equal(mtime) {
			return true // пропала или изменилась: обход разберётся
		}
	}
	return false
}

// refresh перечитывает список листьев; у исчезнувших закрывает файлы и забывает счётчики. Лист,
// созданный после прошлого обхода, начинает с нулевых счётчиков.
func (r *cgroupReader) refresh(now time.Time) error {
	prev := r.leavesAt
	tree, err := walkCgroups(r.root)
	if err != nil {
		return fmt.Errorf("обойти cgroup %s: %w", r.root, err)
	}
	live := make(map[string]bool, len(tree.leaves))
	for _, rel := range tree.leaves {
		live[rel] = true
		if r.groups[rel] == nil {
			base := filepath.Join(r.root, rel)
			created, err := createdAfter(base, prev)
			if err != nil {
				return err
			}
			file := func(name string) cachedFile { return cachedFile{path: filepath.Join(base, name)} }
			r.groups[rel] = &cgroupEntry{ioStat: file("io.stat"), memStat: file("memory.stat"), ioPSI: file("io.pressure"),
				memPSI: file("memory.pressure"), cpuStat: file("cpu.stat"), io: make([][ioFields]int64, len(r.devices)),
				primed: r.newEmpty[rel] || created}
		}
	}
	for rel, g := range r.groups {
		if !live[rel] {
			g.close()
			delete(r.groups, rel)
		}
	}
	newEmpty := map[string]bool{}
	for _, rel := range tree.empty {
		created, err := createdAfter(filepath.Join(r.root, rel), prev)
		if err != nil {
			return err
		}
		if created {
			newEmpty[rel] = true
		}
	}
	r.newEmpty = newEmpty
	r.leaves, r.leavesAt, r.inner = tree.leaves, now, tree.inner
	keep := make(map[string]bool, len(tree.leaves)+len(tree.inner))
	for _, rel := range tree.leaves {
		keep[rel] = true
	}
	for dir := range tree.inner {
		rel, err := filepath.Rel(r.root, dir)
		if err != nil {
			return fmt.Errorf("папка %s вне %s: %w", dir, r.root, err)
		}
		keep[rel] = true
	}
	r.dropCapFiles(keep)
	return nil
}

// createdAfter — папка cgroup создана после t (время папки — её создание; первый обход — нет).
// Пропавшая папка — не новая: следующий обход её уберёт.
func createdAfter(dir string, t time.Time) (bool, error) {
	if t.IsZero() {
		return false, nil
	}
	var st syscall.Stat_t
	err := syscall.Stat(dir, &st)
	if errors.Is(err, syscall.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("время создания %s: %w", dir, err)
	}
	return time.Unix(st.Ctim.Unix()).After(t), nil
}

// readGroup читает пять файлов листа и считает дельты. Нет io.stat, memory.stat или cpu.stat
// (контроллер не включён; cpu.stat в cgroup v2 есть всегда, но лист мог пропасть) — по ним нули.
func (r *cgroupReader) readGroup(rel string, g *cgroupEntry) (Cgroup, error) {
	item := Cgroup{Path: "/" + rel}
	var data []byte
	var err error
	if r.buf, data, err = g.ioPSI.read(r.buf); err != nil {
		return item, err
	}
	_, ioFull, err := parsePressureTotals(data)
	if err != nil {
		return item, fmt.Errorf("разобрать %s: %w", g.ioPSI.path, err)
	}
	if r.buf, data, err = g.memPSI.read(r.buf); err != nil {
		return item, err
	}
	_, memFull, err := parsePressureTotals(data)
	if err != nil {
		return item, fmt.Errorf("разобрать %s: %w", g.memPSI.path, err)
	}
	clear(r.ioScr)
	if r.buf, data, err = g.ioStat.read(r.buf); err == nil {
		err = parseIOStat(data, r.devices, r.ioScr)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return item, fmt.Errorf("io.stat %s: %w", rel, err)
	}
	var mem memStat
	if r.buf, data, err = g.memStat.read(r.buf); err == nil {
		mem, err = parseMemStat(data)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return item, fmt.Errorf("memory.stat %s: %w", rel, err)
	}
	var cpuUS int64
	if r.buf, data, err = g.cpuStat.read(r.buf); err == nil {
		cpuUS, err = parseCPUStatUsage(data)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return item, fmt.Errorf("cpu.stat %s: %w", rel, err)
	}
	item.AnonB = mem.anon
	if g.primed {
		item.ReadBytes, item.WriteBytes = maxDelta(r.ioScr, g.io, ioReadBytes), maxDelta(r.ioScr, g.io, ioWriteBytes)
		item.ReadOps, item.WriteOps = maxDelta(r.ioScr, g.io, ioReadOps), maxDelta(r.ioScr, g.io, ioWriteOps)
		item.SwapIn, item.SwapOut, item.Majflt = delta(mem.swapIn, g.swapIn), delta(mem.swapOut, g.swapOut), delta(mem.majflt, g.majflt)
		item.IOFullUS, item.MemFullUS, item.CPUUS = delta(ioFull, g.ioFull), delta(memFull, g.memFull), delta(cpuUS, g.cpuUS)
	}
	copy(g.io, r.ioScr)
	g.swapIn, g.swapOut, g.majflt, g.ioFull, g.memFull, g.cpuUS, g.primed = mem.swapIn, mem.swapOut, mem.majflt, ioFull, memFull, cpuUS, true
	return item, nil
}

// cgroupTree — итог обхода: листья с процессами и без (пути от root), внутренние папки со временем.
type cgroupTree struct {
	leaves, empty []string
	inner         map[string]time.Time
}

// walkCgroups обходит дерево cgroup: листья (без подпапок) с непустым cgroup.procs — по правилу
// cgroup v2 процессы с контроллерами живут только в листьях.
func walkCgroups(root string) (cgroupTree, error) {
	tree := cgroupTree{inner: map[string]time.Time{}}
	buf := make([]byte, 16)
	var walk func(dir, rel string) error
	walk = func(dir, rel string) error {
		info, err := os.Stat(dir) // до чтения: потомок, созданный во время обхода, сменит время
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
			return nil // папки уже нет или она чужая
		}
		if err != nil {
			return err
		}
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
			return nil
		}
		if err != nil {
			return err
		}
		leaf := true
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			leaf = false
			if err := walk(filepath.Join(dir, entry.Name()), filepath.Join(rel, entry.Name())); err != nil {
				return err
			}
		}
		if !leaf || rel == "" {
			tree.inner[dir] = info.ModTime()
			return nil
		}
		populated, err := hasProcesses(filepath.Join(dir, "cgroup.procs"), buf)
		if err != nil {
			return err
		}
		if populated {
			tree.leaves = append(tree.leaves, rel)
		} else {
			tree.empty = append(tree.empty, rel)
		}
		return nil
	}
	if err := walk(root, ""); err != nil {
		return cgroupTree{}, err
	}
	return tree, nil
}

func hasProcesses(path string, buf []byte) (bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("открыть %s: %w", path, err)
	}
	defer f.Close()
	n, err := f.Read(buf)
	if errors.Is(err, io.EOF) || errors.Is(err, syscall.ENODEV) {
		return false, nil // процессов нет или лист пропал
	}
	if err != nil {
		return false, fmt.Errorf("прочитать %s: %w", path, err)
	}
	return len(bytes.TrimSpace(buf[:n])) > 0, nil
}

// maxDelta — наибольшая по устройствам дельта поля io.stat: одна и та же запись видна и на диске,
// и на dm поверх него, сумма посчитала бы её дважды.
func maxDelta(cur, prev [][ioFields]int64, field int) int64 {
	var best int64
	for dev := range cur {
		best = max(best, delta(cur[dev][field], prev[dev][field]))
	}
	return best
}

// memStat — нужное из memory.stat листа: счётчики свопа и majflt, моментальный anon (байт).
type memStat struct{ swapIn, swapOut, majflt, anon int64 }

// parseMemStat — pswpin, pswpout, pgmajfault и anon из memory.stat.
func parseMemStat(data []byte) (memStat, error) {
	var m memStat
	err := cgroupstats.EachKeyed(data, func(key []byte, value int64) {
		switch {
		case bytes.Equal(key, []byte("pswpin")):
			m.swapIn = value
		case bytes.Equal(key, []byte("pswpout")):
			m.swapOut = value
		case bytes.Equal(key, []byte("pgmajfault")):
			m.majflt = value
		case bytes.Equal(key, []byte("anon")):
			m.anon = value
		}
	})
	return m, err
}

// parseCPUStatUsage — usage_usec из cpu.stat листа, мкс; поля нет — ошибка.
func parseCPUStatUsage(data []byte) (int64, error) {
	usage, seen := int64(0), false
	err := cgroupstats.EachKeyed(data, func(key []byte, value int64) {
		if bytes.Equal(key, []byte("usage_usec")) {
			usage, seen = value, true
		}
	})
	if err == nil && !seen {
		err = errors.New("нет usage_usec")
	}
	return usage, err
}
