package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/activity"
	"github.com/aiseeq/aguard/internal/cpuwatch"
	"github.com/aiseeq/aguard/internal/sigguard"
	"github.com/aiseeq/akit/session"
)

type sigguardRecord struct {
	Message  json.RawMessage `json:"MESSAGE"`
	Cursor   string          `json:"__CURSOR"`
	Realtime string          `json:"__REALTIME_TIMESTAMP"`
}

func (r sigguardRecord) message() string {
	var text string
	if json.Unmarshal(r.Message, &text) == nil {
		return text
	}
	var raw []byte
	if json.Unmarshal(r.Message, &raw) == nil {
		return string(raw)
	}
	return ""
}

type sigguardSource func(context.Context, string, bool) (io.ReadCloser, error)
type sigguardSender func(string, string) error

var errSigguardCursorSeek = errors.New("journalctl failed to seek to cursor")

type journalReader struct {
	io.ReadCloser
	cmd    *exec.Cmd
	stderr *bytes.Buffer
	wait   func() error
}

func (r *journalReader) Close() error {
	closeErr := r.ReadCloser.Close()
	var waitErr error
	if r.wait != nil {
		waitErr = r.wait()
	} else {
		waitErr = r.cmd.Wait()
	}
	if waitErr != nil && strings.Contains(r.stderr.String(), "Failed to seek to cursor") {
		waitErr = errors.Join(waitErr, fmt.Errorf("%w: %s", errSigguardCursorSeek, strings.TrimSpace(r.stderr.String())))
	} else if waitErr != nil && r.stderr.Len() > 0 {
		waitErr = fmt.Errorf("%w: %s", waitErr, strings.TrimSpace(r.stderr.String()))
	}
	return errors.Join(closeErr, waitErr)
}
func startJournal(ctx context.Context, args ...string) (io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, "journalctl", args...)
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("journalctl stdout: %w", err)
	}
	if err = cmd.Start(); err != nil {
		return nil, fmt.Errorf("запуск journalctl: %w", err)
	}
	return &journalReader{ReadCloser: stdout, cmd: cmd, stderr: stderr}, nil
}

func journalArgs(cursor string, initial bool) []string {
	args := []string{"-u", "agent-signal-guard-log", "-f", "-o", "json"}
	if cursor != "" {
		return append(args, "--after-cursor", cursor)
	}
	if initial {
		return append(args, "-n", "0")
	}
	return args
}

func sigguardTickEvery(window time.Duration) time.Duration {
	return max(time.Millisecond, min(100*time.Millisecond, window/10))
}

func sigguardPassDue(now, next time.Time) bool { return !now.Before(next) }

func (a *app) sigguardLoop(ctx context.Context) {
	source := a.sigSource
	if source == nil {
		source = func(ctx context.Context, cursor string, initial bool) (io.ReadCloser, error) {
			return startJournal(ctx, journalArgs(cursor, initial)...)
		}
	}
	delay := time.Second
	for ctx.Err() == nil {
		cfg, err := a.config()
		if err != nil {
			a.warn("sigguard", "конфиг", "err", err)
			if !a.pauseSigguard(ctx, delay) {
				return
			}
			delay = nextSigguardPause(delay)
			continue
		}
		err = a.sigguardReadStream(ctx, &cfg.Sigguard, source, func() { delay = time.Second })
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if resetSigguardCursor(a.sigguardCursorPath(), err) {
				a.warn("sigguard", "сброс неверного курсора", "err", err)
			}
			a.warn("sigguard", "журнал завершился", "err", err)
		}
		if !a.pauseSigguard(ctx, delay) {
			return
		}
		delay = nextSigguardPause(delay)
	}
}

func nextSigguardPause(d time.Duration) time.Duration {
	if d >= time.Minute/2 {
		return time.Minute
	}
	return d * 2
}

func resetSigguardCursor(path string, err error) bool {
	if !errors.Is(err, errSigguardCursorSeek) {
		return false
	}
	if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return false
	}
	return true
}

func (a *app) pauseSigguard(ctx context.Context, delay time.Duration) bool {
	if a.sigPause != nil {
		return a.sigPause(ctx, delay)
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (a *app) sigguardCursorPath() string { return filepath.Join(a.dir("sigguard"), "cursor") }
func (a *app) sigguardStream(ctx context.Context, cfg *sigguard.Config, source sigguardSource) (retErr error) {
	return a.sigguardReadStream(ctx, cfg, source, nil)
}

func (a *app) sigguardReadStream(ctx context.Context, cfg *sigguard.Config, source sigguardSource, onRead func()) (retErr error) {
	path := a.sigguardCursorPath()
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	cursor := strings.TrimSpace(string(raw))
	initial := cursor == ""
	r, err := source(ctx, cursor, initial)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, r.Close()) }()
	b := sigguard.NewBatcher(cfg.Window.D())
	timer := time.NewTicker(sigguardTickEvery(cfg.Window.D()))
	defer timer.Stop()
	passAt := time.Now()
	a.note("sigguard", activity.KindPass, "", "", 0, "")
	passAt = passAt.Add(10 * time.Minute)
	lines := make(chan string, 32)
	scanErr := make(chan error, 1)
	go func() {
		s := bufio.NewScanner(r)
		s.Buffer(make([]byte, 4096), 1024*1024)
		for s.Scan() {
			lines <- s.Text()
		}
		scanErr <- s.Err()
	}()
	for {
		select {
		case <-ctx.Done():
			a.sigguardDrainUntilDone(cfg, b, path, &cursor, lines, scanErr, onRead)
			a.flushSigguard(cfg, b)
			return ctx.Err()
		case <-timer.C:
			now := time.Now()
			if sigguardPassDue(now, passAt) {
				a.note("sigguard", activity.KindPass, "", "", 0, "")
				passAt = now.Add(10 * time.Minute)
			}
			for _, g := range b.Flush(now) {
				a.sigguardSend(cfg, g)
			}
		case e := <-scanErr:
			a.sigguardDrain(cfg, b, path, &cursor, lines, onRead)
			a.flushSigguard(cfg, b)
			if e == nil {
				return errors.New("journalctl stdout закрыт")
			}
			return e
		case line := <-lines:
			a.sigguardProcessLine(cfg, b, path, &cursor, line, onRead)
		}
	}
}

func (a *app) sigguardProcessLine(cfg *sigguard.Config, b *sigguard.Batcher, path string, cursor *string, line string, onRead func()) {
	if onRead != nil {
		onRead()
	}
	for _, g := range a.sigguardRecord(cfg, b, path, cursor, line) {
		a.sigguardSend(cfg, g)
	}
}

func (a *app) sigguardDrain(cfg *sigguard.Config, b *sigguard.Batcher, path string, cursor *string, lines <-chan string, onRead func()) {
	for _, line := range drainSigguardLines(lines) {
		a.sigguardProcessLine(cfg, b, path, cursor, line, onRead)
	}
}

func (a *app) sigguardDrainUntilDone(cfg *sigguard.Config, b *sigguard.Batcher, path string, cursor *string, lines <-chan string, done <-chan error, onRead func()) {
	for {
		select {
		case line := <-lines:
			a.sigguardProcessLine(cfg, b, path, cursor, line, onRead)
		case <-done:
			return
		}
	}
}

func (a *app) flushSigguard(cfg *sigguard.Config, b *sigguard.Batcher) {
	for _, g := range b.Flush(time.Now().Add(cfg.Window.D())) {
		a.sigguardSend(cfg, g)
	}
}

func drainSigguardLines(lines <-chan string) []string {
	var drained []string
	for {
		select {
		case line := <-lines:
			drained = append(drained, line)
		default:
			return drained
		}
	}
}

func (a *app) sigguardRecord(cfg *sigguard.Config, b *sigguard.Batcher, path string, cursor *string, line string) []sigguard.Group {
	var rec sigguardRecord
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		a.warn("sigguard", "строка журнала", "err", err)
		return nil
	}
	if rec.Cursor != "" {
		*cursor = rec.Cursor
		if err := writeSigguardCursor(path, *cursor); err != nil {
			a.warn("sigguard", "сохранение курсора", "err", err)
		}
	}
	message := rec.message()
	d, ok := sigguard.Parse(message)
	isDeny := strings.HasPrefix(message, "deny ") || strings.HasPrefix(message, "would-deny ")
	if !ok && !isDeny {
		return nil
	}
	at := time.Now()
	if us, e := strconv.ParseInt(rec.Realtime, 10, 64); e == nil {
		at = time.UnixMicro(us)
	}
	if time.Since(at) > cfg.Stale.D() {
		unit, count := "", int64(0)
		if ok {
			unit, count = d.Unit, int64(d.Count)
		}
		a.note("sigguard", "stale", unit, "", count, "signals")
		return nil
	}
	if !ok {
		a.warn("sigguard", "не разобрана строка отказа", "message", message)
		a.note("sigguard", "unparsed", "", cfg.Lead, 0, "signals")
		return b.AddUnparsed(message, time.Now())
	}
	return b.Add(d, time.Now())
}

func writeSigguardCursor(path, cursor string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("папка курсора: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".cursor-*")
	if err != nil {
		return fmt.Errorf("временный курсор: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return fmt.Errorf("права курсора: %w", err)
	}
	if _, err := fmt.Fprintln(f, cursor); err != nil {
		_ = f.Close()
		return fmt.Errorf("запись курсора: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("закрытие курсора: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("замена курсора: %w", err)
	}
	return nil
}

func (a *app) sigguardAlive() (map[string]session.Info, error) {
	if a.sigAlive != nil {
		return a.sigAlive()
	}
	return a.reg.Alive()
}

func (a *app) sigguardSend(cfg *sigguard.Config, g sigguard.Group) {
	if !g.Unparsed && len(g.Denies) == 0 {
		return
	}
	target := cfg.Lead
	workerID := ""
	alive, aliveErr := a.sigguardAlive()
	if aliveErr != nil {
		a.warn("sigguard", "реестр сессий прочитан не полностью", "err", aliveErr)
	}
	jobs := a.sigJobs
	if jobs == nil {
		jobs = &cpuwatch.Jobs{Dir: filepath.Join(a.home, ".local", "state", "worker")}
	}
	what := "unparsed"
	msg := sigguard.UnparsedMessage(g)
	if !g.Unparsed {
		d := g.Denies[0]
		target, workerID = sigguardRoute(d, cfg.Lead, alive, jobs)
		what = d.Unit
		msg = sigguard.Message(g, workerID)
	}
	if cfg.Mode != "alert" {
		a.log.Info("aguard sigguard: написал бы", "to", target, "msg", msg)
		a.note("sigguard", "would_send", what, target, int64(g.Count), "signals")
		return
	}
	var err error
	if a.sigSend != nil {
		err = a.sigSend(target, msg)
	} else {
		err = a.sendAlive(alive, target, cfg.SendAuth, msg)
	}
	if err != nil {
		if a.quietFailure("sigguard", err) {
			return
		}
		a.warn("sigguard", "письмо", "to", target, "err", err)
		a.note("sigguard", "send_failed", what, target, int64(g.Count), "signals")
		return
	}
	a.note("sigguard", "sent", what, target, int64(g.Count), "signals")
}

// sigguardRoute находит живую сессию или мастера задания бойца; иначе возвращает лида.
func sigguardRoute(d sigguard.Deny, lead string, alive map[string]session.Info, jobs *cpuwatch.Jobs) (string, string) {
	// unit бойца — worker-<id>.service (защита пишет имя целиком: NAME_LEN 128 с 11.11 18:58, до того 47 символов
	// резали его, и письмо уходило лиду); задания нет — ищем сессию по root, как у прочих unit
	if len(cpuwatch.JobIDs(d.Unit)) > 0 && !strings.HasSuffix(d.Unit, ".scope") && jobs != nil {
		if j, err := jobs.ByUnit(d.Unit); err == nil && j != nil {
			if _, live := alive[j.Master]; live {
				return j.Master, j.ID
			}
			return lead, j.ID
		}
	}
	if pid, e := strconv.Atoi(d.Root); e == nil {
		for _, name := range slices.Sorted(maps.Keys(alive)) {
			in := alive[name]
			if in.Pid == pid {
				return name, ""
			}
		}
	}
	return lead, ""
}
