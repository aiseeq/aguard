package main

import (
	"flag"
	"fmt"
	"path/filepath"
	"time"

	"github.com/aiseeq/aguard/internal/cpuwatch"
	"github.com/aiseeq/aguard/internal/remotebroken"
	"github.com/aiseeq/akit/session"
)

func (a *app) remoteCmd(args []string) error {
	cmd, rest := sub(args)
	cfg, err := a.config()
	if err != nil {
		return err
	}
	if cmd != "tick" {
		usage()
		return nil
	}
	fl := flag.NewFlagSet("remote tick", flag.ContinueOnError)
	dry := fl.Bool("dry-run", false, "показать события и письма, ничего не отправлять и не сохранять")
	file := fl.String("file", "", "путь к журналу graft")
	fromStart := fl.Bool("from-start", false, "читать с начала файла (только с -dry-run)")
	parse(fl, rest)
	if *fromStart && !*dry {
		return fmt.Errorf("-from-start можно использовать только с -dry-run")
	}
	return a.remoteRound(&cfg.Remote, *dry, *file, *fromStart)
}

func (a *app) remoteRound(cfg *remotebroken.Config, dry bool, fileArg string, fromStart bool) error {
	if cfg.Mode == remotebroken.ModeOff {
		return nil
	}
	file := cfg.File
	if fileArg != "" {
		file = fileArg
	}
	statePath := filepath.Join(a.dir("remote"), "state.json")
	st, err := remotebroken.LoadState(statePath)
	if err != nil {
		return err
	}
	if fromStart {
		st.Seeded, st.Offset = true, 0
	}
	events, err := remotebroken.ReadNew(file, &st, func(err error) { a.warn("remote", "строка журнала пропущена", "err", err) })
	if err != nil {
		return fmt.Errorf("журнал graft: %w", err)
	}
	events, queued := remotebroken.SplitQueued(events)
	for i := range queued {
		e := &queued[i]
		if dry {
			fmt.Fprintf(a.out, "%s очередь, не поломка: %s\n", e.At.Local().Format("15:04"), remotebroken.Message(*e))
			continue
		}
		a.log.Info("aguard remote: очередь PC, не поломка — переход на ноут", "project", e.Project, "task", e.Task, "host", e.Host, "reason", e.Reason)
		a.note("remote", "queued", e.Host, e.Project, 1, "")
	}
	alive, aliveErr := a.reg.Alive()
	if aliveErr != nil && !dry {
		a.warn("remote", "реестр сессий", "err", aliveErr)
	}
	owners := a.remoteOwners(cfg, events)
	if dry {
		for i := range events {
			e := &events[i]
			fmt.Fprintf(a.out, "%s owner=%s: %s\n", e.At.Local().Format("15:04"), owners[i], remotebroken.Message(*e))
		}
		for _, n := range st.Apply(events, owners, cfg.Lead, time.Now(), cfg.Repeat.D()) {
			fmt.Fprintf(a.out, "  письмо %s: %s\n", n.To, n.Message)
		}
		return nil
	}
	for _, n := range st.Apply(events, owners, cfg.Lead, time.Now(), cfg.Repeat.D()) {
		a.remoteSend(cfg, alive, n)
	}
	return st.Save(statePath)
}

func (a *app) remoteOwners(cfg *remotebroken.Config, events []remotebroken.Event) []string {
	jobs := &cpuwatch.Jobs{Dir: filepath.Join(a.home, ".local", "state", "worker")}
	owners := make([]string, len(events))
	for i := range events {
		e := &events[i]
		resolved := false
		owner, ownerErr := remotebroken.Owner(*e, func(id string) (string, error) {
			j, err := jobs.ByID(id)
			if err != nil {
				return "", err
			}
			if j == nil {
				return "", nil
			}
			resolved = j.Master != ""
			return j.Master, nil
		}, func(pid int) (string, error) {
			in, err := a.reg.Get(pid)
			if err != nil || in == nil {
				return "", err
			}
			resolved = true
			return in.Name, nil
		}, cfg.Lead)
		if ownerErr != nil {
			a.warn("remote", "не удалось определить владельца", "worker", e.Worker, "claude_pid", e.ClaudePID, "err", ownerErr)
		}
		if !resolved {
			a.warn("remote", "владелец события неизвестен", "project", e.Project, "host", e.Host)
		}
		owners[i] = owner
	}
	return owners
}

func (a *app) remoteSend(cfg *remotebroken.Config, alive map[string]session.Info, n remotebroken.Notice) {
	if cfg.Mode == remotebroken.ModeLog {
		a.log.Info("aguard remote: написал бы", "session", n.To, "msg", n.Message)
		a.note("remote", "would_send", n.Key, n.To, int64(n.Count), "events")
		return
	}
	if err := a.sendAlive(alive, n.To, cfg.SendAuth, n.Message); err != nil {
		if a.quietFailure("remote", err) {
			return
		}
		a.warn("remote", "письмо не отправлено", "session", n.To, "err", err)
		a.note("remote", "send_failed", n.Key, n.To, int64(n.Count), "events")
		return
	}
	a.log.Info("aguard remote: написал", "session", n.To, "msg", n.Message)
	a.note("remote", "sent", n.Key, n.To, int64(n.Count), "events")
}
