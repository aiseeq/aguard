// Наблюдатель said: реплики пользователя в сессиях лида, админов и мастеров размечает модель (правило, решение,
// обещание или оперативное); первые три в режиме write дописываются в указатель сказанного ~/.claude/said.tsv.
// Каждый вызов модели с находками и расходом — в дневные файлы ~/.local/state/aguard/said/<день>.jsonl.
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"path/filepath"
	"time"

	"github.com/aiseeq/aguard/internal/names"
	"github.com/aiseeq/aguard/internal/said"
)

// saidNames — имя событиям без строки agent-name в прочитанном куске (сессия началась до первого прохода): по реестру
// живых сессий, иначе по таблице имён cl и папке запуска.
func (a *app) saidNames(evs []said.Event) {
	byID := map[string]string{}
	if infos, err := a.reg.List(); err != nil {
		a.warn("said", "реестр сессий", "err", err)
	} else {
		for i := range infos {
			byID[infos[i].SessionID] = infos[i].Name
		}
	}
	table, err := names.ReadTable(filepath.Join(a.home, names.File), a.home)
	if err != nil {
		a.warn("said", "таблица имён", "err", err)
	}
	for i := range evs {
		if ev := &evs[i]; ev.Name == "" {
			ev.Name = cmp.Or(byID[ev.Session], names.For(ev.Cwd, a.home, table))
		}
	}
}

func (a *app) saidCmd(ctx context.Context, args []string) error {
	cmd, rest := sub(args)
	cfg, err := a.config()
	if err != nil {
		return err
	}
	switch cmd {
	case "tick":
		fl := flag.NewFlagSet("said tick", flag.ContinueOnError)
		dry := fl.Bool("dry-run", false, "напечатать реплики в очереди, модель не звать и ничего не писать")
		parse(fl, rest)
		return a.saidRound(ctx, &cfg.Said, *dry)
	case "report":
		fl := flag.NewFlagSet("said report", flag.ContinueOnError)
		since := fl.Duration("since", 7*24*time.Hour, "за какой срок")
		parse(fl, rest)
		st := said.NewStore(a.dir("said"))
		days, bad, err := said.Summary(func(fn func(*said.Record)) (int, error) {
			return st.Read(time.Now().Add(-*since), fn)
		})
		if err != nil {
			return err
		}
		if bad > 0 {
			a.warn("said", "битые строки журнала", "count", bad)
		}
		return said.Print(a.out, days)
	}
	usage()
	return nil
}

// saidRound — один проход: новые реплики в очередь, до Batch из очереди — модели. Состояние сохраняется после
// записи журнала: сбой записи — те же реплики размечаются снова.
func (a *app) saidRound(ctx context.Context, cfg *said.Config, dry bool) error {
	if cfg.Mode == said.ModeOff {
		return nil
	}
	now := time.Now()
	path := filepath.Join(a.dir("said"), "state.json")
	st, err := said.LoadState(path)
	if err != nil {
		return err
	}
	evs, err := said.NewTail(a.projects).Pass(st.Tail, now)
	if err != nil { // ошибки отдельных файлов: остальные реплики всё равно размечаем
		a.warn("said", "часть транскриптов не прочиталась", "err", err)
	}
	a.saidNames(evs)
	cfg.Collect(st, evs)
	if dry {
		enc := json.NewEncoder(a.out)
		for i := range st.Pending {
			if err := enc.Encode(&st.Pending[i]); err != nil {
				return fmt.Errorf("вывод: %w", err)
			}
		}
		return nil
	}
	recs, err := a.saidClassify(ctx, cfg, st)
	if err != nil {
		return err
	}
	store := said.NewStore(a.dir("said"))
	if err := store.Append(recs); err != nil {
		return err
	}
	if err := st.Save(path); err != nil {
		return err
	}
	a.log.Debug("aguard said", "replies", len(recs), "queue", len(st.Pending))
	return store.Rotate(now, cfg.Keep.D())
}

// saidClassify — до Batch реплик из очереди — модели, находки в режиме write — в указатель. В очереди остаются
// неразмеченные: сверх Batch, с непригодным ответом модели (до Retries попыток), с несохранённой записью и все, если
// модель не ответила: тогда проход кончается и до pause модель не зовётся — лимит или сбой мощности одинаковы для
// всех реплик, попытки не тратятся.
func (a *app) saidClassify(ctx context.Context, cfg *said.Config, st *said.State) ([]said.Record, error) {
	if len(st.Pending) == 0 || time.Now().Before(st.Pause) {
		return nil, nil
	}
	model := cfg.OneShot()
	prior, bad, err := said.Read(cfg.File)
	if err != nil {
		return nil, err
	}
	if bad > 0 {
		a.warn("said", "битые строки указателя", "file", cfg.File, "count", bad)
	}
	var recs []said.Record
	var left []said.Pending
	down := false
	for i := range st.Pending {
		p := &st.Pending[i]
		if i >= cfg.Batch || down || ctx.Err() != nil {
			left = append(left, *p)
			continue
		}
		p.Attempts++
		rec, entries := cfg.Classify(ctx, model, p, a.home, prior, time.Now())
		switch {
		case rec.Down && ctx.Err() != nil: // демон останавливается: вызов отменён, модель ни при чём — без паузы
			p.Attempts--
			left = append(left, *p)
			continue // в журнал не идёт: вызова модели по сути не было
		case rec.Down:
			a.warn("said", "модель не ответила, очередь ждёт", "queue", len(st.Pending), "err", rec.Err)
			p.Attempts--
			down, st.Pause = true, time.Now().Add(cfg.Pause.D())
			left = append(left, *p)
		case rec.Err != "":
			a.warn("said", "модель не разметила реплику", "ref", rec.Ref, "attempt", p.Attempts, "err", rec.Err)
			if p.Attempts < cfg.Retries {
				left = append(left, *p)
			}
		case cfg.Mode == said.ModeWrite && len(entries) > 0:
			if err := said.Append(cfg.File, entries); err != nil { // реплика остаётся в очереди
				a.warn("said", "указатель не записан", "file", cfg.File, "err", err)
				rec.Err, rec.Items = err.Error(), nil
				left = append(left, *p)
			} else {
				rec.Written = true
				prior = append(prior, entries...)
			}
		}
		recs = append(recs, rec)
	}
	st.Pending = left
	return recs, nil
}
