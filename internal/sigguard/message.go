package sigguard

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/aiseeq/aguard/internal/cpuwatch"
	"golang.org/x/sys/unix"
)

// UnparsedMessage — письмо о строках отказа, которые не разобрались (сменился формат): число и первая строка.
func UnparsedMessage(g Group) string {
	raw := []rune(g.Raw)
	if len(raw) > 300 {
		raw = raw[:300]
	}
	return fmt.Sprintf("aguard: авария — agent-signal-guard: не разобрано строк %d; первая строка:\n%s", g.Lines, string(raw))
}

// Message — текст письма об аварии по группе отказов: виды с суммами, отправители, первая цель, SIGKILL ядра;
// workerID — задание бойца-отправителя.
func Message(g Group, workerID string) string {
	if len(g.Denies) == 0 {
		return ""
	}
	first := killedMessage(g.Denies)
	kinds := kindSummary(g.Denies)
	senders := senders(g.Denies)
	target := firstTarget(g.Denies)
	verb := "отбил"
	for i := range g.Denies {
		if g.Denies[i].Mode == "log" {
			verb = "в режиме log пропустил (действие прошло)"
			break
		}
	}
	extraTargets := targets(g.Denies)
	msg := fmt.Sprintf("aguard: авария — %sagent-signal-guard %s %s; отправители %s%s%s. Агенту нельзя трогать чужие процессы — найдите, что это послало.", first, verb, strings.Join(kinds, "; "), senders, target, extraTargets)
	if workerID != "" {
		msg += fmt.Sprintf(" Задание worker %s.", workerID)
	}
	if hint := workerStopHint(g.Denies); hint != "" {
		msg += "\n" + hint
	}
	return msg
}

func workerStopHint(denies []Deny) string {
	for i := range denies {
		d := &denies[i]
		if d.Kind != "signal" {
			continue
		}
		targetIDs := fighterJobIDs(d.TargetUnit)
		if len(targetIDs) == 0 {
			continue
		}
		targetID := targetIDs[0]
		senderIDs := fighterJobIDs(d.Unit)
		if len(senderIDs) > 0 && slices.Contains(senderIDs, targetID) {
			continue
		}
		return fmt.Sprintf("Бойца снимает его мастер: `worker stop %s`; чужого бойца — `worker stop --other-master %s` по слову пользователя.", targetID, targetID)
	}
	return ""
}

func fighterJobIDs(unit string) []string {
	unit = cpuwatch.UnescapeUnit(unit)
	if !strings.HasSuffix(unit, ".service") {
		return nil
	}
	if !strings.HasPrefix(unit, "worker-job-") && !strings.HasPrefix(unit, "worker-round-") {
		return nil
	}
	return cpuwatch.JobIDs(unit)
}

type sigguardKindTotal struct {
	kind, arg, label string
	count            int
}

func kindSummary(denies []Deny) []string {
	totals := map[string]*sigguardKindTotal{}
	for i := range denies {
		d := &denies[i]
		key := d.Kind + "\x00" + d.Arg
		t := totals[key]
		if t == nil {
			t = &sigguardKindTotal{kind: d.Kind, arg: d.Arg, label: action(d)}
			totals[key] = t
		}
		t.count += d.Count
	}
	keys := slices.Sorted(maps.Keys(totals))
	ordered := make([]*sigguardKindTotal, 0, len(keys))
	for _, key := range keys {
		ordered = append(ordered, totals[key])
	}
	slices.SortFunc(ordered, func(a, b *sigguardKindTotal) int {
		if a.count != b.count {
			return b.count - a.count
		}
		if a.kind != b.kind {
			return strings.Compare(a.kind, b.kind)
		}
		return strings.Compare(a.arg, b.arg)
	})
	limit := min(5, len(ordered))
	out := make([]string, 0, limit+1)
	for _, t := range ordered[:limit] {
		out = append(out, fmt.Sprintf("%s ×%d", t.label, t.count))
	}
	if len(ordered) > limit {
		out = append(out, fmt.Sprintf("и ещё %d видов", len(ordered)-limit))
	}
	return out
}

func action(d *Deny) string {
	switch d.Kind {
	case "signal":
		if n, err := strconv.Atoi(d.Arg); err == nil {
			if name := unix.SignalName(syscall.Signal(n)); name != "" {
				return "signal " + name
			}
		}
	case "cgroup_write":
		if d.Arg == "1" {
			return "перенос процесса в cgroup " + d.TargetUnit
		}
		if d.Arg == "0" {
			return "запись в чужую cgroup"
		}
	}
	return d.Kind + " " + d.Arg
}

func killedMessage(denies []Deny) string {
	for i := range denies {
		d := &denies[i]
		if d.Killed {
			return fmt.Sprintf("ядро убило %s(%d) SIGKILL после %d чужих целей (код 137 — у этого процесса, а не у всей команды агента); ", d.Comm, d.PID, d.Targets)
		}
	}
	return ""
}

func senders(denies []Deny) string {
	byID := map[string]*Deny{}
	for i := range denies {
		d := &denies[i]
		key := fmt.Sprintf("%s\x00%d", d.Comm, d.PID)
		if byID[key] == nil {
			byID[key] = d
		}
	}
	keys := slices.Sorted(maps.Keys(byID))
	limit := min(3, len(keys))
	parts := make([]string, 0, limit+1)
	for _, key := range keys[:limit] {
		d := byID[key]
		part := fmt.Sprintf("%s(%d)", d.Comm, d.PID)
		if d.PPID != 0 || d.Pcomm != "" {
			part += fmt.Sprintf(" (родитель %s(%d))", d.Pcomm, d.PPID)
		}
		parts = append(parts, part)
	}
	if len(keys) > limit {
		return strings.Join(parts, ", ") + fmt.Sprintf(" и ещё %d", len(keys)-limit)
	}
	return strings.Join(parts, ", ")
}

func firstTarget(denies []Deny) string {
	for i := range denies {
		d := &denies[i]
		if d.Target != 0 || d.TargetComm != "" {
			return fmt.Sprintf("; первая цель %s(%d) в %s", d.TargetComm, d.Target, d.TargetUnit)
		}
	}
	return ""
}

func targets(denies []Deny) string {
	maxTargets := 0
	count := 0
	for i := range denies {
		d := &denies[i]
		if d.HasTargets {
			count++
			maxTargets = max(maxTargets, d.Targets)
		}
	}
	if count > 1 {
		return fmt.Sprintf("; разных целей не меньше %d", maxTargets)
	}
	if count == 1 {
		return fmt.Sprintf("; разных целей %d", maxTargets)
	}
	return ""
}
