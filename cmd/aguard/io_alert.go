package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/iowatch"
	"github.com/aiseeq/akit/proc"
)

// Письмо о длинных случаях фриза: сводка главного (самого длинного) с виновниками и список
// остальных — лиду и сессии-виновнику главного случая. Сводка с виновниками и есть разбор: лид
// получает её сразу и сам решает, что делать.

// alertIO — письмо о случаях; mode = log — строка журнала «написал бы». Зовётся из своей горутины.
func (a *app) alertIO(cfg iowatch.Config, cases []iowatch.Case) error {
	if len(cases) == 0 {
		return nil
	}
	message, main := ioAlertText(cases, cfg.AlertMin.D())
	owner := ""
	if main.Culprits != nil && !main.Culprits.SnapshotDeletion() && !main.Culprits.ReadMostly() {
		if name, _, ok := proc.SessionScope(main.Culprits.Main); ok {
			owner = name
		}
	}
	recipients := ioRecipients(cfg.Lead, owner)
	if cfg.Mode == iowatch.ModeLog {
		a.log.Info("aguard io: написал бы", "to", strings.Join(recipients, ","), "message", message)
		a.note("io", "написал бы", main.Start.Format(time.RFC3339), strings.Join(recipients, ","), main.DurMS, "ms")
		return nil
	}
	alive, err := a.reg.Alive()
	if err != nil {
		return fmt.Errorf("реестр сессий: %w", err)
	}
	var sendErr error
	for _, to := range recipients {
		if _, ok := alive[to]; !ok && to != cfg.Lead {
			a.log.Info("aguard io: сессия-виновник уже закрыта, письмо только лиду", "session", to)
			continue
		}
		if err := a.sendAlive(alive, to, cfg.SendAuth, message); err != nil {
			if a.quietFailure("io", err) {
				continue
			}
			sendErr = errors.Join(sendErr, fmt.Errorf("письмо %s: %w", to, err))
			continue
		}
		a.note("io", "alert_sent", main.Start.Format(time.RFC3339), to, main.DurMS, "ms")
	}
	return sendErr
}

// ioRecipients — лид всегда, сессия-виновник — если она есть и это не лид.
func ioRecipients(lead, owner string) []string {
	recipients := []string{lead}
	if owner != "" && owner != lead {
		recipients = append(recipients, owner)
	}
	return recipients
}

// ioAlertText — текст письма и главный случай.
func ioAlertText(cases []iowatch.Case, alertMin time.Duration) (string, iowatch.Case) {
	main := cases[0]
	for i := range cases {
		if cases[i].DurMS > main.DurMS {
			main = cases[i]
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "aguard io: фриз %s\n", ioCaseLine(&main))
	writeIOCase(&b, &main, 3)
	if len(cases) > 1 {
		var total int64
		for i := range cases {
			total += cases[i].DurMS
		}
		fmt.Fprintf(&b, "Всего случаев от %s с %s: %d, вместе %s:", ioSeconds(alertMin.Milliseconds()), cases[0].Start.Format("15:04"),
			len(cases), ioSeconds(total))
		for i := range cases {
			c := &cases[i]
			fmt.Fprintf(&b, " %s %s", c.Start.Format("15:04:05"), ioSeconds(c.DurMS))
			if attribution := ioCaseAttribution(c); attribution != "" {
				fmt.Fprintf(&b, " (%s)", attribution)
			}
			b.WriteByte(';')
		}
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "Подробнее: aguard io report -since %s", ioReportSince(cases[0].Start, time.Now()))
	return b.String(), main
}

// ioReportSince — глубина для команды из письма: от первого случая, с запасом, целыми часами.
func ioReportSince(first, now time.Time) string {
	hours := int(now.Sub(first)/time.Hour) + 1
	return fmt.Sprintf("%dh", hours)
}
