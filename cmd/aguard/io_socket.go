package main

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/aiseeq/aguard/internal/activity"
	"github.com/aiseeq/aguard/internal/iowatch"
	"github.com/aiseeq/aguard/internal/linesock"
)

// Сокет моментального состояния диска ([io] socket): now — снимком Live под его мьютексом, без похода
// в цикл замера; series, profiles и cases — через extra горутины цикла.

// ioAskTimeout — сколько соединение ждёт данные от горутины цикла: меньше срока соединения linesock.
const ioAskTimeout = 1500 * time.Millisecond

// serve — сокет до остановки службы. Путь — из конфига на старте: смена — после перезапуска службы.
func (s *ioService) serve() {
	c, err := s.a.config()
	if err != nil {
		s.a.log.Error("aguard io: конфиг, сокет не открыт", "err", err)
		s.a.note("io", activity.KindConfigSkipped, "сокет не открыт", "", 0, "")
		return
	}
	ln, err := linesock.Listen(c.IO.Socket)
	if err != nil {
		s.a.log.Error("aguard io: сокет не открыт", "err", err)
		s.a.note("io", "socket_failed", "", "", 0, "")
		return
	}
	if err := linesock.Serve(s.ctx, ln, s.answer, s.a.log, "aguard io"); err != nil {
		s.a.log.Error("aguard io: сокет", "err", err)
		s.a.note("io", "socket_failed", "", "", 0, "")
	}
}

// answer — ответ на строку запроса.
func (s *ioService) answer(line []byte) any {
	fail := func(err error) iowatch.Answer { return iowatch.Answer{V: iowatch.ProtoVersion, Error: err.Error()} }
	var req iowatch.Request
	if err := json.Unmarshal(line, &req); err != nil {
		return fail(errors.New("запрос не JSON: " + err.Error()))
	}
	since, err := iowatch.CheckRequest(req)
	if err != nil {
		return fail(err)
	}
	switch req.Q {
	case iowatch.QueryNow:
		now, ok := s.live.Now(req.Kind)
		if !ok {
			return fail(errors.New("замер ещё не дал ни одного шага (mode = off или служба только стартовала)"))
		}
		return iowatch.Answer{V: iowatch.ProtoVersion, Now: &now}
	case iowatch.QuerySeries:
		rows, err := ioAsk(s, func() iowatch.SeriesRows { return s.series.Rows(since) })
		if err != nil {
			return fail(err)
		}
		return iowatch.Answer{V: iowatch.ProtoVersion, Series: rows.Seconds()}
	case iowatch.QueryCases:
		cases, err := ioAsk(s, func() []iowatch.Case {
			kept := s.kept(time.Now())
			filtered := make([]iowatch.Case, 0, len(kept))
			for i := range kept {
				if !kept[i].End.Before(since) {
					filtered = append(filtered, kept[i])
				}
			}
			return filtered
		})
		if err != nil {
			return fail(err)
		}
		return iowatch.Answer{V: iowatch.ProtoVersion, Cases: cases}
	default: // QueryProfiles: CheckRequest других не пропускает
		profiles, err := ioAsk(s, func() []iowatch.Profile { return iowatch.FilterProfiles(s.ended, req.Kind, since) })
		if err != nil {
			return fail(err)
		}
		return iowatch.Answer{V: iowatch.ProtoVersion, Profiles: profiles}
	}
}

var errIOBusy = errors.New("цикл замера не ответил за 1,5 с")

// ioAsk — f в горутине цикла (через extra) и её итог; цикл не ответил за ioAskTimeout — errIOBusy.
func ioAsk[T any](s *ioService, f func() T) (T, error) {
	var zero T
	out := make(chan T, 1)
	timer := time.NewTimer(ioAskTimeout)
	defer timer.Stop()
	select {
	case s.extra <- func() { out <- f() }:
	case <-timer.C:
		return zero, errIOBusy
	case <-s.ctx.Done():
		return zero, s.ctx.Err()
	}
	select {
	case v := <-out:
		return v, nil
	case <-timer.C:
		return zero, errIOBusy
	case <-s.ctx.Done():
		return zero, s.ctx.Err()
	}
}
