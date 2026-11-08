package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/iowatch"
)

// Текст письма о случаях фриза: сводка главного (самого длинного) с виновниками и список остальных. Кому — решает
// маршрут (io_route.go).

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
