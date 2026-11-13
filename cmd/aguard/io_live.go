package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/iowatch"
	"github.com/aiseeq/aguard/internal/linesock"
)

// Живые запросы к службе через сокет [io] socket: `aguard io now | series | profiles`. Службы нет —
// ошибка, файлы не читаются.

// ioLiveTimeout — срок обмена: ряд за сутки — около 10 МБ JSON; больше срока службы, чтобы ошибку назвала она.
const ioLiveTimeout = 6 * time.Second

// ioLiveSince — запрос с since из аргумента (30m, 3d, ЧЧ:ММ, RFC3339).
func (a *app) ioLiveSince(req iowatch.Request, since string, asJSON bool) error {
	at, _, err := ioWindow(since, "", time.Now())
	if err != nil {
		return err
	}
	req.Since = at.Format(time.RFC3339)
	return a.ioLive(req, asJSON)
}

// ioLive — запрос службе и ответ: JSON как есть или текст человеку.
func (a *app) ioLive(req iowatch.Request, asJSON bool) error {
	cfg, err := a.ioConfig()
	if err != nil {
		return err
	}
	ans, err := askIO(cfg.Socket, req)
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(a.out)
		return enc.Encode(ans)
	}
	switch req.Q {
	case iowatch.QueryNow:
		writeIONow(a.out, ans.Now)
	case iowatch.QuerySeries:
		writeIOSeries(a.out, ans.Series)
	default:
		writeIOProfiles(a.out, ans.Profiles)
	}
	return nil
}

// askIO — запрос текущей версии протокола; службы нет — errIONotRunning, ошибка в ответе — ошибка.
func askIO(socket string, req iowatch.Request) (iowatch.Answer, error) {
	req.V = iowatch.ProtoVersion
	var ans iowatch.Answer
	err := linesock.Ask(socket, ioLiveTimeout, req, &ans)
	switch {
	case errors.Is(err, linesock.ErrNoServer):
		return ans, fmt.Errorf("%w (сокет %s)", errIONotRunning, socket)
	case err != nil:
		return ans, fmt.Errorf("сокет io %s: %w", socket, err)
	case ans.V != iowatch.ProtoVersion:
		return ans, fmt.Errorf("версия протокола службы %d, aguard знает %d", ans.V, iowatch.ProtoVersion)
	case ans.Error != "":
		return ans, errors.New("служба: " + ans.Error)
	case req.Q == iowatch.QueryNow && ans.Now == nil:
		return ans, errors.New("служба: в ответе now нет состояния")
	}
	return ans, nil
}

// writeIONow — две строки: окна 1/5/30 с; CPU и память за 1 с, dirty, случай, свежие случаи, кто
// пишет.
func writeIONow(w io.Writer, n *iowatch.Now) {
	win := func(label string, x iowatch.NowWindow) string {
		line := fmt.Sprintf("%s: full %d мс, простой %d мс, W/R %.0f/%.0f МБ/с, ожид W %d мс, очередь %d", label, x.IOFullMS, x.StallMS,
			megabytes(x.WriteBps), megabytes(x.ReadBps), x.WAwaitMS, x.InFlightMax)
		if x.Capped.Capped {
			line += fmt.Sprintf(", под потолком io.max (вне потолка до %d мс)", x.Capped.OutsideMS)
		}
		return line
	}
	fmt.Fprintf(w, "%s  %s | %s | %s\n", n.At.Format("15:04:05.0"), win("1с", n.W1s), win("5с", n.W5s), win("30с", n.W30s))
	open := "случая нет"
	if n.Open.Open {
		open = "идёт случай " + ioSeconds(n.Open.MS)
		if n.Open.Capped {
			open += " (ожидание под потолком io.max, не фриз)"
		}
	}
	writers := make([]string, len(n.Writers))
	for i, x := range n.Writers {
		writers[i] = fmt.Sprintf("%s %.0f МБ/с", x.Name, megabytes(x.WriteBps))
	}
	cpu := fmt.Sprintf("cpu some %d мс/с, runnable %d/%d, avail %s ГиБ, swap-in %s МБ/с", n.W1s.CPUSomeMS, n.W1s.RunnableMax, n.NProc,
		ioDecimal(float64(n.MemAvailable)/(1<<30)), ioDecimal(megabytes(n.W1s.SwapIn*int64(os.Getpagesize()))))
	if len(n.Swappers) > 0 {
		cpu += " (" + n.Swappers[0].Name + ")"
	}
	line := fmt.Sprintf("%s; dirty %.0f МБ, writeback %.0f МБ; %s; случаев за 60 с: %d (от 200 мс: %d)", cpu, megabytes(n.Dirty),
		megabytes(n.Writeback), open, n.Cases.From100ms60s, n.Cases.From200ms60s)
	if len(writers) > 0 {
		line += "; пишут: " + strings.Join(writers, ", ")
	}
	fmt.Fprintln(w, line)
}

// writeIOSeries — строка на секунду.
func writeIOSeries(w io.Writer, seconds []iowatch.MachineSecond) {
	if len(seconds) == 0 {
		fmt.Fprintln(w, "ряд пуст")
		return
	}
	for i := range seconds {
		s := &seconds[i]
		fmt.Fprintf(w, "%s  io full %d мс  mem full %d мс  простой %d мс  R/W %.0f/%.0f МБ  ожид W %d мс  очередь %d  flush %d  dirty %.0f МБ  writeback %.0f МБ  swapin %d  cpu some %d мс  runnable %d  avail %.0f МБ\n",
			s.At.Format("02.01 15:04:05"), s.IOFullMS, s.MemFullMS, s.StallMS, megabytes(s.ReadB), megabytes(s.WriteB), s.WAwaitMS,
			s.InFlightMax, s.Flushes, megabytes(s.Dirty), megabytes(s.Writeback), s.SwapIn, s.CPUSomeMS, s.RunnableMax, megabytes(s.MemAvail))
	}
}

// writeIOProfiles — строка на профиль.
func writeIOProfiles(w io.Writer, profiles []iowatch.Profile) {
	if len(profiles) == 0 {
		fmt.Fprintln(w, "профилей нет")
		return
	}
	for i := range profiles {
		p := &profiles[i]
		rw := "IO не мерено (нет io.stat)"
		if !p.NoIO {
			rw = fmt.Sprintf("W %.0f МБ (пик %.0f МБ/с)  R %.0f МБ (пик %.0f МБ/с)", megabytes(p.WriteB), megabytes(p.PeakWriteBps), megabytes(p.ReadB), megabytes(p.PeakReadBps))
		}
		line := fmt.Sprintf("%s  %s  %s  %s  %s  простой IO %d мс  swapin %d  CPU %s мин (пик %s ядра)",
			p.End.Format("02.01 15:04"), ioSeconds(p.DurMS), p.Kind, p.Name, rw, p.IOFullMS, p.SwapIn,
			ioDecimal(float64(p.CPUMS)/float64(time.Minute.Milliseconds())), ioDecimal(float64(p.PeakCPUMilli)/1000))
		if len(p.Procs) > 0 {
			procs := make([]string, len(p.Procs))
			for j, r := range p.Procs {
				procs[j] = ioValue(r.Name, r.Value, ioBytes)
			}
			line += "; процессы: " + strings.Join(procs, ", ")
		}
		fmt.Fprintln(w, line)
	}
}
