// Наблюдатель waits: отказы и ожидания агентов по транскриптам ~/.claude/projects — хуки, снятые по таймауту,
// ошибки API, сжатия контекста — в дневные файлы ~/.local/state/aguard/waits/<день>.jsonl. Смещения по
// транскриптам — в offsets.json там же. Ничего не ограничивает.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"path/filepath"
	"time"

	"github.com/aiseeq/aguard/internal/waits"
)

func (a *app) waitsCmd(args []string) error {
	cmd, rest := sub(args)
	cfg, err := a.config()
	if err != nil {
		return err
	}
	switch cmd {
	case "tick":
		fl := flag.NewFlagSet("waits tick", flag.ContinueOnError)
		dry := fl.Bool("dry-run", false, "напечатать события, ничего не писать (смещения тоже)")
		parse(fl, rest)
		return a.waitsRound(&cfg.Waits, *dry)
	case "report":
		fl := flag.NewFlagSet("waits report", flag.ContinueOnError)
		since := fl.Duration("since", 24*time.Hour, "за какой срок")
		parse(fl, rest)
		st := waits.NewStore(a.dir("waits"))
		r, err := waits.Collect(func(fn func(*waits.Event)) (int, error) {
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

// waitsRound — один проход: дописанное в транскриптах и кусок догрузки, затем сжатие прошлых дней.
// Смещения сохраняются только после записи событий: сбой записи — те же строки перечитаются в следующий проход.
func (a *app) waitsRound(cfg *waits.Config, dry bool) error {
	now := time.Now()
	offsets := filepath.Join(a.dir("waits"), "offsets.json")
	st, err := waits.LoadState(offsets)
	if err != nil {
		return err
	}
	evs, err := cfg.Tail(a.projects).Pass(st, now)
	if err != nil { // битые строки и ошибки отдельных файлов: остальные события всё равно пишем
		a.warn("waits", "часть транскриптов не прочиталась", "err", err)
	}
	if dry {
		enc := json.NewEncoder(a.out)
		for i := range evs {
			if err := enc.Encode(&evs[i]); err != nil {
				return fmt.Errorf("вывод: %w", err)
			}
		}
		return nil
	}
	store := waits.NewStore(a.dir("waits"))
	if err := store.Append(evs); err != nil {
		return err
	}
	if err := st.Save(offsets); err != nil {
		return err
	}
	a.log.Debug("aguard waits", "events", len(evs), "files", len(st.Files))
	return store.Rotate(now, cfg.Keep.D())
}
