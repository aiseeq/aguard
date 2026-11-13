package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aiseeq/aguard/internal/iowatch"
	"github.com/aiseeq/akit/timearg"
	"golang.org/x/sys/unix"
)

// Команды наблюдателя io: запись в переднем плане, итог случаев, ручной снимок службы и окно по нему,
// перевод старых снимков событий в случаи.

// ioCmd — aguard io report | record | dump | at | import | now | series | profiles.
func (a *app) ioCmd(ctx context.Context, args []string) error {
	cmd, rest := sub(args)
	events := filepath.Join(a.dir("io"), "events")
	cases := filepath.Join(a.dir("io"), ioCasesFile)
	switch cmd {
	case "report":
		fl := flag.NewFlagSet("io report", flag.ContinueOnError)
		since := fl.String("since", "24h", "с какого времени: 30m, 2h, 3d, ЧЧ:ММ, RFC3339")
		until := fl.String("until", "", "до какого времени, в тех же формах; пусто — сейчас")
		who := fl.String("who", "", "только случаи, где подстрока есть в главной cgroup, в имени процесса топов (запись, чтение, своп) или его cmdline")
		minDur := fl.Duration("min", 0, "случаи от такой длительности — списком и в виновниках (0 — case_min из конфига)")
		asJSON := fl.Bool("json", false, "итог JSON")
		file := fl.String("file", cases, "файл сводок; не файл службы — читается как есть, без запроса службе")
		parse(fl, rest)
		from, to, err := ioWindow(*since, *until, time.Now())
		if err != nil {
			return err
		}
		return a.ioReport(*file, *file == cases, from, to, *minDur, *who, *asJSON)
	case "route":
		fl := flag.NewFlagSet("io route", flag.ContinueOnError)
		since := fl.Duration("since", 7*24*time.Hour, "глубина")
		verbose := fl.Bool("v", false, "каждое письмо строкой")
		file := fl.String("file", cases, "файл сводок; не файл службы — читается как есть, без запроса службе")
		parse(fl, rest)
		return a.ioRouteReplay(*file, *file == cases, time.Now().Add(-*since), *verbose)
	case "record":
		fl := flag.NewFlagSet("io record", flag.ContinueOnError)
		dur := fl.Duration("for", 30*time.Second, "длительность записи")
		dir := fl.String("dir", events, "папка снимков")
		parse(fl, rest)
		return a.ioRecord(ctx, *dur, *dir)
	case "dump":
		return a.ioDump(events)
	case "at":
		fl := flag.NewFlagSet("io at", flag.ContinueOnError)
		window := fl.Duration("window", 2*time.Minute, "ширина окна")
		dir := fl.String("dir", events, "папка снимков")
		var when string
		if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
			when, rest = rest[0], rest[1:]
		}
		parse(fl, rest)
		if when == "" && fl.NArg() == 1 {
			when = fl.Arg(0)
		}
		if when == "" {
			return errors.New("aguard io at: укажите время ЧЧ:ММ[:СС] или RFC3339")
		}
		at, err := parseIOTime(when, time.Now())
		if err != nil {
			return err
		}
		return a.ioAt(*dir, at, *window)
	case "import":
		fl := flag.NewFlagSet("io import", flag.ContinueOnError)
		dir := fl.String("dir", events, "папка снимков событий")
		remove := fl.Bool("remove", false, "удалить снимки событий после записи случаев")
		out := fl.String("out", cases, "файл сводок")
		parse(fl, rest)
		return a.ioImport(*dir, *out, *out == cases, *remove)
	case "now":
		fl := flag.NewFlagSet("io now", flag.ContinueOnError)
		asJSON := fl.Bool("json", false, "ответ службы JSON как есть")
		parse(fl, rest)
		return a.ioLive(iowatch.Request{Q: iowatch.QueryNow}, *asJSON)
	case "series":
		fl := flag.NewFlagSet("io series", flag.ContinueOnError)
		since := fl.String("since", "1h", "с какого времени: 30m, 2h, 3d, ЧЧ:ММ, RFC3339")
		asJSON := fl.Bool("json", false, "ответ службы JSON как есть")
		parse(fl, rest)
		return a.ioLiveSince(iowatch.Request{Q: iowatch.QuerySeries}, *since, *asJSON)
	case "profiles":
		fl := flag.NewFlagSet("io profiles", flag.ContinueOnError)
		kind := fl.String("kind", "", "вид: worker:<класс>, agent, service:<unit>; пусто — все")
		since := fl.String("since", "7d", "конец профиля не раньше: 30m, 2h, 3d, ЧЧ:ММ, RFC3339")
		asJSON := fl.Bool("json", false, "ответ службы JSON как есть")
		parse(fl, rest)
		return a.ioLiveSince(iowatch.Request{Q: iowatch.QueryProfiles, Kind: *kind}, *since, *asJSON)
	}
	return fmt.Errorf("aguard io: неизвестная команда %s (report, now, series, profiles, record, dump, at, import; справка — aguard -h)", cmd)
}

// ioConfig — раздел [io] конфига; конфиг не прочитался — ошибка, по неизвестным правилам не пишем.
func (a *app) ioConfig() (iowatch.Config, error) {
	c, err := a.config()
	if err != nil {
		return iowatch.Config{}, err
	}
	return c.IO, nil
}

// ioRecord — замер в переднем плане тем же циклом, буфером и случаями, что у службы, без писем;
// случаи — строкой в вывод, в конце — буфер ручным снимком в dir.
func (a *app) ioRecord(ctx context.Context, dur time.Duration, dir string) error {
	if dur <= 0 {
		return errors.New("aguard io record: длительность должна быть положительной")
	}
	cfg, err := a.ioConfig()
	if err != nil {
		return err
	}
	rec := iowatch.NewRecorder(cfg)
	sink := &ioRecordSink{a: a, rec: rec}
	ctx, cancel := context.WithTimeout(ctx, dur)
	defer cancel()
	err = runIOSampling(ctx, cfg, iowatch.SystemRoots(), sink, nil)
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		err = nil
	}
	sink.print(rec.Close())
	snap := rec.Dump(dir, time.Now())
	dumpErr := snap.Wait()
	if dumpErr == nil {
		fmt.Fprintln(a.out, snap.Path)
	}
	return errors.Join(err, dumpErr)
}

// ioRecordSink — получатель замеров `aguard io record`: случаи — строкой в вывод.
type ioRecordSink struct {
	a   *app
	rec *iowatch.Recorder
}

func (s *ioRecordSink) step(st iowatch.Step) { s.print(s.rec.Step(st)) }

func (s *ioRecordSink) print(cases []iowatch.Case) {
	for i := range cases {
		fmt.Fprintln(s.a.out, ioCaseLine(&cases[i]))
	}
}

func (s *ioRecordSink) detail(st iowatch.Step) { s.rec.Detail(st) }

func (s *ioRecordSink) sampleErr(_ string, err error) {
	s.a.log.Warn("aguard io: замер", "err", err)
}

// ioSignal — сигнал службе aguard (0 — только проверка, что она запущена); служба не запущена — ошибка
// errIONotRunning. Сигнал шлёт systemd, не этот процесс: agent-signal-guard отказывает агенту в kill(2)
// чужому unit (тикет aguard-io-dump-signal-guard).
func ioSignal(sig syscall.Signal) error {
	out, err := exec.Command("systemctl", "--user", "show", "-p", "MainPID", "--value", "aguard").Output()
	if err != nil {
		return fmt.Errorf("systemctl MainPID: %w", err)
	}
	value := strings.TrimSpace(string(out))
	pid, err := strconv.Atoi(value)
	if err != nil || pid <= 0 {
		return fmt.Errorf("%w (MainPID=%q)", errIONotRunning, value)
	}
	if sig == 0 {
		return nil
	}
	out, err = exec.Command("systemctl", "--user", "kill", "--kill-whom=main", "--signal="+unix.SignalName(sig),
		"aguard.service").CombinedOutput()
	if err != nil {
		return fmt.Errorf("сигнал службе %d: %w: %s", pid, err, strings.TrimSpace(string(out)))
	}
	return nil
}

var errIONotRunning = errors.New("служба aguard не запущена")

// ioDump — SIGUSR2 службе и путь ручного снимка, когда он записан (до 10 с).
func (a *app) ioDump(dir string) error {
	requested := time.Now().Add(-time.Second)
	if err := ioSignal(syscall.SIGUSR2); err != nil {
		return err
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		entries, err := os.ReadDir(dir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("прочитать папку снимков %s: %w", dir, err)
		}
		for _, entry := range entries {
			if !iowatch.IsManual(entry.Name()) || !strings.HasSuffix(entry.Name(), ".jsonl") {
				continue
			}
			if info, err := entry.Info(); err == nil && info.ModTime().After(requested) {
				fmt.Fprintln(a.out, filepath.Join(dir, entry.Name()))
				return nil
			}
		}
	}
	return errors.New("aguard io dump: снимок не появился за 10 с (mode = off или служба не отвечает — см. журнал)")
}

// ioWindow — окно [since, until) из аргументов в формах timearg (30m, 3d, ЧЧ:ММ, RFC3339) или Go Duration (3h45m — так
// -since report понимал до 10.11); until пусто — now; until раньше since — ошибка.
func ioWindow(since, until string, now time.Time) (time.Time, time.Time, error) {
	from, err := ioTimeArg(since, now)
	if err != nil {
		return from, now, fmt.Errorf("-since: %w", err)
	}
	to := now
	if until != "" {
		if to, err = ioTimeArg(until, now); err != nil {
			return from, to, fmt.Errorf("-until: %w", err)
		}
	}
	if to.Before(from) {
		if until == "" {
			return from, to, fmt.Errorf("-since %s в будущем", from.Format(time.RFC3339))
		}
		return from, to, fmt.Errorf("-until %s раньше -since %s", to.Format(time.RFC3339), from.Format(time.RFC3339))
	}
	return from, to, nil
}

// ioTimeArg — время по timearg, иначе now минус Go Duration; ошибка — от timearg, с подсказкой форм.
func ioTimeArg(s string, now time.Time) (time.Time, error) {
	at, err := timearg.Parse(s, now)
	if err == nil {
		return at, nil
	}
	d, durErr := time.ParseDuration(s)
	if durErr != nil {
		return at, fmt.Errorf("%w (и не Go Duration: %v)", err, durErr)
	}
	return now.Add(-d), nil
}

// parseIOTime — ЧЧ:ММ[:СС] сегодня по местному времени или RFC3339.
func parseIOTime(s string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, layout := range []string{"15:04:05", "15:04"} {
		if t, err := time.ParseInLocation(layout, s, now.Location()); err == nil {
			return time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), t.Second(), 0, now.Location()), nil
		}
	}
	return time.Time{}, fmt.Errorf("время %q: нужно ЧЧ:ММ[:СС] или RFC3339", s)
}

// ioAt — окно ±window/2 вокруг at по всем снимкам dir.
func (a *app) ioAt(dir string, at time.Time, window time.Duration) error {
	cfg, err := a.ioConfig()
	if err != nil {
		return err
	}
	win, err := iowatch.ReadSnapshots(dir, at.Add(-window/2), at.Add(window/2), cfg.Ring.D(), a.ioSnapshotWarn)
	if err != nil {
		return err
	}
	if win.Empty() {
		fmt.Fprintln(a.out, "нет данных в окне")
		nearest, ok, err := iowatch.NearestSnapshot(dir, at)
		if err != nil {
			return err
		}
		if ok {
			fmt.Fprintf(a.out, "ближайший снимок: %s\n", nearest.Format(time.RFC3339))
		}
		return nil
	}
	writeIOWindow(a.out, win)
	return nil
}

func (a *app) ioSnapshotWarn(err error) { a.log.Warn("aguard io: " + err.Error()) }

// writeIOWindow — секунды окна, затем итог окна по виновникам.
func writeIOWindow(w io.Writer, win iowatch.Window) {
	writeIOSeconds(w, win)
	writeIOSummary(w, win)
}

// writeIOSeconds — строка на секунду с быстрыми шагами.
func writeIOSeconds(w io.Writer, win iowatch.Window) {
	seconds := win.Seconds()
	for i := range seconds {
		sec := &seconds[i]
		if sec.Steps == 0 {
			continue
		}
		fmt.Fprintf(w, "%s  io full %d мс (пик %d)  mem full %d мс  swapin %d стр  диск R/W %.0f/%.0f МБ  занят %.0f%%  ожид R/W %d/%d мс  очередь %d  flush %d\n",
			sec.At.Format("15:04:05"), sec.IOFullUS/1000, sec.PeakUS/1000, sec.MemFullUS/1000, sec.SwapIn,
			megabytes(sec.ReadBytes), megabytes(sec.WriteBytes), float64(sec.IOTicks)/10,
			iowatch.AwaitMS(sec.RMS, sec.RIOS), iowatch.AwaitMS(sec.WMS, sec.WIOS), sec.InQueue, sec.Flushes)
	}
}

// writeIOSummary — итог окна: виновники по процессам и cgroup.
func writeIOSummary(w io.Writer, win iowatch.Window) {
	c := iowatch.SumCulprits(win.Details)
	const names = 5
	writeProcessIOTop(w, "запись процессов", iowatch.Rank(c.ProcWrite, names), ioBytes, c.ProcessDetails)
	writeProcessIOTop(w, "чтение процессов", iowatch.Rank(c.ProcRead, names), ioBytes, c.ProcessDetails)
	writeIOTop(w, "своп процессов", iowatch.Rank(c.ProcMajflt, names), ioMajflt)
	writeIOTop(w, "запись cgroup", iowatch.Rank(c.CgroupWrite, names), ioBytes)
	writeIOTop(w, "чтение cgroup", iowatch.Rank(c.CgroupRead, names), ioBytes)
	writeIOTop(w, "своп cgroup", iowatch.Rank(c.CgroupSwapIn, names), ioPages)
	writeIOTop(w, "простой IO cgroup", iowatch.Rank(c.CgroupIdle, names), ioMillis)
	writeIOTop(w, "операции записи cgroup", iowatch.Rank(c.CgroupWriteOps, names), ioOps)
}

// Единицы строк итога.
const (
	ioBytes  = iota // N МБ
	ioPages         // N стр
	ioMillis        // N мс
	ioOps           // N оп
	ioMajflt        // N, в конце строки «(majflt)»
)

// writeIOTop — строка «мера: имя N ед, …»; пусто — «нет».
func writeIOTop(w io.Writer, label string, top []iowatch.Ranked, unit int) {
	if len(top) == 0 {
		fmt.Fprintf(w, "%s: нет\n", label)
		return
	}
	parts := make([]string, len(top))
	for i, item := range top {
		parts[i] = ioValue(item.Name, item.Value, unit)
	}
	suffix := ""
	if unit == ioMajflt {
		suffix = " (majflt)"
	}
	fmt.Fprintf(w, "%s: %s%s\n", label, strings.Join(parts, ", "), suffix)
}

// ioValue — «имя N ед» в единицах unit.
func ioValue(name string, value int64, unit int) string {
	switch unit {
	case ioBytes:
		return fmt.Sprintf("%s %.0f МБ", name, megabytes(value))
	case ioPages:
		return fmt.Sprintf("%s %d стр", name, value)
	case ioMillis:
		return fmt.Sprintf("%s %d мс", name, value)
	case ioOps:
		return fmt.Sprintf("%s %d оп", name, value)
	}
	return fmt.Sprintf("%s %d", name, value)
}

func megabytes(b int64) float64 { return float64(b) / (1 << 20) }
