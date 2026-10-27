// Наблюдатель cpu: перегрузка процессора ноута и PC и чья она (internal/cpuwatch). Раз в интервал — load average;
// за 5 минут не меньше потоков — снимок готовых потоков, письмо мастерам, чьих готовых задач больше всего, и сводка
// лиду, пока длится — не чаще repeat. Ничего не снимает и не понижает.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/cpuwatch"
	"github.com/aiseeq/akit/deliver"
	"github.com/aiseeq/akit/session"
)

// sshTimeout — замер PC целиком; не успел — замера нет, окно PC ждёт следующего.
const sshTimeout = 10 * time.Second

// cpuHost — машина под наблюдением и её перегрузка.
type cpuHost struct {
	ssh  string // "" — своя машина
	root string // откуда читать load average: /proc или папка с файлами, снятыми по ssh
	win  cpuwatch.Host
}

func (a *app) cpuCmd(ctx context.Context, args []string) error {
	cmd, rest := sub(args)
	cfg, err := a.config()
	if err != nil {
		return err
	}
	if cmd == "tick" {
		fl := flag.NewFlagSet("cpu tick", flag.ContinueOnError)
		dry := fl.Bool("dry-run", false, "один замер каждой машины: load average и готовые задачи по владельцам, ничего не слать")
		parse(fl, rest)
		if !*dry {
			return a.cpuRound(ctx, &cfg.Cpu)
		}
		return a.cpuDry(ctx, &cfg.Cpu)
	}
	usage()
	return nil
}

// cpuHosts — машины по конфигу; окна живут между проходами демона.
func (a *app) cpuHosts(cfg *cpuwatch.Config) []*cpuHost {
	if a.cpu == nil {
		a.cpu = map[string]*cpuHost{}
	}
	names := append([]string{""}, cfg.Hosts...)
	out := make([]*cpuHost, 0, len(names))
	for _, n := range names {
		h := a.cpu[n]
		if h == nil {
			h = &cpuHost{ssh: n, root: procRoot, win: cpuwatch.Host{Name: cpuwatch.HostLocal}}
			if n != "" {
				h.root, h.win.Name = filepath.Join(a.dir("cpu"), "proc-"+n), n
			}
			a.cpu[n] = h
		}
		out = append(out, h)
	}
	return out
}

// cpuSample — замер машины h: load average и потоки машины, а в перегрузку (или all) — готовые потоки. Без
// перегрузки потоки не обходит: владельцы нужны только ей, а обход ~5600 потоков — 80 мс процессора (замер 27.10
// 23:2x CET).
func (a *app) cpuSample(ctx context.Context, h *cpuHost, now time.Time, all bool) (*cpuwatch.Sample, error) {
	s := &cpuwatch.Sample{At: now}
	var err error
	if h.ssh != "" {
		if _, err = a.cpuRemote(ctx, h, false); err != nil {
			return nil, err
		}
	}
	if s.Load, s.Threads, err = cpuwatch.ReadLoad(h.root); err != nil {
		return nil, err
	}
	if !all && !s.Over() {
		return s, nil
	}
	if h.ssh != "" {
		if s.Tasks, err = a.cpuRemote(ctx, h, true); err != nil {
			return nil, err
		}
		return s, nil
	}
	// часть процессов не прочиталась — замер всё равно идёт: load average от этого не зависит
	if s.Tasks, err = cpuwatch.ReadTasks(procRoot); err != nil {
		a.warn("cpu", "процессы прочитаны не все", "err", err)
	}
	return s, nil
}

// cpuRemote — замер по ssh: файлы load average — в h.root, с tasks — и готовые потоки в ответ. Соединение одно на
// много замеров (ControlPersist): ssh раз в минуту не нагружает ни ноут, ни PC.
func (a *app) cpuRemote(ctx context.Context, h *cpuHost, tasks bool) ([]cpuwatch.Task, error) {
	ctx, cancel := context.WithTimeout(ctx, sshTimeout)
	defer cancel()
	arg := "load"
	if tasks {
		arg = "tasks"
	}
	c := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "-o", "ControlMaster=auto",
		"-o", "ControlPath="+filepath.Join(a.dir("cpu"), "ssh-%C"), "-o", "ControlPersist=10m", h.ssh, "sh", "-s", arg)
	c.Stdin = strings.NewReader(cpuwatch.RemoteScript)
	if err := os.MkdirAll(a.dir("cpu"), 0o700); err != nil { // у команды папку не завёл run
		return nil, fmt.Errorf("папка состояния: %w", err)
	}
	out, err := c.Output()
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return nil, fmt.Errorf("ssh %s: %w: %s", h.ssh, err, strings.TrimSpace(string(ee.Stderr)))
	}
	if err != nil {
		return nil, fmt.Errorf("ssh %s: %w", h.ssh, err)
	}
	return cpuwatch.ParseRemote(out, h.root)
}

// cpuResolver — владельцы потоков на этот проход.
func (a *app) cpuResolver(cfg *cpuwatch.Config) *cpuwatch.Resolver {
	return &cpuwatch.Resolver{
		Jobs:     &cpuwatch.Jobs{Dir: cfg.Workers},
		ProcRoot: procRoot,
		Sessions: func(pid int) (string, error) {
			in, err := a.reg.Get(pid)
			if err != nil || in == nil {
				return "", err
			}
			return in.Name, nil
		},
	}
}

// cpuRound — проход демона: замер каждой машины, решение о перегрузке и письма.
func (a *app) cpuRound(ctx context.Context, cfg *cpuwatch.Config) error {
	if cfg.Mode == cpuwatch.ModeOff {
		return nil
	}
	r := a.cpuResolver(cfg)
	for _, h := range a.cpuHosts(cfg) {
		now := time.Now()
		s, err := a.cpuSample(ctx, h, now, false)
		if err != nil {
			if !h.win.Down {
				a.warn("cpu", "замер "+h.win.Name+" не удался, жду", "err", err)
			}
			h.win.Down = true
			continue
		}
		if h.win.Down {
			a.log.Info("aguard cpu: замер снова идёт", "host", h.win.Name)
			h.win.Down = false
		}
		owner := r.Owner
		if h.ssh != "" {
			owner = r.RemoteOwner
		}
		v := h.win.Judge(s, owner)
		a.cpuAct(cfg, &h.win, &v, now)
	}
	if err := r.Err(); err != nil { // поток без найденного владельца идёт без мастера: в сводке лиду он виден
		a.warn("cpu", "владельцы найдены не все", "err", err)
	}
	return nil
}

// cpuAct — журнал начала и конца перегрузки и письма по ней.
func (a *app) cpuAct(cfg *cpuwatch.Config, h *cpuwatch.Host, v *cpuwatch.Verdict, now time.Time) {
	if !v.Ended.IsZero() {
		a.log.Info("aguard cpu: перегрузка кончилась", "host", h.Name, "lasted", now.Sub(v.Ended).Round(time.Second).String())
		a.note("cpu", "overload_end", h.Name, "", int64(now.Sub(v.Ended).Minutes()), "min")
	}
	if !v.Over {
		return
	}
	if v.Started {
		a.log.Info("aguard cpu: перегрузка", "host", h.Name, "load5", v.Avg5, "threads", v.Threads)
		a.note("cpu", "overload", h.Name, "", int64(v.Avg5), "load5")
	}
	alive, err := a.reg.Alive()
	if err != nil {
		a.warn("cpu", "реестр сессий", "err", err)
	}
	var wrote []string
	for _, m := range v.Recipients(cfg.Notify) {
		first := !h.Seen(m.Name)
		if !h.Due(m.Name, now, cfg.Repeat.D()) {
			continue
		}
		if err := a.cpuSend(cfg, alive, m.Name, cpuwatch.MasterMessage(h.Name, v, &m, now, first), int64(m.Avg)); err != nil {
			a.cpuFailed(m.Name, int64(m.Avg), err)
			continue
		}
		wrote = append(wrote, m.Name)
	}
	if h.Due(leadKey+cfg.Lead, now, cfg.Repeat.D()) {
		lead := cpuwatch.LeadMessage(h.Name, v, wrote, now)
		if err := a.cpuSend(cfg, alive, cfg.Lead, lead, int64(v.Avg5)); err != nil {
			a.cpuFailed(cfg.Lead, int64(v.Avg5), err)
		}
	}
}

// leadKey — сводка лиду в отметках писем: лид бывает и мастером (свои бойцы), это другое письмо.
const leadKey = "сводка:"

func (a *app) cpuFailed(to string, value int64, err error) {
	a.warn("cpu", "письмо о перегрузке", "session", to, "err", err)
	a.note("cpu", "send_failed", "", to, value, "runnable")
}

// cpuSend — письмо сессии to; в режиме log — только журнал «написал бы».
func (a *app) cpuSend(cfg *cpuwatch.Config, alive map[string]session.Info, to, msg string, value int64) error {
	if cfg.Mode != cpuwatch.ModeAlert {
		a.log.Info("aguard cpu: написал бы", "session", to, "msg", msg)
		a.note("cpu", "would_send", "", to, value, "runnable")
		return nil
	}
	in, ok := alive[to]
	if !ok || in.SocketPath == "" {
		return fmt.Errorf("сессии %s нет", to)
	}
	token := ""
	if cfg.SendAuth {
		var err error
		if token, err = a.reg.Token(in.Pid); err != nil {
			return fmt.Errorf("токен %s: %w", to, err)
		}
	}
	if err := deliver.Send("aguard", in.SocketPath, token, msg); err != nil {
		return fmt.Errorf("сокет %s: %w", to, err)
	}
	a.log.Info("aguard cpu: написал", "session", to, "msg", msg)
	a.note("cpu", "sent", "", to, value, "runnable")
	return nil
}

// cpuDry — один замер каждой машины: load average, готовые потоки по владельцам и, в перегрузку, письма.
func (a *app) cpuDry(ctx context.Context, cfg *cpuwatch.Config) error {
	r := a.cpuResolver(cfg)
	for _, h := range a.cpuHosts(cfg) {
		now := time.Now()
		s, err := a.cpuSample(ctx, h, now, true)
		if err != nil {
			fmt.Fprintf(a.out, "%s: %v\n", h.win.Name, err)
			continue
		}
		owner := r.Owner
		if h.ssh != "" {
			owner = r.RemoteOwner
		}
		fmt.Fprintf(a.out, "%s: load average %.2f %.2f %.2f при %d потоках, перегружена=%v\n", h.win.Name, s.Load.Avg1,
			s.Load.Avg5, s.Load.Avg15, s.Threads, s.Over())
		for _, l := range cpuwatch.Owners(s.Tasks, owner) { // владельцы готовых потоков и у свободной машины
			fmt.Fprintf(a.out, "  %-20s %-8s %-40s %.0f %v\n", l.Owner.Master, l.Owner.Kind, l.Owner.Label, l.Avg, l.Comms)
		}
		var win cpuwatch.Host // своя перегрузка: окно демона пробой не трогается
		if v := win.Judge(s, owner); v.Over {
			for _, m := range v.Recipients(cfg.Notify) {
				fmt.Fprintf(a.out, "  → %s: %s\n", m.Name, cpuwatch.MasterMessage(h.win.Name, &v, &m, now, true))
			}
			fmt.Fprintf(a.out, "  → %s: %s\n", cfg.Lead, cpuwatch.LeadMessage(h.win.Name, &v, nil, now))
		}
	}
	return r.Err()
}
