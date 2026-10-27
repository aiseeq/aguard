// Наблюдатель bgtask: забытые фоновые задачи Bash. Обычная команда, которую Claude Code увёл в фон по таймауту, идёт
// дольше age, и вывод не меняется дольше quiet — письмо сессии-владельцу одной строкой, по разу на задачу. Снимать
// задачу решает владелец (TaskStop), aguard её не трогает.
package main

import (
	"flag"
	"fmt"
	"path/filepath"
	"time"

	"github.com/aiseeq/aguard/internal/bgtask"
	"github.com/aiseeq/akit/deliver"
	"github.com/aiseeq/akit/proc"
	"github.com/aiseeq/akit/session"
)

const bgtaskFrom = "aguard"

func (a *app) bgtaskCmd(args []string) error {
	cmd, rest := sub(args)
	cfg, err := a.config()
	if err != nil {
		return err
	}
	if cmd == "tick" {
		fl := flag.NewFlagSet("bgtask tick", flag.ContinueOnError)
		dry := fl.Bool("dry-run", false, "напечатать живые фоновые задачи и решение по ним, ничего не слать и не писать")
		parse(fl, rest)
		return a.bgtaskRound(&cfg.Bgtask, *dry)
	}
	usage()
	return nil
}

// bgtaskRound — один проход: живые задачи, по застывшим — происхождение по транскрипту и письмо.
func (a *app) bgtaskRound(cfg *bgtask.Config, dry bool) error {
	if cfg.Mode == bgtask.ModeOff {
		return nil
	}
	now := time.Now()
	snap, err := proc.ReadSnapshot(procRoot, a.uid, now)
	if snap == nil {
		return fmt.Errorf("процессы: %w", err)
	}
	if err != nil {
		a.warn("bgtask", "процессы прочитаны не все", "err", err)
	}
	tasks, err := bgtask.Find(snap, procRoot, a.uid)
	if err != nil {
		a.warn("bgtask", "задачи", "err", err)
	}
	statePath := filepath.Join(a.dir("bgtask"), "state.json")
	st, err := bgtask.LoadState(statePath)
	if err != nil {
		return err
	}
	for i := range tasks {
		t := &tasks[i]
		stale := t.Stale(now, cfg.Age.D(), cfg.Quiet.D())
		if dry {
			fmt.Fprintf(a.out, "%s pids=%v сессия=%d идёт %s, вывод молчит %s, застыла=%v %s\n", t.ID, t.Pids, t.Holder,
				bgtask.Span(now.Sub(t.Start)), bgtask.Span(now.Sub(t.Written)), stale, t.Output)
		}
		if _, seen := st[t.ID]; !stale || (seen && !dry) {
			continue
		}
		o, err := bgtask.FindOrigin(session.ProjectsDir(a.home), t)
		if err != nil {
			a.warn("bgtask", "транскрипт задачи "+t.ID, "err", err)
			continue
		}
		if dry {
			fmt.Fprintf(a.out, "  вызов найден=%v по таймауту=%v суб-агент=%q «%s»\n", o.Found, o.Auto, o.Agent, o.Description)
			continue
		}
		if !o.Found { // вызова в транскрипте (ещё) нет: решить в следующий проход
			continue
		}
		st[t.ID] = bgtask.Seen{At: now, Auto: o.Auto}
		a.bgtaskNotify(cfg, t, &o, now)
	}
	if dry {
		return nil
	}
	return st.Save(statePath, tasks)
}

// bgtaskNotify — письмо владельцу уведённой по таймауту задачи (в log — только журнал); нарочно фоновая — запись.
func (a *app) bgtaskNotify(cfg *bgtask.Config, t *bgtask.Task, o *bgtask.Origin, now time.Time) {
	quiet := int64(now.Sub(t.Written).Minutes())
	if !o.Auto {
		a.log.Info("aguard bgtask: застывшая задача запущена фоном нарочно — не пишу", "task", t.ID,
			"description", o.Description, "age", bgtask.Span(now.Sub(t.Start)), "quiet", bgtask.Span(now.Sub(t.Written)))
		a.note("bgtask", "intended", t.ID, "", quiet, "min")
		return
	}
	msg := bgtask.Message(t, o, now)
	in, err := a.reg.Get(t.Holder)
	switch {
	case err != nil || in == nil || in.SocketPath == "":
		a.warn("bgtask", "некому написать о задаче "+t.ID, "holder", t.Holder, "err", err)
		return
	case cfg.Mode != bgtask.ModeAlert:
		a.log.Info("aguard bgtask: написал бы", "session", in.Name, "msg", msg)
		a.note("bgtask", "would_send", t.ID, in.Name, quiet, "min")
		return
	}
	if err := a.bgtaskSend(cfg, in, msg); err != nil {
		a.warn("bgtask", "письмо о задаче "+t.ID, "session", in.Name, "err", err)
		a.note("bgtask", "send_failed", t.ID, in.Name, quiet, "min")
		return
	}
	a.log.Info("aguard bgtask: написал", "session", in.Name, "msg", msg)
	a.note("bgtask", "sent", t.ID, in.Name, quiet, "min")
}

func (a *app) bgtaskSend(cfg *bgtask.Config, in *session.Info, msg string) error {
	token := ""
	if cfg.SendAuth {
		var err error
		if token, err = a.reg.Token(in.Pid); err != nil {
			return fmt.Errorf("токен %s: %w", in.Name, err)
		}
	}
	if err := deliver.Send(bgtaskFrom, in.SocketPath, token, msg); err != nil {
		return fmt.Errorf("сокет %s: %w", in.Name, err)
	}
	return nil
}
