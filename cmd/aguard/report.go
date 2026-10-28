// Учёт срабатываний и отчёт aguard: что видели и сделали наблюдатели, кому ушли сводки, сбои демона —
// за сутки, неделю и месяц. Учёт — ~/.local/state/aguard/activity/<день>.jsonl, его ведёт только демон.
// Отчёт читает ночью sleepstats для страницы Сна: aguard report -json.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"time"

	"github.com/aiseeq/aguard/internal/activity"
	"github.com/aiseeq/aguard/internal/bgtask"
	"github.com/aiseeq/aguard/internal/config"
	"github.com/aiseeq/aguard/internal/cpuwatch"
	"github.com/aiseeq/aguard/internal/iowatch"
	"github.com/aiseeq/aguard/internal/memwatch"
	"github.com/aiseeq/aguard/internal/restart"
	"github.com/aiseeq/aguard/internal/said"
	"github.com/aiseeq/aguard/internal/waits"
)

const (
	flushEvery   = 10 * time.Minute    // как часто учёт сливается в файл: падение демона теряет не больше
	activityKeep = 60 * 24 * time.Hour // хранение учёта: месяц отчёта с запасом
)

// note — срабатывание в учёт (у команд вне демона учёта нет); value в единицах unit — для наибольшего за период.
func (a *app) note(watcher, kind, what, target string, value int64, unit string) {
	a.act.Add(watcher, kind, what, target, value, unit, time.Now())
}

// warn — предупреждение наблюдателя: в журнал и в учёт. Проход продолжается.
func (a *app) warn(watcher, what string, args ...any) {
	a.log.Warn("aguard "+watcher+": "+what, args...)
	a.note(watcher, activity.KindWarn, what, "", 0, "")
}

func (a *app) flush() {
	if err := a.act.Flush(time.Now(), activityKeep); err != nil {
		a.log.Error("aguard: учёт срабатываний", "err", err)
	}
}

func (a *app) flushLoop(ctx context.Context) {
	t := time.NewTicker(flushEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.flush()
		}
	}
}

func (a *app) reportCmd(args []string) error {
	fl := flag.NewFlagSet("report", flag.ContinueOnError)
	asJSON := fl.Bool("json", false, "JSON для sleepstats (формат v1, activity.Report)")
	atText := fl.String("at", "", "момент отчёта, RFC3339 (по умолчанию сейчас): окна — сутки, неделя, месяц до него")
	parse(fl, args)
	at := time.Now()
	if *atText != "" {
		t, err := time.Parse(time.RFC3339, *atText)
		if err != nil {
			return fmt.Errorf("-at: %w", err)
		}
		at = t
	}
	cfg, err := a.config()
	if err != nil {
		return err
	}
	r, err := a.buildReport(&cfg, at)
	if err != nil {
		return err
	}
	if !*asJSON {
		return r.Print(a.out)
	}
	enc := json.NewEncoder(a.out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		return fmt.Errorf("вывод: %w", err)
	}
	return nil
}

func (a *app) buildReport(cfg *config.Config, at time.Time) (*activity.Report, error) {
	month := activity.Windows[len(activity.Windows)-1].Len
	var recs []activity.Record
	bad, err := activity.NewStore(a.dir("activity")).Read(at.Add(-month), func(r *activity.Record) { recs = append(recs, *r) })
	if err != nil {
		return nil, err
	}
	r, err := activity.Build(at, recs, a.reportSpecs(cfg))
	if r != nil {
		r.Bad += bad
	}
	return r, err
}

// reportSpecs — наблюдатели в отчёте: каждый из app.watchers и restart (вопросы cl). Наблюдателя без строки здесь в
// отчёте нет, и его сбои не видны (disk и said до 26.10: сбой disk 15:00–17:40 страница Сна не показала).
func (a *app) reportSpecs(cfg *config.Config) []activity.Spec {
	return []activity.Spec{
		{Name: "orphans", Mode: cfg.Orphans.Mode, Looped: true},
		{Name: "tmp", Mode: cfg.Tmp.Mode, Looped: true},
		{Name: "memory", Looped: true, Extra: a.memoryLines},
		{Name: "waits", Looped: true, Extra: a.waitsLines},
		{Name: "agentwaits", Looped: true},
		{Name: "restart", Mode: cfg.Restart.Mode},
		{Name: "compact", Mode: cfg.Compact.Mode, Looped: cfg.Compact.Mode != restart.CompactOff},
		{Name: "disk", Mode: cfg.Disk.Mode, Looped: true},
		{Name: "said", Mode: cfg.Said.Mode, Looped: cfg.Said.Mode != said.ModeOff},
		{Name: "bgtask", Mode: cfg.Bgtask.Mode, Looped: cfg.Bgtask.Mode != bgtask.ModeOff},
		{Name: "cpu", Mode: cfg.Cpu.Mode, Looped: cfg.Cpu.Mode != cpuwatch.ModeOff},
		{Name: "io", Mode: cfg.IO.Mode, Looped: cfg.IO.Mode != iowatch.ModeOff},
		{Name: "remote", Mode: cfg.Remote.Mode, Looped: cfg.Remote.Mode != "off"},
		{Name: "night", Mode: cfg.Night.Mode, Looped: cfg.Night.Mode != "off"},
	}
}

// memoryLines — по имени сессии: наибольший пик за окно, события memory.high и OOM.
func (a *app) memoryLines(from, to time.Time) ([]activity.Line, error) {
	st := a.memoryStore()
	r, err := memwatch.Collect(func(fn func(*memwatch.Sample)) (int, error) {
		return st.Read(from, func(s *memwatch.Sample) {
			if s.At.Before(to) {
				fn(s)
			}
		})
	})
	if err != nil {
		return nil, err
	}
	var out []activity.Line
	for _, rs := range r.Roles {
		out = append(out, activity.Line{Kind: "peak", What: rs.Name, Count: rs.Samples, Max: rs.Max.Peak, Unit: "bytes", Last: rs.Last})
		if rs.HighHits > 0 {
			out = append(out, activity.Line{Kind: "memory_high", What: rs.Name, Count: int(rs.HighHits)})
		}
		if rs.OOM > 0 {
			out = append(out, activity.Line{Kind: "oom_kill", What: rs.Name, Count: int(rs.OOM)})
		}
	}
	return out, nil
}

// waitsLines — отказы и ожидания по виду и подробности: сколько раз, самая долгая длительность.
func (a *app) waitsLines(from, to time.Time) ([]activity.Line, error) {
	st := waits.NewStore(a.dir("waits"))
	r, err := waits.Collect(func(fn func(*waits.Event)) (int, error) {
		return st.Read(from, func(e *waits.Event) {
			if e.At.Before(to) {
				fn(e)
			}
		})
	})
	if err != nil {
		return nil, err
	}
	out := make([]activity.Line, 0, len(r.Groups))
	for _, g := range r.Groups {
		l := activity.Line{Kind: g.Kind, What: g.Detail, Count: g.Count, Last: g.Last}
		if len(g.Ms) > 0 {
			l.Max, l.Unit = g.Ms[len(g.Ms)-1], "ms"
		}
		out = append(out, l)
	}
	return out, nil
}
