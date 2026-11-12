package main

import (
	"context"
	"path/filepath"
	"time"

	"github.com/aiseeq/aguard/internal/disk"
)

// Маршрут находок disk (тикет disk-alert-forecast): fast — письмом хозяину, не в сети — лиду; grew, stale, следы от
// trace_min — тикетом класса в репо хозяина (общий исполнитель classTicket, как у io); mode = log — «написал бы /
// завёл бы» в журнал.

// diskRoute — письма о fast-находках прохода и тикеты несрочных. Ошибки писем и тикетов — в журнал, проход идёт.
func (a *app) diskRoute(ctx context.Context, cfg *disk.Config, st *disk.State, recs []disk.Record, now time.Time) error {
	for i := range recs {
		if recs[i].Kind == disk.KindFast {
			a.diskFastLetters(cfg, st, recs, now)
			break
		}
	}
	return a.diskTickets(ctx, cfg, recs, now)
}

// diskFastLetters — письма о fast-находках прохода: одно на хозяина, пара в паузе — без письма.
func (a *app) diskFastLetters(cfg *disk.Config, st *disk.State, recs []disk.Record, now time.Time) {
	alive, regErr := a.reg.Alive()
	if regErr != nil {
		a.warn("disk", "реестр сессий: хозяин считается в сети, письмо покажет", "err", regErr)
	}
	isAlive := func(name string) bool {
		_, ok := alive[name]
		return ok || regErr != nil
	}
	for _, l := range st.FastLetters(cfg, now, recs, isAlive) {
		alert := cfg.Mode == disk.ModeAlert
		err := a.diskSend(cfg, alert, l.To, "fast", "disk.fast", l.Text(cfg, a.home), l.Free)
		if err == nil || a.quietFailure("disk", err) {
			continue
		}
		if l.To != cfg.Lead { // хозяин кончился после проверки — лиду: срочное не должно потеряться
			l.To, l.Offline = cfg.Lead, true
			if err = a.diskSend(cfg, alert, l.To, "fast", "disk.fast", l.Text(cfg, a.home), l.Free); err == nil || a.quietFailure("disk", err) {
				continue
			}
		}
		a.warn("disk", "письмо о быстрой убыли", "owner", l.Owner, "to", l.To, "err", err)
	}
}

// diskTickets — несрочные находки в ожидание и тикеты тем классам хозяев, кому пора; состояние — сразу в tickets.json.
func (a *app) diskTickets(ctx context.Context, cfg *disk.Config, recs []disk.Record, now time.Time) error {
	path := filepath.Join(a.dir("disk"), "tickets.json")
	ts, err := disk.LoadTickets(path)
	if err != nil {
		return err
	}
	added := ts.Add(cfg, recs)
	due := ts.Due(now)
	if !added && len(due) == 0 {
		return nil
	}
	for _, b := range due {
		err := a.classTicket(ctx, classTicket{watcher: "disk", owner: b.Owner, lead: cfg.Lead, tk: cfg.Tk,
			log: cfg.Mode != disk.ModeAlert, base: b.Class, title: b.Title(), what: b.Class, count: int64(len(b.Recs)),
			unit: "records", text: func(prev, redirect string) string { return b.Text(a.home, prev, redirect) }, note: b.Note(a.home)})
		if err != nil {
			a.warn("disk", "тикет находок", "owner", b.Owner, "class", b.Class, "err", err)
			a.note("disk", "ticket_failed", b.Class, b.Owner, int64(len(b.Recs)), "records")
		}
		ts.Done(&b, now, err == nil)
	}
	return ts.Save(path)
}

// diskForecast — точка ряда свободного места и прогноз запаса: строка журнала — при смене вывода, письмо лиду и
// free_tell — раз в сутки, пока до free_min меньше forecast_within.
func (a *app) diskForecast(cfg *disk.Config, now time.Time, free int64) error {
	path := filepath.Join(a.dir("disk"), "forecast.json")
	fst, err := disk.LoadForecast(path)
	if err != nil {
		return err
	}
	changed := fst.Add(cfg, now, free)
	fc, ok := cfg.Predict(fst.Series, now, free)
	if key := disk.ForecastKey(fc, ok); key != fst.Last {
		fst.Last, changed = key, true
		r := disk.Record{At: now, Kind: disk.KindForecast, Owner: cfg.Lead, Why: "lead", Free: free, Text: cfg.ForecastLine(fc, ok)}
		a.log.Info("aguard disk: " + r.Line(a.home))
		if err := disk.NewStore(a.dir("disk")).Append([]disk.Record{r}); err != nil {
			return err
		}
	}
	if cfg.Warn(fc, ok) {
		if due := fst.Told.Pending(now, append([]string{cfg.Lead}, cfg.FreeTell...)); len(due) > 0 {
			var grew []disk.Record
			if _, err := disk.NewStore(a.dir("disk")).Read(now.Add(-disk.ForecastGrew), func(r *disk.Record) {
				if r.Kind == disk.KindGrew {
					grew = append(grew, *r)
				}
			}); err != nil {
				a.warn("disk", "журнал для письма прогноза", "err", err)
			}
			msg := cfg.ForecastMessage(fc, grew, a.home)
			for _, to := range due {
				err := a.diskSend(cfg, cfg.Mode == disk.ModeAlert, to, "forecast", "disk.forecast", msg, free)
				if err != nil && !a.quietFailure("disk", err) {
					a.warn("disk", "письмо прогноза", "session", to, "err", err)
				}
				fst.Told.Mark(to, now, err == nil)
				changed = true
			}
		}
	}
	if !changed {
		return nil
	}
	return fst.Save(path)
}
