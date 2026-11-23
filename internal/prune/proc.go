package prune

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type Inode struct {
	Dev     uint64
	Ino     uint64
	MountID uint64
}

type inodeKey struct {
	Dev uint64
	Ino uint64
}

var errMountIDUnknown = errors.New("mount ID is absent from mountinfo")

type procTargets struct {
	Inodes       map[inodeKey]struct{}
	MountDevices map[uint64]uint64
}

func statxInode(path string) (Inode, error) {
	var st unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_INO|unix.STATX_MNT_ID|unix.STATX_BASIC_STATS, &st); err != nil {
		return Inode{}, fmt.Errorf("statx %s: %w", path, err)
	}
	return Inode{Dev: unix.Mkdev(st.Dev_major, st.Dev_minor), Ino: st.Ino, MountID: st.Mnt_id}, nil
}

// O_PATH follows procfs magic links without invoking filesystem operations on
// the target. proc_cwd_link() in fs/proc/base.c returns the target path through
// nd_jump_link(), so lookup does not call getattr on a potentially stalled FUSE.
var openProcPath = func(path string) (int, error) {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open O_PATH %s: %w", path, err)
	}
	return fd, nil
}

var readProcDir = os.ReadDir
var readProcFile = os.ReadFile
var readProcLink = os.Readlink
var openMountInfo = os.Open
var readSelfFDInfo = func(fd int) ([]byte, error) {
	return readProcFile(filepath.Join("/proc/self/fdinfo", strconv.Itoa(fd)))
}

// openProcTarget returns only the inode and the descriptor's mount ID. The
// caller resolves that ID through the target process's mountinfo.
var openProcTarget = func(path string) (Inode, error) {
	fd, err := openProcPath(path)
	if err != nil {
		return Inode{}, err
	}
	defer unix.Close(fd)
	data, err := readSelfFDInfo(fd)
	if err != nil {
		return Inode{}, fmt.Errorf("read /proc/self/fdinfo/%d: %w", fd, err)
	}
	mountID, inode, err := parseFDInfo(data)
	if err != nil {
		return Inode{}, fmt.Errorf("parse /proc/self/fdinfo/%d: %w", fd, err)
	}
	return Inode{Ino: inode, MountID: mountID}, nil
}

func statChangeTime(info os.FileInfo) time.Time {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return info.ModTime()
	}
	return time.Unix(st.Ctim.Sec, st.Ctim.Nsec)
}

func collectProcTargets(ctx context.Context, procRoot, scopeRoot string, uid int, yield func(context.Context) error) (procTargets, error) {
	if procRoot == "" {
		procRoot = "/proc"
	}
	targets := procTargets{Inodes: map[inodeKey]struct{}{}, MountDevices: map[uint64]uint64{}}
	selfMountInfo := filepath.Join(procRoot, "self", "mountinfo")
	devices, err := readMountDevices(ctx, selfMountInfo, yield)
	if err != nil {
		return targets, err
	}
	targets.MountDevices = devices
	paths := map[string]struct{}{}
	procs, err := os.ReadDir(procRoot)
	if err != nil {
		return targets, fmt.Errorf("read %s: %w", procRoot, err)
	}
	for _, proc := range procs {
		_, err := strconv.Atoi(proc.Name())
		if err != nil || !proc.IsDir() {
			continue
		}
		if err := procYield(ctx, yield); err != nil {
			return targets, err
		}
		base := filepath.Join(procRoot, proc.Name())
		status, err := readProcFile(filepath.Join(base, "status"))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return targets, fmt.Errorf("read process status %s: %w", base, err)
		}
		processUID, err := statusUID(status)
		if err != nil {
			return targets, fmt.Errorf("read process uid %s: %w", base, err)
		}
		if processUID != uid {
			continue
		}
		if err := collectProcessTargets(ctx, base, scopeRoot, targets, paths, yield); err != nil {
			return targets, err
		}
	}
	if err := collectUnixSocketPaths(ctx, filepath.Join(procRoot, "net", "unix"), scopeRoot, paths, yield); err != nil {
		return targets, err
	}
	if err := statScopedPaths(ctx, paths, targets, yield); err != nil {
		return targets, err
	}
	return targets, nil
}

// processExited — процесс исчез или все его потоки (/proc/<pid>/task/*) в State Z (зомби) или X (умер).
// Смотрятся все потоки: главный поток после pthread_exit — зомби, а живые потоки держат общую таблицу файлов.
// Любая другая ошибка чтения возвращается: вызывающий оставляет сомнение и пишет её в причину.
func processExited(base string) (bool, error) {
	tasks, err := readProcDir(filepath.Join(base, "task"))
	if gone(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read threads %s: %w", base, err)
	}
	for _, t := range tasks {
		exited, err := taskExited(filepath.Join(base, "task", t.Name(), "status"))
		if gone(err) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("read thread %s/task/%s: %w", base, t.Name(), err)
		}
		if !exited {
			return false, nil
		}
	}
	return true, nil
}

// processSurelyExited — processExited в двух проходах подряд (гонка exec и нового потока, см. collectProcessTargets).
func processSurelyExited(base string) (bool, error) {
	for range 2 {
		if exited, err := processExited(base); err != nil || !exited {
			return false, err
		}
	}
	return true, nil
}

func gone(err error) bool { return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) }

func taskExited(statusPath string) (bool, error) {
	status, err := readProcFile(statusPath)
	if err != nil {
		return false, err
	}
	for line := range strings.SplitSeq(string(status), "\n") {
		if state, ok := strings.CutPrefix(line, "State:"); ok {
			state = strings.TrimSpace(state)
			return strings.HasPrefix(state, "Z") || strings.HasPrefix(state, "X"), nil
		}
	}
	return false, errors.New("state field is missing")
}

func statusUID(status []byte) (int, error) {
	for line := range strings.SplitSeq(string(status), "\n") {
		if !strings.HasPrefix(line, "Uid:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "Uid:"))
		if len(fields) != 4 {
			return 0, errors.New("malformed Uid field")
		}
		uid, err := strconv.Atoi(fields[0])
		if err != nil {
			return 0, fmt.Errorf("parse real uid: %w", err)
		}
		return uid, nil
	}
	return 0, errors.New("uid field is missing")
}

func collectProcessTargets(ctx context.Context, base, scopeRoot string, targets procTargets, paths map[string]struct{}, yield func(context.Context) error) error {
	processMountInfo := filepath.Join(base, "mountinfo")
	processDevices, err := readMountDevices(ctx, processMountInfo, yield)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if errors.Is(err, syscall.EINVAL) {
		exited, xerr := processSurelyExited(base)
		if xerr != nil {
			return fmt.Errorf("read process mountinfo %s: %w; %w", base, err, xerr)
		}
		if !exited {
			return fmt.Errorf("read process mountinfo %s: %w", base, err)
		}
		// Ядро отдаёт EINVAL на mountinfo (fs/proc_namespace.c, mounts_open_common), когда задачи нет или у главного
		// потока nsproxy уже NULL: в do_exit exit_files и exit_fs идут раньше exit_nsproxy_namespaces (v7.2).
		// Держать запись такому процессу нечем, если завершились все потоки. Проверка дважды: между чтением task/
		// и status поток может сделать exec (de_thread меняет tid с главным) или породить новый поток — второй
		// проход тогда видит живой поток.
		// Случай 23.11 02:16: один такой pid сделал «сомнением» все 20 созревших записей вне claude-1000.
		return nil
	}
	if isProcInvisible(err) {
		// cwd/root/exe resolution may still use the daemon's mountinfo. FD
		// entries require the process's own mountinfo and will be skipped below.
		processDevices = map[uint64]uint64{}
	} else if err != nil {
		return fmt.Errorf("read process mountinfo %s: %w", base, err)
	}
	for _, name := range []string{"cwd", "root", "exe"} {
		if err := addProcTarget(ctx, filepath.Join(base, name), targets, processDevices, targets.MountDevices, yield); err != nil {
			return err
		}
	}
	if err := collectFDTargets(ctx, base, scopeRoot, processDevices, targets.MountDevices, targets, yield); err != nil {
		return err
	}
	tasks := filepath.Join(base, "task")
	entries, err := os.ReadDir(tasks)
	if errors.Is(err, fs.ErrNotExist) || isProcInvisible(err) {
		return collectMapPaths(ctx, filepath.Join(base, "maps"), scopeRoot, paths, yield)
	}
	if err != nil {
		return fmt.Errorf("read process tasks %s: %w", tasks, err)
	}
	for _, task := range entries {
		if _, err := strconv.Atoi(task.Name()); err != nil || !task.IsDir() {
			continue
		}
		if err := addProcTarget(ctx, filepath.Join(tasks, task.Name(), "cwd"), targets, processDevices, targets.MountDevices, yield); err != nil {
			return err
		}
	}
	return collectMapPaths(ctx, filepath.Join(base, "maps"), scopeRoot, paths, yield)
}

func addProcTarget(ctx context.Context, procPath string, targets procTargets, processDevices, selfDevices map[uint64]uint64, yield func(context.Context) error) error {
	if err := procYield(ctx, yield); err != nil {
		return err
	}
	id, err := openProcTarget(procPath)
	if errors.Is(err, fs.ErrNotExist) || isProcInvisible(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open process target %s: %w", procPath, err)
	}
	dev, ok := processDevices[id.MountID]
	if !ok {
		dev, ok = selfDevices[id.MountID]
	}
	if !ok {
		return fmt.Errorf("mount ID %d for process target %s is absent from process and self mountinfo: %w", id.MountID, procPath, errMountIDUnknown)
	}
	targets.Inodes[inodeKey{Dev: dev, Ino: id.Ino}] = struct{}{}
	return nil
}

func collectFDTargets(ctx context.Context, base, scopeRoot string, processDevices, selfDevices map[uint64]uint64, targets procTargets, yield func(context.Context) error) error {
	fdDir := filepath.Join(base, "fd")
	entries, err := readProcDir(fdDir)
	if errors.Is(err, fs.ErrNotExist) || isProcInvisible(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read process links %s: %w", fdDir, err)
	}
	for _, entry := range entries {
		if err := procYield(ctx, yield); err != nil {
			return err
		}
		fdInfoPath := filepath.Join(base, "fdinfo", entry.Name())
		data, err := readProcFile(fdInfoPath)
		if errors.Is(err, fs.ErrNotExist) || isProcInvisible(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read process fdinfo %s: %w", fdInfoPath, err)
		}
		mountID, inode, err := parseFDInfo(data)
		if err != nil {
			return fmt.Errorf("parse process fdinfo %s: %w", fdInfoPath, err)
		}
		dev, ok := processDevices[mountID]
		if !ok {
			dev, ok = selfDevices[mountID]
		}
		if !ok {
			fdPath := filepath.Join(fdDir, entry.Name())
			// With no mountinfo mapping, only a readable path in the pruned tree
			// can be a target. An unreadable link has no path to compare.
			target, _ := readProcLink(fdPath) //nolint:ignored-error // an unreadable link has no scoped path to protect
			target = strings.TrimSuffix(target, " (deleted)")
			within, err := pathWithinRoot(scopeRoot, target)
			if err != nil {
				return fmt.Errorf("resolve process fd path %s: %w", fdPath, err)
			}
			if !within {
				continue
			}
			return fmt.Errorf("mount ID %d for process fd %s (%s) is absent from process and self mountinfo: %w", mountID, fdPath, target, errMountIDUnknown)
		}
		targets.Inodes[inodeKey{Dev: dev, Ino: inode}] = struct{}{}
	}
	return nil
}

func parseFDInfo(data []byte) (mountID uint64, inode uint64, retErr error) {
	var haveMount, haveInode bool
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		switch fields[0] {
		case "mnt_id:":
			parsed, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0, 0, fmt.Errorf("parse mnt_id: %w", err)
			}
			mountID, haveMount = parsed, true
		case "ino:":
			parsed, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0, 0, fmt.Errorf("parse ino: %w", err)
			}
			inode, haveInode = parsed, true
		}
	}
	if !haveMount || !haveInode {
		return 0, 0, errors.New("mnt_id or ino field is missing")
	}
	return mountID, inode, nil
}

func readMountDevices(ctx context.Context, path string, yield func(context.Context) error) (map[uint64]uint64, error) {
	f, err := openMountInfo(path)
	if err != nil {
		return nil, fmt.Errorf("open mountinfo %s: %w", path, err)
	}
	defer f.Close()
	devices := map[uint64]uint64{}
	s := bufio.NewScanner(f)
	for s.Scan() {
		if err := procYield(ctx, yield); err != nil {
			return nil, err
		}
		fields := strings.Fields(s.Text())
		if len(fields) < 3 {
			return nil, fmt.Errorf("malformed mountinfo line %q", s.Text())
		}
		mountID, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse mount ID %q: %w", fields[0], err)
		}
		majorText, minorText, ok := strings.Cut(fields[2], ":")
		if !ok {
			return nil, fmt.Errorf("parse mount device %q", fields[2])
		}
		major, err := strconv.ParseUint(majorText, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("parse mount device major %q: %w", majorText, err)
		}
		minor, err := strconv.ParseUint(minorText, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("parse mount device minor %q: %w", minorText, err)
		}
		devices[mountID] = unix.Mkdev(uint32(major), uint32(minor))
	}
	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("read mountinfo %s: %w", path, err)
	}
	return devices, nil
}

func collectMapPaths(ctx context.Context, mapsPath, scopeRoot string, paths map[string]struct{}, yield func(context.Context) error) error {
	f, err := os.Open(mapsPath)
	if errors.Is(err, fs.ErrNotExist) || isProcInvisible(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open process maps %s: %w", mapsPath, err)
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 4096), 1024*1024)
	for s.Scan() {
		if err := procYield(ctx, yield); err != nil {
			return err
		}
		fields := strings.Fields(s.Text())
		if len(fields) < 6 {
			continue
		}
		name := strings.Join(fields[5:], " ")
		if !strings.HasPrefix(name, "/") {
			continue
		}
		name = decodeProcPath(strings.TrimSuffix(name, " (deleted)"))
		within, err := pathWithinRoot(scopeRoot, name)
		if err != nil {
			return fmt.Errorf("resolve mapped path %s: %w", name, err)
		}
		if within {
			paths[name] = struct{}{}
		}
	}
	if err := s.Err(); err != nil {
		if isProcInvisible(err) {
			return nil
		}
		return fmt.Errorf("read process maps %s: %w", mapsPath, err)
	}
	return nil
}

func collectUnixSocketPaths(ctx context.Context, tablePath, scopeRoot string, paths map[string]struct{}, yield func(context.Context) error) error {
	f, err := os.Open(tablePath)
	if err != nil {
		return fmt.Errorf("open unix sockets %s: %w", tablePath, err)
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if err := procYield(ctx, yield); err != nil {
			return err
		}
		fields := strings.Fields(s.Text())
		if len(fields) < 8 || fields[0] == "Num" {
			continue
		}
		name := decodeProcPath(strings.TrimSuffix(strings.Join(fields[7:], " "), " (deleted)"))
		within, err := pathWithinRoot(scopeRoot, name)
		if err != nil {
			return fmt.Errorf("resolve unix socket path %s: %w", name, err)
		}
		if within {
			paths[name] = struct{}{}
		}
	}
	if err := s.Err(); err != nil {
		return fmt.Errorf("read unix sockets %s: %w", tablePath, err)
	}
	return nil
}

func statScopedPaths(ctx context.Context, paths map[string]struct{}, targets procTargets, yield func(context.Context) error) error {
	for path := range paths {
		if err := procYield(ctx, yield); err != nil {
			return err
		}
		id, err := openProcTarget(path)
		if errors.Is(err, fs.ErrNotExist) || isProcInvisible(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("open scoped proc path %s: %w", path, err)
		}
		dev, ok := targets.MountDevices[id.MountID]
		if !ok {
			return fmt.Errorf("mount ID %d for scoped proc path %s is absent from self mountinfo: %w", id.MountID, path, errMountIDUnknown)
		}
		targets.Inodes[inodeKey{Dev: dev, Ino: id.Ino}] = struct{}{}
	}
	return nil
}

func pathWithinRoot(root, path string) (bool, error) {
	if root == "" || !filepath.IsAbs(path) {
		return false, nil
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false, fmt.Errorf("resolve root %s: %w", root, err)
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return false, fmt.Errorf("resolve path %s: %w", path, err)
	}
	rel, err := filepath.Rel(filepath.Clean(rootAbs), filepath.Clean(pathAbs))
	if err != nil {
		return false, fmt.Errorf("relativize %s under %s: %w", pathAbs, rootAbs, err)
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)), nil
}

func isProcInvisible(err error) bool {
	return errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM)
}

func decodeProcPath(path string) string {
	r := strings.NewReplacer("\\040", " ", "\\011", "\t", "\\012", "\n", "\\134", "\\")
	return r.Replace(path)
}

func procYield(ctx context.Context, yield func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if yield != nil {
		return yield(ctx)
	}
	return nil
}
