// Package prune implements the guarded cleanup of Claude scratchpad sessions.
package prune

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
	"golang.org/x/sys/unix"
)

const (
	ModeLog       = "log"
	ModeAct       = "act"
	DefaultWindow = "02:00-05:00"
)

var ErrWindowEnded = errors.New("night window ended")
var ErrYield = errors.New("machine yield failed")
var ErrMountBoundary = errors.New("session directory crosses a mount boundary")
var ErrLiveSession = errors.New("session became live before deletion")

var uuidRE = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var uuidInTextRE = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

type Config struct {
	Mode     string          `toml:"mode"`
	Age      policy.Duration `toml:"age"`
	Window   string          `toml:"-"`
	Root     string          `toml:"-"`
	Projects string          `toml:"-"`
	Home     string          `toml:"-"`
}
type Candidate struct {
	Path, ID, Reason string
	Bytes            int64
	Age              time.Duration
	Removed          bool
}
type Report struct {
	Items, Outside []Candidate
	Linked         int64
	Err            error
}
type Hooks struct {
	Now        func() time.Time
	Live       func(context.Context) (map[string]bool, error)
	Yield      func(context.Context) error
	SetIdle    func() (func() error, error)
	Tickets    func(context.Context) (map[string]bool, error)
	Measure    func(context.Context, string) (int64, time.Time, error)
	References func(context.Context, time.Time, func(context.Context) error) (*References, error)
}

// SetThreadIdle sets Linux ioprio on the current OS thread. LockOSThread is required
// because Go may otherwise move filesystem work to a thread with default priority.
func SetThreadIdle() (func() error, error) {
	runtime.LockOSThread()
	tid := unix.Gettid()
	const whoProcess = 1
	old, _, errno := unix.Syscall(unix.SYS_IOPRIO_GET, whoProcess, uintptr(tid), 0)
	if errno != 0 {
		runtime.UnlockOSThread()
		return nil, errno
	}
	_, _, errno = unix.Syscall(unix.SYS_IOPRIO_SET, whoProcess, uintptr(tid), uintptr(3<<13))
	if errno != 0 {
		runtime.UnlockOSThread()
		return nil, errno
	}
	return restoreIOPriority(func() error {
		_, _, restoreErr := unix.Syscall(unix.SYS_IOPRIO_SET, whoProcess, uintptr(tid), old)
		if restoreErr != 0 {
			return restoreErr
		}
		return nil
	}, runtime.UnlockOSThread), nil
}

// restoreIOPriority keeps a thread pinned if restoring its old priority fails.
// The locked goroutine exits after Run; the runtime then retires that OS thread.
func restoreIOPriority(set func() error, unlock func()) func() error {
	return func() error {
		if err := set(); err != nil {
			return fmt.Errorf("restore IO priority: %w", err)
		}
		unlock()
		return nil
	}
}

// InWindow uses a local, half-open wall-clock window and supports midnight crossing.
func InWindow(window string, now time.Time) bool {
	start, end, ok := windowBounds(window)
	if !ok || start == end {
		return false
	}
	n := now.In(time.Local)
	m := n.Hour()*60 + n.Minute()
	if start < end {
		return m >= start && m < end
	}
	return m >= start || m < end
}

// WindowKey identifies the local calendar date on which an overnight window began.
func WindowKey(window string, now time.Time) string {
	start, _, ok := windowBounds(window)
	if !ok {
		return ""
	}
	local := now.In(time.Local)
	if start > local.Hour()*60+local.Minute() {
		local = local.AddDate(0, 0, -1)
	}
	return local.Format("2006-01-02")
}

func windowBounds(window string) (int, int, bool) {
	if len(window) != 11 || window[5] != '-' {
		return 0, 0, false
	}
	a, ea := time.Parse("15:04", window[:5])
	b, eb := time.Parse("15:04", window[6:])
	if ea != nil || eb != nil {
		return 0, 0, false
	}
	return a.Hour()*60 + a.Minute(), b.Hour()*60 + b.Minute(), true
}

// Run performs only one bounded pass. It never considers entries outside Root/project/UUID.
func Run(ctx context.Context, c Config, h Hooks) Report {
	result := make(chan Report, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				result <- Report{Err: fmt.Errorf("prune panic: %v", recovered)}
			}
		}()
		result <- run(ctx, c, h)
	}()
	return <-result
}

// run owns a dedicated goroutine so a failed ioprio restore can leave its OS
// thread locked until this goroutine exits, without pinning the scheduler loop.
func run(ctx context.Context, c Config, h Hooks) (report Report) {
	now := time.Now()
	if h.Now != nil {
		now = h.Now()
	}
	if !InWindow(c.Window, now) {
		return report
	}
	if c.Mode != ModeLog && c.Mode != ModeAct {
		report.Err = fmt.Errorf("invalid mode %q", c.Mode)
		return report
	}
	pass := newScanPass(ctx, c, h, now)
	pass.yield = pass.guardedYield
	if h.SetIdle == nil {
		report.Err = errors.New("missing IO priority setter")
		return report
	}
	restore, err := h.SetIdle()
	if err != nil {
		report.Err = fmt.Errorf("set IO idle: %w", err)
		return report
	}
	defer func() {
		if restore != nil {
			if restoreErr := restore(); restoreErr != nil {
				report.Err = errors.Join(report.Err, restoreErr)
			}
		}
	}()
	if err := pass.loadLive(); err != nil {
		report.Err = err
		return report
	}
	if err := pass.loadReferences(now); err != nil {
		if !errors.Is(err, ErrWindowEnded) {
			report.Err = err
		}
		return report
	}
	if err := pass.scan(&report); err != nil && !errors.Is(err, ErrWindowEnded) {
		report.Err = err
	}
	for _, item := range report.Items {
		if strings.Contains(item.Reason, "ссылка") {
			report.Linked++
		}
	}
	return report
}

func (p *scanPass) loadLive() error {
	if p.hooks.Live == nil {
		return errors.New("missing liveness checker")
	}
	live, err := p.hooks.Live(p.ctx)
	if err != nil {
		return fmt.Errorf("live sessions: %w", err)
	}
	p.live = live
	return nil
}

func (p *scanPass) loadReferences(now time.Time) error {
	if p.hooks.References != nil {
		return p.loadIndexedReferences(now)
	}
	return p.loadBasicReferences(now)
}

func (p *scanPass) loadIndexedReferences(now time.Time) error {
	refs, err := p.hooks.References(p.ctx, now.Add(-p.cfg.Age.D()), p.yield)
	if err != nil {
		if errors.Is(err, ErrWindowEnded) {
			return ErrWindowEnded
		}
		return fmt.Errorf("references: %w", err)
	}
	if refs == nil {
		return errors.New("references: empty snapshot")
	}
	p.refs, p.links, p.transcripts = refs, refs.TicketIDs(), refs.TranscriptIDs()
	return nil
}

func (p *scanPass) loadBasicReferences(now time.Time) error {
	if p.hooks.Tickets == nil {
		return errors.New("missing ticket checker")
	}
	links, err := p.hooks.Tickets(p.ctx)
	if err != nil {
		return fmt.Errorf("open tickets: %w", err)
	}
	transcripts, err := freshTranscriptIDs(p.ctx, p.cfg.Projects, now.Add(-p.cfg.Age.D()), p.yield)
	if err != nil {
		if errors.Is(err, ErrWindowEnded) {
			return ErrWindowEnded
		}
		return fmt.Errorf("transcripts: %w", err)
	}
	p.links, p.transcripts = links, transcripts
	return nil
}

func newScanPass(ctx context.Context, c Config, h Hooks, now time.Time) scanPass {
	return scanPass{ctx: ctx, cfg: c, hooks: h, now: now, inside: func() bool {
		at := time.Now()
		if h.Now != nil {
			at = h.Now()
		}
		return InWindow(c.Window, at)
	}}
}

type scanPass struct {
	ctx                      context.Context
	cfg                      Config
	hooks                    Hooks
	now                      time.Time
	inside                   func() bool
	yield                    func(context.Context) error
	live, links, transcripts map[string]bool
	refs                     *References
}

func (p *scanPass) guardedYield(ctx context.Context) error {
	if !p.inside() {
		return ErrWindowEnded
	}
	if p.hooks.Yield != nil {
		if err := p.hooks.Yield(ctx); err != nil {
			return fmt.Errorf("%w: %w", ErrYield, err)
		}
	}
	if !p.inside() {
		return ErrWindowEnded
	}
	return nil
}

func (p *scanPass) scan(report *Report) error {
	info, err := os.Lstat(p.cfg.Root)
	if err != nil {
		return fmt.Errorf("claude-1000: lstat root: %w", err)
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return errors.New("claude-1000: root is not a real directory")
	}
	projects, err := os.ReadDir(p.cfg.Root)
	if err != nil {
		return fmt.Errorf("claude-1000: read root: %w", err)
	}
	for _, projectEntry := range projects {
		if err := p.guardedYield(p.ctx); err != nil {
			return err
		}
		if err := p.scanProject(projectEntry, report); err != nil {
			return err
		}
	}
	return p.scanOutside(report)
}

func (p *scanPass) scanProject(entry fs.DirEntry, report *Report) error {
	if !entry.IsDir() || entry.Type()&fs.ModeSymlink != 0 || !strings.HasPrefix(entry.Name(), "-") {
		return nil
	}
	project := filepath.Join(p.cfg.Root, entry.Name())
	info, err := os.Lstat(project)
	if err != nil || !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		if err == nil {
			err = errors.New("not a real directory")
		}
		report.Items = append(report.Items, Candidate{Path: project, Reason: "ошибка проверки проекта: " + err.Error()})
		return nil
	}
	entries, err := os.ReadDir(project)
	if err != nil {
		report.Items = append(report.Items, Candidate{Path: project, Reason: "ошибка чтения проекта: " + err.Error()})
		return nil
	}
	for _, entry := range entries {
		if err := p.guardedYield(p.ctx); err != nil {
			return err
		}
		if !entry.IsDir() || entry.Type()&fs.ModeSymlink != 0 || !uuidRE.MatchString(entry.Name()) {
			continue
		}
		item, err := p.inspectCandidate(project, entry)
		if item != nil {
			report.Items = append(report.Items, *item)
		}
		if errors.Is(err, ErrWindowEnded) {
			return ErrWindowEnded
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *scanPass) inspectCandidate(project string, entry fs.DirEntry) (*Candidate, error) {
	path := filepath.Join(project, entry.Name())
	item := &Candidate{Path: path, ID: entry.Name()}
	bytes, changed, err := p.measureCandidate(path)
	if err != nil {
		if errors.Is(err, ErrWindowEnded) {
			return nil, ErrWindowEnded
		}
		if errors.Is(err, ErrYield) {
			return nil, err
		}
		item.Reason = "ошибка обхода: " + err.Error()
		return item, nil
	}
	item.Bytes, item.Age = bytes, p.now.Sub(changed)
	item.Reason = eligibleReason(*item, p.cfg.Age.D(), p.live, p.transcripts, p.links)
	if item.Reason == "" && p.cfg.Mode == ModeLog {
		item.Reason = "удалил бы"
	}
	if item.Reason == "" && p.cfg.Mode == ModeAct {
		stop, err := p.revalidateAndAct(item, entry.Name(), filepath.Base(project))
		if err != nil {
			return nil, err
		}
		if stop {
			return item, ErrWindowEnded
		}
	}
	return item, nil
}

func (p *scanPass) measureCandidate(path string) (int64, time.Time, error) {
	return measureWith(p.hooks, path, p.ctx, p.yield)
}

func (p *scanPass) revalidateAndAct(item *Candidate, id, project string) (bool, error) {
	links, transcripts := p.links, p.transcripts
	var err error
	if p.refs != nil {
		if err := p.refs.Refresh(p.ctx, p.yield); err != nil {
			return false, fmt.Errorf("refresh reference snapshot: %w", err)
		}
		links, transcripts = p.refs.TicketIDs(), p.refs.TranscriptIDs()
	} else {
		links, err = p.hooks.Tickets(p.ctx)
		if err != nil {
			return false, fmt.Errorf("recheck ticket links: %w", err)
		}
	}
	bytes, changed, err := p.measureCandidate(item.Path)
	if err != nil {
		return p.handleCandidateError(item, err, "повторная проверка возраста: ")
	}
	item.Bytes, item.Age = bytes, p.now.Sub(changed)
	item.Reason = eligibleReason(*item, p.cfg.Age.D(), nil, transcripts, links)
	if item.Reason != "" {
		return false, nil
	}
	if !p.inside() {
		item.Reason = "ночное окно закончилось"
		return true, nil
	}
	err = removeSessionChecked(p.cfg.Root, project, id, p.yield, p.ctx, func() error {
		live, err := p.hooks.Live(p.ctx)
		if err != nil {
			return fmt.Errorf("recheck session liveness: %w", err)
		}
		if live[strings.ToLower(id)] || live[id] {
			return ErrLiveSession
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrLiveSession) {
			item.Reason = "живая сессия"
			return false, nil
		}
		return p.handleCandidateError(item, err, "ошибка удаления: ")
	}
	item.Removed, item.Reason = true, "удалил"
	return false, nil
}

func (p *scanPass) handleCandidateError(item *Candidate, err error, reason string) (bool, error) {
	if errors.Is(err, ErrWindowEnded) {
		item.Reason = "ночное окно закончилось"
		return true, nil
	}
	if errors.Is(err, ErrYield) {
		return false, err
	}
	if errors.Is(err, ErrMountBoundary) {
		item.Reason = "точка монтирования: " + err.Error()
		return false, nil
	}
	item.Reason = reason + err.Error()
	return false, nil
}

func (p *scanPass) scanOutside(report *Report) error {
	parent := filepath.Dir(p.cfg.Root)
	entries, err := os.ReadDir(parent)
	if err != nil {
		return fmt.Errorf("вне claude-1000: read parent: %w", err)
	}
	for _, entry := range entries {
		if !p.inside() {
			return ErrWindowEnded
		}
		if entry.Name() == filepath.Base(p.cfg.Root) {
			continue
		}
		path := filepath.Join(parent, entry.Name())
		bytes, changed, err := measure(path, p.yield, p.ctx)
		item := Candidate{Path: path, Bytes: bytes}
		if err != nil {
			if errors.Is(err, ErrWindowEnded) {
				return err
			}
			if errors.Is(err, ErrYield) {
				return err
			}
			item.Reason = "ошибка обхода: " + err.Error()
		} else {
			item.Age, item.Reason = p.now.Sub(changed), "вне claude-1000"
		}
		report.Outside = append(report.Outside, item)
	}
	return nil
}

func removeSession(root, project, id string, yield func(context.Context) error, ctx context.Context) (result error) {
	return removeSessionChecked(root, project, id, yield, ctx, nil)
}

func removeSessionChecked(root, project, id string, yield func(context.Context) error, ctx context.Context, beforeRemove func() error) (result error) {
	const dirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW
	rootFD, err := unix.Open(root, dirFlags, 0)
	if err != nil {
		return fmt.Errorf("open cleanup root %s: %w", root, err)
	}
	defer func() {
		if err := unix.Close(rootFD); err != nil {
			result = errors.Join(result, fmt.Errorf("close cleanup root %s: %w", root, err))
		}
	}()
	var rootStat unix.Stat_t
	if err := unix.Fstat(rootFD, &rootStat); err != nil {
		return fmt.Errorf("stat cleanup root %s: %w", root, err)
	}
	rootMountID, err := mountIDAt(rootFD, "", unix.AT_EMPTY_PATH)
	if err != nil {
		return fmt.Errorf("mount id cleanup root %s: %w", root, err)
	}
	projectFD, err := unix.Openat(rootFD, project, dirFlags, 0)
	if err != nil {
		return fmt.Errorf("open project %s/%s: %w", root, project, err)
	}
	defer func() {
		if err := unix.Close(projectFD); err != nil {
			result = errors.Join(result, fmt.Errorf("close project %s/%s: %w", root, project, err))
		}
	}()
	var projectStat unix.Stat_t
	if err := unix.Fstat(projectFD, &projectStat); err != nil {
		return fmt.Errorf("stat project %s/%s: %w", root, project, err)
	}
	if crossesMount(rootStat.Dev, projectStat.Dev) {
		return fmt.Errorf("%w: project %s", ErrMountBoundary, project)
	}
	projectMountID, err := mountIDAt(projectFD, "", unix.AT_EMPTY_PATH)
	if err != nil {
		return fmt.Errorf("mount id project %s: %w", project, err)
	}
	if projectMountID != rootMountID {
		return fmt.Errorf("%w: project %s", ErrMountBoundary, project)
	}
	if err := preflightDirAt(projectFD, id, rootStat.Dev, rootMountID, yield, ctx); err != nil {
		return err
	}
	if beforeRemove != nil {
		if err := beforeRemove(); err != nil {
			return err
		}
	}
	return removeDirAt(projectFD, id, rootStat.Dev, rootMountID, yield, ctx)
}

func crossesMount(rootDev, entryDev uint64) bool { return rootDev != entryDev }

func preflightDirAt(parentFD int, name string, rootDev, rootMountID uint64, yield func(context.Context) error, ctx context.Context) (retErr error) {
	return preflightDirAtWithStat(parentFD, name, rootDev, rootMountID, yield, ctx, unix.Fstatat)
}

func preflightDirAtWithStat(parentFD int, name string, rootDev, rootMountID uint64, yield func(context.Context) error, ctx context.Context, statEntry func(int, string, *unix.Stat_t, int) error) (retErr error) {
	if err := yield(ctx); err != nil {
		return err
	}
	const dirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW
	fd, err := unix.Openat(parentFD, name, dirFlags, 0)
	if err != nil {
		return fmt.Errorf("open for mount preflight %s: %w", name, err)
	}
	dir := os.NewFile(uintptr(fd), name)
	defer func() { retErr = errors.Join(retErr, dir.Close()) }()
	if err := verifyDirectoryMount(fd, name, rootDev, rootMountID); err != nil {
		return err
	}
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return fmt.Errorf("read for mount preflight %s: %w", name, err)
	}
	for _, entry := range entries {
		if err := yield(ctx); err != nil {
			return err
		}
		var stat unix.Stat_t
		if err := statEntry(fd, entry.Name(), &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("stat for mount preflight %s/%s: %w", name, entry.Name(), err)
		}
		if crossesMount(rootDev, stat.Dev) {
			return fmt.Errorf("%w: %s/%s", ErrMountBoundary, name, entry.Name())
		}
		if err := verifyMountBoundaryAt(fd, entry.Name(), unix.AT_SYMLINK_NOFOLLOW, filepath.Join(name, entry.Name()), rootMountID); err != nil {
			return err
		}
		if stat.Mode&unix.S_IFMT == unix.S_IFDIR {
			if err := preflightDirAtWithStat(fd, entry.Name(), rootDev, rootMountID, yield, ctx, statEntry); err != nil {
				return err
			}
		}
	}
	return nil
}

func removeDirAt(parentFD int, name string, rootDev, rootMountID uint64, yield func(context.Context) error, ctx context.Context) error {
	if err := yield(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	const dirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW
	fd, err := unix.Openat(parentFD, name, dirFlags, 0)
	if err != nil {
		return fmt.Errorf("open session entry %s: %w", name, err)
	}
	dir := os.NewFile(uintptr(fd), name)
	if err := verifyDirectoryMount(fd, name, rootDev, rootMountID); err != nil {
		return closeDirectoryAfterError(dir, err)
	}
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return closeDirectoryAfterError(dir, fmt.Errorf("read session directory %s: %w", name, err))
	}
	for _, entry := range entries {
		if err := yield(ctx); err != nil {
			return closeDirectoryAfterError(dir, err)
		}
		if err := ctx.Err(); err != nil {
			return closeDirectoryAfterError(dir, err)
		}
		if err := removeSessionEntry(fd, name, entry.Name(), rootDev, rootMountID, yield, ctx); err != nil {
			return closeDirectoryAfterError(dir, err)
		}
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("close session directory %s: %w", name, err)
	}
	if err := yield(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR); err != nil {
		return fmt.Errorf("remove session directory %s: %w", name, err)
	}
	return nil
}

func verifyDirectoryMount(fd int, name string, rootDev, rootMountID uint64) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("stat directory %s: %w", name, err)
	}
	if crossesMount(rootDev, stat.Dev) {
		return fmt.Errorf("%w: %s", ErrMountBoundary, name)
	}
	return verifyMountBoundaryAt(fd, "", unix.AT_EMPTY_PATH, name, rootMountID)
}

func removeSessionEntry(fd int, parentName, entryName string, rootDev, rootMountID uint64, yield func(context.Context) error, ctx context.Context) error {
	return removeSessionEntryWithStat(fd, parentName, entryName, rootDev, rootMountID, yield, ctx, unix.Fstatat)
}

func removeSessionEntryWithStat(fd int, parentName, entryName string, rootDev, rootMountID uint64, yield func(context.Context) error, ctx context.Context, statEntry func(int, string, *unix.Stat_t, int) error) error {
	var stat unix.Stat_t
	if err := statEntry(fd, entryName, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("stat session entry %s/%s: %w", parentName, entryName, err)
	}
	if crossesMount(rootDev, stat.Dev) {
		return fmt.Errorf("%w: %s/%s", ErrMountBoundary, parentName, entryName)
	}
	if err := verifyMountBoundaryAt(fd, entryName, unix.AT_SYMLINK_NOFOLLOW, filepath.Join(parentName, entryName), rootMountID); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT == unix.S_IFDIR {
		return removeDirAt(fd, entryName, rootDev, rootMountID, yield, ctx)
	}
	if err := unix.Unlinkat(fd, entryName, 0); err != nil {
		return fmt.Errorf("unlink session entry %s/%s: %w", parentName, entryName, err)
	}
	return nil
}

func verifyMountBoundaryAt(dirfd int, path string, flags int, label string, rootMountID uint64) error {
	var err error
	mountID, err := mountIDAt(dirfd, path, flags)
	if err != nil {
		return fmt.Errorf("stat mount id %s: %w", label, err)
	}
	if mountID != rootMountID {
		return fmt.Errorf("%w: %s", ErrMountBoundary, label)
	}
	mountRoot, err := mountRootAt(dirfd, path, flags)
	if err != nil {
		return fmt.Errorf("stat mount-root attribute %s: %w", label, err)
	}
	if mountRoot {
		return fmt.Errorf("%w: %s", ErrMountBoundary, label)
	}
	return nil
}

func mountIDAt(dirfd int, path string, flags int) (uint64, error) {
	var stat unix.Statx_t
	if err := unix.Statx(dirfd, path, flags, unix.STATX_BASIC_STATS|unix.STATX_MNT_ID, &stat); err != nil {
		return 0, fmt.Errorf("statx %q: %w", path, err)
	}
	if stat.Mask&unix.STATX_MNT_ID == 0 {
		return 0, errors.New("statx did not provide mount id")
	}
	return stat.Mnt_id, nil
}

func mountRootAt(dirfd int, path string, flags int) (bool, error) {
	var stat unix.Statx_t
	if err := unix.Statx(dirfd, path, flags, unix.STATX_BASIC_STATS|unix.STATX_MNT_ID, &stat); err != nil {
		return false, fmt.Errorf("statx %q: %w", path, err)
	}
	if stat.Attributes_mask&unix.STATX_ATTR_MOUNT_ROOT == 0 {
		return false, nil
	}
	return stat.Attributes&unix.STATX_ATTR_MOUNT_ROOT != 0, nil
}

func closeDirectoryAfterError(dir *os.File, cause error) error {
	if err := dir.Close(); err != nil {
		return errors.Join(cause, fmt.Errorf("close session directory %s: %w", dir.Name(), err))
	}
	return cause
}

func measureWith(h Hooks, path string, ctx context.Context, yield func(context.Context) error) (int64, time.Time, error) {
	if h.Measure != nil {
		return h.Measure(ctx, path)
	}
	return measure(path, yield, ctx)
}

func eligibleReason(i Candidate, age time.Duration, live, transcripts, links map[string]bool) string {
	id := strings.ToLower(i.ID)
	if live[id] || live[i.ID] || transcripts[id] || transcripts[i.ID] {
		return "живая сессия"
	}
	if links[id] || links[i.ID] {
		return "ссылка из открытого тикета"
	}
	if i.Age < age {
		return "свежее порога"
	}
	return ""
}

func measure(root string, yield func(context.Context) error, ctx context.Context) (int64, time.Time, error) {
	var size int64
	var newest time.Time
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk %s: %w", path, err)
		}
		if e := ctx.Err(); e != nil {
			return e
		}
		if yield != nil {
			if e := yield(ctx); e != nil {
				return e
			}
		}
		info, e := d.Info()
		if e != nil {
			return fmt.Errorf("stat %s: %w", path, e)
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		if info.Mode().IsRegular() {
			size += info.Size()
		}
		return nil
	})
	if err != nil {
		return size, newest, fmt.Errorf("measure tree %s: %w", root, err)
	}
	return size, newest, nil
}
