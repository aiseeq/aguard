package iowatch

import (
	"cmp"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/aiseeq/akit/proc"
)

// Culprits — кто дал I/O в детальных записях: сумма по имени процесса и cgroup.
type Culprits struct {
	ProcWrite, ProcRead, ProcMajflt                   map[string]int64 // байты, majflt
	CgroupWrite, CgroupRead, CgroupSwapIn, CgroupIdle map[string]int64 // байты, страницы, мс PSI full io
	CgroupWriteOps                                    map[string]int64
	cgroupTotal                                       map[string]int64 // запись + чтение + своп: главный виновник
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
		CgroupIdle: map[string]int64{}, CgroupWriteOps: map[string]int64{}, cgroupTotal: map[string]int64{}}
	names := processNames(details)
	for i := range details {
		d := &details[i]
		for _, p := range d.Processes {
			name := names[procID{p.PID, p.Start}]
			c.ProcWrite[name] += p.WriteBytes
			c.ProcRead[name] += p.ReadBytes
			c.ProcMajflt[name] += p.Majflt
		}
		for _, g := range d.Cgroups {
			name := CgroupName(g.Path)
			c.CgroupWrite[name] += g.WriteBytes
			c.CgroupRead[name] += g.ReadBytes
			c.CgroupSwapIn[name] += g.SwapIn
			c.CgroupIdle[name] += g.IOFullUS / 1000
			c.CgroupWriteOps[name] += g.WriteOps
			c.cgroupTotal[g.Path] += g.WriteBytes + g.ReadBytes + g.SwapIn*4096
		}
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

// MainCgroup — путь cgroup с наибольшим I/O (запись, чтение, своп в байтах); "" — нет.
func (c *Culprits) MainCgroup() string {
	var best string
	for path, v := range c.cgroupTotal {
		if v > 0 && (best == "" || v > c.cgroupTotal[best] || v == c.cgroupTotal[best] && path < best) {
			best = path
		}
	}
	return best
}

// CgroupName — имя cgroup для человека: scope сессии агента — имя сессии, иначе последний компонент.
func CgroupName(path string) string {
	if name, _, ok := proc.SessionScope(path); ok {
		return name
	}
	return filepath.Base(filepath.Clean(path))
}

// Ranked — до limit имён меры по убыванию значения (при равенстве — по имени), нули пропускаются.
type Ranked struct {
	Name  string
	Value int64
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
