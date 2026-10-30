// Наблюдатель agentwaits: ожидания, которые агенты заводят сами (циклы sleep, wfor, фоновые команды до уведомления,
// письмо другой сессии до ответа, у бойцов codex — опросы write_stdin), по транскриптам Claude и codex — в дневные
// файлы ~/.local/state/aguard/agentwaits/<день>.jsonl, смещения — в offsets.json там же. Сводка с очередью graft —
// `aguard agentwaits report`, -json — для страницы Сна. Ничего не ограничивает.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"path/filepath"
	"time"

	"github.com/aiseeq/akit/session"

	"github.com/aiseeq/aguard/internal/agentwait"
	"github.com/aiseeq/aguard/internal/names"
)

func (a *app) agentwaitsCmd(args []string) error {
	cmd, rest := sub(args)
	cfg, err := a.config()
	if err != nil {
		return err
	}
	switch cmd {
	case "tick":
		fl := flag.NewFlagSet("agentwaits tick", flag.ContinueOnError)
		dry := fl.Bool("dry-run", false, "напечатать ожидания, ничего не писать (смещения тоже)")
		parse(fl, rest)
		return a.agentwaitsRound(&cfg.Agentwaits, *dry)
	case "report":
		fl := flag.NewFlagSet("agentwaits report", flag.ContinueOnError)
		asJSON := fl.Bool("json", false, "сводка JSON (окна day и week) для страницы Сна")
		window := fl.String("window", "week", "окно текста: day или week")
		parse(fl, rest)
		now := time.Now()
		st := agentwait.NewStore(a.dir("agentwaits"))
		graft, err := agentwait.GraftStats(a.home)
		if err != nil {
			return err
		}
		s, err := agentwait.Collect(now, func(fn func(*agentwait.Wait)) (int, error) {
			return st.Read(now.Add(-7*24*time.Hour), fn)
		}, graft)
		if err != nil {
			return err
		}
		if *asJSON {
			enc := json.NewEncoder(a.out)
			enc.SetIndent("", "  ")
			if err := enc.Encode(s); err != nil {
				return fmt.Errorf("вывод: %w", err)
			}
			return nil
		}
		return s.Print(a.out, *window)
	}
	usage()
	return nil
}

// agentwaitsRound — один проход: дописанное в транскриптах, затем сжатие прошлых дней. Смещения сохраняются только
// после записи ожиданий: сбой записи — те же строки перечитаются в следующий проход.
func (a *app) agentwaitsRound(cfg *agentwait.Config, dry bool) error {
	now := time.Now()
	offsets := filepath.Join(a.dir("agentwaits"), "offsets.json")
	st, err := agentwait.LoadState(offsets)
	if err != nil {
		return err
	}
	table, err := names.ReadTable(filepath.Join(a.home, names.File), a.home)
	if err != nil {
		return err
	}
	p := &agentwait.Pass{
		Projects: session.ProjectsDir(a.home), Workers: filepath.Join(a.home, ".local", "state", "worker"),
		Backfill: cfg.Backfill.D(), Budget: cfg.BackfillMB << 20, MaxOpen: cfg.MaxOpen.D(),
		Name: func(cwd string) string { return names.For(cwd, a.home, table) },
	}
	ws, err := p.Run(st, now)
	if err != nil { // битые строки и ошибки отдельных файлов: остальные ожидания всё равно пишем
		a.warn("agentwaits", "часть транскриптов не прочиталась", "err", err)
	}
	if dry {
		enc := json.NewEncoder(a.out)
		for i := range ws {
			if err := enc.Encode(&ws[i]); err != nil {
				return fmt.Errorf("вывод: %w", err)
			}
		}
		return nil
	}
	store := agentwait.NewStore(a.dir("agentwaits"))
	if err := store.Append(ws); err != nil {
		return err
	}
	if err := st.Save(offsets); err != nil {
		return err
	}
	a.log.Debug("aguard agentwaits", "waits", len(ws), "files", len(st.Files))
	return store.Rotate(now, cfg.Keep.D())
}
