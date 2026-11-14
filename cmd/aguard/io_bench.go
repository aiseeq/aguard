package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aiseeq/aguard/internal/iowatch"
	"github.com/aiseeq/aguard/internal/policy"
	"github.com/aiseeq/akit/pressure"
	"github.com/aiseeq/akit/proc"
	"golang.org/x/sys/unix"
)

const benchMiB = int64(1 << 20)

const (
	benchStateDirMode     = 0755
	benchLockFileMode     = 0600
	benchChildFailure     = 127
	benchChildSignalGrace = time.Second
)

type benchVariant struct{ label, command string }
type benchRun struct {
	number         int
	variant, group string
	seconds        float64
	self, foreign  int64
	psi            float64
	mark           string
	clean          bool
}
type benchOptions struct {
	variants                                 []benchVariant
	file                                     string
	dirtyFloor                               int64
	runs                                     int
	cap                                      string
	unthrottled                              bool
	runTimeout, prepTimeout, prepStall, wait time.Duration
}

type repeatedString []string

func (r *repeatedString) String() string     { return strings.Join(*r, ",") }
func (r *repeatedString) Set(s string) error { *r = append(*r, s); return nil }

func parseBenchOptions(args []string) (benchOptions, error) {
	o := benchOptions{runs: 3, cap: "30M", dirtyFloor: 4 * benchMiB, runTimeout: 60 * time.Second, prepTimeout: 60 * time.Second, prepStall: time.Second}
	fl := flag.NewFlagSet("aguard io bench", flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	var values repeatedString
	fl.Var(&values, "variant", "метка: команда")
	fl.StringVar(&o.file, "file", "", "файл пробы")
	fl.IntVar(&o.runs, "runs", 3, "число блоков")
	fl.StringVar(&o.cap, "cap", "30M", "потолок IO")
	dirtyFloor := fl.String("dirty-floor", "4M", "порог Dirty+Writeback после sync")
	fl.BoolVar(&o.unthrottled, "unthrottled", false, "добавить прогоны без потолка")
	fl.DurationVar(&o.runTimeout, "run-timeout", 60*time.Second, "предел прогона")
	fl.DurationVar(&o.prepTimeout, "prep-timeout", 60*time.Second, "предел подготовки")
	fl.DurationVar(&o.prepStall, "prep-stall", time.Second, "порог PSI full подготовки")
	fl.DurationVar(&o.wait, "wait", 0, "ожидание замка и чужого окна")
	if err := fl.Parse(args); err != nil {
		return o, fmt.Errorf("аргументы: %w", err)
	}
	if fl.NArg() != 0 {
		return o, fmt.Errorf("лишние аргументы: %v", fl.Args())
	}
	if err := validateBenchLimits(o); err != nil {
		return o, err
	}
	if err := parseBenchSizes(&o, *dirtyFloor); err != nil {
		return o, err
	}
	variants, err := parseBenchVariants(values)
	if err != nil {
		return o, err
	}
	o.variants = variants
	if err := validateBenchFile(o.file); err != nil {
		return o, err
	}
	return o, nil
}

func validateBenchLimits(o benchOptions) error {
	if o.runs < 1 || o.runTimeout <= 0 || o.prepTimeout <= 0 || o.prepStall <= 0 || o.wait < 0 {
		return errors.New("runs должен быть ≥ 1; тайм-ауты и prep-stall положительны; wait не отрицателен")
	}
	return nil
}

func parseBenchSizes(o *benchOptions, dirtyFloor string) error {
	capBytes, err := policy.ParseSize(o.cap)
	if err != nil {
		return fmt.Errorf("--cap: %w", err)
	}
	if capBytes <= 0 {
		return errors.New("--cap: размер должен быть положительным")
	}
	o.dirtyFloor, err = policy.ParseSize(dirtyFloor)
	if err != nil {
		return fmt.Errorf("--dirty-floor: %w", err)
	}
	if o.dirtyFloor <= 0 {
		return errors.New("--dirty-floor: размер должен быть положительным")
	}
	return nil
}

func parseBenchVariants(values []string) ([]benchVariant, error) {
	variants := make([]benchVariant, 0, len(values))
	labels := map[string]bool{}
	for _, raw := range values {
		label, command, ok := strings.Cut(raw, ":")
		label, command = strings.TrimSpace(label), strings.TrimSpace(command)
		if !ok || label == "" || command == "" {
			return nil, errors.New("--variant должен иметь вид 'метка: команда'")
		}
		if labels[label] {
			return nil, fmt.Errorf("метка варианта повторяется: %s", label)
		}
		labels[label] = true
		variants = append(variants, benchVariant{label, command})
	}
	if len(variants) < 2 {
		return nil, errors.New("нужно не менее двух --variant")
	}
	return variants, nil
}

func validateBenchFile(path string) error {
	if path == "" {
		return nil
	}
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("файл пробы: %w", err)
	}
	if !st.Mode().IsRegular() {
		return errors.New("файл пробы должен быть обычным файлом")
	}
	if st.Size() > 64*benchMiB {
		return fmt.Errorf("файл пробы больше 64 MiB: %d байт", st.Size())
	}
	return nil
}

type benchScope interface {
	Wait(context.Context) (int, error)
	Stats() (self int64, psiFull int64, err error)
	CloseInput() error
	Close() error
	Stop(context.Context) error
}
type benchBackend interface {
	Now() time.Time
	Wait(context.Context, time.Duration) error
	ReadMeminfo() ([]byte, error)
	ReadPressure() ([]byte, error)
	Sync() error
	Fadvise(string) error
	Lock(context.Context, string, time.Duration) (func() error, error)
	QuietPath() (string, error)
	ReadQuiet(string) (time.Time, error)
	SetQuiet(string, pressure.QuietNote) error
	ClearQuiet(string) error
	Start(context.Context, benchVariant, string, string, string, bool) (benchScope, error)
	DeviceBytes(string) (int64, error)
}

type realBenchBackend struct{ blockRoot, cgroupRoot, procRoot string }

var benchUnitSequence atomic.Uint64

func (realBenchBackend) Now() time.Time { return time.Now() }
func (realBenchBackend) Wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
func (b realBenchBackend) ReadMeminfo() ([]byte, error) {
	return os.ReadFile(filepath.Join(b.procRoot, "meminfo"))
}
func (b realBenchBackend) ReadPressure() ([]byte, error) {
	return os.ReadFile(filepath.Join(b.procRoot, "pressure/io"))
}
func (realBenchBackend) Sync() error { unix.Sync(); return nil }
func (realBenchBackend) Fadvise(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("открыть файл пробы %s: %w", path, err)
	}
	defer f.Close()
	if err := unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED); err != nil {
		return fmt.Errorf("posix_fadvise DONTNEED %s: %w", path, err)
	}
	return nil
}
func (realBenchBackend) Lock(ctx context.Context, path string, wait time.Duration) (func() error, error) {
	if err := os.MkdirAll(filepath.Dir(path), benchStateDirMode); err != nil {
		return nil, fmt.Errorf("создать каталог замка %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, benchLockFileMode)
	if err != nil {
		return nil, fmt.Errorf("открыть замок %s: %w", path, err)
	}
	deadline := time.Now().Add(wait)
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() error {
				unlockErr := unix.Flock(int(f.Fd()), unix.LOCK_UN)
				closeErr := f.Close()
				return errors.Join(unlockErr, closeErr)
			}, nil
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
			_ = f.Close()
			return nil, fmt.Errorf("захватить flock %s: %w", path, err)
		}
		if !time.Now().Before(deadline) {
			_ = f.Close()
			return nil, errors.New("идёт другой замер")
		}
		t := time.NewTimer(min(100*time.Millisecond, time.Until(deadline)))
		select {
		case <-ctx.Done():
			t.Stop()
			_ = f.Close()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}
func (realBenchBackend) QuietPath() (string, error)            { return pressure.QuietPath() }
func (realBenchBackend) ReadQuiet(p string) (time.Time, error) { return pressure.ReadQuiet(p) }
func (realBenchBackend) SetQuiet(p string, n pressure.QuietNote) error {
	return pressure.SetQuiet(p, n)
}
func (realBenchBackend) ClearQuiet(p string) error { return pressure.ClearQuiet(p) }
func (b realBenchBackend) DeviceBytes(device string) (int64, error) {
	data, e := os.ReadFile(filepath.Join(b.procRoot, "diskstats"))
	if e != nil {
		return 0, fmt.Errorf("прочитать diskstats: %w", e)
	}
	return iowatch.BenchDiskBytes(data, device)
}
func (b realBenchBackend) Start(ctx context.Context, v benchVariant, file, device, cap string, unthrottled bool) (benchScope, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("путь к aguard: %w", err)
	}
	unit := fmt.Sprintf("aguard-bench-%d-%d", os.Getpid(), benchUnitSequence.Add(1))
	argv := benchScopeArgv("systemd-run", exe, unit, device, cap, unthrottled, v.command)
	psi, err := os.ReadFile(filepath.Join(b.procRoot, "pressure/io"))
	if err != nil {
		return nil, fmt.Errorf("прочитать PSI перед scope: %w", err)
	}
	_, full, err := iowatch.BenchPressureTotals(psi)
	if err != nil {
		return nil, fmt.Errorf("разобрать PSI перед scope: %w", err)
	}
	cmd := newBenchCommand(argv, exec.Command)
	cmd.Env = benchCommandEnv(os.Environ(), file)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin systemd-run: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout systemd-run: %w", err)
	}
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	s := &systemdBenchScope{cmd: cmd, stdin: in, stdout: stdout, unit: unit, holderPID: cmd.Process.Pid, procRoot: b.procRoot, blockRoot: b.blockRoot, cgroupRoot: b.cgroupRoot, device: device, startFull: full}
	return s, nil
}

func newBenchCommand(argv []string, command func(string, ...string) *exec.Cmd) *exec.Cmd {
	return command(argv[0], argv[1:]...)
}

func benchCommandEnv(environment []string, file string) []string {
	filtered := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, "AGUARD_BENCH_FILE=") {
			filtered = append(filtered, entry)
		}
	}
	if file != "" {
		filtered = append(filtered, "AGUARD_BENCH_FILE="+file)
	}
	return filtered
}

func benchScopeArgv(systemdRun, executable, unit, device, cap string, unthrottled bool, command string) []string {
	argv := []string{systemdRun, "--user", "--scope", "--quiet", "--collect", "--property=IOAccounting=yes", "--unit=" + unit}
	if !unthrottled {
		argv = append(argv, "--property=IOReadBandwidthMax=/dev/"+device+" "+cap, "--property=IOWriteBandwidthMax=/dev/"+device+" "+cap)
	}
	return append(argv, "--", executable, "io", "bench-hold", "--", "sh", "-c", command)
}

type systemdBenchScope struct {
	cmd                                           *exec.Cmd
	stdin                                         io.WriteCloser
	stdout                                        io.ReadCloser
	unit, procRoot, blockRoot, cgroupRoot, device string
	holderPID                                     int
	startFull                                     int64
	code                                          int
}

func (s *systemdBenchScope) Wait(ctx context.Context) (int, error) {
	ch := make(chan error, 1)
	go func() { code, err := readBenchExit(s.stdout); s.code = code; ch <- err }()
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case e := <-ch:
		if e != nil {
			return 0, e
		}
		return s.code, nil
	}
}

func readBenchExit(reader io.Reader) (int, error) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "exit ") {
			continue
		}
		code, err := strconv.Atoi(strings.TrimPrefix(line, "exit "))
		if err != nil {
			return 0, fmt.Errorf("неверный код держателя %q: %w", line, err)
		}
		return code, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("прочитать stdout держателя: %w", err)
	}
	return 0, errors.New("scope не прислал exit code")
}
func (s *systemdBenchScope) Stats() (int64, int64, error) {
	cgroupFile := filepath.Join(s.procRoot, strconv.Itoa(s.holderPID), "cgroup")
	cg, err := unifiedCgroup(cgroupFile)
	if err != nil {
		return 0, 0, fmt.Errorf("cgroup держателя: %w", err)
	}
	devices, err := iowatch.BenchBlockDevices(s.blockRoot, s.device)
	if err != nil {
		return 0, 0, err
	}
	data, err := os.ReadFile(filepath.Join(s.cgroupRoot, cg, "io.stat"))
	if err != nil {
		return 0, 0, fmt.Errorf("прочитать io.stat scope: %w", err)
	}
	r, w, err := iowatch.BenchIOStatBytes(data, devices)
	if err != nil {
		return 0, 0, err
	}
	pressureData, err := os.ReadFile(filepath.Join(s.procRoot, "pressure/io"))
	if err != nil {
		return 0, 0, fmt.Errorf("прочитать PSI io: %w", err)
	}
	_, full, err := iowatch.BenchPressureTotals(pressureData)
	return r + w, max(int64(0), full-s.startFull), err
}
func (s *systemdBenchScope) CloseInput() error {
	if err := s.stdin.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		return fmt.Errorf("закрыть stdin держателя: %w", err)
	}
	return nil
}
func (s *systemdBenchScope) Close() error {
	return s.cmd.Wait()
}
func (s *systemdBenchScope) Stop(ctx context.Context) error {
	cmd := exec.CommandContext(context.WithoutCancel(ctx), "systemctl", "--user", "stop", s.unit+".scope")
	return cmd.Run()
}

func (a *app) ioBench(ctx context.Context, args []string) error {
	o, err := parseBenchOptions(args)
	if err != nil {
		fmt.Fprintf(a.out, "aguard io bench: %v\n", err)
		return exitCode(2)
	}
	cfg, err := a.ioConfig()
	if err != nil {
		fmt.Fprintf(a.out, "aguard io bench: конфигурация: %v\n", err)
		return exitCode(2)
	}
	b := a.benchIO
	if b == nil {
		b = realBenchBackend{procRoot: "/proc", blockRoot: "/sys/block", cgroupRoot: "/sys/fs/cgroup"}
	}
	return runIOBench(ctx, a.out, b, o, filepath.Join(a.dir("io"), "bench.lock"), cfg.Device, os.Getpid())
}

func runIOBench(ctx context.Context, out io.Writer, b benchBackend, o benchOptions, lockPath, device string, pid int) (retErr error) {
	waitDeadline := b.Now().Add(o.wait)
	unlock, err := b.Lock(ctx, lockPath, o.wait)
	if err != nil {
		fmt.Fprintf(out, "aguard io bench: замок: %v\n", err)
		return exitCode(2)
	}
	defer func() {
		if err := unlock(); err != nil {
			fmt.Fprintf(out, "aguard io bench: освободить замок: %v\n", err)
			retErr = exitCode(2)
		}
	}()
	windowPath, err := waitBenchWindow(ctx, b, waitDeadline)
	if err != nil {
		fmt.Fprintf(out, "aguard io bench: окно: %v\n", err)
		return exitCode(2)
	}
	window := &benchWindow{backend: b, path: windowPath, owner: fmt.Sprintf("aguard io bench pid %d", pid)}
	defer func() {
		if e := window.Close(); e != nil {
			fmt.Fprintf(out, "aguard io bench: очистка окна: %v\n", e)
			if code, ok := exitStatus(retErr); !ok || code != 2 {
				retErr = exitCode(2)
			}
		}
	}()
	order := benchOrder(o)
	printBenchOrder(out, order)
	results := make([]benchRun, 0, len(order))
	status := 0
	for i, item := range order {
		row, hasRow, err := runBenchTrial(ctx, b, window, o, device, i+1, item)
		if hasRow {
			results = append(results, row)
		}
		if err != nil {
			fmt.Fprintf(out, "aguard io bench: прогон %d: %v\n", i+1, err)
			status = 2
			break
		}
		if !row.clean {
			if status < 1 {
				status = 1
			}
		}
	}
	printBenchResults(out, results, o)
	if status == 2 {
		return exitCode(2)
	}
	if status == 1 {
		return exitCode(1)
	}
	return nil
}

func waitBenchWindow(ctx context.Context, b benchBackend, deadline time.Time) (string, error) {
	path, err := b.QuietPath()
	if err != nil {
		return "", fmt.Errorf("путь тихого окна: %w", err)
	}
	for {
		now := b.Now()
		until, err := b.ReadQuiet(path)
		if err != nil {
			return "", fmt.Errorf("прочитать тихое окно: %w", err)
		}
		if !now.Before(until) {
			return path, nil
		}
		if !now.Before(deadline) {
			return "", errors.New("чужое тихое окно активно; идёт другой замер")
		}
		if err := b.Wait(ctx, min(time.Second, deadline.Sub(now))); err != nil {
			return "", fmt.Errorf("ожидание тихого окна: %w", err)
		}
	}
}

type benchWindow struct {
	backend     benchBackend
	path, owner string
	last        time.Time
	owned       bool
}

func (w *benchWindow) Renew(o benchOptions) error {
	cur, err := w.backend.ReadQuiet(w.path)
	if err != nil {
		return fmt.Errorf("прочитать окно перед продлением: %w", err)
	}
	now := w.backend.Now()
	if w.owned {
		if !cur.Truncate(time.Second).Equal(w.last) {
			return errors.New("тихое окно стало чужим")
		}
	} else if now.Before(cur) {
		return errors.New("чужое тихое окно активно")
	}
	next := now.Add(o.prepTimeout + o.runTimeout + 10*time.Second).Truncate(time.Second)
	if err := w.backend.SetQuiet(w.path, pressure.QuietNote{Until: next, By: w.owner, Why: "aguard io bench"}); err != nil {
		if observed, readErr := w.backend.ReadQuiet(w.path); readErr == nil && observed.Truncate(time.Second).Equal(next) {
			w.last, w.owned = next, true
		}
		return fmt.Errorf("SetQuiet: %w", err)
	}
	w.last, w.owned = next, true
	return nil
}

func (w *benchWindow) Close() error {
	if !w.owned {
		return nil
	}
	cur, err := w.backend.ReadQuiet(w.path)
	if err != nil {
		return fmt.Errorf("прочитать тихое окно перед снятием: %w", err)
	}
	if !cur.Truncate(time.Second).Equal(w.last) {
		return nil
	}
	// Между ReadQuiet и ClearQuiet другой владелец всё ещё может заменить общий файл окна.
	if err := w.backend.ClearQuiet(w.path); err != nil {
		return fmt.Errorf("ClearQuiet: %w", err)
	}
	return nil
}

func printBenchOrder(out io.Writer, order []orderedBench) {
	labels := make([]string, len(order))
	for i, item := range order {
		labels[i] = item.variant.label
	}
	fmt.Fprintf(out, "порядок: %s\n", strings.Join(labels, " "))
}

func runBenchTrial(ctx context.Context, b benchBackend, window *benchWindow, o benchOptions, device string, number int, item orderedBench) (benchRun, bool, error) {
	row := benchRun{number: number, variant: item.variant.label, group: item.group}
	if err := window.Renew(o); err != nil {
		return row, false, fmt.Errorf("продление окна: %w", err)
	}
	prepDeadline := b.Now().Add(o.prepTimeout)
	err := prepareBench(ctx, b, prepDeadline, o)
	if err != nil {
		return row, false, err
	}
	if o.file != "" {
		if err := b.Fadvise(o.file); err != nil {
			return row, false, fmt.Errorf("fadvise %s: %w", o.file, err)
		}
	}
	before, err := b.DeviceBytes(device)
	if err != nil {
		return row, false, fmt.Errorf("diskstats до прогона: %w", err)
	}
	begin := b.Now()
	waitCtx, cancel := context.WithTimeout(ctx, o.runTimeout)
	defer cancel()
	scope, err := b.Start(waitCtx, item.variant, o.file, device, o.cap, item.group == "без потолка")
	if err != nil {
		return row, false, fmt.Errorf("запуск scope: %w", err)
	}
	code, err := scope.Wait(waitCtx)
	row.seconds = b.Now().Sub(begin).Seconds()
	if err != nil {
		inputErr := scope.CloseInput()
		stopErr := scope.Stop(ctx)
		closeErr := scope.Close()
		return row, false, fmt.Errorf("тайм-аут или отмена: %w (stdin: %v; остановка scope: %v; закрытие: %v)", err, inputErr, stopErr, closeErr)
	}
	self, psi, statsErr := scope.Stats()
	after, diskErr := b.DeviceBytes(device)
	inputErr := scope.CloseInput()
	closeErr := scope.Close()
	if statsErr != nil || diskErr != nil || inputErr != nil || closeErr != nil {
		return row, false, errors.Join(
			benchCheckError("scope", statsErr),
			benchCheckError("diskstats", diskErr),
			benchCheckError("stdin", inputErr),
			benchCheckError("закрытие", closeErr),
		)
	}
	if code != 0 {
		row.mark = "не проверен"
		return row, true, fmt.Errorf("вариант %s завершился с кодом %d", item.variant.label, code)
	}
	disk := max(int64(0), after-before)
	row.self = self
	row.foreign = max(int64(0), disk-self)
	row.psi = float64(psi) / max(row.seconds, 0.000001) / 10000
	row.mark, row.clean = "чистый", true
	if row.foreign > max(4*benchMiB, self/20) {
		row.mark, row.clean = "гипотеза", false
	}
	return row, true, nil
}

func benchCheckError(label string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("прогон не проверен (%s): %w", label, err)
}

type orderedBench struct {
	variant benchVariant
	group   string
}

func benchOrder(o benchOptions) []orderedBench {
	var out []orderedBench
	groups := []string{"с потолком"}
	if o.unthrottled {
		groups = append(groups, "без потолка")
	}
	for _, group := range groups {
		for k := 0; k < o.runs; k++ {
			for j := range o.variants {
				idx := j
				if k%2 == 1 {
					idx = len(o.variants) - 1 - j
				}
				out = append(out, orderedBench{o.variants[idx], group})
			}
		}
	}
	return out
}
func readFull(b benchBackend) (int64, error) {
	data, err := b.ReadPressure()
	if err != nil {
		return 0, err
	}
	_, full, err := iowatch.BenchPressureTotals(data)
	return full, err
}

var errBenchPrepStall = errors.New("фриз подготовки")

func prepareBench(ctx context.Context, b benchBackend, deadline time.Time, o benchOptions) error {
	startFull, err := readFull(b)
	if err != nil {
		return fmt.Errorf("PSI перед подготовкой: %w", err)
	}
	checkStall := func() error {
		full, err := readFull(b)
		if err != nil {
			return fmt.Errorf("PSI подготовки: %w", err)
		}
		if full-startFull >= o.prepStall.Microseconds() {
			return fmt.Errorf("%w: PSI io full вырос на %s", errBenchPrepStall, time.Duration(full-startFull)*time.Microsecond)
		}
		return nil
	}
	waitBelow := func(limitBytes int64) error {
		for {
			if err := checkStall(); err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("отмена подготовки: %w", err)
			}
			if !b.Now().Before(deadline) {
				if err := checkStall(); err != nil {
					return err
				}
				return errors.New("превышен --prep-timeout")
			}
			data, err := b.ReadMeminfo()
			if err != nil {
				return fmt.Errorf("meminfo: %w", err)
			}
			dirty, err := pressure.ParseDirty(string(data))
			if err != nil {
				return fmt.Errorf("Dirty+Writeback: %w", err)
			}
			dirtyKiB := int64(dirty * 1024)
			if dirtyKiB*1024 <= limitBytes {
				return nil
			}
			remaining := deadline.Sub(b.Now())
			if remaining <= 0 {
				continue
			}
			if err = b.Wait(ctx, min(time.Second, remaining)); err != nil {
				return fmt.Errorf("ожидание Dirty+Writeback: %w", err)
			}
		}
	}
	if err := waitBelow(64 * benchMiB); err != nil {
		return err
	}
	if err := b.Sync(); err != nil {
		return fmt.Errorf("unix.Sync: %w", err)
	}
	return waitBelow(o.dirtyFloor)
}
func printBenchResults(w io.Writer, rs []benchRun, o benchOptions) {
	if o.file != "" {
		fmt.Fprintln(w, "ограничение: сброшен кэш страниц файла; метаданные ФС не сбрасываются")
	}
	fmt.Fprintln(w, "ограничение: IO вне scope (службы, kworker, метаданные ФС) считается чужим; «гипотеза» означает, что чистота не доказана")
	fmt.Fprintln(w, "№\tвариант\tпотолок\tвремя, с\tсвои, байт\tчужие, байт\tPSI io full, %\tпометка")
	for _, r := range rs {
		cap := o.cap
		if r.group == "без потолка" {
			cap = "нет"
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%d\t%d\t%s\t%s\n", r.number, r.variant, cap, benchDecimal(r.seconds), r.self, r.foreign, benchDecimal(r.psi), r.mark)
	}
	type key struct{ label, group string }
	keys := map[key][]float64{}
	all := map[key]int{}
	for _, r := range rs {
		k := key{r.variant, r.group}
		all[k]++
		if r.clean {
			keys[k] = append(keys[k], r.seconds)
		}
	}
	for _, group := range []string{"с потолком", "без потолка"} {
		if group == "без потолка" && !o.unthrottled {
			continue
		}
		for _, v := range o.variants {
			k := key{v.label, group}
			vals := keys[k]
			fmt.Fprintf(w, "итог %s/%s: ", v.label, group)
			if len(vals) == 0 {
				fmt.Fprintf(w, "нет чистых, n=0/%d", all[k])
				if len(vals) < o.runs {
					fmt.Fprint(w, "; выборка неполная")
				}
				fmt.Fprintln(w)
				continue
			}
			sort.Float64s(vals)
			med := vals[len(vals)/2]
			if len(vals)%2 == 0 {
				med = (vals[len(vals)/2-1] + med) / 2
			}
			fmt.Fprintf(w, "медиана %s с, мин–макс %s–%s с, n=%d/%d", benchDecimal(med), benchDecimal(vals[0]), benchDecimal(vals[len(vals)-1]), len(vals), all[k])
			if len(vals) < o.runs {
				fmt.Fprint(w, "; выборка неполная")
			}
			fmt.Fprintln(w)
		}
	}
}

func benchDecimal(v float64) string {
	return strings.ReplaceAll(strconv.FormatFloat(v, 'f', 2, 64), ".", ",")
}

type benchChild interface {
	Wait() error
	Signal(os.Signal) error
}

type benchChildStart func([]string, io.Writer) (benchChild, error)

type execBenchChild struct{ cmd *exec.Cmd }

func (c execBenchChild) Wait() error { return c.cmd.Wait() }
func (c execBenchChild) Signal(sig os.Signal) error {
	_, err := proc.StopChild(c.cmd.Process.Pid, benchChildSignalGrace, func() error { return c.cmd.Process.Signal(sig) })
	return err
}

func startBenchChild(args []string, stderr io.Writer) (benchChild, error) {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout, cmd.Stderr = stderr, stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("запустить дочернюю команду: %w", err)
	}
	return execBenchChild{cmd: cmd}, nil
}

// benchHold держит scope после команды до EOF на stdin и пересылает полученный сигнал её процессу.
func benchHold(ctx context.Context, args []string) int {
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGTERM, os.Interrupt)
	defer signal.Stop(signals)
	return runBenchHold(ctx, signals, args, os.Stdout, os.Stderr, os.Stdin, startBenchChild)
}

func runBenchHold(ctx context.Context, signals <-chan os.Signal, args []string, stdout, stderr io.Writer, stdin io.Reader, start benchChildStart) int {
	if len(args) < 2 {
		return 2
	}
	if args[0] == "--" {
		args = args[1:]
	}
	if len(args) < 3 || args[0] != "sh" || args[1] != "-c" {
		return 2
	}
	child, err := start(args, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "aguard io bench: команда: %v\n", err)
		_, _ = fmt.Fprintf(stdout, "exit %d\n", benchChildFailure)
		return 0
	}
	wait := make(chan error, 1)
	go func() { wait <- child.Wait() }()
	var childErr error
	select {
	case childErr = <-wait:
	case sig := <-signals:
		if err := child.Signal(sig); err != nil {
			fmt.Fprintf(stderr, "aguard io bench: передать сигнал дочерней команде: %v\n", err)
			return 2
		}
		return 0
	case <-ctx.Done():
		select {
		case sig := <-signals:
			if err := child.Signal(sig); err != nil {
				fmt.Fprintf(stderr, "aguard io bench: передать сигнал дочерней команде: %v\n", err)
				return 2
			}
			return 0
		default:
		}
		if err := child.Signal(syscall.SIGTERM); err != nil {
			fmt.Fprintf(stderr, "aguard io bench: остановить дочернюю команду: %v\n", err)
			return 2
		}
		return 0
	}
	code := 0
	if childErr != nil {
		fmt.Fprintf(stderr, "aguard io bench: дочерняя команда завершилась с ошибкой: %v\n", childErr)
		var ee *exec.ExitError
		if errors.As(childErr, &ee) {
			code = ee.ExitCode()
		} else {
			code = benchChildFailure
			fmt.Fprintf(stderr, "aguard io bench: дождаться команды: %v\n", childErr)
		}
	}
	_, _ = fmt.Fprintf(stdout, "exit %d\n", code)
	if _, err := io.Copy(io.Discard, stdin); err != nil {
		fmt.Fprintf(stderr, "aguard io bench: stdin держателя: %v\n", err)
		return 2
	}
	return 0
}
