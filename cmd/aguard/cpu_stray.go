// Процессы без родителя на ноуте и PC (internal/cpuwatch/stray.go, тикет stray-processes): раз в strays.every — письмо
// мастеру, чей процесс, или лиду. Ничего не снимает.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/cpuwatch"
	"github.com/aiseeq/akit/session"
)

// strayReader — один замер машины h; подменяется в тестах.
type strayReader func(ctx context.Context, h *cpuHost) ([]byte, error)

// readStrays — StrayScript на машине: по ssh тем же соединением, что и замер нагрузки, на своей — через sh.
func (a *app) readStrays(ctx context.Context, h *cpuHost) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, sshTimeout)
	defer cancel()
	var c *exec.Cmd
	if h.ssh == "" {
		c = exec.CommandContext(ctx, "sh", "-s")
	} else {
		if err := os.MkdirAll(a.dir("cpu"), 0o700); err != nil {
			return nil, fmt.Errorf("папка состояния: %w", err)
		}
		c = exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "-o", "ControlMaster=auto",
			"-o", "ControlPath="+filepath.Join(a.dir("cpu"), "ssh-%C"), "-o", "ControlPersist=10m", h.ssh, "sh", "-s")
	}
	c.Stdin = strings.NewReader(cpuwatch.StrayScript)
	out, err := c.Output()
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return nil, fmt.Errorf("замер процессов %s: %w: %s", h.win.Name, err, strings.TrimSpace(string(ee.Stderr)))
	}
	if err != nil {
		return nil, fmt.Errorf("замер процессов %s: %w", h.win.Name, err)
	}
	return out, nil
}

// strayJudge — замер машины и процессы, о которых пора писать.
func (a *app) strayJudge(ctx context.Context, cfg *cpuwatch.Config, h *cpuHost, now time.Time) ([]cpuwatch.Stray, error) {
	if a.strayRead == nil {
		return nil, errors.New("замер процессов без родителя не подключён")
	}
	raw, err := a.strayRead(ctx, h)
	if err != nil {
		return nil, err
	}
	procs, details, err := cpuwatch.ParseStrays(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", h.win.Name, err)
	}
	return h.strays.Judge(now, procs, details, &cfg.Strays), nil
}

// strayRound — проход по машинам: раз в strays.every замер и письма, сбой одной не мешает другой.
func (a *app) strayRound(ctx context.Context, cfg *cpuwatch.Config, r *cpuwatch.Resolver) {
	path := filepath.Join(a.dir("cpu"), "strays.json")
	st, err := cpuwatch.LoadStrayState(path)
	if err != nil { // без отметок письма пошли бы заново: пропуск прохода дешевле повтора всем мастерам
		a.warn("cpu", "процессы без родителя: состояние не прочитано, проход пропущен", "err", err)
		return
	}
	changed := false
	for _, h := range a.cpuHosts(cfg) {
		now := time.Now()
		if now.Sub(h.strayAt) < cfg.Strays.Every.D() {
			continue
		}
		h.strayAt = now
		strays, err := a.strayJudge(ctx, cfg, h, now)
		if err != nil {
			if !h.strayBad {
				a.warn("cpu", "процессы без родителя: замер не удался, жду", "host", h.win.Name, "err", err)
			}
			h.strayBad = true
			continue
		}
		h.strayBad = false
		changed = st.Forget(h.win.Name, strays) || changed
		changed = a.strayLetters(cfg, r, h, strays, &st, now) || changed
	}
	if changed {
		if err := st.Save(path); err != nil {
			a.warn("cpu", "процессы без родителя: состояние не записано", "err", err)
		}
	}
}

// strayLetters — письма по адресатам: о новых процессах и о тех, кому вышел повтор; повтор — ещё и лиду.
func (a *app) strayLetters(cfg *cpuwatch.Config, r *cpuwatch.Resolver, h *cpuHost, strays []cpuwatch.Stray,
	st *cpuwatch.StrayState, now time.Time) bool {
	alive, err := a.reg.Alive()
	if err != nil {
		a.warn("cpu", "реестр сессий", "err", err)
	}
	type group struct {
		absent string
		list   []cpuwatch.Stray
	}
	byTo := map[string]*group{}
	repeat := map[string]bool{}
	for i := range strays {
		s := &strays[i]
		told, was := st.Told[s.Key(h.win.Name)]
		if was && now.Sub(told) < cfg.Strays.Repeat.D() {
			continue
		}
		to, absent := strayAddress(r.StrayOwner(h.ssh != "", &s.Detail, s.Comm).Master, cfg.Lead, alive)
		if byTo[to] == nil {
			byTo[to] = &group{absent: absent}
		}
		byTo[to].list = append(byTo[to].list, *s)
		repeat[to] = repeat[to] || was
		a.log.Info("aguard cpu: процесс без родителя", "host", h.win.Name, "pid", s.Pid, "comm", s.Comm, "age", s.Age.String(), "to", to)
		a.note("cpu", "stray", h.win.Name, to, 1, "")
	}
	changed := false
	for to, g := range byTo {
		if err := a.cpuSend(cfg, alive, to, cpuwatch.StrayMessage(h.win.Name, g.list, repeat[to], g.absent), int64(len(g.list))); err != nil {
			a.cpuFailed(to, int64(len(g.list)), err)
			continue // отметки нет: следующий проход пробует снова
		}
		if repeat[to] && to != cfg.Lead {
			msg := "копия, " + to + " не снял: " + cpuwatch.StrayMessage(h.win.Name, g.list, true, "")
			if err := a.cpuSend(cfg, alive, cfg.Lead, msg, int64(len(g.list))); err != nil {
				a.cpuFailed(cfg.Lead, int64(len(g.list)), err)
			}
		}
		for i := range g.list {
			st.Told[g.list[i].Key(h.win.Name)] = now
		}
		changed = true
	}
	return changed
}

// strayAddress — кому письмо: мастер, если он в сессиях; иначе лид и имя отсутствующего мастера.
func strayAddress(master, lead string, alive map[string]session.Info) (to, absent string) {
	if master == "" {
		return lead, ""
	}
	if _, ok := alive[master]; ok {
		return master, ""
	}
	return lead, master
}

// strayDry — процессы без родителя машины h за один замер: занятость считается только между проходами, поэтому
// показывается всё, что подходит под правило остатка сессии, и письмо, какое ушло бы.
func (a *app) strayDry(ctx context.Context, cfg *cpuwatch.Config, r *cpuwatch.Resolver, h *cpuHost) {
	saved := h.strays
	h.strays = cpuwatch.StrayWatch{}
	defer func() { h.strays = saved }()
	strays, err := a.strayJudge(ctx, cfg, h, time.Now())
	if err != nil {
		fmt.Fprintf(a.out, "  процессы без родителя: %v\n", err)
		return
	}
	for i := range strays {
		s := &strays[i]
		fmt.Fprintf(a.out, "  без родителя: pid %d %s живёт %v, владелец %+v\n", s.Pid, s.Comm, s.Age, r.StrayOwner(h.ssh != "", &s.Detail, s.Comm))
	}
	if len(strays) > 0 {
		fmt.Fprintf(a.out, "  письмо: %s\n", cpuwatch.StrayMessage(h.win.Name, strays, false, ""))
	}
}
