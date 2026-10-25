// procguard — сторож процессов умерших сессий Claude Code: раз в интервал
// находит процессы, у которых CLAUDE_PID указывает на мёртвую сессию, и
// снимает их деревья, прожившие после её смерти дольше min_dead (режим act),
// или только пишет в журнал, что снял бы (observe).
//
//	procguard run                              цикл (user-сервис)
//	procguard tick [-dry-run] [-min-dead 0s]   один проход; -dry-run печатает план и ничего не меняет
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"tmpguard/internal/deliver"
	"tmpguard/internal/orphans"
	"tmpguard/internal/policy"
	"tmpguard/internal/sessions"
)

const (
	fromName  = "procguard"
	procRoot  = "/proc"
	stampForm = "2006-01-02 15:04:05 MST"
	shortForm = "02.01 15:04"
	maxArgs   = 160
)

type app struct {
	home     string
	cfgPath  string
	stateDir string
	projects string
	reg      sessions.Registry
	uid      int
	log      *slog.Logger
}

func main() {
	lg := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{ReplaceAttr: dropTime}))
	home, err := os.UserHomeDir()
	if err != nil {
		lg.Error("procguard: домашняя папка", "err", err)
		os.Exit(1)
	}
	a := &app{
		home:     home,
		cfgPath:  filepath.Join(home, ".config", "procguard", "config.toml"),
		stateDir: filepath.Join(home, ".local", "state", "procguard"),
		projects: filepath.Join(home, ".claude", "projects"),
		reg:      sessions.ForHome(home),
		uid:      os.Getuid(),
		log:      lg,
	}
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "run":
		err = a.run(ctx)
	case "tick":
		fl := flag.NewFlagSet("tick", flag.ContinueOnError)
		dry := fl.Bool("dry-run", false, "напечатать план, ничего не снимать, не слать и не писать")
		minDead := fl.Duration("min-dead", -1, "порог вместо min_dead из конфига (только с -dry-run)")
		if perr := fl.Parse(args); perr != nil {
			lg.Error("procguard tick: флаги", "err", perr)
			usage()
		}
		err = a.tick(ctx, *dry, *minDead)
	default:
		usage()
	}
	if err != nil {
		lg.Error("procguard "+cmd, "err", err)
		stop()
		os.Exit(1)
	}
}

// dropTime убирает время из строк: его ставит journald.
func dropTime(_ []string, at slog.Attr) slog.Attr {
	if at.Key == slog.TimeKey {
		return slog.Attr{}
	}
	return at
}

func usage() {
	fmt.Fprintln(os.Stderr, "использование: procguard run | tick [-dry-run] [-min-dead 0s]")
	os.Exit(2)
}

func (a *app) statePath() string   { return filepath.Join(a.stateDir, "state.json") }
func (a *app) journalPath() string { return filepath.Join(a.stateDir, "journal.log") }

func (a *app) run(ctx context.Context) error {
	if err := os.MkdirAll(a.stateDir, 0o700); err != nil {
		return fmt.Errorf("папка состояния: %w", err)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(a.pass(ctx)):
		}
	}
}

// pass — проход цикла; возвращает паузу до следующего. Конфиг не читается —
// проход пропускается (ничего не снимаем по неизвестным правилам), пауза по умолчанию.
func (a *app) pass(ctx context.Context) time.Duration {
	cfg, err := orphans.Load(a.cfgPath, a.home)
	if err != nil {
		a.log.Error("procguard: конфиг, проход пропущен", "err", err)
		return orphans.Default(a.home).Interval.D()
	}
	if err := a.round(ctx, &cfg, false); err != nil {
		a.log.Error("procguard: проход", "err", err)
	}
	return cfg.Interval.D()
}

func (a *app) tick(ctx context.Context, dry bool, minDead time.Duration) error {
	cfg, err := orphans.Load(a.cfgPath, a.home)
	if err != nil {
		return err
	}
	if minDead >= 0 {
		if !dry {
			return errors.New("-min-dead только вместе с -dry-run")
		}
		cfg.MinDead = policy.Duration(minDead)
	}
	if !dry {
		if err := os.MkdirAll(a.stateDir, 0o700); err != nil {
			return fmt.Errorf("папка состояния: %w", err)
		}
	}
	return a.round(ctx, &cfg, dry)
}

// round — один проход: снимок, план, журнал, снятие и сводки (в act).
func (a *app) round(ctx context.Context, cfg *orphans.Config, dry bool) error {
	now := time.Now()
	snap, err := orphans.ReadSnapshot(procRoot, a.uid, now)
	if snap == nil {
		return err
	}
	if err != nil {
		a.log.Warn("procguard: часть процессов не прочиталась", "err", err)
	}
	st, err := orphans.LoadState(a.statePath())
	if err != nil { //nolint:fallback-return // битое состояние — начать заново: сроки смерти отсчитаются с этого прохода, то есть позже, не раньше
		a.log.Warn("procguard: состояние сброшено", "err", err)
	}
	trees := orphans.Plan(snap, cfg, st.Seen)
	info := a.describe(trees)
	counts := map[orphans.Verdict]int{}
	for i := range trees {
		counts[trees[i].Verdict]++
	}
	a.log.Info(fmt.Sprintf("procguard %s: деревьев сирот %d: снять %d, ждут %d, не трогать %d",
		cfg.Mode, len(trees), counts[orphans.Reap], counts[orphans.Wait], counts[orphans.Skip]))
	if dry {
		a.printPlan(trees, info, now)
		return nil
	}
	alive, err := a.reg.Alive()
	if err != nil { //nolint:fallback-return // Alive отдаёт прочитанную часть реестра; неизвестного адресата заменит лид
		a.log.Warn("procguard: реестр сессий", "err", err)
	}
	if cfg.Mode == orphans.ModeAct {
		a.act(ctx, cfg, trees, info, alive, now)
	} else {
		a.observe(cfg, trees, info, alive, st, now)
	}
	st.Prune(trees, now)
	return st.Save(a.statePath())
}

// observe пишет в журнал «снял бы» — по разу на дерево.
func (a *app) observe(cfg *orphans.Config, trees []orphans.Tree, info map[string]orphans.SessionInfo,
	alive map[string]sessions.Info, st *orphans.State, now time.Time) {
	var lines []string
	for i := range trees {
		t := &trees[i]
		if t.Verdict != orphans.Reap || !st.Logged[t.RootKey()].IsZero() {
			continue
		}
		in := info[t.Session.ID]
		to, note := a.recipient(cfg, sessionCwd(t, in), alive, now)
		lines = append(lines, a.journalLine(now, "снял бы", t, in, "сводка: "+recipientText(to, note), planText(t)))
		st.Logged[t.RootKey()] = now
	}
	a.appendJournal(lines)
}

// act снимает деревья, пишет журнал и шлёт сводки.
func (a *app) act(ctx context.Context, cfg *orphans.Config, trees []orphans.Tree, info map[string]orphans.SessionInfo,
	alive map[string]sessions.Info, now time.Time) {
	var reap []*orphans.Tree
	for i := range trees {
		if trees[i].Verdict == orphans.Reap {
			reap = append(reap, &trees[i])
		}
	}
	if len(reap) == 0 {
		return
	}
	outs := orphans.Stop(ctx, reap, cfg.TermWait.D(), procRoot)
	byTo := map[string][]string{}
	var lines []string
	for i := range outs {
		o := &outs[i]
		in := info[o.Tree.Session.ID]
		to, note := a.recipient(cfg, sessionCwd(o.Tree, in), alive, now)
		lines = append(lines, a.journalLine(time.Now(), "снял", o.Tree, in, "сводка: "+recipientText(to, note), o.Text()))
		if to != "" {
			byTo[to] = append(byTo[to], a.summaryLine(o, in, now))
		}
	}
	a.appendJournal(lines)
	names := make([]string, 0, len(byTo))
	for to := range byTo {
		names = append(names, to)
	}
	sort.Strings(names)
	for _, to := range names {
		if err := a.send(cfg, alive[to], a.message(cfg, byTo[to])); err != nil {
			a.log.Warn("procguard: сводка не доставлена", "to", to, "err", err)
			continue
		}
		a.log.Info("procguard: сводка", "to", to, "trees", len(byTo[to]))
	}
}

// recipient — кому сводка по сессии с этим cwd: админ пространства, иначе лид;
// "" с пояснением, если пространство молчит или адресатов нет в живых.
func (a *app) recipient(cfg *orphans.Config, cwd string, alive map[string]sessions.Info, now time.Time) (string, string) {
	if key, until := orphans.LongestPrefix(cfg.Notify.MuteUntil, cwd); key != "" {
		if t, err := orphans.ParseUntil(until); err == nil && now.Before(t) {
			return "", fmt.Sprintf("%s молчит до %s", a.tilde(key), until)
		}
	}
	_, to := orphans.LongestPrefix(cfg.Notify.Admins, cwd)
	if to == "" {
		to = cfg.Lead
	}
	if alive[to].SocketPath != "" {
		return to, ""
	}
	if alive[cfg.Lead].SocketPath != "" {
		return cfg.Lead, to + " не в сети"
	}
	if to == cfg.Lead {
		return "", to + " нет в живых"
	}
	return "", fmt.Sprintf("ни %s, ни %s нет в живых", to, cfg.Lead)
}

func recipientText(to, note string) string {
	switch {
	case to == "":
		return "нет (" + note + ")"
	case note != "":
		return to + " (" + note + ")"
	default:
		return to
	}
}

func (a *app) send(cfg *orphans.Config, in sessions.Info, msg string) error {
	token := ""
	if cfg.SendAuth {
		var err error
		if token, err = a.reg.Token(in.Pid); err != nil {
			return fmt.Errorf("токен %s: %w", in.Name, err)
		}
	}
	if err := deliver.SendFrom(fromName, in.SocketPath, token, msg); err != nil {
		return fmt.Errorf("сокет %s: %w", in.Name, err)
	}
	return nil
}

func (a *app) message(cfg *orphans.Config, lines []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "procguard: снял процессы умерших сессий Claude Code (жили после смерти сессии дольше %s). Отвечать не нужно.\n",
		policy.HumanAge(cfg.MinDead.D()))
	for _, l := range lines {
		b.WriteString("- " + l + "\n")
	}
	fmt.Fprintf(&b, "Нужный процесс держите юнитом (systemd-run --user, graft start) или исключением в %s. Журнал: %s",
		a.tilde(a.cfgPath), a.tilde(a.journalPath()))
	return b.String()
}

func (a *app) summaryLine(o *orphans.Outcome, in orphans.SessionInfo, now time.Time) string {
	t := o.Tree
	return fmt.Sprintf("%s pid %d (%s), процессов %d, жил %s; сессия %d, %s: %s",
		t.Root.Comm, t.Root.Pid, clip(t.Root.Args), len(t.Members), policy.HumanAge(now.Sub(t.Root.StartAt)),
		t.Session.Pid, a.tilde(sessionCwd(t, in)), o.Text())
}

// journalLine — строка журнала: время, что, дерево, сессия, адресат, действие, команда.
func (a *app) journalLine(at time.Time, what string, t *orphans.Tree, in orphans.SessionInfo, to, action string) string {
	sess := fmt.Sprintf("сессия %d %s, cwd %s", t.Session.Pid, t.Session.ID, a.tilde(sessionCwd(t, in)))
	if in.Transcript != "" {
		sess += fmt.Sprintf(", транскрипт %s (последняя запись %s)", a.tilde(in.Transcript), in.LastWrite.Format(shortForm))
	}
	return strings.Join([]string{
		at.Format(stampForm), what,
		fmt.Sprintf("%s pid %d, процессов %d, жил %s; %s", t.Root.Comm, t.Root.Pid, len(t.Members),
			policy.HumanAge(at.Sub(t.Root.StartAt)), t.Reason),
		sess, "cwd процесса " + a.tilde(t.Root.Cwd), to, action, ":: " + clip(t.Root.Args),
	}, "\t")
}

func planText(t *orphans.Tree) string {
	if t.Container != "" {
		return "podman stop " + orphans.ShortID(t.Container)
	}
	return fmt.Sprintf("SIGTERM %d процессам, через срок SIGKILL", len(t.Members))
}

func (a *app) appendJournal(lines []string) {
	if len(lines) == 0 {
		return
	}
	f, err := os.OpenFile(a.journalPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		a.log.Error("procguard: журнал", "err", err)
		return
	}
	if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		a.log.Error("procguard: журнал", "err", err)
	}
	if err := f.Close(); err != nil {
		a.log.Error("procguard: журнал", "err", err)
	}
}

// describe — транскрипты мёртвых сессий по id.
func (a *app) describe(trees []orphans.Tree) map[string]orphans.SessionInfo {
	out := map[string]orphans.SessionInfo{}
	for i := range trees {
		id := trees[i].Session.ID
		if _, ok := out[id]; ok || id == "" {
			continue
		}
		in, err := orphans.Describe(a.projects, id)
		if err != nil {
			a.log.Warn("procguard: транскрипт не прочитан, в журнале будет cwd процесса", "session", id, "err", err)
			out[id] = orphans.SessionInfo{}
			continue
		}
		out[id] = in
	}
	return out
}

func (a *app) printPlan(trees []orphans.Tree, info map[string]orphans.SessionInfo, now time.Time) {
	if len(trees) == 0 {
		fmt.Println("сирот нет")
		return
	}
	for i := range trees {
		t := &trees[i]
		in := info[t.Session.ID]
		fmt.Printf("[%s] %s pid %d, процессов %d, жил %s — %s\n    сессия %d %s, cwd %s; cwd процесса %s\n    действие: %s\n    :: %s\n",
			t.Verdict, t.Root.Comm, t.Root.Pid, len(t.Members), policy.HumanAge(now.Sub(t.Root.StartAt)), t.Reason,
			t.Session.Pid, t.Session.ID, a.tilde(sessionCwd(t, in)), a.tilde(t.Root.Cwd), planText(t), clip(t.Root.Args))
	}
}

// sessionCwd — cwd сессии из транскрипта, а нет его — cwd корня дерева.
func sessionCwd(t *orphans.Tree, in orphans.SessionInfo) string {
	if in.Cwd != "" {
		return in.Cwd
	}
	return t.Root.Cwd
}

func (a *app) tilde(p string) string {
	if rest, ok := strings.CutPrefix(p, a.home); ok && (rest == "" || rest[0] == '/') {
		return "~" + rest
	}
	return p
}

func clip(s string) string {
	if r := []rune(s); len(r) > maxArgs {
		return string(r[:maxArgs]) + "…"
	}
	return s
}
