// aguard — страж агентов: один демон с одним конфигом
// (~/.config/aguard/config.toml) следит за всеми сессиями агентов и машиной.
// Наблюдатели идут каждый со своим интервалом:
//
//   - orphans — процессы умерших сессий Claude Code (бывший procguard);
//
//   - tmp — записи в /tmp по снимку tmpguard-track и сводки владельцам (бывший tmpguard);
//
//   - memory — пики памяти сессий для выбора мягкого потолка;
//
//   - restart — ответ cl в сокет, можно ли перезапустить сессию на новую версию Claude Code;
//
//   - waits — отказы и ожидания агентов по транскриптам: таймауты хуков, ошибки API, сжатия;
//
//   - compact — своё сжатие сессий в простое, пока кэш подсказки жив: пока только журнал «сжал бы» и сверка;
//
//   - disk — рост папок кэшей и состояний за сутки, остатки временного, быстрое наполнение: журнал, кому сообщил бы;
//
//   - said — реплики пользователя лиду, админам и мастерам: правила, решения и обещания — в ~/.claude/said.tsv.
//
//     aguard run                                         демон (user-сервис)
//     aguard orphans tick [-dry-run] [-min-dead 0s]      один проход сирот
//     aguard tmp tick [-dry-run] | report [-since 24h] | send-test [-to lead] [-dry-run]
//     aguard memory tick [-dry-run] | report [-since 168h]
//     aguard restart ask -pid N [-name имя]              вопрос, как его задаёт cl
//     aguard waits tick [-dry-run] | report [-since 24h]
//     aguard compact tick [-dry-run] | report [-since 168h] [-json] [-at время]
//     aguard disk tick [-dry-run] | retro [-days 7] | traces [-all] | report [-since 168h]
//     aguard said tick [-dry-run] | report [-since 168h]  разметка реплик, расход по суткам
//     aguard report [-json] [-at время]                 срабатывания и сбои за сутки, неделю, месяц
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"github.com/aiseeq/aguard/internal/activity"
	"github.com/aiseeq/aguard/internal/config"
	"github.com/aiseeq/aguard/internal/journal"
	"github.com/aiseeq/aguard/internal/memwatch"
	"github.com/aiseeq/aguard/internal/restart"
	"github.com/aiseeq/akit/session"
)

const procRoot = "/proc"

type app struct {
	home     string
	cfgPath  string
	stateDir string // ~/.local/state/aguard: у каждого наблюдателя своя подпапка
	projects string
	reg      session.Registry
	uid      int
	log      *slog.Logger
	out      io.Writer          // вывод команд для человека и агента (dry-run, report)
	act      *activity.Recorder // учёт срабатываний; только у демона, у команд nil
}

func main() {
	lg := journal.New()
	home, err := os.UserHomeDir()
	if err != nil {
		lg.Error("aguard: домашняя папка", "err", err)
		os.Exit(1)
	}
	a := &app{
		home:     home,
		cfgPath:  config.Path(home),
		stateDir: filepath.Join(home, ".local", "state", "aguard"),
		projects: filepath.Join(home, ".claude", "projects"),
		reg:      session.ForHome(home),
		uid:      os.Getuid(),
		log:      lg,
		out:      os.Stdout,
	}
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	cmd := os.Args[1]
	switch cmd {
	case "run":
		err = a.run(ctx)
	case "orphans":
		err = a.orphansCmd(ctx, os.Args[2:])
	case "tmp":
		err = a.tmpCmd(os.Args[2:])
	case "memory":
		err = a.memoryCmd(os.Args[2:])
	case "restart":
		err = a.restartCmd(os.Args[2:])
	case "waits":
		err = a.waitsCmd(os.Args[2:])
	case "compact":
		err = a.compactCmd(os.Args[2:])
	case "disk":
		err = a.diskCmd(os.Args[2:])
	case "said":
		err = a.saidCmd(ctx, os.Args[2:])
	case "report":
		err = a.reportCmd(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		lg.Error("aguard "+cmd, "err", err)
		stop()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `использование:
  aguard run
  aguard orphans tick [-dry-run] [-min-dead 0s]
  aguard tmp tick [-dry-run] | report [-since 24h] | send-test [-to lead] [-dry-run]
  aguard memory tick [-dry-run] | report [-since 168h]
  aguard restart ask -pid N [-name имя]
  aguard waits tick [-dry-run] | report [-since 24h]
  aguard compact tick [-dry-run] | report [-since 168h] [-json] [-at время RFC3339]
  aguard disk tick [-dry-run] | retro [-days 7] | traces [-all] | report [-since 168h]
  aguard said tick [-dry-run] | report [-since 168h]
  aguard report [-json] [-at время RFC3339]`)
	os.Exit(2)
}

// sub — подкоманда наблюдателя и её аргументы.
func sub(args []string) (string, []string) {
	if len(args) == 0 {
		usage()
	}
	return args[0], args[1:]
}

// parse разбирает флаги подкоманды; ошибка — текст ошибки и справка, код 2.
func parse(fl *flag.FlagSet, args []string) {
	fl.SetOutput(io.Discard)
	if err := fl.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "aguard %s: %v\n", fl.Name(), err)
		fl.SetOutput(os.Stderr)
		fl.PrintDefaults()
		os.Exit(2)
	}
}

func (a *app) closeWatcher(mw *memwatch.Watcher) {
	if err := mw.Close(); err != nil {
		a.log.Warn("aguard memory: закрытие", "err", err)
	}
}

func (a *app) config() (config.Config, error) { return config.Load(a.cfgPath, a.home, a.uid) }

func (a *app) dir(name string) string { return filepath.Join(a.stateDir, name) }

// watcher — наблюдатель демона: проход и пауза до следующего из конфига.
type watcher struct {
	name  string
	pass  func(ctx context.Context, cfg *config.Config) error
	every func(cfg *config.Config) time.Duration
}

// run — демон: наблюдатели в своих горутинах. Конфиг перечитывается каждый
// проход; не прочитался — проход пропускается (по неизвестным правилам ничего
// не делаем), пауза — по умолчанию.
func (a *app) run(ctx context.Context) error {
	for _, n := range []string{"orphans", "tmp", "memory", "waits", "compact", "disk", "said", "activity"} {
		if err := os.MkdirAll(a.dir(n), 0o700); err != nil {
			return fmt.Errorf("папка состояния: %w", err)
		}
	}
	mw := memwatch.NewWatcher(a.reg)
	defer a.closeWatcher(mw)
	judge := restart.NewJudge(a.home) // общий разбор транскриптов для ответов cl и сжатия
	ws := []watcher{
		{"orphans", func(ctx context.Context, c *config.Config) error { return a.orphansRound(ctx, &c.Orphans, false) },
			func(c *config.Config) time.Duration { return c.Orphans.Interval.D() }},
		{"tmp", func(_ context.Context, c *config.Config) error { return a.tmpRound(&c.Tmp, false) },
			func(c *config.Config) time.Duration { return c.Tmp.Interval.D() }},
		{"memory", func(_ context.Context, c *config.Config) error { return a.memoryRound(mw, &c.Memory, false) },
			func(c *config.Config) time.Duration { return c.Memory.Interval.D() }},
		{"waits", func(_ context.Context, c *config.Config) error { return a.waitsRound(&c.Waits, false) },
			func(c *config.Config) time.Duration { return c.Waits.Interval.D() }},
		{"compact", func(_ context.Context, c *config.Config) error { return a.compactRound(judge, c, false) },
			func(c *config.Config) time.Duration { return c.Compact.Interval.D() }},
		{"disk", func(_ context.Context, c *config.Config) error { return a.diskRound(c) },
			func(c *config.Config) time.Duration { return c.Disk.Interval.D() }},
		{"said", func(ctx context.Context, c *config.Config) error { return a.saidRound(ctx, &c.Said, false) },
			func(c *config.Config) time.Duration { return c.Said.Interval.D() }},
	}
	a.act = activity.New(a.dir("activity"), time.Now())
	a.note("aguard", activity.KindStart, "", "", 0, "")
	var wg sync.WaitGroup
	wg.Go(func() { a.restartServe(ctx, judge) })
	for _, w := range ws {
		wg.Go(func() { a.loop(ctx, w) }) //nolint:unbounded-goroutine-fanout // наблюдателей шесть, список задан в коде выше
	}
	wg.Go(func() { a.flushLoop(ctx) })
	wg.Wait()
	a.flush() // остановка: несохранённое — в файл
	return nil
}

func (a *app) loop(ctx context.Context, w watcher) {
	for {
		pause := a.safePass(ctx, w)
		select {
		case <-ctx.Done():
			return
		case <-time.After(pause):
		}
	}
}

// safePass — один проход наблюдателя; паника пишется в журнал и не роняет
// остальных наблюдателей.
func (a *app) safePass(ctx context.Context, w watcher) (pause time.Duration) {
	def := config.Default(a.home)
	pause = w.every(&def)
	defer func() {
		if r := recover(); r != nil {
			a.log.Error("aguard "+w.name+": паника в проходе", "panic", r, "stack", string(debug.Stack()))
			a.note(w.name, activity.KindPanic, fmt.Sprint(r), "", 0, "")
		}
	}()
	a.note(w.name, activity.KindPass, "", "", 0, "")
	cfg, err := a.config()
	if err != nil {
		a.log.Error("aguard "+w.name+": конфиг, проход пропущен", "err", err)
		a.note(w.name, activity.KindConfigSkipped, "", "", 0, "")
		return pause
	}
	if err := w.pass(ctx, &cfg); err != nil {
		a.log.Error("aguard "+w.name+": проход", "err", err)
		a.note(w.name, activity.KindPassFailed, "", "", 0, "")
	}
	return w.every(&cfg)
}
