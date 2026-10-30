package iowatch

import (
	"cmp"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/aiseeq/akit/proc"
)

// Culprits — кто дал I/O в детальных записях: сумма по имени процесса и cgroup.
//
// Запись cgroup по io.stat — не то, что её процессы записали: на btrfs коммит транзакции по fsync уходит на cgroup
// того, кто вызвал fsync (сам он мог написать килобайты), а метаданные чужих unlink и reflink пишут потоки ядра в
// корневую cgroup (лид 30.10, bpftrace: 9,5 из 10,9 ГБ записи в бурю метаданных теста). Свою запись процесс видит в
// /proc/<pid>/io write_bytes в момент, когда пачкает страницы, и метаданные тоже: reflink-копия и удаление 5000
// файлов — 8,7–12,6 МиБ write_bytes при нулевых данных (проба aguard-master 30.10). Поэтому главный виновник — по
// записи процессов cgroup (CgroupDirty), а не по io.stat.
type Culprits struct {
	ProcWrite, ProcRead, ProcMajflt                   map[string]int64 // байты, majflt
	CgroupWrite, CgroupRead, CgroupSwapIn, CgroupIdle map[string]int64 // байты, страницы, мс PSI full io
	CgroupWriteOps                                    map[string]int64
	CgroupDirty                                       map[string]int64 // write_bytes процессов топа cgroup: данные и метаданные
	// Readable — cgroup своего user@<uid>.service: /proc/<pid>/io её процессов aguard читает, своя запись известна.
	// У системных служб (postgres, journald) процессы чужого uid, их write_bytes не видны — ноль там не значит «не писал».
	Readable    map[string]bool
	cgroupTotal map[string]int64 // запись процессов + чтение + своп: главный виновник
	foreignIO   map[string]int64 // по имени cgroup чужого uid: запись по io.stat + чтение + своп — мера, сравнимая с главной
}

type procID struct {
	pid   int
	start uint64
}

// SumCulprits складывает детальные записи. Имя процесса — comm(pid) последней его записи: обёртки
// с exec (systemd-run, timeout, nice) меняют comm на команду, а процесс тот же. У процесса, которому
// за окно пришёл I/O ушедших детей, — «+дети».
func SumCulprits(details []Step) Culprits {
	c := Culprits{ProcWrite: map[string]int64{}, ProcRead: map[string]int64{}, ProcMajflt: map[string]int64{},
		CgroupWrite: map[string]int64{}, CgroupRead: map[string]int64{}, CgroupSwapIn: map[string]int64{},
		CgroupIdle: map[string]int64{}, CgroupWriteOps: map[string]int64{}, CgroupDirty: map[string]int64{}, Readable: map[string]bool{},
		cgroupTotal: map[string]int64{}, foreignIO: map[string]int64{}}
	userTree := "/user@" + strconv.Itoa(os.Getuid()) + ".service/"
	foreign := map[string]bool{} // имя есть и вне user@<uid> (dbus-broker.service и в system.slice): своя запись неизвестна
	names := processNames(details)
	for i := range details {
		d := &details[i]
		for _, p := range d.Processes {
			name := names[procID{p.PID, p.Start}]
			c.ProcWrite[name] += p.WriteBytes
			c.ProcRead[name] += p.ReadBytes
			c.ProcMajflt[name] += p.Majflt
			if p.Cgroup != "" && p.WriteBytes > 0 {
				c.CgroupDirty[CgroupName(p.Cgroup)] += p.WriteBytes
				c.cgroupTotal[p.Cgroup] += p.WriteBytes
			}
		}
		for _, g := range d.Cgroups {
			name := CgroupName(g.Path)
			c.CgroupWrite[name] += g.WriteBytes
			c.CgroupRead[name] += g.ReadBytes
			c.CgroupSwapIn[name] += g.SwapIn
			c.CgroupIdle[name] += g.IOFullUS / 1000
			c.CgroupWriteOps[name] += g.WriteOps
			c.cgroupTotal[g.Path] += g.ReadBytes + g.SwapIn*4096
			if strings.Contains(g.Path, userTree) {
				c.Readable[name] = true
			} else {
				foreign[name] = true
				c.foreignIO[name] += g.WriteBytes + g.ReadBytes + g.SwapIn*4096
			}
		}
	}
	for name := range foreign {
		delete(c.Readable, name)
	}
	return c
}

// processNames — имя каждого процесса окна: последний comm, «+дети», если хоть раз был Children.
func processNames(details []Step) map[procID]string {
	comm := map[procID]string{}
	kids := map[procID]bool{}
	for i := range details {
		for _, p := range details[i].Processes {
			id := procID{p.PID, p.Start}
			comm[id] = p.Comm
			kids[id] = kids[id] || p.Children
		}
	}
	names := make(map[procID]string, len(comm))
	for id, name := range comm {
		suffix := ""
		if kids[id] {
			suffix = "+дети"
		}
		names[id] = fmt.Sprintf("%s(%d)%s", name, id.pid, suffix)
	}
	return names
}

// ProcBase — имя процесса из processNames без pid и пометки детей: comm(123)+дети → comm.
func ProcBase(name string) string {
	if i := strings.LastIndex(name, "("); i > 0 {
		return name[:i]
	}
	return name
}

// MainCgroup — путь cgroup с наибольшим I/O (запись её процессов, чтение, своп в байтах); "" — нет.
func (c *Culprits) MainCgroup() string {
	var best string
	for path, v := range c.cgroupTotal {
		if v > 0 && (best == "" || v > c.cgroupTotal[best] || v == c.cgroupTotal[best] && path < best) {
			best = path
		}
	}
	return best
}

// ForeignOver — cgroup чужого uid (не Readable, не главная) с наибольшим I/O по io.stat (запись, чтение, своп), если
// он больше всей меры главной: своя запись её процессов не видна, и главная выбрана без неё. Это может быть и коммит
// btrfs по её fsync (postgres 30.10 12:15), и настоящая запись (контейнеры docker) — отличить нечем, поэтому главная
// остаётся, а случай помечается неточным (проверка 30.10). Главной нет — нечего и помечать.
func (c *Culprits) ForeignOver(main string) (Ranked, bool) {
	var best Ranked
	if main == "" {
		return best, false
	}
	for _, name := range slices.Sorted(maps.Keys(c.foreignIO)) {
		v := c.foreignIO[name]
		if name != CgroupName(main) && v > c.cgroupTotal[main] && v > best.Value {
			best = Ranked{name, v}
		}
	}
	return best, best.Value > 0
}

// CgroupName — имя cgroup для человека: scope сессии агента — имя сессии; приложение рабочего стола
// (app-<id>@<хэш>.service, app-<id>-<pid>.scope) — его id: экземпляры одного приложения в итоге
// складываются; контейнер docker — короткий id; иначе последний компонент.
func CgroupName(path string) string {
	if name, _, ok := proc.SessionScope(path); ok {
		return name
	}
	base := filepath.Base(filepath.Clean(path))
	if id, ok := strings.CutPrefix(base, "app-"); ok {
		id = strings.TrimSuffix(strings.TrimSuffix(id, ".service"), ".scope")
		if at := strings.IndexByte(id, '@'); at > 0 {
			return id[:at]
		}
		if dash := strings.LastIndexByte(id, '-'); dash > 0 && strings.Trim(id[dash+1:], "0123456789") == "" {
			return id[:dash]
		}
		return id
	}
	if id, ok := strings.CutPrefix(base, "docker-"); ok && len(id) > 12+len(".scope") {
		return "docker-" + id[:12]
	}
	return base
}

// Ranked — до limit имён меры по убыванию значения (при равенстве — по имени), нули пропускаются.
type Ranked struct {
	Name  string `json:"name"`
	Value int64  `json:"value"`
}

func Rank(m map[string]int64, limit int) []Ranked {
	out := make([]Ranked, 0, len(m))
	for name, v := range m {
		if v > 0 {
			out = append(out, Ranked{name, v})
		}
	}
	slices.SortFunc(out, func(a, b Ranked) int {
		if c := cmp.Compare(b.Value, a.Value); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
	return out[:min(limit, len(out))]
}
