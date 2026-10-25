package orphans

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
	"github.com/aiseeq/akit/proc"
)

// Verdict — что делать с деревом сироты.
type Verdict int

// Решения по дереву.
const (
	Reap Verdict = iota // снять
	Wait                // сессия мертва, но меньше min_dead
	Skip                // не трогать
)

func (v Verdict) String() string {
	switch v {
	case Reap:
		return "снять"
	case Wait:
		return "ждёт"
	default:
		return "не трогать"
	}
}

// SessionKey — сессия, запустившая процесс: CLAUDE_PID и id сессии.
type SessionKey struct {
	Pid int
	ID  string
}

// String — ключ для состояния.
func (k SessionKey) String() string { return strconv.Itoa(k.Pid) + "/" + k.ID }

// Tree — сирота-корень и всё, что под ним.
type Tree struct {
	Root      *proc.Proc
	Members   []*proc.Proc // корень первым, затем потомки
	Session   SessionKey
	DeadBy    time.Time // сессия была мертва уже к этому моменту (оценка сверху)
	Container string    // id контейнера podman, если корень — его conmon
	Storage   []string  // --root/--runroot хранилища контейнера (из exit-command conmon)
	Verdict   Verdict
	Reason    string
}

// RootKey — корень с моментом старта: pid переиспользуется, пара — нет.
func (t *Tree) RootKey() string { return strconv.Itoa(t.Root.Pid) + "@" + t.Root.StartRaw }

// deadSession — сессия мертва; reusedAt — когда её pid занял другой процесс (если занял).
type deadSession struct {
	reusedAt time.Time
}

// Plan находит деревья сирот и решает, что с ними делать. seen — когда каждая
// мёртвая сессия впервые замечена мёртвой; Plan дописывает туда новые.
func Plan(s *proc.Snapshot, cfg *Config, seen map[string]time.Time) []Tree {
	dead := deadSessions(s)
	orphan := map[int]SessionKey{}
	for pid, p := range s.Procs {
		k := SessionKey{p.Session, p.SessionID}
		if _, ok := dead[k]; ok {
			orphan[pid] = k
		}
	}
	children := childIndex(s)
	var trees []Tree
	for _, pid := range sortedKeys(orphan) {
		p := s.Procs[pid]
		if _, parentOrphan := orphan[p.Ppid]; parentOrphan {
			continue // войдёт в дерево предка
		}
		k := orphan[pid]
		t := Tree{Root: p, Members: collect(s, children, pid), Session: k}
		t.DeadBy = deadBy(seen, k, dead[k], s.Now)
		judge(s, cfg, &t)
		trees = append(trees, t)
	}
	linkPasta(trees)
	return trees
}

// deadSessions — сессии, которых нет: процесса с pid CLAUDE_PID нет или он
// стартовал позже самого раннего процесса этой сессии (pid переиспользован —
// сессия старше любого своего потомка).
func deadSessions(s *proc.Snapshot) map[SessionKey]deadSession {
	first := map[SessionKey]uint64{}
	for _, p := range s.Procs {
		if p.Session == 0 {
			continue
		}
		k := SessionKey{p.Session, p.SessionID}
		if v, ok := first[k]; !ok || p.Start < v {
			first[k] = p.Start
		}
	}
	out := map[SessionKey]deadSession{}
	for k, start := range first {
		holder, ok := s.Procs[k.Pid]
		switch {
		case !ok:
			out[k] = deadSession{}
		case holder.Start > start:
			out[k] = deadSession{reusedAt: holder.StartAt}
		}
	}
	return out
}

// deadBy — сессия мертва не позже этого момента: когда её впервые увидели
// мёртвой или когда её pid занял другой процесс, что раньше.
func deadBy(seen map[string]time.Time, k SessionKey, d deadSession, now time.Time) time.Time {
	t, ok := seen[k.String()]
	if !ok {
		t = now
		seen[k.String()] = t
	}
	if !d.reusedAt.IsZero() && d.reusedAt.Before(t) {
		t = d.reusedAt
	}
	return t
}

// judge выносит решение по дереву; первая сработавшая причина не трогать — в Reason.
func judge(s *proc.Snapshot, cfg *Config, t *Tree) {
	if t.Root.Comm == "conmon" {
		// процессы контейнера под conmon — забота podman stop, сами по себе не судятся
		t.Members = t.Members[:1]
		t.Storage = containerStorage(t.Root.Args)
		if t.Container = containerID(t.Root.Args); t.Container == "" {
			t.Verdict, t.Reason = Skip, "conmon без id контейнера в командной строке"
			return
		}
	}
	if a := claudeAncestor(s, t.Root); a != nil {
		t.Verdict, t.Reason = Skip, fmt.Sprintf("под живой claude pid %d", a.Pid)
		return
	}
	for _, m := range t.Members {
		if reason := memberBlock(cfg, m, s.Now); reason != "" {
			t.Verdict, t.Reason = Skip, reason
			return
		}
	}
	if dead := s.Now.Sub(t.DeadBy); dead < cfg.MinDead.D() {
		t.Verdict, t.Reason = Wait, fmt.Sprintf("сессия мертва не меньше %s, порог %s", policy.HumanAge(dead), policy.HumanAge(cfg.MinDead.D()))
		return
	}
	t.Verdict, t.Reason = Reap, fmt.Sprintf("сессия мертва не меньше %s", policy.HumanAge(s.Now.Sub(t.DeadBy)))
}

// memberBlock — почему этот процесс дерева запрещает его снимать, или "".
func memberBlock(cfg *Config, m *proc.Proc, now time.Time) string {
	switch {
	case m.Comm == "claude":
		return fmt.Sprintf("внутри claude pid %d", m.Pid)
	case m.Partial != "":
		return fmt.Sprintf("pid %d (%s) не прочитался: %s", m.Pid, m.Comm, m.Partial)
	}
	if svc := proc.ServiceUnit(m.Cgroup); svc != "" {
		return fmt.Sprintf("pid %d под сервисом %s", m.Pid, svc)
	}
	if e := cfg.excluded(m, now); e != nil {
		return fmt.Sprintf("исключение (pid %d %s): %s", m.Pid, m.Comm, e.Reason)
	}
	return ""
}

// linkPasta: сеть контейнера (pasta) идёт за его conmon той же сессии —
// уйдёт сама после podman stop, а при контейнере, который не трогаем, остаётся.
func linkPasta(trees []Tree) {
	containers := map[SessionKey]*Tree{}
	for i := range trees {
		if trees[i].Container != "" {
			containers[trees[i].Session] = &trees[i]
		}
	}
	for i := range trees {
		t := &trees[i]
		c, ok := containers[t.Session]
		if !ok || !strings.HasPrefix(t.Root.Comm, "pasta") || t.Verdict == Skip {
			continue
		}
		switch c.Verdict {
		case Reap:
			t.Verdict, t.Reason = Skip, fmt.Sprintf("сеть контейнера %s: уйдёт с podman stop", ShortID(c.Container))
		case Wait:
			t.Verdict, t.Reason = Wait, fmt.Sprintf("сеть контейнера %s: %s", ShortID(c.Container), c.Reason)
		default:
			t.Verdict, t.Reason = Skip, fmt.Sprintf("сеть контейнера %s, который не трогаем", ShortID(c.Container))
		}
	}
}

// claudeAncestor — ближайший предок-claude (живая сессия выше по дереву), или nil.
func claudeAncestor(s *proc.Snapshot, p *proc.Proc) *proc.Proc {
	const maxDepth = 64
	for q, depth := p.Ppid, 0; q > 1 && depth < maxDepth; depth++ {
		a, ok := s.Procs[q]
		if !ok {
			return nil
		}
		if a.Comm == "claude" {
			return a
		}
		q = a.Ppid
	}
	return nil
}

func childIndex(s *proc.Snapshot) map[int][]int {
	out := map[int][]int{}
	for pid, p := range s.Procs {
		out[p.Ppid] = append(out[p.Ppid], pid)
	}
	for _, c := range out {
		sort.Ints(c)
	}
	return out
}

// collect — корень и все потомки по ppid, в ширину.
func collect(s *proc.Snapshot, children map[int][]int, root int) []*proc.Proc {
	var out []*proc.Proc
	queue := []int{root}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		out = append(out, s.Procs[pid])
		queue = append(queue, children[pid]...)
	}
	return out
}

var hexID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// containerID — id контейнера из командной строки conmon ("-c <id>").
func containerID(args string) string {
	f := strings.Fields(args)
	for i := 0; i+1 < len(f); i++ {
		if f[i] == "-c" && hexID.MatchString(f[i+1]) {
			return f[i+1]
		}
	}
	return ""
}

// containerStorage — флаги --root и --runroot, с которыми podman запускал
// контейнер: conmon передаёт их в своей exit-command (--exit-command-arg X).
// Без них podman stop ищет контейнер в хранилище по умолчанию текущего окружения.
func containerStorage(args string) []string {
	var exit []string
	f := strings.Fields(args)
	for i := 0; i+1 < len(f); i++ {
		if f[i] == "--exit-command-arg" {
			exit = append(exit, f[i+1])
			i++
		}
	}
	var out []string
	for i := 0; i+1 < len(exit); i++ {
		if exit[i] == "--root" || exit[i] == "--runroot" {
			out = append(out, exit[i], exit[i+1])
			i++
		}
	}
	return out
}

// ShortID — первые 12 знаков id контейнера, как у podman ps.
func ShortID(id string) string {
	const n = 12
	if len(id) > n {
		return id[:n]
	}
	return id
}

func sortedKeys[V any](m map[int]V) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}
