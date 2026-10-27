// Наблюдатель compact: своё сжатие сессий в простое. В режиме log пишет, какую сессию сжал бы сейчас (или почему не
// стал бы), и сверяет решение с тем, что случилось потом: сжало ли встроенное сжатие, умер ли кэш к возвращению
// человека. Журнал — ~/.local/state/aguard/compact/<день>.jsonl, решения без исхода — state.json там же.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/activity"
	"github.com/aiseeq/aguard/internal/cllog"
	"github.com/aiseeq/aguard/internal/compact"
	"github.com/aiseeq/aguard/internal/config"
	"github.com/aiseeq/aguard/internal/restart"
)

// compactLostAfter — решение без исхода дольше этого — сессия пропала.
const compactLostAfter = 7 * 24 * time.Hour

func (a *app) compactCmd(args []string) error {
	cmd, rest := sub(args)
	cfg, err := a.config()
	if err != nil {
		return err
	}
	switch cmd {
	case "tick":
		fl := flag.NewFlagSet("compact tick", flag.ContinueOnError)
		dry := fl.Bool("dry-run", false, "напечатать проверку каждой интерактивной сессии, ничего не писать")
		parse(fl, rest)
		return a.compactRound(restart.NewJudge(a.home), &cfg, *dry)
	case "report":
		fl := flag.NewFlagSet("compact report", flag.ContinueOnError)
		since := fl.Duration("since", 7*24*time.Hour, "за какой срок (без -json)")
		asJSON := fl.Bool("json", false, "JSON для sleepstats (формат v1, compact.Summary): сутки, неделя, месяц")
		atText := fl.String("at", "", "момент отчёта, RFC3339 (по умолчанию сейчас)")
		parse(fl, rest)
		at := time.Now()
		if *atText != "" {
			if at, err = time.Parse(time.RFC3339, *atText); err != nil {
				return fmt.Errorf("-at: %w", err)
			}
		}
		if *asJSON {
			return a.compactSummary(at)
		}
		r, err := a.compactReport(at.Add(-*since), at)
		if err != nil {
			return err
		}
		return r.Print(a.out)
	}
	usage()
	return nil
}

// compactReport — сводка журнала за [from, to) и что cl сделал за то же время по своему журналу.
func (a *app) compactReport(from, to time.Time) (*compact.Report, error) {
	st := compact.NewStore(a.dir("compact"))
	r, err := compact.Collect(func(fn func(*compact.Record)) (int, error) {
		return st.Read(from, func(r *compact.Record) {
			if r.At.Before(to) {
				fn(r)
			}
		})
	})
	if err != nil {
		return r, err
	}
	done, _, err := cllog.Read(cllog.Path(a.home), from, to)
	r.AddDone(done)
	return r, err
}

// compactSummary — сводки за сутки, неделю и месяц до at в JSON.
func (a *app) compactSummary(at time.Time) error {
	s := compact.Summary{V: compact.Version, At: at, Windows: map[string]*compact.Report{}}
	for _, w := range activity.Windows {
		r, err := a.compactReport(at.Add(-w.Len), at)
		if err != nil {
			return err
		}
		s.Windows[w.Name] = r
	}
	enc := json.NewEncoder(a.out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(&s); err != nil {
		return fmt.Errorf("вывод: %w", err)
	}
	return nil
}

// compactRound — один проход по живым интерактивным сессиям реестра: исходы прошлых решений и новые решения.
func (a *app) compactRound(j *restart.Judge, cfg *config.Config, dry bool) error {
	if cfg.Compact.Mode == restart.CompactOff {
		return nil
	}
	now := time.Now()
	infos, err := a.reg.List()
	if err != nil { // часть реестра не прочиталась: остальные сессии проверяем
		a.warn("compact", "реестр сессий", "err", err)
	}
	statePath := filepath.Join(a.dir("compact"), "state.json")
	tr, err := compact.Load(statePath)
	if err != nil {
		return err
	}
	var recs []compact.Record
	enc := json.NewEncoder(a.out)
	for i := range infos {
		in := &infos[i]
		if !a.reg.IsAlive(in.Pid, in.ProcStart) {
			continue
		}
		c := j.Compact(&cfg.Restart, &cfg.Compact, restart.Request{Pid: in.Pid, Name: in.Name}, now)
		if c.SessionKind != "interactive" {
			continue
		}
		if dry {
			if err := enc.Encode(&c); err != nil {
				return fmt.Errorf("вывод: %w", err)
			}
			continue
		}
		seen := tr.Observe(c, now)
		for i := range seen {
			a.compactNote(&seen[i], &c)
		}
		recs = append(recs, seen...)
	}
	if dry {
		return nil
	}
	lost := tr.Expire(now, compactLostAfter)
	for i := range lost {
		a.compactNote(&lost[i], nil)
	}
	recs = append(recs, lost...)
	store := compact.NewStore(a.dir("compact"))
	if err := store.Append(recs); err != nil {
		return err
	}
	if err := tr.Save(statePath); err != nil {
		return err
	}
	return store.Rotate(now, cfg.Compact.Keep.D())
}

// compactNote — запись журнала в журнал демона и в учёт; c — проверка, из которой запись (доводы против), у исходов
// пропавших сессий — nil.
func (a *app) compactNote(r *compact.Record, c *restart.CompactCheck) {
	switch r.Kind {
	case compact.KindWould, compact.KindWouldReset:
		a.log.Info("aguard compact: "+kindText[r.Kind], "session", r.Name, "tokens", r.Tokens, "request", r.Request,
			"rewrite_micro_usd", r.RewriteMicroUSD, "reply", r.Reply)
		a.note("compact", r.Kind, r.Name, "", r.Tokens, "tokens")
	case compact.KindWouldNot, compact.KindWouldNotReset:
		against := c.Against
		if r.Kind == compact.KindWouldNotReset {
			against = c.Reset.Against
		}
		texts := make([]string, 0, len(against))
		for _, x := range against {
			texts = append(texts, x.Code+": "+x.Text)
		}
		a.log.Info("aguard compact: "+kindText[r.Kind], "session", r.Name, "tokens", r.Tokens, "against", strings.Join(texts, "; "))
		a.note("compact", r.Kind, strings.Join(r.Against, ","), "", r.Tokens, "tokens")
	case compact.KindOutcome:
		a.log.Info("aguard compact: исход", "session", r.Name, "decision", r.Decision, "outcome", r.Outcome,
			"after", time.Duration(r.AfterS)*time.Second, "cache_read", r.CacheRead, "trigger", r.Trigger, "prompt", r.Prompt)
		a.note("compact", r.Decision+"_"+r.Outcome, r.Name, "", r.Tokens, "tokens")
	}
}

// kindText — решения журнала сжатия словами.
var kindText = map[string]string{
	compact.KindWould: "сжал бы", compact.KindWouldNot: "не стал бы",
	compact.KindWouldReset: "сбросил бы", compact.KindWouldNotReset: "не стал бы сбрасывать",
}
