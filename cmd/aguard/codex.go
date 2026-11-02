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
	"github.com/aiseeq/akit/codexlimit"
)

// codexReadTimeout — срок одного вызова app-server: отвечает меньше чем за 8 с (замер 02.11), как в dreamstats.
const codexReadTimeout = 20 * time.Second

// codexReader — один замер лимита Codex через app-server по пути codex.
type codexReader func(ctx context.Context, codex string, now time.Time) (codexwatch.Reading, error)

// codexConsumer — досрочный сброс окна (account/rateLimitResetCredit/consume) с ключом идемпотентности; ответ — outcome.
type codexConsumer func(ctx context.Context, codex, key string) (string, error)

// codexApprovals — «да» пользователя на окно с плановым сбросом resetsAt из файла одобрений; "" — нет.
type codexApprovals func(path string, resetsAt time.Time) (string, error)

// codexNoter — строка в тикет ~/git через tk.
type codexNoter func(ctx context.Context, tk, root, ticket, text string) error

// readCodex — замер через `codex app-server` (akit/codexlimit, общий с dreamstats).
func readCodex(ctx context.Context, codex string, now time.Time) (codexwatch.Reading, error) {
	ctx, cancel := context.WithTimeout(ctx, codexReadTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, codex, "app-server")
	l, err := codexlimit.Read(ctx, cmd)
	if err != nil {
		return codexwatch.Reading{}, fmt.Errorf("%s app-server: %w", codex, err)
	}
	return codexwatch.Reading{At: now, Used: l.UsedPercent, ResetsAt: l.ResetsAt, Allowed: l.OrdinaryUsageAllowed,
		Available: l.Resets.Available}, nil
}

// consumeCodex — досрочный сброс через `codex app-server` (akit/codexlimit); кредит выбирает сервер.
func consumeCodex(ctx context.Context, codex, key string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, codexReadTimeout)
	defer cancel()
	out, err := codexlimit.Consume(ctx, exec.CommandContext(ctx, codex, "app-server"), key, "")
	if err != nil {
		return "", fmt.Errorf("%s app-server consume: %w", codex, err)
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

// codexRound — замер лимита Codex, решение, при взводе — consume, затем письма и доставка исходов. Шаг до следующего
// прохода — в a.codexStep: у самых 100% и при неудачном consume он короче.
func (a *app) codexRound(ctx context.Context, cfg *codexwatch.Config, dry bool) error {
	a.codexStep = 0
	if cfg.Mode == codexwatch.ModeOff {
		return nil
	}
	if a.codexRead == nil || a.codexConsume == nil || a.codexApproved == nil || a.codexNote == nil {
		return errors.New("замер, сброс, одобрения или тикет codex не подключены")
	}
	statePath := filepath.Join(a.dir("codex"), "state.json")
	if !dry {
		release, err := codexLock(statePath + ".lock")
		if err != nil {
			return err
		}
		defer release()
	}
	st, err := codexwatch.LoadState(statePath)
	if err != nil {
		return err
	}
	r, err := a.codexRead(ctx, cfg.Codex, time.Now())
	if err != nil {
		return fmt.Errorf("замер лимита: %w", err)
	}
	if r.Armed, err = a.codexApproved(cfg.Approvals, r.ResetsAt); err != nil {
		// явный отказ от взвода: без «да» consume не зовётся, письма лиду идут как без взвода
		a.warn("codex", "файл одобрений не прочитан — взвод не действует", "file", cfg.Approvals, "err", err)
		r.Armed = ""
	}
	f, notices, consume := st.Apply(r, cfg)
	step := st.Step(r, f, cfg)
	if dry {
		a.codexDry(r, f, &st, step, notices, consume)
		return nil
	}
	a.codexStep = step
	a.log.Info("aguard codex: замер", "used", r.Used, "allowed", r.Allowed, "credits", r.Available, "rate", f.Rate,
		"full_at", f.At, "resets_at", r.ResetsAt, "armed", r.Armed)
	// флаги и ключ — до письма и вызова: падение демона после них не повторит письмо и не заведёт другой ключ
	if err := st.Save(statePath); err != nil {
		return err
	}
	if consume {
		if err := a.codexConsumeNow(ctx, cfg, &st, r, statePath); err != nil {
			a.warn("codex", "consume не вышел — повтор тем же ключом", "key", st.Reset.Key, "err", err)
			a.note("codex", "consume_failed", "", "", 1, "")
			a.codexStep = cfg.Fast.D() // бойцы стоят: повтор скоро, а не через interval
		}
	}
	for _, n := range notices {
		if n.Kind == codexwatch.KindNoCredit { // сброса нет — лиду нечего делать: строка журнала, не письмо
			a.log.Info("aguard codex: " + n.Message)
			a.note("codex", string(n.Kind), "", "", 0, "")
			continue
		}
		if err := a.codexSend(cfg, n.Message); err != nil {
			a.warn("codex", "письмо не отправлено", "session", cfg.Lead, "kind", n.Kind, "err", err)
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
	return st.Save(statePath)
}

// codexConsumeNow — consume с ключом окна; false — ответа нет, повтор на следующем проходе. mode=log — только строка.
func (a *app) codexConsumeNow(ctx context.Context, cfg *codexwatch.Config, st *codexwatch.State, r codexwatch.Reading,
	statePath string) error {
	if cfg.Mode == codexwatch.ModeLog {
		a.log.Info("aguard codex: вызвал бы consume", "key", st.Reset.Key, "armed", r.Armed)
		a.note("codex", "would_consume", "", "", 1, "")
		return nil
	}
	st.Calling()
	if err := st.Save(statePath); err != nil {
		return fmt.Errorf("состояние до вызова не сохранено, consume отложен: %w", err)
	}
	outcome, err := a.codexConsume(ctx, cfg.Codex, st.Reset.Key)
	if err != nil {
		return fmt.Errorf("consume без ответа: %w", err)
	}
	a.log.Info("aguard codex: consume", "outcome", outcome, "key", st.Reset.Key)
	a.note("codex", "consume", outcome, "", 1, "")
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
			if err := a.codexSend(cfg, d.Message); err != nil {
				a.warn("codex", "исход не отправлен лиду", "err", err)
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
	fmt.Fprintf(a.out, "%s used=%.0f%% allowed=%v сбросов=%d плановый сброс %s взвод=%q\n", r.At.Local().Format("02.01 15:04"),
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
func (a *app) codexSend(cfg *codexwatch.Config, msg string) error {
	if cfg.Mode == codexwatch.ModeLog {
		a.log.Info("aguard codex: написал бы", "session", cfg.Lead, "msg", msg)
		return nil
	}
	alive, err := a.reg.Alive()
	if err != nil {
		return fmt.Errorf("реестр сессий: %w", err)
	}
	if err := a.sendAlive(alive, cfg.Lead, cfg.SendAuth, msg); err != nil {
		return err
	}
	a.log.Info("aguard codex: написал", "session", cfg.Lead, "msg", msg)
	return nil
}

// codexEvery — шаг наблюдателя codex: выбранный прошлым проходом, без него — interval.
func (a *app) codexEvery(cfg *codexwatch.Config) time.Duration {
	if a.codexStep > 0 {
		return a.codexStep
	}
	return cfg.Interval.D()
}
