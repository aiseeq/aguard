package main

import (
	"strings"

	"github.com/aiseeq/aguard/internal/codexwatch"
	"github.com/aiseeq/aguard/internal/guide"
	"github.com/aiseeq/aguard/internal/ioroute"
	"github.com/aiseeq/aguard/internal/queue"
	"github.com/aiseeq/aguard/internal/remotebroken"
)

func queueGuideKind(kind string) guide.Kind {
	switch kind {
	case queue.KindCrit:
		return "queue.crit"
	case queue.KindCritCopy:
		return "queue.crit_copy"
	case queue.KindHigh:
		return "queue.high"
	case queue.KindHighAdmin:
		return "queue.high_admin"
	case queue.KindWake:
		return "queue.wake"
	case queue.KindWakeStuck:
		return "queue.wake_stuck"
	case queue.KindWakeAdmin:
		return "queue.wake_admin"
	case queue.KindBack:
		return "queue.back"
	case queue.KindBackLost:
		return "queue.back_lost"
	case queue.KindUnreachable:
		return "queue.unreachable"
	default:
		return "queue.other"
	}
}

func bgtaskGuideKind(auto bool, wait string) guide.Kind {
	switch {
	case auto:
		return "bgtask.stalled"
	case wait != "":
		return "bgtask.wait"
	default:
		return "bgtask.other"
	}
}

func sigguardGuideKind(unparsed bool) guide.Kind {
	if unparsed {
		return "sigguard.unparsed"
	}
	return "sigguard.denied"
}

func codexGuideKind(kind codexwatch.Kind) guide.Kind {
	switch kind {
	case codexwatch.KindWarn:
		return "codex.warn"
	case codexwatch.KindFull:
		return "codex.full"
	case codexwatch.KindBlocked:
		return "codex.blocked"
	default:
		return "codex.other"
	}
}

func remoteGuideKind(n remotebroken.Notice) guide.Kind {
	switch {
	case n.Summary:
		return "remote.summary"
	case strings.HasPrefix(n.Key, "first:"):
		return "remote.copy"
	case n.Key != "":
		return "remote.owner"
	default:
		return "remote.other"
	}
}

func ioGuideKind(why ioroute.Why) guide.Kind {
	switch why {
	case ioroute.WhyUrgent:
		return "io.urgent"
	case ioroute.WhyOffline:
		return "io.offline"
	case ioroute.WhyVictim:
		return "io.victim"
	case ioroute.WhyNoOwner:
		return "io.no_owner"
	case ioroute.WhyMachine:
		return "io.machine"
	case ioroute.WhyCross:
		return "io.cross"
	case ioroute.WhyAfter:
		return "io.after_pause"
	default:
		return "io.other"
	}
}

func nightGuideKind(key string) guide.Kind {
	switch {
	case strings.HasPrefix(key, "dream:"):
		return "night.dream"
	case strings.HasPrefix(key, "dream-pause:"):
		return "night.dream_pause"
	case strings.HasPrefix(key, "digest:"):
		return "night.digest"
	case strings.HasPrefix(key, "trust:"):
		return "night.trust"
	case strings.HasPrefix(key, "dream-fallback-blocked:"):
		return "night.fallback_blocked"
	case strings.HasPrefix(key, "dream-fallback-end:"):
		return "night.fallback_end"
	case strings.HasPrefix(key, "dream-fallback-state:"):
		return "night.fallback_state"
	case strings.HasPrefix(key, "dream-fallback:"):
		return "night.fallback"
	default:
		return "night.other"
	}
}
