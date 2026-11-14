package cpuwatch

import (
	"bufio"
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// CgroupCPU keeps only in-memory counters: after daemon start its first read is a baseline.
type CgroupCPU struct {
	MountRoot string
	lastAt    time.Time
	interval  float64
	last      map[string]int64
	inodes    map[string]uint64
}

type cpuNode struct {
	path     string
	children []*cpuNode
}

type cpuAllocator struct {
	deltas    map[string]int64
	details   map[string]map[Owner]int64
	by        map[Owner]float64
	seconds   float64
	mountRoot string
	owner     func(*Task) Owner
}

// Sample reads usage_usec for a cgroup subtree and attributes each non-overlapping delta.
// Parent remainder (including cgroups that vanished between samples) is explicitly unknown.
func (c *CgroupCPU) Sample(root string, now time.Time, owner func(*Task) Owner, details map[string]int64) ([]Load, bool, error) {
	return c.sampleWith(root, now, owner, details, os.ReadDir, os.ReadFile)
}

func (c *CgroupCPU) sampleWith(root string, now time.Time, owner func(*Task) Owner, details map[string]int64, readDir func(string) ([]os.DirEntry, error), readFile func(string) ([]byte, error)) ([]Load, bool, error) {
	tree, values, inodes, err := readCPUTreeWith(root, readDir, readFile)
	if err != nil {
		return nil, false, err
	}
	if c.lastAt.IsZero() || now.Sub(c.lastAt) <= 0 {
		c.lastAt, c.last, c.inodes = now, values, inodes
		return nil, false, nil
	}
	dt := now.Sub(c.lastAt).Seconds()
	deltas := cpuDeltas(values, c.last, inodes, c.inodes)
	c.lastAt, c.last, c.inodes = now, values, inodes
	c.interval = dt
	mountRoot := c.MountRoot
	if mountRoot == "" {
		mountRoot = root
	}
	alloc := cpuAllocator{deltas: deltas, details: detailCPUOwners(mountRoot, values, details, owner), by: map[Owner]float64{}, seconds: dt, mountRoot: mountRoot, owner: owner}
	alloc.add(tree)
	var out []Load
	for o, cpu := range alloc.by {
		if cpu > 0 {
			out = append(out, Load{Owner: o, CPU: cpu})
		}
	}
	slices.SortFunc(out, func(a, b Load) int { return cmp.Or(cmp.Compare(b.CPU, a.CPU), ownerCmp(a.Owner, b.Owner)) })
	return out, true, nil
}

func (c *CgroupCPU) IntervalSeconds() float64 { return c.interval }

func cpuDeltas(values, last map[string]int64, inodes, oldInodes map[string]uint64) map[string]int64 {
	deltas := make(map[string]int64, len(values))
	for p, n := range values {
		prev, ok := last[p]
		if ok && inodes[p] != oldInodes[p] {
			deltas[p] = n
		} else if ok && n >= prev {
			deltas[p] = n - prev
		} else if !ok {
			deltas[p] = n
		}
	}
	return deltas
}

func detailCPUOwners(mountRoot string, values map[string]int64, details map[string]int64, owner func(*Task) Owner) map[string]map[Owner]int64 {
	out := map[string]map[Owner]int64{}
	for p, usec := range details {
		full := filepath.Join(mountRoot, strings.TrimPrefix(p, "/"))
		if _, live := values[full]; live || usec <= 0 {
			continue
		}
		parent := filepath.Dir(full)
		for {
			if _, ok := values[parent]; ok {
				break
			}
			next := filepath.Dir(parent)
			if next == parent {
				parent = ""
				break
			}
			parent = next
		}
		if _, ok := values[parent]; !ok {
			continue
		}
		cgroup := strings.TrimPrefix(full, mountRoot)
		if cgroup == full {
			cgroup = p
		}
		t := Task{Cgroup: cgroup}
		o := owner(&t)
		if out[parent] == nil {
			out[parent] = map[Owner]int64{}
		}
		out[parent][o] += usec
	}
	return out
}

func (a *cpuAllocator) add(n *cpuNode) {
	parent := a.deltas[n.path]
	var children int64
	for _, ch := range n.children {
		children += a.deltas[ch.path]
	}
	rem := max(parent-children, 0)
	for o, used := range proportionalCPU(rem, a.details[n.path]) {
		rem -= used
		a.by[o] += float64(used) / 1e6 / a.seconds
	}
	if len(n.children) == 0 {
		if strings.HasSuffix(filepath.Base(n.path), ".slice") {
			a.by[Owner{Kind: KindOther, Label: "cgroup " + filepath.Base(n.path) + " (хозяин неизвестен)"}] += float64(rem) / 1e6 / a.seconds
		} else {
			cgroup := strings.TrimPrefix(n.path, a.mountRoot)
			t := Task{Cgroup: cgroup}
			a.by[a.owner(&t)] += float64(rem) / 1e6 / a.seconds
		}
		return
	}
	if rem > 0 {
		a.by[Owner{Kind: KindOther, Label: "cgroup " + filepath.Base(n.path) + " (хозяин неизвестен)"}] += float64(rem) / 1e6 / a.seconds
	}
	for _, ch := range n.children {
		a.add(ch)
	}
}

func proportionalCPU(available int64, requested map[Owner]int64) map[Owner]int64 {
	owners := make([]Owner, 0, len(requested))
	var total int64
	for o, n := range requested {
		if n > 0 {
			owners = append(owners, o)
			total += n
		}
	}
	slices.SortFunc(owners, ownerCmp)
	out := make(map[Owner]int64, len(owners))
	if available <= 0 || total <= 0 {
		return out
	}
	pool := min(available, total)
	assigned := int64(0)
	for _, o := range owners {
		share := pool * requested[o] / total
		out[o] = share
		assigned += share
	}
	for _, o := range owners {
		if assigned >= pool {
			break
		}
		if out[o] < requested[o] {
			out[o]++
			assigned++
		}
	}
	return out
}

func readCPUTreeWith(root string, readDir func(string) ([]os.DirEntry, error), readFile func(string) ([]byte, error)) (*cpuNode, map[string]int64, map[string]uint64, error) {
	values := map[string]int64{}
	inodes := map[string]uint64{}
	var walk func(string, bool) (*cpuNode, error)
	walk = func(path string, isRoot bool) (*cpuNode, error) {
		info, err := os.Stat(path)
		if err != nil {
			if !isRoot && vanishedCgroup(err) {
				return nil, nil
			}
			return nil, err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if ok {
			inodes[path] = st.Ino
		}
		use, exists, err := readUsageWith(path, isRoot, readFile)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, nil
		}
		n := &cpuNode{path: path}
		values[path] = use
		ents, e := readDir(path)
		if e != nil {
			if !isRoot && vanishedCgroup(e) {
				delete(values, path)
				delete(inodes, path)
				return nil, nil
			}
			return nil, e
		}
		for _, ent := range ents {
			if !ent.IsDir() {
				continue
			}
			ch, e := walk(filepath.Join(path, ent.Name()), false)
			if e != nil {
				return nil, e
			}
			if ch != nil {
				n.children = append(n.children, ch)
			}
		}
		return n, nil
	}
	n, err := walk(root, true)
	return n, values, inodes, err
}

func readUsageWith(path string, isRoot bool, readFile func(string) ([]byte, error)) (int64, bool, error) {
	data, err := readFile(filepath.Join(path, "cpu.stat"))
	if vanishedCgroup(err) {
		if !isRoot {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("%s/cpu.stat: %w", path, err)
	}
	if err != nil && !vanishedCgroup(err) {
		return 0, false, err
	}
	s := bufio.NewScanner(strings.NewReader(string(data)))
	for s.Scan() {
		f := strings.Fields(s.Text())
		if len(f) == 2 && f[0] == "usage_usec" {
			use, e := strconv.ParseInt(f[1], 10, 64)
			if e != nil {
				return 0, false, fmt.Errorf("%s: %w", path, e)
			}
			return use, true, nil
		}
	}
	return 0, true, nil
}

func vanishedCgroup(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENODEV)
}
