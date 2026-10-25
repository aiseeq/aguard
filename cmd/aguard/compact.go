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
		since := fl.Duration("since", 7*24*time.Hour, "за какой срок")
		parse(fl, rest)
		st := compact.NewStore(a.dir("compact"))
		r, err := compact.Collect(func(fn func(*compact.Record)) (int, error) {
			return st.Read(time.Now().Add(-*since), fn)
		})
		if err != nil {
			return err
		}
		return r.Print(a.out)
	}
	usage()
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
	for _, in := range infos {
		if !a.reg.IsAlive(in.Pid, in.ProcStart) {
			continue
		}
		c := j.Compact(&cfg.Restart, &cfg.Compact, in.Pid, now)
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
			a.compactNote(&seen[i], c.Against)
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

// compactNote — запись журнала в журнал демона и в учёт.
func (a *app) compactNote(r *compact.Record, against []restart.Reason) {
	switch r.Kind {
	case compact.KindWould:
		a.log.Info("aguard compact: сжал бы", "session", r.Name, "tokens", r.Tokens, "request", r.Request)
		a.note("compact", r.Kind, r.Name, "", r.Tokens, "tokens")
	case compact.KindWouldNot:
		texts := make([]string, 0, len(against))
		for _, x := range against {
			texts = append(texts, x.Code+": "+x.Text)
		}
		a.log.Info("aguard compact: не стал бы", "session", r.Name, "tokens", r.Tokens, "against", strings.Join(texts, "; "))
		a.note("compact", r.Kind, strings.Join(r.Against, ","), "", r.Tokens, "tokens")
	case compact.KindOutcome:
		a.log.Info("aguard compact: исход", "session", r.Name, "decision", r.Decision, "outcome", r.Outcome,
			"after", time.Duration(r.AfterS)*time.Second, "cache_read", r.CacheRead, "trigger", r.Trigger)
		a.note("compact", r.Decision+"_"+r.Outcome, r.Name, "", r.Tokens, "tokens")
	}
}
