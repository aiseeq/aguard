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
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/codexwatch"
	"github.com/aiseeq/aguard/internal/config"
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
		return a.saidRound(ctx, &cfg, *dry)
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
func (a *app) saidRound(ctx context.Context, c *config.Config, dry bool) error {
	cfg := &c.Said
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
	recs, err := a.saidClassify(ctx, c, st)
	if err != nil {
		return err
	}
	store := said.NewStore(a.dir("said"))
	if err := store.Append(recs); err != nil {
		return err
	}
	// Сначала состояние: при несохранённом Signal письмо уходило бы каждый проход.
	if err := st.Save(path); err != nil {
		return err
	}
	if ctx.Err() != nil { // демон останавливается: вызов мог быть отменён, о простое судить не по чему
		return nil
	}
	signal := st.Signal
	a.saidStall(c, st, now)
	if !st.Signal.Equal(signal) {
		if err := st.Save(path); err != nil {
			return err
		}
	}
	a.log.Debug("aguard said", "replies", len(recs), "queue", len(st.Pending))
	return store.Rotate(now, cfg.Keep.D())
}

// saidClassify — до Batch реплик из очереди — модели, находки в режиме write — в указатель. В очереди остаются
// неразмеченные: сверх Batch, с непригодным ответом модели (до Retries попыток), с несохранённой записью и все, если
// модель не ответила: тогда проход кончается и до pause модель не зовётся — лимит или сбой мощности одинаковы для
// всех реплик, попытки не тратятся. Отказ учётки по кредитам не конец прохода: та же реплика идёт следующей учётке.
func (a *app) saidClassify(ctx context.Context, c *config.Config, st *said.State) ([]said.Record, error) {
	cfg := &c.Said
	if len(st.Pending) == 0 || time.Now().Before(st.Pause) {
		return nil, nil
	}
	accounts, skipped := a.saidAccounts(c, st, time.Now())
	if len(accounts) == 0 {
		st.Pause = time.Now().Add(cfg.Pause.D())
		st.Waiting(time.Now(), "нет учётки OpenAI с лимитом: "+strings.Join(skipped, "; "))
		return nil, nil
	}
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
		rec, entries, refused := a.saidAsk(ctx, cfg, st, &accounts, p, prior)
		recs = append(recs, refused...)
		if !rec.Down {
			st.Answered()
		}
		switch {
		case rec.Down && ctx.Err() != nil: // демон останавливается: вызов отменён, модель ни при чём — без паузы
			p.Attempts--
			left = append(left, *p)
			continue // в журнал не идёт: вызова модели по сути не было
		case rec.Down:
			a.warn("said", "модель не ответила, очередь ждёт", "queue", len(st.Pending), "err", rec.Err)
			p.Attempts--
			down, st.Pause = true, time.Now().Add(cfg.Pause.D())
			st.Down(time.Now(), rec.Err)
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

// saidAsk — реплика первой учётке списка; отказ по кредитам метит учётку и отдаёт реплику следующей. Кончились учётки —
// запись с Down, как любой сбой модели. refused — отказы перед сменой учётки: каждый вызов идёт в журнал.
func (a *app) saidAsk(ctx context.Context, cfg *said.Config, st *said.State, accounts *[]said.Account, p *said.Pending,
	prior []said.Entry) (rec said.Record, entries []said.Entry, refused []said.Record) {
	for {
		acc := (*accounts)[0]
		rec, entries = cfg.Classify(ctx, cfg.OneShot(acc.Home), p, a.home, prior, time.Now())
		rec.Account = acc.Name
		if !rec.Down || ctx.Err() != nil || acc.Name == "" || !said.AccountOut(rec.Err) {
			return rec, entries, refused
		}
		a.warn("said", "учётка отказала по кредитам", "account", acc.Name, "err", rec.Err)
		if st.Out == nil {
			st.Out = map[string]time.Time{}
		}
		st.Out[acc.Name] = time.Now().Add(said.AccountRetry)
		if len(*accounts) == 1 {
			return rec, entries, refused
		}
		refused = append(refused, rec)
		*accounts = (*accounts)[1:]
	}
}

// saidAccounts — учётки для прохода по замерам codexwatch, в его порядке (тот же, что у бойцов worker). Битый файл
// замера убирает только свою учётку. Модель не на codex — один вызов без учётки. skipped — пропущенные с причиной.
func (a *app) saidAccounts(c *config.Config, st *said.State, now time.Time) (ok []said.Account, skipped []string) {
	if !c.Said.OnCodex() {
		return []said.Account{{}}, nil
	}
	accounts := make([]said.Account, 0, len(c.Codex.Accounts))
	var broken []string
	for _, acc := range c.Codex.Accounts {
		m, err := codexwatch.LoadState(filepath.Join(a.dir("codex"), acc.Name+".json"))
		if err != nil {
			a.warn("said", "замер учётки не прочитан, учётка пропущена", "account", acc.Name, "err", err)
			broken = append(broken, acc.Name+": замер не прочитан")
			continue
		}
		one := said.Account{Name: acc.Name, Home: acc.Home, ResetsAt: m.ResetsAt, Denied: m.Denied}
		if n := len(m.Samples); n > 0 && !m.ResetsAt.IsZero() {
			one.Known, one.Used = true, m.Samples[n-1].Used
		}
		accounts = append(accounts, one)
	}
	ok, skipped = said.Usable(accounts, st.Out, c.Said.Ceiling, now)
	skipped = append(broken, skipped...)
	if len(ok) == 0 {
		a.warn("said", "нет учётки с лимитом, очередь ждёт", "queue", len(st.Pending), "skipped", strings.Join(skipped, "; "))
	}
	return ok, skipped
}

// saidStallRepeat — повтор письма о простое, пока модель не отвечает.
const saidStallRepeat = 24 * time.Hour

// saidStall — модель не отвечает дольше Stall: письмо лиду, иначе указатель сказанного молча стоит. Не ушло — повтор на
// следующем проходе. Настоящее письмо — только в режиме write, как и запись в указатель.
func (a *app) saidStall(c *config.Config, st *said.State, now time.Time) {
	if st.DownSince.IsZero() || now.Sub(st.DownSince) < c.Said.Stall.D() || now.Sub(st.Signal) < saidStallRepeat {
		return
	}
	msg := fmt.Sprintf("aguard said: модель не отвечает с %s, очередь %d, указатель %s не пополняется. %s",
		st.DownSince.Format(time.RFC3339), len(st.Pending), c.Said.File, st.Why())
	if c.Said.Mode != said.ModeWrite {
		a.log.Info("aguard said: написал бы", "session", c.Lead, "msg", msg)
		st.Signal = now
		return
	}
	alive, err := a.reg.Alive()
	if err == nil {
		err = a.sendAlive(alive, c.Lead, c.SendAuth, msg)
	}
	if err != nil {
		a.warn("said", "письмо о простое не отправлено", "session", c.Lead, "err", err)
		return
	}
	a.log.Info("aguard said: написал", "session", c.Lead, "msg", msg)
	st.Signal = now
}
