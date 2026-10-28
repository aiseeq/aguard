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
)

// Команды наблюдателя io: запись в переднем плане, ручной снимок службы, окно по времени, фризы.

// ioCmd — aguard io record | dump | at | freezes.
func (a *app) ioCmd(ctx context.Context, args []string) error {
	cmd, rest := sub(args)
	events := filepath.Join(a.dir("io"), "events")
	switch cmd {
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
	case "freezes":
		fl := flag.NewFlagSet("io freezes", flag.ContinueOnError)
		since := fl.Duration("since", 24*time.Hour, "глубина поиска")
		minIdle := fl.Duration("min", 50*time.Millisecond, "простой шага, с которого он — фриз")
		dir := fl.String("dir", events, "папка снимков")
		parse(fl, rest)
		return a.ioFreezes(*dir, time.Now().Add(-*since), time.Now(), *minIdle)
	}
	return fmt.Errorf("aguard io: неизвестная команда %s", cmd)
}

// ioConfig — раздел [io] конфига; конфиг не прочитался — ошибка, по неизвестным правилам не пишем.
func (a *app) ioConfig() (iowatch.Config, error) {
	c, err := a.config()
	if err != nil {
		return iowatch.Config{}, err
	}
	return c.IO, nil
}

// ioRecord — замер в переднем плане тем же циклом, буфером и событиями, что у службы, без разбора
// и писем; в конце — буфер ручным снимком в dir.
func (a *app) ioRecord(ctx context.Context, dur time.Duration, dir string) error {
	if dur <= 0 {
		return errors.New("aguard io record: длительность должна быть положительной")
	}
	cfg, err := a.ioConfig()
	if err != nil {
		return err
	}
	rec := iowatch.NewRecorder(cfg, dir)
	sink := &ioRecordSink{a: a, rec: rec}
	ctx, cancel := context.WithTimeout(ctx, dur)
	defer cancel()
	err = runIOSampling(ctx, cfg, iowatch.SystemRoots(), sink, nil)
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		err = nil
	}
	ev, open, closeErr := rec.Close()
	if open {
		fmt.Fprintln(a.out, ioEventLine(ev))
	}
	snap := rec.Dump(time.Now())
	dumpErr := snap.Wait()
	if dumpErr == nil {
		fmt.Fprintln(a.out, snap.Path)
	}
	return errors.Join(err, closeErr, dumpErr)
}

// ioRecordSink — получатель замеров `aguard io record`: события — строкой в вывод.
type ioRecordSink struct {
	a   *app
	rec *iowatch.Recorder
}

func (s *ioRecordSink) step(st iowatch.Step) {
	ev, err := s.rec.Step(st)
	if err != nil {
		s.a.log.Warn("aguard io: снимок фриза", "err", err)
	}
	if ev != nil {
		fmt.Fprintln(s.a.out, ioEventLine(*ev))
	}
}

func (s *ioRecordSink) detail(st iowatch.Step) { s.rec.Detail(st) }

func (s *ioRecordSink) sampleErr(_ string, err error) {
	s.a.log.Warn("aguard io: замер", "err", err)
}

// ioEventLine — строка журнала о закрытом событии.
func ioEventLine(ev iowatch.Event) string {
	return fmt.Sprintf("io: фриз с %s по %s, простой всего %d мс, пик шага %d мс",
		ev.Start.Format("15:04:05.000"), ev.End.Format("15:04:05.000"), ev.IdleUS/1000, ev.PeakUS/1000)
}

// ioDump — SIGUSR1 службе и путь ручного снимка, когда он записан (до 10 с).
func (a *app) ioDump(dir string) error {
	requested := time.Now().Add(-time.Second)
	out, err := exec.Command("systemctl", "--user", "show", "-p", "MainPID", "--value", "aguard").Output()
	if err != nil {
		return fmt.Errorf("systemctl MainPID: %w", err)
	}
	value := strings.TrimSpace(string(out))
	pid, err := strconv.Atoi(value)
	if err != nil || pid <= 0 {
		return fmt.Errorf("служба aguard не запущена (MainPID=%q)", value)
	}
	if err := syscall.Kill(pid, syscall.SIGUSR1); err != nil {
		return fmt.Errorf("сигнал службе %d: %w", pid, err)
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
	writeIOSeconds(w, win, false)
	writeIOSummary(w, win)
}

// writeIOSeconds — строка на секунду с быстрыми шагами; onlyIdle — только секунды с простоем.
func writeIOSeconds(w io.Writer, win iowatch.Window, onlyIdle bool) {
	seconds := win.Seconds()
	for i := range seconds {
		sec := &seconds[i]
		if sec.Steps == 0 || onlyIdle && sec.IOFullUS+sec.MemFullUS == 0 {
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
	writeIOTop(w, "запись процессов", c.ProcWrite, ioBytes, names)
	writeIOTop(w, "чтение процессов", c.ProcRead, ioBytes, names)
	writeIOTop(w, "своп процессов", c.ProcMajflt, ioMajflt, names)
	writeIOTop(w, "запись cgroup", c.CgroupWrite, ioBytes, names)
	writeIOTop(w, "чтение cgroup", c.CgroupRead, ioBytes, names)
	writeIOTop(w, "своп cgroup", c.CgroupSwapIn, ioPages, names)
	writeIOTop(w, "простой IO cgroup", c.CgroupIdle, ioMillis, names)
	writeIOTop(w, "операции записи cgroup", c.CgroupWriteOps, ioOps, names)
}

// Единицы строк итога.
const (
	ioBytes  = iota // N МБ
	ioPages         // N стр
	ioMillis        // N мс
	ioOps           // N оп
	ioMajflt        // N, в конце строки «(majflt)»
)

// writeIOTop — строка «мера: имя N ед, …» до limit имён по убыванию; пусто — «нет».
func writeIOTop(w io.Writer, label string, m map[string]int64, unit, limit int) {
	top := iowatch.Rank(m, limit)
	if len(top) == 0 {
		fmt.Fprintf(w, "%s: нет\n", label)
		return
	}
	parts := make([]string, len(top))
	for i, item := range top {
		switch unit {
		case ioBytes:
			parts[i] = fmt.Sprintf("%s %.0f МБ", item.Name, megabytes(item.Value))
		case ioPages:
			parts[i] = fmt.Sprintf("%s %d стр", item.Name, item.Value)
		case ioMillis:
			parts[i] = fmt.Sprintf("%s %d мс", item.Name, item.Value)
		case ioOps:
			parts[i] = fmt.Sprintf("%s %d оп", item.Name, item.Value)
		default:
			parts[i] = fmt.Sprintf("%s %d", item.Name, item.Value)
		}
	}
	suffix := ""
	if unit == ioMajflt {
		suffix = " (majflt)"
	}
	fmt.Fprintf(w, "%s: %s%s\n", label, strings.Join(parts, ", "), suffix)
}

func megabytes(b int64) float64 { return float64(b) / (1 << 20) }

// ioFreezes — фризы [since, until) по снимкам dir: шаги с простоем ≥ minIdle подряд.
func (a *app) ioFreezes(dir string, since, until time.Time, minIdle time.Duration) error {
	cfg, err := a.ioConfig()
	if err != nil {
		return err
	}
	win, err := iowatch.ReadSnapshots(dir, since.Add(-time.Second), until.Add(time.Second), cfg.Ring.D(), a.ioSnapshotWarn)
	if err != nil {
		return err
	}
	printed := false
	for _, f := range win.Freezes(minIdle) {
		if f.End.Before(since) {
			continue
		}
		length := f.End.Sub(f.Start) + cfg.Step.D()
		fmt.Fprintf(a.out, "%s — %s  длительность %d мс, простой %d мс, пик %d мс\n", f.Start.Format("2006-01-02 15:04:05.000"),
			f.End.Format("15:04:05.000"), length.Milliseconds(), f.IdleUS/1000, f.PeakUS/1000)
		c := iowatch.SumCulprits(f.Details)
		const names = 3
		writeIOTop(a.out, "запись процессов", c.ProcWrite, ioBytes, names)
		writeIOTop(a.out, "чтение процессов", c.ProcRead, ioBytes, names)
		writeIOTop(a.out, "своп процессов", c.ProcMajflt, ioMajflt, names)
		printed = true
	}
	if !printed {
		fmt.Fprintln(a.out, "фризов нет")
	}
	return nil
}
