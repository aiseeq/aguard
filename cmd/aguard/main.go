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
//   - waits — отказы и ожидания агентов по транскриптам: таймауты хуков, ошибки API, сжатия.
//
//     aguard run                                         демон (user-сервис)
//     aguard orphans tick [-dry-run] [-min-dead 0s]      один проход сирот
//     aguard tmp tick [-dry-run] | report [-since 24h] | send-test [-to lead] [-dry-run]
//     aguard memory tick [-dry-run] | report [-since 168h]
//     aguard restart ask -pid N [-name имя]              вопрос, как его задаёт cl
//     aguard waits tick [-dry-run] | report [-since 24h]
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

	"github.com/aiseeq/aguard/internal/config"
	"github.com/aiseeq/aguard/internal/journal"
	"github.com/aiseeq/aguard/internal/memwatch"
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
	out      io.Writer // вывод команд для человека и агента (dry-run, report)
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
  aguard waits tick [-dry-run] | report [-since 24h]`)
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
	for _, n := range []string{"orphans", "tmp", "memory", "waits"} {
		if err := os.MkdirAll(a.dir(n), 0o700); err != nil {
			return fmt.Errorf("папка состояния: %w", err)
		}
	}
	mw := memwatch.NewWatcher(a.reg)
	defer a.closeWatcher(mw)
	ws := []watcher{
		{"orphans", func(ctx context.Context, c *config.Config) error { return a.orphansRound(ctx, &c.Orphans, false) },
			func(c *config.Config) time.Duration { return c.Orphans.Interval.D() }},
		{"tmp", func(_ context.Context, c *config.Config) error { return a.tmpRound(&c.Tmp, false) },
			func(c *config.Config) time.Duration { return c.Tmp.Interval.D() }},
		{"memory", func(_ context.Context, c *config.Config) error { return a.memoryRound(mw, &c.Memory, false) },
			func(c *config.Config) time.Duration { return c.Memory.Interval.D() }},
		{"waits", func(_ context.Context, c *config.Config) error { return a.waitsRound(&c.Waits, false) },
			func(c *config.Config) time.Duration { return c.Waits.Interval.D() }},
	}
	var wg sync.WaitGroup
	wg.Go(func() { a.restartServe(ctx) })
	for _, w := range ws {
		wg.Go(func() { a.loop(ctx, w) }) //nolint:unbounded-goroutine-fanout // наблюдателей четыре, список задан в коде выше
	}
	wg.Wait()
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
		}
	}()
	cfg, err := a.config()
	if err != nil {
		a.log.Error("aguard "+w.name+": конфиг, проход пропущен", "err", err)
		return pause
	}
	if err := w.pass(ctx, &cfg); err != nil {
		a.log.Error("aguard "+w.name+": проход", "err", err)
	}
	return w.every(&cfg)
}
