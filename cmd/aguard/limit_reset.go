package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/aiseeq/aguard/internal/queue"
)

// exitCode — код выхода команды, итог которой уже напечатан: не ошибка демона и не строка журнала.
type exitCode int

func (c exitCode) Error() string { return fmt.Sprintf("код выхода %d", int(c)) }

const (
	resetRefused exitCode = 1 // сброса нет: снимать паузу рано, причина в выводе
	resetBadArgs exitCode = 2 // сверка не состоялась: ошибка входа, причина в stderr
)

// exitStatus — код выхода команды с уже напечатанным итогом; ok == false — обычная ошибка или успех.
func exitStatus(err error) (code int, ok bool) {
	c, ok := errors.AsType[exitCode](err)
	return int(c), ok
}

// limitResetCheck — сверка перед снятием паузы «до сброса лимита»: код 0 — сброс названной учётки после --since есть.
func (a *app) limitResetCheck(args []string, now time.Time) error {
	fl := flag.NewFlagSet("limit-reset-check", flag.ContinueOnError)
	account := fl.String("account", "", "учётка, чьего сброса ждём: почта, как в истории лимитов")
	since := fl.String("since", "", "момент постановки паузы, RFC3339 (date -Iseconds)")
	history := fl.String("history", "", "JSONL истории лимитов; по умолчанию queue.history из конфига")
	parse(fl, args)
	if fl.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "aguard limit-reset-check: лишние аргументы: %v\n", fl.Args())
		return resetBadArgs
	}
	check, err := a.runResetCheck(*account, *since, *history, now)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aguard limit-reset-check: %v\n", err)
		return resetBadArgs
	}
	fmt.Fprintln(a.out, check.Reason)
	if !check.Confirmed {
		return resetRefused
	}
	return nil
}

func (a *app) runResetCheck(account, since, history string, now time.Time) (queue.ResetCheck, error) {
	if account == "" || since == "" {
		return queue.ResetCheck{}, errors.New("нужны --account и --since")
	}
	pausedAt, err := time.Parse(time.RFC3339, since)
	if err != nil {
		return queue.ResetCheck{}, fmt.Errorf("--since: %w", err)
	}
	if history == "" {
		cfg, err := a.config()
		if err != nil {
			return queue.ResetCheck{}, err
		}
		history = cfg.Queue.History
	}
	rows, err := queue.ReadHistory(history)
	if err != nil {
		return queue.ResetCheck{}, fmt.Errorf("история лимитов %s: %w", history, err)
	}
	return queue.CheckReset(rows, account, pausedAt, now)
}
