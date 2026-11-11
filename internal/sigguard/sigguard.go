// Package sigguard разбирает и сводит отказы agent-signal-guard.
package sigguard

import (
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type Deny struct {
	Kind, Arg, First, Last, Unit, Root, Comm, Pcomm, TargetComm, TargetUnit, Mode string
	Count, PID, PPID, Target                                                      int
	Targets                                                                       int
	Killed                                                                        bool
	HasTargets                                                                    bool
}

// Parse принимает только строки deny и would-deny с полями key=value.
func Parse(msg string) (Deny, bool) {
	var d Deny
	fields := splitFields(msg)
	if len(fields) < 2 || (fields[0] != "deny" && fields[0] != "would-deny") {
		return d, false
	}
	d.Mode = "enforce"
	if fields[0] == "would-deny" {
		d.Mode = "log"
	}
	values := make(map[string]string, len(fields)-1)
	for _, f := range fields[1:] {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		values[k] = strings.Trim(v, "\"")
	}
	get := func(k string) string { return values[k] }
	d.Kind, d.Arg, d.First, d.Last, d.Unit, d.Root = get("kind"), get("arg"), get("first"), get("last"), get("unit"), get("root")
	d.Comm, d.Pcomm, d.TargetComm, d.TargetUnit = get("comm"), get("pcomm"), get("target_comm"), get("target_unit")
	if !parseNumbers(&d, get) {
		return Deny{}, false
	}
	if d.Kind == "" || d.Arg == "" || d.Unit == "" || d.Root == "" || d.Count < 1 || d.PID < 1 {
		return Deny{}, false
	}
	return d, true
}

func splitFields(s string) []string {
	var fields []string
	start, quoted := -1, false
	for i, r := range s {
		if r == '"' {
			quoted = !quoted
		}
		if unicode.IsSpace(r) && !quoted {
			if start >= 0 {
				fields = append(fields, s[start:i])
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		fields = append(fields, s[start:])
	}
	return fields
}

func parseNumbers(d *Deny, get func(string) string) bool {
	parse := func(key string) (int, bool) { n, err := strconv.Atoi(get(key)); return n, err == nil }
	var ok bool
	if d.Count, ok = parse("count"); !ok {
		return false
	}
	if d.PID, ok = parse("pid"); !ok {
		return false
	}
	if get("ppid") != "" {
		if d.PPID, ok = parse("ppid"); !ok {
			return false
		}
	}
	if get("target") != "" {
		if d.Target, ok = parse("target"); !ok {
			return false
		}
	}
	if get("targets") != "" {
		if d.Targets, ok = parse("targets"); !ok || d.Targets < 0 {
			return false
		}
		d.HasTargets = true
	}
	if get("killed") != "" {
		killed, valid := parse("killed")
		if !valid || (killed != 0 && killed != 1) {
			return false
		}
		d.Killed = killed == 1
	}
	return true
}

type Group struct {
	Root         string
	Denies       []Deny
	Count, Lines int
	Started      time.Time
	Unparsed     bool
	Raw          string
}
type Batcher struct {
	Window time.Duration
	groups map[string]*Group
}

func NewBatcher(window time.Duration) *Batcher {
	return &Batcher{Window: window, groups: map[string]*Group{}}
}
func (b *Batcher) Add(d Deny, at time.Time) []Group {
	out := b.Flush(at)
	g := b.groups[d.Root]
	if g == nil {
		g = &Group{Root: d.Root, Started: at}
		b.groups[d.Root] = g
	}
	g.Denies = append(g.Denies, d)
	g.Count += d.Count
	g.Lines++
	return out
}

func (b *Batcher) AddUnparsed(raw string, at time.Time) []Group {
	out := b.Flush(at)
	const key = "\x00unparsed"
	g := b.groups[key]
	if g == nil {
		g = &Group{Root: "unparsed", Started: at, Unparsed: true, Raw: raw}
		b.groups[key] = g
	}
	g.Count++
	g.Lines++
	return out
}
func (b *Batcher) Flush(now time.Time) []Group {
	var out []Group
	roots := slices.Sorted(maps.Keys(b.groups))
	for _, root := range roots {
		g := b.groups[root]
		if now.Sub(g.Started) >= b.Window {
			out = append(out, *g)
			delete(b.groups, root)
		}
	}
	return out
}
