// Наблюдатель memory: пики памяти сессий агентов в дневные файлы
// ~/.local/state/aguard/memory/<день>.jsonl. Почасовая сводка кандидатов живёт
// в ~/.local/state/aguard/memory/evictors.json.
// Потолок управляется отдельно командой ceiling.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"time"

	"github.com/aiseeq/aguard/internal/daily"
	"github.com/aiseeq/aguard/internal/evictors"
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
	case "evictors":
		fl := flag.NewFlagSet("memory evictors", flag.ContinueOnError)
		since := fl.Duration("since", 24*time.Hour, "за какой срок")
		top := fl.Int("top", 15, "сколько кандидатов показать")
		asJSON := fl.Bool("json", false, "итог JSON")
		parse(fl, rest)
		if *since <= 0 || *top < 0 {
			return fmt.Errorf("since должен быть положительным, top — неотрицательным")
		}
		st, err := evictors.Load(evictorStorePath(a), time.Now())
		if err != nil {
			return err
		}
		r := st.Report(time.Now().Add(-*since), *top)
		if *asJSON {
			rows := make([]evictorJSONRow, 0, len(r.Rows))
			for _, row := range r.Rows {
				rows = append(rows, evictorJSONRow{Cgroup: row.Cgroup, Session: row.Session, Process: row.Process,
					NetAnonB: row.Anon, MatchedSwapB: row.Matched, OtherSwapB: row.Swap, Intervals: row.Intervals, Sessions: row.Sessions})
			}
			sum := evictorJSON{From: r.From.Format(time.RFC3339), To: r.To.Format(time.RFC3339), Bad: r.Bad,
				Note: "кандидаты, не доказанная причина", Rows: rows}
			enc := json.NewEncoder(a.out)
			enc.SetIndent("", "  ")
			return enc.Encode(sum)
		}
		return r.Print(a.out)
	}
	usage()
	return nil
}

type evictorJSON struct {
	From string           `json:"from"`
	To   string           `json:"to"`
	Bad  int              `json:"bad"`
	Note string           `json:"note"`
	Rows []evictorJSONRow `json:"rows"`
}

type evictorJSONRow struct {
	Cgroup       string `json:"cgroup"`
	Session      bool   `json:"session"`
	Process      string `json:"process"`
	NetAnonB     int64  `json:"net_anon_b"`
	MatchedSwapB int64  `json:"matched_swap_b"`
	OtherSwapB   int64  `json:"other_swap_b"`
	Intervals    int    `json:"intervals"`
	Sessions     int    `json:"sessions"`
}

func (a *app) memoryStore() daily.Store[memwatch.Sample] { return memwatch.NewStore(a.dir("memory")) }

func evictorStorePath(a *app) string { return a.dir("memory/evictors.json") }

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
