// Наблюдатель tmp (бывший tmpguard, user-часть): раз в интервал читает снимок
// слежения, считает «размер × время» по владельцам и шлёт сводки
// виновникам (режим alert) или только пишет, что отправил бы (observe).
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/aiseeq/aguard/internal/cpuwatch"
	"github.com/aiseeq/aguard/internal/guide"
	"github.com/aiseeq/aguard/internal/policy"
	"github.com/aiseeq/aguard/internal/record"
	"github.com/aiseeq/aguard/internal/report"
	"github.com/aiseeq/akit/pressure"
	"github.com/aiseeq/akit/session"
)

func (a *app) tmpCmd(args []string) error {
	cmd, rest := sub(args)
	cfg, err := a.config()
	if err != nil {
		return err
	}
	switch cmd {
	case "tick":
		fl := flag.NewFlagSet("tmp tick", flag.ContinueOnError)
		dry := fl.Bool("dry-run", false, "напечатать сводки, ничего не слать и не менять")
		parse(fl, rest)
		if !*dry {
			if err := os.MkdirAll(a.dir("tmp"), 0o700); err != nil {
				return fmt.Errorf("папка состояния: %w", err)
			}
		}
		return a.tmpRound(&cfg.Tmp, *dry)
	case "report":
		fl := flag.NewFlagSet("tmp report", flag.ContinueOnError)
		since := fl.Duration("since", 0, "только записи, закрытые за этот срок (0 — весь журнал)")
		parse(fl, rest)
		return a.tmpReport(&cfg.Tmp, *since)
	case "send-test":
		fl := flag.NewFlagSet("tmp send-test", flag.ContinueOnError)
		to := fl.String("to", "", "имя сессии-адресата (по умолчанию лид из конфига)")
		dry := fl.Bool("dry-run", false, "только напечатать")
		parse(fl, rest)
		return a.tmpSendTest(&cfg.Tmp, *to, *dry)
	}
	usage()
	return nil
}

func (a *app) tmpStatePath() string { return filepath.Join(a.dir("tmp"), "state.json") }

// snapshot — свежий снимок трекера: /run, а если его нет — диск.
func (a *app) snapshot(cfg *policy.Config) (*record.State, error) {
	var best *record.State
	for _, dir := range []string{cfg.RunDir, cfg.TrackDir} {
		s, err := record.ReadState(filepath.Join(dir, record.StateFile))
		if err != nil {
			return nil, err
		}
		if best == nil || s.Updated.After(best.Updated) {
			best = s
		}
	}
	if best.Updated.IsZero() {
		return nil, errors.New("снимка слежения нет: проверьте aguard-track-helper и строки «слежение» в журнале aguard")
	}
	for _, p := range best.NoBorn {
		a.warn("tmp", "запись снимка без born исключена из сводок", "path", p)
	}
	return best, nil
}

// liveSessions — policy.Live поверх реестра сессий.
type liveSessions struct {
	reg    session.Registry
	log    *slog.Logger
	byName map[string]session.Info
	jobs   *cpuwatch.Jobs
}

func (l *liveSessions) ByPid(pid int, procStart string) (policy.Target, bool) {
	if !l.reg.IsAlive(pid, procStart) {
		return policy.Target{}, false
	}
	in, err := l.reg.Get(pid)
	if err != nil {
		l.log.Warn("aguard tmp: реестр сессий", "pid", pid, "err", err)
		return policy.Target{}, false
	}
	if in == nil || in.SocketPath == "" {
		return policy.Target{}, false
	}
	return policy.Target{Name: in.Name, Pid: in.Pid, Socket: in.SocketPath}, true
}

func (l *liveSessions) WorkerByUnit(unit string) (policy.WorkerJob, bool, error) {
	job, err := l.jobs.ByUnit(unit)
	if err != nil || job == nil {
		return policy.WorkerJob{}, false, err
	}
	unitLive, err := l.jobs.UnitLive(job.Unit)
	if err != nil {
		return policy.WorkerJob{}, false, err
	}
	cwd := ""
	if in, ok := l.byName[job.Master]; ok {
		cwd = in.Cwd
	}
	return policy.WorkerJob{ID: job.ID, Master: job.Master, Cwd: cwd, Live: unitLive}, true, nil
}

func (l *liveSessions) ByName(name string) (policy.Target, bool) {
	in, ok := l.byName[name]
	if !ok || in.SocketPath == "" {
		return policy.Target{}, false
	}
	return policy.Target{Name: in.Name, Pid: in.Pid, Socket: in.SocketPath}, true
}

func (a *app) live() (*liveSessions, error) {
	m, err := a.reg.Alive()
	if err != nil {
		return nil, fmt.Errorf("живые сессии: %w", err)
	}
	return &liveSessions{reg: a.reg, log: a.log, byName: m, jobs: a.workerJobs()}, nil
}

// workerJobs — задания worker на ноуте: папка состояния и папка cgroup живых unit бойцов (подменяются в тестах).
func (a *app) workerJobs() *cpuwatch.Jobs {
	jobsDir := a.workerJobsDir
	if jobsDir == "" {
		jobsDir = filepath.Join(a.home, ".local", "state", "worker")
	}
	unitsDir := a.workerUnitsDir
	if unitsDir == "" {
		unitsDir = fmt.Sprintf("/sys/fs/cgroup/user.slice/user-%d.slice/user@%d.service/app.slice", a.uid, a.uid)
	}
	return &cpuwatch.Jobs{Dir: jobsDir, Units: unitsDir}
}

// tmpFactor — множитель порога по давлению памяти. Давление не прочиталось —
// порог без поправки (множитель 1), с записью в журнал: недочитанный /proc
// не повод ни пропускать проход, ни ужесточать порог.
func (a *app) tmpFactor(cfg *policy.Config) float64 {
	p, err := pressure.ReadMemory(procRoot)
	if err != nil {
		a.warn("tmp", "давление памяти не прочиталось, порог без поправки", "err", err)
		return 1
	}
	return cfg.Factor(p)
}

// loadUserState — состояние эскалации; нет файла или он битый — с нуля
// (битый — с записью в журнал: счётчики сводок начнутся заново).
func (a *app) loadUserState() *policy.UserState {
	st := &policy.UserState{}
	b, err := os.ReadFile(a.tmpStatePath())
	if errors.Is(err, os.ErrNotExist) {
		return st
	}
	if err == nil {
		err = json.Unmarshal(b, st)
	}
	if err != nil {
		a.warn("tmp", "состояние сброшено", "err", err)
		return &policy.UserState{}
	}
	return st
}

func (a *app) saveUserState(st *policy.UserState) error {
	b, err := json.MarshalIndent(st, "", " ")
	if err != nil {
		return fmt.Errorf("состояние tmp: %w", err)
	}
	return record.WriteFileAtomic(a.tmpStatePath(), b, 0o600)
}

func copyAlerts(m map[string]*policy.AlertState) map[string]*policy.AlertState {
	out := make(map[string]*policy.AlertState, len(m))
	for k, v := range m {
		c := *v
		out[k] = &c
	}
	return out
}

const tmpSummaryRoot = "/tmp"

// tmpfilesOctalDigits — длина восьмеричного escape в systemd (\NNN).
const tmpfilesOctalDigits = 3

// errTmpfilesNUL — escape даёт NUL: systemd отвергает такую строку целиком, в любом поле.
var errTmpfilesNUL = errors.New("escape даёт NUL")

func tmpfilesSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

func tmpfilesFields(line string) ([]string, error) {
	var fields []string
	for i := 0; i < len(line) && len(fields) < 6; {
		for i < len(line) && tmpfilesSpace(line[i]) {
			i++
		}
		if i == len(line) {
			break
		}
		field, next, err := tmpfilesField(line, i)
		if err != nil {
			return nil, err
		}
		fields = append(fields, field)
		i = next
	}
	return fields, nil
}

func tmpfilesField(line string, i int) (string, int, error) {
	var field strings.Builder
	var quote byte
	for i < len(line) {
		c := line[i]
		if quote != 0 && c == quote {
			quote = 0
			i++
			continue
		}
		if quote == 0 && (c == '\'' || c == '"') {
			quote = c
			i++
			continue
		}
		if quote == 0 && tmpfilesSpace(c) {
			return field.String(), i, nil
		}
		if c == '\\' {
			decoded, next, err := tmpfilesEscape(line, i)
			if err != nil {
				return "", i, err
			}
			field.WriteString(decoded)
			i = next
			continue
		}
		field.WriteByte(c)
		i++
	}
	if quote != 0 {
		return "", i, fmt.Errorf("незакрытая кавычка")
	}
	return field.String(), i, nil
}

func tmpfilesEscape(line string, i int) (string, int, error) {
	if i+1 >= len(line) {
		return "", i, fmt.Errorf("неполная C escape-последовательность")
	}
	c := line[i+1]
	plain := map[byte]byte{'a': '\a', 'b': '\b', 'f': '\f', 'n': '\n', 'r': '\r', 't': '\t', 'v': '\v', '\\': '\\', '\'': '\'', '"': '"', 's': ' '}
	if value, ok := plain[c]; ok {
		return string(value), i + 2, nil
	}
	width, bits := 0, 0
	switch c {
	case 'x':
		width, bits = 2, 8
	case 'u':
		width, bits = 4, 16
	case 'U':
		width, bits = 8, 32
	case '0', '1', '2', '3', '4', '5', '6', '7':
		// systemd cunescape: ровно три восьмеричные цифры, не больше \377.
		if i+tmpfilesOctalDigits+1 > len(line) {
			return "", i, fmt.Errorf("неполная C escape-последовательность \\%c", c)
		}
		value, err := strconv.ParseUint(line[i+1:i+tmpfilesOctalDigits+1], 8, 8)
		if err != nil {
			return "", i, fmt.Errorf("неверная C escape-последовательность \\%c", c)
		}
		if value == 0 {
			return "", i, errTmpfilesNUL
		}
		return string([]byte{byte(value)}), i + tmpfilesOctalDigits + 1, nil
	default:
		return "", i, fmt.Errorf("неизвестная C escape-последовательность \\%c", c)
	}
	if i+2+width > len(line) {
		return "", i, fmt.Errorf("неполная C escape-последовательность \\%c", c)
	}
	value, err := strconv.ParseUint(line[i+2:i+2+width], 16, bits)
	if err != nil {
		return "", i, fmt.Errorf("неверная C escape-последовательность \\%c", c)
	}
	if value == 0 {
		return "", i, errTmpfilesNUL
	}
	if bits == 8 {
		return string([]byte{byte(value)}), i + 2 + width, nil
	}
	if !utf8.ValidRune(rune(value)) {
		return "", i, fmt.Errorf("недопустимый Unicode escape")
	}
	return string(rune(value)), i + 2 + width, nil
}

// tmpfilesDirectory разбирает тип и путь правила, возвращая причину пропуска.
func tmpfilesDirectory(line string) (path, reason string, err error) {
	fields, err := tmpfilesFields(line)
	if err != nil {
		return "", "", err
	}
	if len(fields) < 2 {
		return "", "не хватает полей", nil
	}
	typeAndMods := fields[0]
	if typeAndMods == "" {
		return "", "пустой тип правила", nil
	}
	switch typeAndMods[0] {
	case 'd', 'D', 'v', 'q', 'Q':
	default:
		return "", "тип строки не создаёт каталог", nil
	}
	for _, mod := range typeAndMods[1:] {
		if !strings.ContainsRune("!-=~^+$", mod) {
			return "", "неизвестный модификатор", nil
		}
	}
	path = fields[1]
	if path == "" {
		return "", "пустой путь", nil
	}
	if strings.Contains(path, "%") {
		return "", "спецификатор %", nil
	}
	if !filepath.IsAbs(path) {
		return "", "путь не абсолютный", nil
	}
	path = filepath.Clean(path)
	rel, err := filepath.Rel(tmpSummaryRoot, path)
	if err != nil {
		return "", "", fmt.Errorf("путь %q: проверить корень сводки tmp: %w", path, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "путь вне корня сводки tmp", nil
	}
	if rel == "." { // d /tmp — сама папка сводки: исключить её значило бы молча выключить всю сводку
		return "", "корень сводки tmp", nil
	}
	return path, "", nil
}

// tmpConfig добавляет user-tmpfiles.d к обычным правилам ignore для всех потребителей [tmp].
func (a *app) tmpConfig(cfg *policy.Config) *policy.Config {
	updated := *cfg
	updated.Ignore = append([]string(nil), cfg.Ignore...)
	dir := cfg.TmpfilesDir
	if dir == "" {
		return &updated
	}
	patterns, err := filepath.Glob(filepath.Join(dir, "*.conf"))
	if err != nil {
		a.log.Warn("aguard tmp: список user-tmpfiles.d", "err", err)
		return &updated
	}
	for _, name := range patterns {
		f, err := os.Open(name)
		if err != nil {
			a.log.Warn("aguard tmp: файл user-tmpfiles.d не прочитан", "path", name, "err", err)
			continue
		}
		scanner := bufio.NewScanner(f)
		lineNo := 0
		for scanner.Scan() {
			lineNo++
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			path, reason, parseErr := tmpfilesDirectory(line)
			if parseErr != nil {
				a.log.Debug("aguard tmp: строка user-tmpfiles.d пропущена", "path", name, "line", lineNo, "reason", parseErr.Error(), "text", line)
				continue
			}
			if reason != "" {
				a.log.Debug("aguard tmp: строка user-tmpfiles.d пропущена", "path", name, "line", lineNo, "reason", reason, "text", line)
				continue
			}
			updated.Ignore = append(updated.Ignore, path, filepath.Join(path, "**"))
		}
		if err := scanner.Err(); err != nil {
			a.log.Warn("aguard tmp: файл user-tmpfiles.d прочитан не полностью", "path", name, "err", err)
		}
		if err := f.Close(); err != nil {
			a.log.Warn("aguard tmp: файл user-tmpfiles.d не закрыт", "path", name, "err", err)
		}
	}
	return &updated
}

// tmpRound — один проход: учёт, план, доставка (или запись в observe), кеши.
func (a *app) tmpRound(cfg *policy.Config, dry bool) error {
	cfg = a.tmpConfig(cfg)
	now := time.Now()
	snap, err := a.snapshot(cfg)
	if err != nil {
		return err
	}
	if age := now.Sub(snap.Updated); age > 3*cfg.Interval.D() {
		a.warn("tmp", "снимок трекера старый: есть ли строки «слежение: простой» в журнале, жив ли aguard-track-helper?", "age", policy.HumanAge(age))
	}
	live, err := a.live()
	if err != nil {
		return err
	}
	st := a.loadUserState()
	if st.Mode != cfg.Mode { // счётчики observe не должны сразу эскалировать в alert
		st = &policy.UserState{Mode: cfg.Mode}
	}
	prev := copyAlerts(st.Alerts)
	items := cfg.Items(snap.Entries, a.uid)
	if err := policy.ResolveWorkerJobs(items, live); err != nil {
		a.warn("tmp", "часть заданий worker не прочиталась, записи сгруппированы прежним способом", "err", err)
		a.note("tmp", "worker_lookup_failed", "", "", 1, "errors")
	}
	factor := a.tmpFactor(cfg)
	groups := cfg.Over(now, items, factor)
	sums, deferred := cfg.Plan(now, groups, live, st, factor)
	st.Forget(items)
	a.log.Info(fmt.Sprintf("aguard tmp %s: %s, сводок %d (записей %d), отложено записей %d",
		cfg.Mode, policy.OverText(items, groups, factor), len(sums), policy.Count(sums), policy.Count(deferred)))

	if dry {
		a.printDry(cfg, groups, sums, deferred, now)
		a.caches(cfg, items, true)
		return nil
	}
	var texts []byte
	for i := range sums {
		s := &sums[i]
		msg := cfg.Message(s, now, false)
		total := policy.LinesBytes(s.Lines)
		texts = fmt.Appendf(texts, "=== %s %s, записей %d\n%s\n\n", now.Format(time.RFC3339), s.Target.Key(), len(s.Lines), msg)
		if cfg.Mode != policy.ModeAlert {
			a.log.Info("aguard tmp observe: отправил бы", "to", s.Target.Key(), "entries", len(s.Lines), "bytes", policy.HumanBytes(total))
			a.note("tmp", "would_send", "", s.Target.Key(), int64(len(s.Lines)), "entries")
			continue
		}
		if err := a.tmpSend(cfg, s.Target, "tmp.summary", msg); err != nil {
			quiet := a.quietFailure("tmp", err)
			if !quiet {
				a.log.Warn("aguard tmp: сводка не доставлена", "to", s.Target.Key(), "err", err)
				a.note("tmp", "send_failed", "", s.Target.Key(), int64(len(s.Lines)), "entries")
			}
			restoreTmpAlerts(st, s.Lines, prev)
			continue
		}
		a.log.Info("aguard tmp: сводка", "to", s.Target.Key(), "entries", len(s.Lines), "bytes", policy.HumanBytes(total))
		a.note("tmp", "sent", "", s.Target.Key(), int64(len(s.Lines)), "entries")
	}
	if len(texts) > 0 {
		if err := os.WriteFile(filepath.Join(a.dir("tmp"), "last-summaries.txt"), texts, 0o600); err != nil {
			a.warn("tmp", "последние сводки", "err", err)
		}
	}
	a.caches(cfg, items, false)
	return a.saveUserState(st)
}

func restoreTmpAlerts(st *policy.UserState, lines []policy.Line, prev map[string]*policy.AlertState) {
	for i := range lines {
		k := policy.EntryKey(&lines[i].Item)
		if p, ok := prev[k]; ok {
			st.Alerts[k] = p
		} else {
			delete(st.Alerts, k)
		}
	}
}

// caches — общие кеши: при превышении размера чистим старое содержимое
// (только в alert и только своё; в observe и dry-run — запись в журнал).
func (a *app) caches(cfg *policy.Config, items []policy.Item, dry bool) {
	for i := range items {
		it := &items[i]
		if it.Class != policy.ClassCache || it.Entry.Bytes <= int64(cfg.Cache.Max) {
			continue
		}
		if dry || cfg.Mode != policy.ModeAlert || !cfg.Cache.Clean {
			a.log.Info(fmt.Sprintf("aguard tmp: кеш %s %s > %s — почистил бы файлы старше %s", it.Entry.Path,
				policy.HumanBytes(it.Entry.Bytes), policy.HumanBytes(int64(cfg.Cache.Max)), cfg.Cache.CleanOlder.D()))
			if !dry {
				a.note("tmp", "would_clean", it.Entry.Path, "", it.Entry.Bytes, "bytes")
			}
			continue
		}
		c := cleanOld(it.Entry.Path, time.Now().Add(-cfg.Cache.CleanOlder.D()), a.uid)
		a.log.Info("aguard tmp: кеш почищен", "path", it.Entry.Path, "files", c.files, "bytes", policy.HumanBytes(c.freed))
		a.note("tmp", "cleaned", it.Entry.Path, "", c.freed, "bytes")
		if c.failed > 0 {
			a.warn("tmp", "часть кеша не почищена", "path", it.Entry.Path, "failed", c.failed, "first", c.first)
		}
	}
}

// cleaned — итог чистки кеша: сколько файлов снято и места освободилось, сколько не вышло и первая ошибка.
type cleaned struct {
	files  int
	freed  int64
	failed int
	first  error
}

func (c *cleaned) fail(err error) {
	if c.first == nil {
		c.first = err
	}
	c.failed++
}

// cleanOld удаляет свои файлы, к которым не обращались и которые не меняли
// с cutoff, затем опустевшие папки (кроме самой папки кеша). Файлы, ушедшие
// во время обхода, ошибкой не считаются: кеш живёт своей жизнью.
func cleanOld(root string, cutoff time.Time, uid int) cleaned {
	var c cleaned
	var dirs []string
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				c.fail(err)
			}
			return nil // обход продолжается: одна нечитаемая папка не мешает остальным
		}
		if d.IsDir() {
			if p != root {
				dirs = append(dirs, p)
			}
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				c.fail(err)
			}
			return nil
		}
		at, ok := statTimes(fi)
		if !ok || at.uid != uid || at.mtime.After(cutoff) || at.atime.After(cutoff) {
			return nil
		}
		if err := os.Remove(p); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				c.fail(err)
			}
			return nil
		}
		c.files++
		c.freed += at.blocks * 512
		return nil
	})
	if walkErr != nil {
		c.fail(walkErr)
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		// удаляется только пустая папка; непустая или уже ушедшая — не ошибка
		err := os.Remove(dirs[i])
		if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) {
			c.fail(err)
		}
	}
	return c
}

func (a *app) tmpReport(cfg *policy.Config, since time.Duration) error {
	cfg = a.tmpConfig(cfg)
	snap, err := a.snapshot(cfg)
	if err != nil {
		return err
	}
	var events []record.Event
	bad, err := record.ReadEventFiles(cfg.TrackDir, func(e record.Event) { events = append(events, e) })
	if err != nil {
		return err
	}
	now := time.Now()
	from := time.Time{}
	if since > 0 {
		from = now.Add(-since)
	}
	d := report.Collect(cfg, events, snap, from, a.uid, now)
	d.BadLines = bad
	if err := report.Print(a.out, d); err != nil {
		return err
	}
	live, err := a.live()
	if err != nil {
		return err
	}
	factor := a.tmpFactor(cfg)
	items := cfg.Items(snap.Entries, a.uid)
	groups := cfg.Over(now, items, factor)
	fmt.Fprintf(a.out, "\n5. Сейчас (режим %s): %s.\n", cfg.Mode, policy.OverText(items, groups, factor))
	st := a.loadUserState() // только читаем: report счётчики не меняет
	if st.Mode != cfg.Mode {
		st = &policy.UserState{Mode: cfg.Mode}
	}
	sums, deferred := cfg.Plan(now, groups, live, st, factor)
	for i := range sums {
		s := &sums[i]
		fmt.Fprintf(a.out, "   ближайшая сводка %s: записей %d, %s\n", s.Target.Key(), len(s.Lines), policy.HumanBytes(policy.LinesBytes(s.Lines)))
	}
	for i := range deferred {
		s := &deferred[i]
		fmt.Fprintf(a.out, "   отложено для %s до %s: записей %d, %s\n", s.Target.Key(), s.NextAt.Format("15:04"), len(s.Lines), policy.HumanBytes(policy.LinesBytes(s.Lines)))
	}
	return nil
}

func (a *app) tmpSendTest(cfg *policy.Config, to string, dry bool) error {
	cfg = a.tmpConfig(cfg)
	if to == "" {
		to = cfg.Lead
	}
	live, err := a.live()
	if err != nil {
		return err
	}
	target, ok := live.ByName(to)
	if !ok {
		return fmt.Errorf("живой сессии %q нет", to)
	}
	snap, err := a.snapshot(cfg)
	if err != nil {
		return err
	}
	now := time.Now()
	factor := a.tmpFactor(cfg)
	st := &policy.UserState{}
	var sum *policy.Summary
	planned, _ := cfg.Plan(now, cfg.Over(now, cfg.Items(snap.Entries, a.uid), factor), live, st, factor)
	for i := range planned {
		if planned[i].Target.Key() == target.Key() {
			sum = &planned[i]
		}
	}
	if sum == nil { // адресату сейчас нечего слать — покажем формат на любых записях сверх порога
		sum = &policy.Summary{Target: target, Factor: factor, Pressure: factor < 1}
		for _, g := range cfg.Over(now, cfg.Items(snap.Entries, a.uid), factor) {
			for i := range g.Items {
				sum.Lines = append(sum.Lines, testLine(&g.Items[i]))
			}
		}
	}
	sort.SliceStable(sum.Lines, func(i, j int) bool {
		if sum.Lines[i].Rel != sum.Lines[j].Rel {
			return sum.Lines[i].Rel < sum.Lines[j].Rel
		}
		return sum.Lines[i].Item.GBMin(now) > sum.Lines[j].Item.GBMin(now)
	})
	if len(sum.Lines) > 5 {
		sum.Lines = sum.Lines[:5]
	}
	msg := cfg.Message(sum, now, true)
	fmt.Fprintln(a.out, msg)
	if dry {
		return nil
	}
	if err := a.tmpSend(cfg, target, "tmp.test", msg); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "\nотправлено в %s (%s)\n", target.Key(), target.Socket)
	return nil
}

// testLine — строка тестовой сводки для записи, адресованной не этой сессии.
func testLine(it *policy.Item) policy.Line {
	l := policy.Line{Item: *it, Rel: policy.RelNoCreator}
	switch it.Class {
	case policy.ClassSession, policy.ClassFighter:
		l.Rel, l.CreatorName = policy.RelIgnored, it.Entry.Owner.Session.Name
	case policy.ClassNonAgent:
		l.Rel = policy.RelNonAgent
	case policy.ClassOtherUID:
		l.Rel = policy.RelOtherUID
	}
	return l
}

// tmpSend доставляет текст в сокет сессии; токен — только если включён send_auth.
func (a *app) tmpSend(cfg *policy.Config, t policy.Target, kind guide.Kind, msg string) error {
	return a.sendSocket(t.Pid, t.Key(), t.Socket, cfg.SendAuth, kind, msg)
}

// printDry печатает то, что ушло бы в этот проход, и отложенное: строки,
// которые верхняя ступень уже получала, и когда им будет повтор.
func (a *app) printDry(cfg *policy.Config, groups []policy.Group, sums, deferred []policy.Summary, now time.Time) {
	for i := range sums {
		fmt.Fprintf(a.out, "=== сводка %s (pid %d), записей %d\n%s\n\n", sums[i].Target.Key(), sums[i].Target.Pid,
			len(sums[i].Lines), cfg.Message(&sums[i], now, false))
	}
	for i := range deferred {
		fmt.Fprintf(a.out, "=== отложено: %s уже получал(а) эти записи, повтор не раньше %s (top_repeat), записей %d; текст повтора:\n%s\n\n",
			deferred[i].Target.Key(), deferred[i].NextAt.Format("15:04"), len(deferred[i].Lines), cfg.Message(&deferred[i], now, false))
	}
	switch {
	case len(groups) == 0:
		fmt.Fprintln(a.out, "сводок нет: никто не превысил порог")
	case len(sums) == 0 && len(deferred) == 0:
		fmt.Fprintln(a.out, "сводок нет: владельцы сверх порога есть, но ни их сессий, ни лида нет в живых")
	case len(sums) == 0:
		fmt.Fprintln(a.out, "в этот проход сводок нет: всё сверх порога отложено до повтора верхней ступени")
	}
}
