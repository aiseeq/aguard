// Наблюдатель memory: пики памяти сессий агентов в дневные файлы
// ~/.local/state/aguard/memory/<день>.jsonl. Потолок управляется отдельно:
// отчёт строится командой aguard memory report; жёсткий потолок ведёт ceiling.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"time"

	"github.com/aiseeq/aguard/internal/daily"
	"github.com/aiseeq/aguard/internal/memwatch"
	"github.com/aiseeq/aguard/internal/policy"
)

func (a *app) memoryCmd(args []string) error {
	cmd, rest := sub(args)
	if cmd == "ceiling" {
		return a.ceilingCmd(rest)
	}
	cfg, err := a.config()
	if err != nil {
		return err
	}
	switch cmd {
	case "tick":
		fl := flag.NewFlagSet("memory tick", flag.ContinueOnError)
		dry := fl.Bool("dry-run", false, "напечатать записи, ничего не писать")
		parse(fl, rest)
		mw := memwatch.NewWatcher(a.reg)
		defer a.closeWatcher(mw)
		return a.memoryRound(mw, &cfg.Memory, *dry)
	case "report":
		fl := flag.NewFlagSet("memory report", flag.ContinueOnError)
		since := fl.Duration("since", 7*24*time.Hour, "за какой срок")
		parse(fl, rest)
		st := a.memoryStore()
		r, err := memwatch.Collect(func(fn func(*memwatch.Sample)) (int, error) {
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

func (a *app) memoryStore() daily.Store[memwatch.Sample] { return memwatch.NewStore(a.dir("memory")) }

// memoryRound — один проход: записи по всем сессиям, сжатие прошлых дней.
func (a *app) memoryRound(mw *memwatch.Watcher, cfg *memwatch.Config, dry bool) error {
	now := time.Now()
	samples, err := mw.Pass(cfg, now)
	if err != nil { // ошибка одного scope: остальные записи всё равно пишем
		a.warn("memory", "часть сессий не прочиталась", "err", err)
	}
	if dry {
		enc := json.NewEncoder(a.out)
		for i := range samples {
			if err := enc.Encode(&samples[i]); err != nil {
				return fmt.Errorf("вывод: %w", err)
			}
		}
		return nil
	}
	st := a.memoryStore()
	if err := st.Append(samples); err != nil {
		return err
	}
	var top memwatch.Sample
	for i := range samples {
		if samples[i].Peak > top.Peak {
			top = samples[i]
		}
	}
	a.log.Debug("aguard memory", "sessions", len(samples), "max", top.Name, "peak", policy.HumanBytes(top.Peak))
	return st.Rotate(now, cfg.Keep.D())
}
