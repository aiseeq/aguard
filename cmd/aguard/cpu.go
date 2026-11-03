// Наблюдатель cpu: серия перегрузки ноута и PC длится, пока load average за минуту не ниже числа потоков. В текущем
// замере готовые задачи дают снимок владельцев и входят в письмо. После after — письмо мастерам и сводка лиду, повторы
// не чаще repeat. Окно и отметки писем хранятся в состоянии aguard; наблюдатель ничего не снимает и не понижает.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/cpuwatch"
	"github.com/aiseeq/aguard/internal/record"
	"github.com/aiseeq/akit/session"
)

// sshTimeout — замер PC целиком; не успел — замера нет, окно PC ждёт следующего.
const sshTimeout = 10 * time.Second

// cpuHost — машина под наблюдением и её перегрузка.
type cpuHost struct {
	ssh      string // "" — своя машина
	root     string // откуда читать load average: /proc или папка с файлами, снятыми по ssh
	state    string // файл сохранённого окна
	win      cpuwatch.Host
	strays   cpuwatch.StrayWatch
	strayAt  time.Time // когда смотрели процессы без родителя
	strayBad bool      // прошлый замер не удался: о сбое пишется раз
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
			stateName := "local.json"
			h = &cpuHost{ssh: n, root: procRoot, win: cpuwatch.Host{Name: cpuwatch.HostLocal}}
			if n != "" {
				h.root, h.win.Name = filepath.Join(a.dir("cpu"), "proc-"+n), n
				stateName = "host-" + n + ".json"
			}
			h.state = filepath.Join(a.dir("cpu"), stateName)
			if err := loadCPUState(h.state, &h.win); err != nil {
				a.warn("cpu", "состояние окна не прочитано, начинаю с нуля", "host", h.win.Name, "err", err)
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
	if err = s.ReadLoad(h.root); err != nil {
		return nil, err
	}
	if !all && !s.Over() {
		return s, nil
	}
	if h.ssh != "" {
		if s.Tasks, err = a.cpuRemote(ctx, h, true); err != nil {
			return nil, err
		}
	} else if s.Tasks, err = cpuwatch.ReadTasks(procRoot); err != nil {
		// часть процессов не прочиталась — замер всё равно идёт: load average от этого не зависит
		a.warn("cpu", "процессы прочитаны не все", "err", err)
	}
	// готовые задачи — в момент снимка, а не минутой раньше: письмо говорит о тех же потоках, что и разбор владельцев
	if err = s.ReadLoad(h.root); err != nil {
		return nil, err
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
		Jobs: &cpuwatch.Jobs{Dir: cfg.Workers,
			Units: fmt.Sprintf("/sys/fs/cgroup/user.slice/user-%d.slice/user@%d.service/app.slice", a.uid, a.uid)},
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
		h.win.Every = cfg.Interval.D()
		if err != nil {
			if !h.win.Down {
				a.warn("cpu", "замер "+h.win.Name+" не удался, жду", "err", err)
			}
			h.win.Down = true
			h.win.Break()
			if err := saveCPUState(h.state, &h.win); err != nil {
				a.warn("cpu", "состояние окна не сохранено", "host", h.win.Name, "err", err)
			}
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
		if err := saveCPUState(h.state, &h.win); err != nil {
			a.warn("cpu", "состояние окна не сохранено", "host", h.win.Name, "err", err)
		}
	}
	a.strayRound(ctx, cfg, r)
	if err := r.Err(); err != nil { // поток без найденного владельца идёт без мастера: в сводке лиду он виден
		a.warn("cpu", "владельцы найдены не все", "err", err)
	}
	return nil
}

// cpuAct — журнал начала и конца перегрузки и письма по ней.
func (a *app) cpuAct(cfg *cpuwatch.Config, h *cpuwatch.Host, v *cpuwatch.Verdict, now time.Time) {
	if !v.Ended.IsZero() {
		a.log.Info("aguard cpu: перегрузка кончилась", "host", h.Name, "lasted", v.Lasted.Round(time.Second).String(),
			"load1", v.Avg1, "runnable", v.Runnable, "threads", v.Threads)
		a.note("cpu", "overload_end", h.Name, "", int64(v.Lasted.Minutes()), "min")
	}
	if !v.Over {
		return
	}
	if v.Started {
		a.log.Info("aguard cpu: перегрузка", "host", h.Name, "load1", v.Avg1, "runnable", v.Runnable, "threads", v.Threads)
		a.note("cpu", "overload", h.Name, "", int64(v.Runnable), "runnable")
	}
	if !v.Long(now, cfg.After.D()) { // короткая — только журнал начала и конца
		return
	}
	alive, err := a.reg.Alive()
	if err != nil {
		a.warn("cpu", "реестр сессий", "err", err)
	}
	for _, m := range v.Recipients(cfg.Notify) {
		if !h.Due(m.Name, now, cfg.Repeat.D()) {
			continue
		}
		if err := a.cpuSend(cfg, alive, m.Name, cpuwatch.MasterMessage(h.Name, v, &m, now, h.First(m.Name)), int64(m.N)); err != nil {
			a.cpuFailed(m.Name, int64(m.N), err)
			continue
		}
		h.Sent(m.Name, now)
	}
	if h.Due(leadKey+cfg.Lead, now, cfg.Repeat.D()) {
		lead := cpuwatch.LeadMessage(h.Name, v, h.Told(), now)
		if err := a.cpuSend(cfg, alive, cfg.Lead, lead, int64(v.Runnable)); err != nil {
			a.cpuFailed(cfg.Lead, int64(v.Runnable), err)
			return
		}
		h.Sent(leadKey+cfg.Lead, now)
	}
}

type cpuStateFile struct {
	Version int            `json:"version"`
	Window  cpuwatch.State `json:"window"`
}

func loadCPUState(path string, h *cpuwatch.Host) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("чтение %s: %w", path, err)
	}
	var saved cpuStateFile
	if err := json.Unmarshal(b, &saved); err != nil {
		return fmt.Errorf("разбор %s: %w", path, err)
	}
	if saved.Version != 1 || !saved.Window.Since.IsZero() &&
		(saved.Window.Last.IsZero() || saved.Window.Last.Before(saved.Window.Since)) {
		return fmt.Errorf("неверная версия или окно состояния")
	}
	h.Restore(saved.Window)
	return nil
}

func saveCPUState(path string, h *cpuwatch.Host) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("папка состояния: %w", err)
	}
	b, err := json.Marshal(cpuStateFile{Version: 1, Window: h.State()})
	if err != nil {
		return fmt.Errorf("кодирование состояния: %w", err)
	}
	return record.WriteFileAtomic(path, append(b, '\n'), 0o600)
}

// leadKey — служебный ключ отметки сводки лиду (с «:» — не сессия): лид бывает и мастером — это другое письмо.
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
	if err := a.sendAlive(alive, to, cfg.SendAuth, msg); err != nil {
		return err
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
		fmt.Fprintf(a.out, "%s: load average за минуту %.2f, готовых сейчас %d при %d потоках, перегружена: %v\n",
			h.win.Name, s.Load.Avg1, s.Runnable, s.Threads, s.Over())
		a.strayDry(ctx, cfg, r, h)
		for _, l := range cpuwatch.Owners(s.Tasks, owner) { // владельцы готовых потоков и у свободной машины
			fmt.Fprintf(a.out, "  %-20s %-8s %-40s %d %v\n", l.Owner.Master, l.Owner.Kind, l.Owner.Label, l.N, l.Comms)
		}
		win := cpuwatch.Host{Name: h.win.Name} // своя перегрузка: окно демона пробой не трогается; письма — какими были бы через after
		if v := win.Judge(s, owner); v.Over {
			cpuDryLetters(a.out, cfg, &win, &v, now)
		}
	}
	return r.Err()
}

// cpuDryLetters — письма перегрузки такими, какими они ушли бы: в сводке лиду — те, кому написано строкой выше.
func cpuDryLetters(w io.Writer, cfg *cpuwatch.Config, win *cpuwatch.Host, v *cpuwatch.Verdict, now time.Time) {
	for _, m := range v.Recipients(cfg.Notify) {
		fmt.Fprintf(w, "  → %s: %s\n", m.Name, cpuwatch.MasterMessage(win.Name, v, &m, now, true))
		win.Sent(m.Name, now) // отметка та же, что у настоящей отправки: список лиду даёт Host.Told
	}
	fmt.Fprintf(w, "  → %s: %s\n", cfg.Lead, cpuwatch.LeadMessage(win.Name, v, win.Told(), now))
}
