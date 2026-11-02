package main

import (
	"context"
	"flag"
	"fmt"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/aiseeq/aguard/internal/codexwatch"
	"github.com/aiseeq/akit/codexlimit"
)

// codexReadTimeout — срок одного замера: app-server отвечает меньше чем за 8 с (замер 02.11), как в dreamstats.
const codexReadTimeout = 20 * time.Second

// codexReader — один замер лимита Codex через app-server по пути codex.
type codexReader func(ctx context.Context, codex string, now time.Time) (codexwatch.Reading, error)

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
	dry := fl.Bool("dry-run", false, "показать замер, темп, прогноз и решение, ничего не отправлять и не сохранять")
	parse(fl, rest)
	return a.codexRound(ctx, &cfg.Codex, *dry)
}

// codexRound — замер лимита Codex и решение по нему. Шаг до следующего прохода — в a.codexStep: у самых 100% он
// короче (codexwatch.Step).
func (a *app) codexRound(ctx context.Context, cfg *codexwatch.Config, dry bool) error {
	a.codexStep = 0
	if cfg.Mode == codexwatch.ModeOff {
		return nil
	}
	statePath := filepath.Join(a.dir("codex"), "state.json")
	st, err := codexwatch.LoadState(statePath)
	if err != nil {
		return err
	}
	if a.codexRead == nil {
		return fmt.Errorf("замер лимита не подключён")
	}
	r, err := a.codexRead(ctx, cfg.Codex, time.Now())
	if err != nil {
		return fmt.Errorf("замер лимита: %w", err)
	}
	f, notices := st.Apply(r, cfg)
	step := codexwatch.Step(r, f, cfg)
	if dry {
		fmt.Fprintf(a.out, "%s used=%.0f%% allowed=%v сбросов=%d плановый сброс %s\n", r.At.Local().Format("02.01 15:04"),
			r.Used, r.Allowed, r.Available, r.ResetsAt.Local().Format("02.01 15:04"))
		if f.At.IsZero() {
			fmt.Fprintf(a.out, "  темп %.1f %%/ч, прогноза нет; замеров окна %d; шаг %v\n", f.Rate, len(st.Samples), step)
		} else {
			fmt.Fprintf(a.out, "  темп %.1f %%/ч, 100%% около %s; замеров окна %d; шаг %v\n", f.Rate,
				f.At.Local().Format("02.01 15:04"), len(st.Samples), step)
		}
		for _, n := range notices {
			fmt.Fprintf(a.out, "  %s: %s\n", n.Kind, n.Message)
		}
		return nil
	}
	a.codexStep = step
	a.log.Info("aguard codex: замер", "used", r.Used, "allowed", r.Allowed, "credits", r.Available, "rate", f.Rate,
		"full_at", f.At, "resets_at", r.ResetsAt)
	// флаги — до письма: падение демона после письма не повторит его; не ушедшее — снова на следующем проходе
	if err := st.Save(statePath); err != nil {
		return err
	}
	unsent := false
	for _, n := range notices {
		if err := a.codexSend(cfg, n); err != nil {
			a.warn("codex", "письмо не отправлено", "session", cfg.Lead, "kind", n.Kind, "err", err)
			a.note("codex", "send_failed", string(n.Kind), cfg.Lead, 1, "")
			st.Unsent(n.Kind)
			unsent = true
		}
	}
	if unsent {
		return st.Save(statePath)
	}
	return nil
}

// codexSend — письмо лиду или строка журнала; ошибка — письмо не ушло.
func (a *app) codexSend(cfg *codexwatch.Config, n codexwatch.Notice) error {
	if n.Kind == codexwatch.KindNoCredit {
		a.log.Info("aguard codex: " + n.Message)
		a.note("codex", string(n.Kind), "", "", 0, "")
		return nil
	}
	if cfg.Mode == codexwatch.ModeLog {
		a.log.Info("aguard codex: написал бы", "session", cfg.Lead, "msg", n.Message)
		a.note("codex", "would_send", string(n.Kind), cfg.Lead, 1, "")
		return nil
	}
	alive, err := a.reg.Alive()
	if err != nil {
		return fmt.Errorf("реестр сессий: %w", err)
	}
	if err := a.sendAlive(alive, cfg.Lead, cfg.SendAuth, n.Message); err != nil {
		return err
	}
	a.log.Info("aguard codex: написал", "session", cfg.Lead, "msg", n.Message)
	a.note("codex", "sent", string(n.Kind), cfg.Lead, 1, "")
	return nil
}

// codexEvery — шаг наблюдателя codex: выбранный прошлым проходом, без него — interval.
func (a *app) codexEvery(cfg *codexwatch.Config) time.Duration {
	if a.codexStep > 0 {
		return a.codexStep
	}
	return cfg.Interval.D()
}
