package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/aiseeq/aguard/internal/codexwatch"
	"github.com/aiseeq/aguard/internal/guide"
	"github.com/aiseeq/akit/codexlimit"
)

// codexReadTimeout — срок одного вызова app-server: отвечает меньше чем за 8 с (замер 02.11), как в dreamstats.
// Переменная — тест зависшего consume укорачивает её, чтобы проверить срок без 20 с ожидания.
var codexReadTimeout = 20 * time.Second

// codexReader — один замер лимита учётки с CODEX_HOME=home через app-server по пути codex.
type codexReader func(ctx context.Context, codex, home string, now time.Time) (codexwatch.Reading, error)

// codexConsumer — досрочный сброс окна учётки (account/rateLimitResetCredit/consume) с ключом идемпотентности; ответ — outcome.
type codexConsumer func(ctx context.Context, codex, home, key string) (string, error)

// codexApprovals — «да» пользователя на окно с плановым сбросом resetsAt из файла одобрений; "" — нет.
type codexApprovals func(path string, resetsAt time.Time) (string, error)

// codexNoter — строка в тикет ~/git через tk.
type codexNoter func(ctx context.Context, tk, root, ticket, text string) error

// appServer — `codex app-server` учётки home: без CODEX_HOME codex взял бы ~/.codex, чужой лимит и чужой кредит.
func appServer(ctx context.Context, codex, home string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, codex, "app-server")
	cmd.Env = append(os.Environ(), "CODEX_HOME="+home)
	return cmd
}

// readCodex — замер через `codex app-server` (akit/codexlimit, общий с dreamstats).
func readCodex(ctx context.Context, codex, home string, now time.Time) (codexwatch.Reading, error) {
	ctx, cancel := context.WithTimeout(ctx, codexReadTimeout)
	defer cancel()
	l, err := codexlimit.Read(ctx, appServer(ctx, codex, home))
	if err != nil {
		return codexwatch.Reading{}, fmt.Errorf("%s app-server (CODEX_HOME=%s): %w", codex, home, err)
	}
	return codexwatch.Reading{At: now, Used: l.UsedPercent, ResetsAt: l.ResetsAt, Allowed: l.OrdinaryUsageAllowed,
		Available: l.Resets.Available}, nil
}

// consumeCodex — досрочный сброс через `codex app-server` (akit/codexlimit); кредит выбирает сервер.
func consumeCodex(ctx context.Context, codex, home, key string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, codexReadTimeout)
	defer cancel()
	out, err := codexlimit.Consume(ctx, appServer(ctx, codex, home), key, "")
	if err != nil {
		return "", fmt.Errorf("%s app-server consume (CODEX_HOME=%s): %w", codex, home, err)
	}
	return string(out), nil
}

// approvedCodex — «да» на окно: строка ближе codexlimit.ApprovalTolerance к resetsAt (akit/codexlimit, общий с dreamstats).
func approvedCodex(path string, resetsAt time.Time) (string, error) {
	ap, err := codexlimit.ReadApproved(path)
	if err != nil {
		return "", fmt.Errorf("одобрения сброса: %w", err)
	}
	src, _ := ap.For(resetsAt)
	return src, nil
}

// noteTicket — `tk note <ticket> <text> --by aguard` в папке root.
func noteTicket(ctx context.Context, tk, root, ticket, text string) error {
	ctx, cancel := context.WithTimeout(ctx, codexReadTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, tk, "note", ticket, text, "--by", "aguard")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tk note %s: %w: %s", ticket, err, out)
	}
	return nil
}

func (a *app) codexCmd(ctx context.Context, args []string) error {
	cmd, rest := sub(args)
	cfg, err := a.config()
	if err != nil {
		return err
	}
	if cmd != "tick" {
		usage()
		return nil
	}
	fl := flag.NewFlagSet("codex tick", flag.ContinueOnError)
	dry := fl.Bool("dry-run", false, "показать замер, темп, прогноз и решение, ничего не отправлять, не сохранять и не сбрасывать")
	parse(fl, rest)
	return a.codexRound(ctx, &cfg.Codex, *dry)
}

// codexLock — исключительная блокировка прохода: ручной `codex tick` и демон не вызывают consume вдвоём (противник
// взвода 02.11, сценарий 5). Занято — ошибка, проход пропускается.
func codexLock(path string) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("блокировка codex: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		closeErr := f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.Join(errors.New("проход codex уже идёт в другом процессе"), closeErr)
		}
		return nil, errors.Join(fmt.Errorf("блокировка codex: %w", err), closeErr)
	}
	return func() {
		if err := f.Close(); err != nil { // закрытие снимает flock
			fmt.Fprintln(os.Stderr, "aguard codex: блокировка:", err)
		}
	}, nil
}

// codexRound — проход по учёткам: замер каждой, затем по каждой решение, при взводе — consume, письма и доставка
// исходов; сбой одной не мешает другой. Шаг до следующего прохода — в a.codexStep, меньший из шагов учёток: у самых 100%
// и при неудачном consume он короче.
func (a *app) codexRound(ctx context.Context, cfg *codexwatch.Config, dry bool) error {
	a.codexStep = 0
	if cfg.Mode == codexwatch.ModeOff {
		return nil
	}
	if a.codexRead == nil || a.codexConsume == nil || a.codexApproved == nil || a.codexNote == nil {
		return errors.New("замер, сброс, одобрения или тикет codex не подключены")
	}
	if !dry {
		release, err := codexLock(filepath.Join(a.dir("codex"), "pass.lock"))
		if err != nil {
			return err
		}
		defer release()
	}
	readings, failed, errs := a.codexReadAll(ctx, cfg)
	stale := false // consume в этом проходе: замер снят до сброса, про лимит учётки он уже ничего не говорит
	for i := range readings {
		name := readings[i].Account.Name
		readings[i].Others = codexwatch.Others(readings, failed, name)
		consumed, err := a.codexAccount(ctx, cfg, readings[i], dry)
		if err != nil {
			errs = append(errs, fmt.Errorf("учётка %s: %w", name, err))
		}
		stale = stale || consumed
	}
	if !stale {
		if err := a.codexFleet(cfg, readings, failed, dry); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// codexReadAll — замер и «да» на окно каждой учётки; не вышел — имя в failed и ошибка, остальные идут дальше.
func (a *app) codexReadAll(ctx context.Context, cfg *codexwatch.Config) ([]codexwatch.Reading, []string, []error) {
	var readings []codexwatch.Reading
	var failed []string
	var errs []error
	for _, acc := range cfg.Accounts {
		r, err := a.codexRead(ctx, cfg.Codex, acc.Home, time.Now())
		if err != nil {
			failed = append(failed, acc.Name)
			errs = append(errs, fmt.Errorf("замер лимита учётки %s: %w", acc.Name, err))
			continue
		}
		r.Account = acc
		if r.Armed, err = a.codexApproved(acc.Approvals, r.ResetsAt); err != nil {
			// явный отказ от взвода: без «да» consume не зовётся, письма лиду идут как без взвода
			a.warn("codex", "файл одобрений не прочитан — взвод не действует", "account", acc.Name, "file", acc.Approvals, "err", err)
			r.Armed = ""
		}
		readings = append(readings, r)
	}
	return readings, failed, errs
}

// codexFleet — письмо «все учётки без лимита» при переходе; не ушло — следующий проход пробует снова.
func (a *app) codexFleet(cfg *codexwatch.Config, readings []codexwatch.Reading, failed []string, dry bool) error {
	path := filepath.Join(a.dir("codex"), "fleet.json")
	fleet, err := codexwatch.LoadFleet(path)
	if err != nil {
		return err
	}
	msg := fleet.Apply(readings, failed)
	if dry {
		if msg != "" {
			fmt.Fprintf(a.out, "все учётки: %s\n", msg)
		}
		return nil
	}
	if msg != "" {
		if err := a.codexSend(cfg, "codex.all_out", msg); err != nil {
			if !a.quietFailure("codex", err) {
				a.warn("codex", "письмо не отправлено", "session", cfg.Lead, "kind", "all_out", "err", err)
				a.note("codex", "send_failed", "all_out", cfg.Lead, 1, "")
			}
			fleet.AllOut = false
		} else {
			a.note("codex", "sent", "all_out", cfg.Lead, 1, "")
		}
	}
	return fleet.Save(path)
}

// codexAccount — решение по одной учётке, при взводе — consume, письма и доставка исходов; true — решение было consume.
func (a *app) codexAccount(ctx context.Context, cfg *codexwatch.Config, r codexwatch.Reading, dry bool) (bool, error) {
	statePath := filepath.Join(a.dir("codex"), r.Account.Name+".json")
	st, err := codexwatch.LoadState(statePath)
	if err != nil {
		return false, err
	}
	f, notices, consume := st.Apply(r, cfg)
	step := st.Step(r, f, cfg)
	if dry {
		a.codexDry(r, f, &st, step, notices, consume)
		return consume, nil
	}
	a.codexShorten(step)
	a.log.Info("aguard codex: замер", "account", r.Account.Name, "used", r.Used, "allowed", r.Allowed, "credits", r.Available,
		"rate", f.Rate, "full_at", f.At, "resets_at", r.ResetsAt, "armed", r.Armed)
	// флаги и ключ — до письма и вызова: падение демона после них не повторит письмо и не заведёт другой ключ
	if err := st.Save(statePath); err != nil {
		return consume, err
	}
	if consume {
		if err := a.codexConsumeNow(ctx, cfg, &st, r, statePath); err != nil {
			a.warn("codex", "consume не вышел — повтор тем же ключом", "account", r.Account.Name, "key", st.Reset.Key, "err", err)
			a.note("codex", "consume_failed", "", r.Account.Name, 1, "")
			a.codexShorten(cfg.Fast.D()) // бойцы стоят: повтор скоро, а не через interval
		}
	}
	for _, n := range notices {
		if n.Kind == codexwatch.KindNoCredit { // сброса нет — лиду нечего делать: строка журнала, не письмо
			a.log.Info("aguard codex: " + n.Message)
			a.note("codex", string(n.Kind), "", r.Account.Name, 0, "")
			continue
		}
		kind := codexGuideKind(n.Kind)
		a.warnOtherGuideInput(kind, string(n.Kind))
		if err := a.codexSend(cfg, kind, n.Message); err != nil {
			if a.quietFailure("codex", err) {
				st.Unsent(n.Kind)
				continue
			}
			a.warn("codex", "письмо не отправлено", "account", r.Account.Name, "session", cfg.Lead, "kind", n.Kind, "err", err)
			a.note("codex", "send_failed", string(n.Kind), cfg.Lead, 1, "")
			st.Unsent(n.Kind)
			continue
		}
		sent := "sent"
		if cfg.Mode == codexwatch.ModeLog {
			sent = "would_send"
		}
		a.note("codex", sent, string(n.Kind), cfg.Lead, 1, "")
	}
	a.codexDeliver(ctx, cfg, &st)
	return consume, st.Save(statePath)
}

// codexConsumeNow — consume с ключом окна; false — ответа нет, повтор на следующем проходе. mode=log — только строка.
func (a *app) codexConsumeNow(ctx context.Context, cfg *codexwatch.Config, st *codexwatch.State, r codexwatch.Reading,
	statePath string) error {
	if cfg.Mode == codexwatch.ModeLog {
		a.log.Info("aguard codex: вызвал бы consume", "account", r.Account.Name, "key", st.Reset.Key, "armed", r.Armed)
		a.note("codex", "would_consume", "", r.Account.Name, 1, "")
		return nil
	}
	st.Calling()
	if err := st.Save(statePath); err != nil {
		return fmt.Errorf("состояние до вызова не сохранено, consume отложен: %w", err)
	}
	outcome, err := a.codexConsume(ctx, cfg.Codex, r.Account.Home, st.Reset.Key)
	if err != nil {
		return fmt.Errorf("consume без ответа: %w", err)
	}
	a.log.Info("aguard codex: consume", "account", r.Account.Name, "outcome", outcome, "key", st.Reset.Key)
	a.note("codex", "consume", outcome, r.Account.Name, 1, "")
	st.Done(outcome, r)
	if err := st.Save(statePath); err != nil {
		a.warn("codex", "исход consume не сохранён — сохраню в конце прохода", "outcome", outcome, "err", err)
	}
	return nil
}

// codexDeliver — исходы взвода: письмо лиду и строка в тикет, каждое до успеха.
func (a *app) codexDeliver(ctx context.Context, cfg *codexwatch.Config, st *codexwatch.State) {
	for i := range st.Pending {
		d := &st.Pending[i]
		if !d.Lead {
			if err := a.codexSend(cfg, "codex.outcome", d.Message); err != nil {
				if !a.quietFailure("codex", err) {
					a.warn("codex", "исход не отправлен лиду", "err", err)
				}
			} else {
				d.Lead = true
			}
		}
		if !d.Ticket {
			if cfg.Mode == codexwatch.ModeLog {
				a.log.Info("aguard codex: записал бы в тикет", "ticket", cfg.Ticket, "note", d.Note)
				d.Ticket = true
			} else if err := a.codexNote(ctx, cfg.Tk, cfg.TicketRoot, cfg.Ticket, d.Note); err != nil {
				a.warn("codex", "исход не записан в тикет", "ticket", cfg.Ticket, "err", err)
			} else {
				d.Ticket = true
			}
		}
	}
	st.Delivered()
}

func (a *app) codexDry(r codexwatch.Reading, f codexwatch.Forecast, st *codexwatch.State, step time.Duration,
	notices []codexwatch.Notice, consume bool) {
	fmt.Fprintf(a.out, "%s %s used=%.0f%% allowed=%v сбросов=%d плановый сброс %s взвод=%q\n", r.Account.Name, r.At.Local().Format("02.01 15:04"),
		r.Used, r.Allowed, r.Available, r.ResetsAt.Local().Format("02.01 15:04"), r.Armed)
	if f.At.IsZero() {
		fmt.Fprintf(a.out, "  темп %.1f %%/ч, прогноза нет; замеров окна %d; шаг %v\n", f.Rate, len(st.Samples), step)
	} else {
		fmt.Fprintf(a.out, "  темп %.1f %%/ч, 100%% около %s; замеров окна %d; шаг %v\n", f.Rate,
			f.At.Local().Format("02.01 15:04"), len(st.Samples), step)
	}
	for _, n := range notices {
		fmt.Fprintf(a.out, "  %s: %s\n", n.Kind, n.Message)
	}
	if consume {
		fmt.Fprintf(a.out, "  consume: вызвал бы с ключом %s\n", st.Reset.Key)
	}
	for _, d := range st.Pending {
		fmt.Fprintf(a.out, "  исход к доставке (лиду %v, в тикет %v): %s\n", d.Lead, d.Ticket, d.Message)
	}
}

// codexSend — письмо лиду; mode=log — строка «написал бы». Ошибка — письмо не ушло.
func (a *app) codexSend(cfg *codexwatch.Config, kind guide.Kind, msg string) error {
	if cfg.Mode == codexwatch.ModeLog {
		a.log.Info("aguard codex: написал бы", "session", cfg.Lead, "msg", msg)
		return nil
	}
	alive, err := a.reg.Alive()
	if err != nil {
		return fmt.Errorf("реестр сессий: %w", err)
	}
	if err := a.sendAlive(alive, cfg.Lead, cfg.SendAuth, kind, msg); err != nil {
		return err
	}
	a.log.Info("aguard codex: написал", "session", cfg.Lead, "msg", msg)
	return nil
}

// codexShorten — шаг следующего прохода не длиннее step.
func (a *app) codexShorten(step time.Duration) {
	if a.codexStep == 0 || step < a.codexStep {
		a.codexStep = step
	}
}

// codexEvery — шаг наблюдателя codex: выбранный прошлым проходом, без него — interval.
func (a *app) codexEvery(cfg *codexwatch.Config) time.Duration {
	if a.codexStep > 0 {
		return a.codexStep
	}
	return cfg.Interval.D()
}
