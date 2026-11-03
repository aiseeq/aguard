// aguard — страж агентов: один демон с одним конфигом
// (~/.config/aguard/config.toml) следит за всеми сессиями агентов и машиной.
// Наблюдатели идут каждый со своим интервалом:
//
//   - orphans — процессы умерших сессий Claude Code (бывший procguard);
//
//   - tmp — записи в /tmp по снимку слежения и сводки владельцам (бывший tmpguard); слежение (internal/track) — тоже здесь;
//
//   - memory — пики памяти сессий для выбора мягкого потолка;
//
//   - restart — ответ cl в сокет, можно ли перезапустить сессию на новую версию Claude Code;
//
//   - waits — отказы и ожидания агентов по транскриптам: таймауты хуков, ошибки API, сжатия;
//
//   - agentwaits — ожидания, которые агенты заводят сами: чего ждали, сколько, во сколько вызовов и токенов;
//
//   - compact — своё сжатие сессий в простое, пока кэш подсказки жив: пока только журнал «сжал бы» и сверка;
//
//   - disk — рост папок кэшей и состояний за сутки, остатки временного, быстрое наполнение: журнал, кому сообщил бы;
//
//   - said — реплики пользователя лиду, админам и мастерам: правила, решения и обещания — в ~/.claude/said.tsv;
//
//   - cpu — перегрузка процессора ноута и PC: письмо мастерам, чьих готовых задач больше всего, и сводка лиду.
//
//   - remote — сломанный путь graft на PC: письмо владельцу задачи и сводка лиду.
//
//   - codex — недельный лимит Codex по каждой учётке: письмо лиду за ~2 ч до 100% раньше планового сброса и при 100%,
//     письмо, когда лимит кончился на всех учётках.
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
//     aguard bgtask tick [-dry-run]                     забытые фоновые задачи: письмо владельцу
//     aguard agentwaits tick [-dry-run] | report [-json] [-window week]  ожидания, которые агенты заводят сами
//     aguard cpu tick [-dry-run]                        замер ноута и PC: готовые задачи по владельцам
//     aguard remote tick [-dry-run] [-file путь] [-from-start]  события сломанного пути graft
//     aguard codex tick [-dry-run]                      замер лимита Codex: темп, прогноз 100%, письма лиду
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
	"github.com/aiseeq/aguard/internal/disk"
	"github.com/aiseeq/aguard/internal/journal"
	"github.com/aiseeq/aguard/internal/memwatch"
	"github.com/aiseeq/aguard/internal/restart"
	"github.com/aiseeq/aguard/internal/track"
	"github.com/aiseeq/aguard/internal/trackfd"
	"github.com/aiseeq/akit/session"
)

const procRoot = "/proc"

type app struct {
	home           string
	cfgPath        string
	stateDir       string          // ~/.local/state/aguard: у каждого наблюдателя своя подпапка
	nightSent      nightState      // отправленное ночным сторожем; nil — ещё не прочитано из state.json
	nightLogged    map[string]bool // mode=log: ключи, о которых уже написал «написал бы»
	nightFb        fallbackState   // запуски запасного Сна; nil — ещё не прочитано из fallback.json
	units          unitRunner      // запуск запасного Сна отдельным unit; nil — запасной Сон не запускается (ошибка)
	projects       string
	reg            session.Registry
	uid            int
	log            *slog.Logger
	out            io.Writer                    // вывод команд для человека и агента (dry-run, report)
	act            *activity.Recorder           // учёт срабатываний; только у демона, у команд nil
	cpu            map[string]*cpuHost          // окна наблюдателя cpu по машинам ("" — своя): живут между проходами
	strayRead      strayReader                  // замер процессов без родителя; подменяется в тестах
	trackFetch     func() (trackfd.Pair, error) // пара дескрипторов слежения; подменяется в тестах
	diskState      *disk.State                  // состояние наблюдателя disk; загружается один раз демоном
	diskWalkLoaded bool                         // sizes и traces загружены до первого обхода
	codexStep      time.Duration                // шаг codex, выбранный прошлым проходом; 0 — interval
	codexRead      codexReader                  // замер лимита Codex; подменяется в тестах
	codexConsume   codexConsumer                // досрочный сброс Codex; подменяется в тестах
	codexApproved  codexApprovals               // «да» на окно из файла одобрений
	codexNote      codexNoter                   // строка исхода в тикет
}

func main() {
	lg := journal.New()
	home, err := os.UserHomeDir()
	if err != nil {
		lg.Error("aguard: домашняя папка", "err", err)
		os.Exit(1)
	}
	a := &app{
		home:          home,
		cfgPath:       config.Path(home),
		stateDir:      filepath.Join(home, ".local", "state", "aguard"),
		projects:      filepath.Join(home, ".claude", "projects"),
		reg:           session.ForHome(home),
		uid:           os.Getuid(),
		log:           lg,
		out:           os.Stdout,
		units:         systemdUnits{},
		codexRead:     readCodex,
		codexConsume:  consumeCodex,
		codexApproved: approvedCodex,
		codexNote:     noteTicket,
	}
	a.strayRead = a.readStrays
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	cmd := os.Args[1]
	err = a.dispatch(ctx, cmd, os.Args[2:])
	if err != nil {
		lg.Error("aguard "+cmd, "err", err)
		stop()
		os.Exit(1)
	}
}

func (a *app) dispatch(ctx context.Context, cmd string, args []string) error {
	switch cmd {
	case "run":
		return a.run(ctx)
	case "orphans":
		return a.orphansCmd(ctx, args)
	case "tmp":
		return a.tmpCmd(args)
	case "track":
		return a.trackCmd(args)
	case "memory":
		return a.memoryCmd(args)
	case "restart":
		return a.restartCmd(args)
	case "waits":
		return a.waitsCmd(args)
	case "compact":
		return a.compactCmd(args)
	case "disk":
		return a.diskCmd(ctx, args)
	case "said":
		return a.saidCmd(ctx, args)
	case "bgtask":
		return a.bgtaskCmd(args)
	case "agentwaits":
		return a.agentwaitsCmd(args)
	case "cpu":
		return a.cpuCmd(ctx, args)
	case "io":
		return a.ioCmd(ctx, args)
	case "gui":
		return a.guiCmd(ctx, args)
	case "remote":
		return a.remoteCmd(args)
	case "codex":
		return a.codexCmd(ctx, args)
	case "night":
		return a.nightCmd(args)
	case "report":
		return a.reportCmd(args)
	default:
		usage()
	}
	return nil
}

func usage() {
	fmt.Fprintln(os.Stderr, `использование:
  aguard run
  aguard orphans tick [-dry-run] [-min-dead 0s]
  aguard tmp tick [-dry-run] | report [-since 24h] | send-test [-to lead] [-dry-run]
  aguard track check
  aguard memory tick [-dry-run] | report [-since 168h]
  aguard restart ask -pid N [-name имя]
  aguard waits tick [-dry-run] | report [-since 24h]
  aguard compact tick [-dry-run] | report [-since 168h] [-json] [-at время RFC3339]
  aguard disk tick [-dry-run] | retro [-days 7] | traces [-all] | report [-since 168h]
  aguard said tick [-dry-run] | report [-since 168h]
  aguard bgtask tick [-dry-run]
  aguard agentwaits tick [-dry-run] | report [-json] [-window week]
  aguard cpu tick [-dry-run]
  aguard io report [-since 24h] [-min 0] [-json] [-file сводки]   прошлые фризы: случаи, виновники
  aguard io now [-kind вид] [-json] | series [-since 1h] [-json] | profiles [-kind вид] [-since 7d] [-json]   из службы: сейчас, посекундный ряд, профили задач
  aguard io record [-for 30s] [-dir папка] | dump | at время [-window 2m] [-dir папка]   ручные снимки: at читает только их — прошлый фриз смотреть через io report и io series
  aguard gui watch -for 10s [-dir папка] [-pid имя=PID]… | report [-since 24h] [-json] [-dir папка]
  aguard io import [-dir папка] [-remove] [-out файл]   снимки событий прежнего наблюдателя — в сводки, при остановленной службе
  aguard night tick [-dry-run]
  aguard codex tick [-dry-run]
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
	started := time.Now()
	// io: SIGUSR1 — сводки случаев в файл, SIGUSR2 — ручной снимок буфера; подписка до всего, иначе
	// сигнал убил бы демон
	usr1, usr2 := make(chan os.Signal, 1), make(chan os.Signal, 1)
	signal.Notify(usr1, syscall.SIGUSR1)
	signal.Notify(usr2, syscall.SIGUSR2)
	defer signal.Stop(usr1)
	defer signal.Stop(usr2)
	for _, n := range []string{"orphans", "tmp", "memory", "waits", "agentwaits", "compact", "disk", "said", "bgtask", "cpu", "io", "gui", "remote", "codex", "night", "activity"} {
		if err := os.MkdirAll(a.dir(n), 0o700); err != nil {
			return fmt.Errorf("папка состояния: %w", err)
		}
	}
	mw := memwatch.NewWatcher(a.reg)
	defer a.closeWatcher(mw)
	judge := restart.NewJudge(a.home) // общий разбор транскриптов для ответов cl и сжатия
	ws := a.watchers(mw, judge)
	a.act = activity.New(a.dir("activity"), time.Now())
	a.note("aguard", activity.KindStart, "", "", 0, "")
	var wg sync.WaitGroup
	wg.Go(func() { a.restartServe(ctx, judge) })
	wg.Go(func() { a.ioLoop(ctx, usr1, usr2) })
	wg.Go(func() { a.guiLoop(ctx) })
	insts := track.Instances(a.home)
	for i := range insts {
		wg.Go(func() { a.trackLoop(ctx, &insts[i]) }) //nolint:unbounded-goroutine-fanout // экземпляров два, список задан в track.Instances
	}
	for _, w := range ws {
		wg.Go(func() { a.loop(ctx, w) }) //nolint:unbounded-goroutine-fanout // наблюдателей семь, список задан в watchers
	}
	wg.Go(func() { a.flushLoop(ctx) })
	wg.Wait()
	a.flush() // остановка: несохранённое — в файл
	a.logRunSummary(started)
	return nil
}

// watchers — наблюдатели демона по кругу; каждому нужна строка в reportSpecs.
func (a *app) watchers(mw *memwatch.Watcher, judge *restart.Judge) []watcher {
	return []watcher{
		{"orphans", func(ctx context.Context, c *config.Config) error { return a.orphansRound(ctx, &c.Orphans, false) },
			func(c *config.Config) time.Duration { return c.Orphans.Interval.D() }},
		{"tmp", func(_ context.Context, c *config.Config) error { return a.tmpRound(&c.Tmp, false) },
			func(c *config.Config) time.Duration { return c.Tmp.Interval.D() }},
		{"memory", func(_ context.Context, c *config.Config) error { return a.memoryRound(mw, &c.Memory, false) },
			func(c *config.Config) time.Duration { return c.Memory.Interval.D() }},
		{"waits", func(_ context.Context, c *config.Config) error { return a.waitsRound(&c.Waits, false) },
			func(c *config.Config) time.Duration { return c.Waits.Interval.D() }},
		{"agentwaits", func(_ context.Context, c *config.Config) error { return a.agentwaitsRound(&c.Agentwaits, false) },
			func(c *config.Config) time.Duration { return c.Agentwaits.Interval.D() }},
		{"compact", func(_ context.Context, c *config.Config) error { return a.compactRound(judge, c, false) },
			func(c *config.Config) time.Duration { return c.Compact.Interval.D() }},
		{"disk", func(ctx context.Context, c *config.Config) error { return a.diskRound(ctx, c) },
			func(c *config.Config) time.Duration { return c.Disk.Interval.D() }},
		{"said", func(ctx context.Context, c *config.Config) error { return a.saidRound(ctx, &c.Said, false) },
			func(c *config.Config) time.Duration { return c.Said.Interval.D() }},
		{"bgtask", func(_ context.Context, c *config.Config) error { return a.bgtaskRound(&c.Bgtask, false) },
			func(c *config.Config) time.Duration { return c.Bgtask.Interval.D() }},
		{"cpu", func(ctx context.Context, c *config.Config) error { return a.cpuRound(ctx, &c.Cpu) },
			func(c *config.Config) time.Duration { return c.Cpu.Interval.D() }},
		{"remote", func(_ context.Context, c *config.Config) error { return a.remoteRound(&c.Remote, false, "", false) },
			func(c *config.Config) time.Duration { return c.Remote.Interval.D() }},
		{"codex", func(ctx context.Context, c *config.Config) error { return a.codexRound(ctx, &c.Codex, false) },
			func(c *config.Config) time.Duration { return a.codexEvery(&c.Codex) }},
		{"night", func(_ context.Context, c *config.Config) error { return a.nightRound(&c.Night, false) },
			func(c *config.Config) time.Duration { return c.Night.Interval.D() }},
	}
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
