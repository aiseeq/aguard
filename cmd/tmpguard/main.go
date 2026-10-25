// tmpguard — user-часть сторожа временных файлов: раз в интервал читает
// снимок tmpguard-track, считает «размер × время» по владельцам и шлёт
// сводки виновникам (режим alert) или только пишет, что отправил бы (observe).
//
//	tmpguard run                 цикл (user-сервис)
//	tmpguard tick [-dry-run]     один проход; -dry-run печатает сводки и ничего не меняет
//	tmpguard report [-since 24h] распределение времени жизни по размерам и классам
//	tmpguard send-test [-to lead] одна тестовая сводка с пометкой «отвечать не нужно»
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	"tmpguard/internal/deliver"
	"tmpguard/internal/policy"
	"tmpguard/internal/record"
	"tmpguard/internal/report"
	"tmpguard/internal/sessions"
)

type app struct {
	home     string
	cfgPath  string
	stateDir string
	reg      sessions.Registry
	uid      int
}

func main() {
	log.SetFlags(0)
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatal(err)
	}
	a := &app{
		home:     home,
		cfgPath:  filepath.Join(home, ".config", "tmpguard", "config.toml"),
		stateDir: filepath.Join(home, ".local", "state", "tmpguard"),
		reg:      sessions.ForHome(home),
		uid:      os.Getuid(),
	}
	if len(os.Args) < 2 {
		usage()
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "run":
		err = a.run()
	case "tick":
		fl := flag.NewFlagSet("tick", flag.ExitOnError)
		dry := fl.Bool("dry-run", false, "напечатать сводки, ничего не слать и не менять")
		_ = fl.Parse(args)
		err = a.tick(*dry)
	case "report":
		fl := flag.NewFlagSet("report", flag.ExitOnError)
		since := fl.Duration("since", 0, "только записи, закрытые за этот срок (0 — весь журнал)")
		_ = fl.Parse(args)
		err = a.report(*since)
	case "send-test":
		fl := flag.NewFlagSet("send-test", flag.ExitOnError)
		to := fl.String("to", "", "имя сессии-адресата (по умолчанию лид из конфига)")
		dry := fl.Bool("dry-run", false, "только напечатать")
		_ = fl.Parse(args)
		err = a.sendTest(*to, *dry)
	default:
		usage()
	}
	if err != nil {
		log.Fatalf("tmpguard %s: %v", cmd, err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "использование: tmpguard run | tick [-dry-run] | report [-since 24h] | send-test [-to lead] [-dry-run]")
	os.Exit(2)
}

func (a *app) config() (policy.Config, error) { return policy.Load(a.cfgPath, a.home) }

// snapshot — свежий снимок трекера: /run, а если его нет — диск.
func (a *app) snapshot(cfg *policy.Config) (*record.State, error) {
	var best *record.State
	for _, dir := range []string{cfg.RunDir, cfg.TrackDir} {
		s, err := record.ReadState(filepath.Join(dir, record.StateFile))
		if err != nil {
			return nil, err
		}
		if best == nil || s.Updated.After(best.Updated) {
			best = s
		}
	}
	if best.Updated.IsZero() {
		return nil, errors.New("снимка tmpguard-track нет: system-сервис tmpguard-track не запущен?")
	}
	return best, nil
}

// liveSessions — policy.Live поверх реестра сессий.
type liveSessions struct {
	reg    sessions.Registry
	byName map[string]sessions.Info
}

func (l *liveSessions) ByPid(pid int, procStart string) (policy.Target, bool) {
	if !l.reg.IsAlive(pid, procStart) {
		return policy.Target{}, false
	}
	in, err := l.reg.Get(pid)
	if err != nil {
		log.Printf("tmpguard: реестр сессий, pid %d: %v", pid, err)
		return policy.Target{}, false
	}
	if in == nil || in.SocketPath == "" {
		return policy.Target{}, false
	}
	return policy.Target{Name: in.Name, Pid: in.Pid, Socket: in.SocketPath}, true
}

func (l *liveSessions) ByName(name string) (policy.Target, bool) {
	in, ok := l.byName[name]
	if !ok || in.SocketPath == "" {
		return policy.Target{}, false
	}
	return policy.Target{Name: in.Name, Pid: in.Pid, Socket: in.SocketPath}, true
}

func (a *app) live() (*liveSessions, error) {
	m, err := a.reg.Alive()
	if err != nil {
		return nil, err
	}
	return &liveSessions{reg: a.reg, byName: m}, nil
}

// readPressure — давление памяти; не прочиталось — считаем, что давления нет,
// и пишем об этом в журнал.
func readPressure() policy.Pressure {
	var p policy.Pressure
	psi, err := os.ReadFile("/proc/pressure/memory")
	if err != nil {
		log.Printf("tmpguard: PSI: %v", err)
		return p
	}
	p.PSISomeAvg60 = policy.ParsePSI(string(psi))
	mi, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		log.Printf("tmpguard: meminfo: %v", err)
		return p
	}
	p.SwapUsedPct = policy.ParseSwapUsedPct(string(mi))
	return p
}

func (a *app) loadUserState() *policy.UserState {
	st := &policy.UserState{}
	b, err := os.ReadFile(filepath.Join(a.stateDir, "state.json"))
	if err == nil && json.Unmarshal(b, st) == nil {
		return st
	}
	return &policy.UserState{}
}

func (a *app) saveUserState(st *policy.UserState) error {
	b, err := json.MarshalIndent(st, "", " ")
	if err != nil {
		return err
	}
	return record.WriteFileAtomic(filepath.Join(a.stateDir, "state.json"), b, 0o600)
}

func copyAlerts(m map[string]*policy.AlertState) map[string]*policy.AlertState {
	out := make(map[string]*policy.AlertState, len(m))
	for k, v := range m {
		c := *v
		out[k] = &c
	}
	return out
}

func (a *app) run() error {
	if err := os.MkdirAll(a.stateDir, 0o700); err != nil {
		return err
	}
	for {
		cfg, err := a.config()
		interval := 5 * time.Minute
		if err != nil {
			log.Printf("tmpguard: конфиг: %v", err)
		} else {
			interval = cfg.Interval.D()
			if err := a.round(&cfg, false); err != nil {
				log.Printf("tmpguard: %v", err)
			}
		}
		time.Sleep(interval)
	}
}

func (a *app) tick(dry bool) error {
	cfg, err := a.config()
	if err != nil {
		return err
	}
	if !dry {
		if err := os.MkdirAll(a.stateDir, 0o700); err != nil {
			return err
		}
	}
	return a.round(&cfg, dry)
}

// round — один проход: учёт, план, доставка (или запись в observe), кеши.
func (a *app) round(cfg *policy.Config, dry bool) error {
	now := time.Now()
	snap, err := a.snapshot(cfg)
	if err != nil {
		return err
	}
	if age := now.Sub(snap.Updated); age > 3*cfg.Interval.D() {
		log.Printf("tmpguard: снимок трекера старый (%s): tmpguard-track жив?", policy.HumanAge(age))
	}
	live, err := a.live()
	if err != nil {
		return err
	}
	st := a.loadUserState()
	if st.Mode != cfg.Mode { // счётчики observe не должны сразу эскалировать в alert
		st = &policy.UserState{Mode: cfg.Mode}
	}
	prev := copyAlerts(st.Alerts)
	items := cfg.Items(snap.Entries, a.uid)
	factor := cfg.Factor(readPressure())
	groups := cfg.Over(items, factor)
	sums, deferred := cfg.Plan(now, groups, live, st, factor)
	log.Printf("tmpguard %s: %s, сводок %d (записей %d), отложено записей %d",
		cfg.Mode, overText(items, groups, factor), len(sums), policy.Count(sums), policy.Count(deferred))

	if dry {
		printDry(cfg, groups, sums, deferred, now)
		a.caches(cfg, items, true)
		return nil
	}
	var texts []byte
	for i := range sums {
		s := &sums[i]
		msg := cfg.Message(s, now, false)
		total := linesBytes(s.Lines)
		texts = fmt.Appendf(texts, "=== %s %s, записей %d\n%s\n\n", now.Format(time.RFC3339), s.Target.Key(), len(s.Lines), msg)
		if cfg.Mode != policy.ModeAlert {
			log.Printf("tmpguard observe: отправил бы %s: записей %d, %s", s.Target.Key(), len(s.Lines), policy.HumanBytes(total))
			continue
		}
		if err := a.send(cfg, s.Target, msg); err != nil {
			log.Printf("tmpguard: сводка %s не доставлена: %v", s.Target.Key(), err)
			for _, l := range s.Lines { // не доставлено — не считается
				k := policy.EntryKey(&l.Item)
				if p, ok := prev[k]; ok {
					st.Alerts[k] = p
				} else {
					delete(st.Alerts, k)
				}
			}
			continue
		}
		log.Printf("tmpguard: сводка %s: записей %d, %s", s.Target.Key(), len(s.Lines), policy.HumanBytes(total))
	}
	if len(texts) > 0 {
		if err := os.WriteFile(filepath.Join(a.stateDir, "last-summaries.txt"), texts, 0o600); err != nil {
			log.Printf("tmpguard: %v", err)
		}
	}
	a.caches(cfg, items, false)
	return a.saveUserState(st)
}

// caches — общие кеши: при превышении размера чистим старое содержимое
// (только в alert и только своё; в observe и dry-run — запись в журнал).
func (a *app) caches(cfg *policy.Config, items []policy.Item, dry bool) {
	for _, it := range items {
		if it.Class != policy.ClassCache || it.Entry.Bytes <= int64(cfg.Cache.Max) {
			continue
		}
		if dry || cfg.Mode != policy.ModeAlert || !cfg.Cache.Clean {
			log.Printf("tmpguard: кеш %s %s > %s — почистил бы файлы старше %s", it.Entry.Path,
				policy.HumanBytes(it.Entry.Bytes), policy.HumanBytes(int64(cfg.Cache.Max)), cfg.Cache.CleanOlder.D())
			continue
		}
		n, freed := cleanOld(it.Entry.Path, time.Now().Add(-cfg.Cache.CleanOlder.D()), a.uid)
		log.Printf("tmpguard: кеш %s: удалено файлов %d, %s", it.Entry.Path, n, policy.HumanBytes(freed))
	}
}

// cleanOld удаляет свои файлы, к которым не обращались и которые не меняли
// с cutoff, затем опустевшие папки (кроме самой папки кеша).
func cleanOld(root string, cutoff time.Time, uid int) (n int, freed int64) {
	var dirs []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != root {
				dirs = append(dirs, p)
			}
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		at, ok := statTimes(fi)
		if !ok || at.uid != uid || at.mtime.After(cutoff) || at.atime.After(cutoff) {
			return nil
		}
		if os.Remove(p) == nil {
			n++
			freed += at.blocks * 512
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Remove(dirs[i]) // удалится только пустая
	}
	return n, freed
}

func (a *app) report(since time.Duration) error {
	cfg, err := a.config()
	if err != nil {
		return err
	}
	snap, err := a.snapshot(&cfg)
	if err != nil {
		return err
	}
	var events []record.Event
	bad, err := record.ReadEventFiles(cfg.TrackDir, func(e record.Event) { events = append(events, e) })
	if err != nil {
		return err
	}
	now := time.Now()
	from := time.Time{}
	if since > 0 {
		from = now.Add(-since)
	}
	d := report.Collect(&cfg, events, snap, from, a.uid, now)
	d.BadLines = bad
	report.Print(os.Stdout, d)

	live, err := a.live()
	if err != nil {
		return err
	}
	factor := cfg.Factor(readPressure())
	items := cfg.Items(snap.Entries, a.uid)
	groups := cfg.Over(items, factor)
	fmt.Printf("\n5. Сейчас (режим %s): %s.\n", cfg.Mode, overText(items, groups, factor))
	st := a.loadUserState() // только читаем: report счётчики не меняет
	if st.Mode != cfg.Mode {
		st = &policy.UserState{Mode: cfg.Mode}
	}
	sums, deferred := cfg.Plan(now, groups, live, st, factor)
	for _, s := range sums {
		fmt.Printf("   ближайшая сводка %s: записей %d, %s\n", s.Target.Key(), len(s.Lines), policy.HumanBytes(linesBytes(s.Lines)))
	}
	for _, s := range deferred {
		fmt.Printf("   отложено для %s до %s: записей %d, %s\n", s.Target.Key(), s.NextAt.Format("15:04"), len(s.Lines), policy.HumanBytes(linesBytes(s.Lines)))
	}
	return nil
}

func (a *app) sendTest(to string, dry bool) error {
	cfg, err := a.config()
	if err != nil {
		return err
	}
	if to == "" {
		to = cfg.Lead
	}
	live, err := a.live()
	if err != nil {
		return err
	}
	target, ok := live.ByName(to)
	if !ok {
		return fmt.Errorf("живой сессии %q нет", to)
	}
	snap, err := a.snapshot(&cfg)
	if err != nil {
		return err
	}
	now := time.Now()
	factor := cfg.Factor(readPressure())
	st := &policy.UserState{}
	var sum *policy.Summary
	planned, _ := cfg.Plan(now, cfg.Over(cfg.Items(snap.Entries, a.uid), factor), live, st, factor)
	for _, s := range planned {
		if s.Target.Key() == target.Key() {
			sum = &s
		}
	}
	if sum == nil { // адресату сейчас нечего слать — покажем формат на любых записях сверх порога
		sum = &policy.Summary{Target: target, Factor: factor, Pressure: factor < 1}
		for _, g := range cfg.Over(cfg.Items(snap.Entries, a.uid), factor) {
			for _, it := range g.Items {
				sum.Lines = append(sum.Lines, testLine(it))
			}
		}
	}
	sort.SliceStable(sum.Lines, func(i, j int) bool { return sum.Lines[i].Rel < sum.Lines[j].Rel })
	if len(sum.Lines) > 5 {
		sum.Lines = sum.Lines[:5]
	}
	msg := cfg.Message(sum, now, true)
	fmt.Println(msg)
	if dry {
		return nil
	}
	if err := a.send(&cfg, target, msg); err != nil {
		return err
	}
	fmt.Printf("\nотправлено в %s (%s)\n", target.Key(), target.Socket)
	return nil
}

// testLine — строка тестовой сводки для записи, адресованной не этой сессии.
func testLine(it policy.Item) policy.Line {
	l := policy.Line{Item: it, Rel: policy.RelNoCreator}
	switch it.Class {
	case policy.ClassSession, policy.ClassFighter:
		l.Rel, l.CreatorName = policy.RelIgnored, it.Entry.Owner.Session.Name
	case policy.ClassNonAgent:
		l.Rel = policy.RelNonAgent
	case policy.ClassOtherUID:
		l.Rel = policy.RelOtherUID
	}
	return l
}

// send доставляет текст в сокет сессии; токен — только если включён send_auth.
func (a *app) send(cfg *policy.Config, t policy.Target, msg string) error {
	token := ""
	if cfg.SendAuth {
		var err error
		if token, err = a.reg.Token(t.Pid); err != nil {
			return fmt.Errorf("токен %s: %w", t.Key(), err)
		}
	}
	return deliver.Send(t.Socket, token, msg)
}

func linesBytes(ls []policy.Line) int64 {
	var n int64
	for _, l := range ls {
		n += l.Item.Entry.Bytes
	}
	return n
}

// overText — «владельцев сверх порога N (записей M, X)»: у записей без
// создателя владелец — сама запись, поэтому числа могут совпадать.
func overText(items []policy.Item, groups []policy.Group, factor float64) string {
	n := 0
	var b int64
	for _, g := range groups {
		n += len(g.Items)
		b += g.Bytes
	}
	return fmt.Sprintf("записей всего %d; владельцев сверх порога %d (их записей %d, %s); порог ×%s",
		len(items), len(groups), n, policy.HumanBytes(b), policy.FormatGBMin(factor))
}

// printDry печатает то, что ушло бы в этот проход, и отложенное: строки,
// которые верхняя ступень уже получала, и когда им будет повтор.
func printDry(cfg *policy.Config, groups []policy.Group, sums, deferred []policy.Summary, now time.Time) {
	for i := range sums {
		fmt.Printf("=== сводка %s (pid %d), записей %d\n%s\n\n", sums[i].Target.Key(), sums[i].Target.Pid,
			len(sums[i].Lines), cfg.Message(&sums[i], now, false))
	}
	for i := range deferred {
		fmt.Printf("=== отложено: %s уже получал(а) эти записи, повтор не раньше %s (top_repeat), записей %d; текст повтора:\n%s\n\n",
			deferred[i].Target.Key(), deferred[i].NextAt.Format("15:04"), len(deferred[i].Lines), cfg.Message(&deferred[i], now, false))
	}
	switch {
	case len(groups) == 0:
		fmt.Println("сводок нет: никто не превысил порог")
	case len(sums) == 0 && len(deferred) == 0:
		fmt.Println("сводок нет: владельцы сверх порога есть, но ни их сессий, ни лида нет в живых")
	case len(sums) == 0:
		fmt.Println("в этот проход сводок нет: всё сверх порога отложено до повтора верхней ступени")
	}
}
