package track

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/aiseeq/aguard/internal/record"
	"github.com/aiseeq/akit/proc"
	"github.com/aiseeq/akit/session"
)

const (
	argsMax    = 200
	chainDepth = 40
)

// Resolver выясняет создателя записи: процесс, цепочку предков и сессию Claude Code.
type Resolver struct {
	tombs   *Tombs
	homes   map[int]string
	cgPaths map[uint64]string
	cgSess  map[string]cgHit
	log     *slog.Logger
}

type cgHit struct {
	s  *record.Session
	at time.Time
}

func newResolver(t *Tombs, lg *slog.Logger) *Resolver {
	return &Resolver{tombs: t, homes: map[int]string{}, cgPaths: map[uint64]string{}, cgSess: map[string]cgHit{}, log: lg}
}

// procData — сведения о живом процессе из /proc.
type procData struct {
	comm, args, cwd string
	uid             int
}

// readProc читает comm, argv (обрезанный), cwd и uid процесса. Ошибка —
// процесс ушёл (или это поток ядра без cwd).
func readProc(pid int) (procData, error) {
	ps := "/proc/" + strconv.Itoa(pid)
	d := procData{uid: -1}
	comm, err := os.ReadFile(ps + "/comm")
	if err != nil {
		return d, fmt.Errorf("процесс %d: comm: %w", pid, err)
	}
	d.comm = strings.TrimSpace(string(comm))
	cmdline, err := os.ReadFile(ps + "/cmdline")
	if err != nil {
		return d, fmt.Errorf("процесс %d: cmdline: %w", pid, err)
	}
	d.args = truncate(strings.ReplaceAll(strings.TrimRight(string(cmdline), "\x00"), "\x00", " "), argsMax)
	if d.cwd, err = os.Readlink(ps + "/cwd"); err != nil {
		return d, fmt.Errorf("процесс %d: cwd: %w", pid, err)
	}
	fi, err := os.Stat(ps)
	if err != nil {
		return d, fmt.Errorf("процесс %d: %w", pid, err)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		d.uid = int(st.Uid)
	}
	return d, nil
}

// truncate режет строку до n байт, не разрывая символ UTF-8.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func readCgroup(pid int) (string, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup")
	if err != nil {
		return "", fmt.Errorf("cgroup процесса %d: %w", pid, err)
	}
	return strings.TrimSpace(strings.TrimPrefix(string(b), "0::")), nil
}

// Owner собирает сведения о создателе по pid и pidfd из события fanotify.
// pidfd закрывается здесь.
func (r *Resolver) Owner(pid, pidfd int) *record.Owner {
	if pidfd >= 0 {
		defer unix.Close(pidfd)
	}
	o := &record.Owner{Proc: record.Proc{Pid: pid, UID: -1, State: record.ProcUnknown}}
	if st, err := r.live(o, pid, pidfd); err == nil {
		if s := r.sessionAt(pid, st.Start, o.Proc.UID); s != nil { // запись создала сама сессия
			o.Session = s
			return o
		}
		o.Chain, o.Session = r.chain(st.Ppid)
	} else { // процесс уже ушёл (err объясняет, на чём споткнулись — не важно)
		o.Chain, o.Session = r.chain(r.dead(o, pid, pidfd))
	}
	if o.Session == nil {
		o.Session = r.byCgroup(o.Proc.Cgroup, o.Proc.UID)
	}
	return o
}

// live заполняет процесс из /proc, если он ещё жив. /proc верим, только если
// pidfd указывает на живой процесс: без pidfd pid мог достаться другому.
func (r *Resolver) live(o *record.Owner, pid, pidfd int) (proc.Stat, error) {
	if pidfd < 0 {
		return proc.Stat{}, errors.New("нет pidfd: процесс ушёл до чтения события")
	}
	if err := unix.PidfdSendSignal(pidfd, 0, nil, 0); err != nil {
		return proc.Stat{}, fmt.Errorf("pidfd процесса %d: %w", pid, err)
	}
	st, err := proc.ReadStat("/proc", pid)
	if err != nil {
		return st, fmt.Errorf("процесс %d: %w", pid, err)
	}
	d, err := readProc(pid)
	if err != nil { // умер между проверкой и чтением
		return st, err
	}
	cg, err := readCgroup(pid)
	if err != nil {
		return st, err
	}
	o.Proc = record.Proc{Pid: pid, Comm: d.comm, Args: d.args, Cwd: d.cwd, UID: d.uid, Cgroup: cg, State: record.ProcOK}
	return st, nil
}

// dead — создатель уже умер: данные из proc connector и PIDFD_GET_INFO.
// Возвращает ppid для цепочки или 0.
func (r *Resolver) dead(o *record.Owner, pid, pidfd int) int {
	ppid := r.fromTomb(o, pid)
	if pidfd < 0 {
		return ppid
	}
	inf, err := pidfdInfo(pidfd)
	if err != nil { //nolint:silent-error-handling // ядро старше 6.15 или не сохранило сведений о вышедшем процессе: остаётся то, что дал proc connector
		return ppid
	}
	if o.Proc.Cgroup == "" {
		o.Proc.Cgroup = r.cgPath(inf.cgroupid)
	}
	if ppid <= 1 && inf.ppid > 1 {
		ppid = int(inf.ppid)
	}
	if o.Proc.UID < 0 && inf.mask&pidfdInfoCreds != 0 {
		o.Proc.UID = int(inf.ruid)
	}
	return ppid
}

// fromTomb заполняет процесс из proc connector; возвращает ppid или 0.
func (r *Resolver) fromTomb(o *record.Owner, pid int) int {
	t, ok := r.tombs.lookup(pid)
	if !ok {
		time.Sleep(5 * time.Millisecond) // даём горутине connector дочитать exit
		t, ok = r.tombs.lookup(pid)
	}
	if !ok {
		return 0
	}
	o.Proc.Comm, o.Proc.Args, o.Proc.Cwd, o.Proc.UID = t.comm, t.args, t.cwd, t.uid
	o.Proc.State = record.ProcReaped
	return t.ppid
}

// chain поднимается по ppid до процесса из реестра сессий.
func (r *Resolver) chain(ppid int) ([]record.Link, *record.Session) {
	var links []record.Link
	for depth := 0; ppid > 1 && depth < chainDepth; depth++ {
		st, err := proc.ReadStat("/proc", ppid)
		if err != nil { //nolint:silent-error-handling // предок тоже умер: берём его из proc connector
			t, ok := r.tombs.lookup(ppid)
			if !ok {
				links = append(links, record.Link{Pid: ppid, Comm: "?"})
				break
			}
			links = append(links, record.Link{Pid: ppid, Comm: t.comm})
			ppid = t.ppid
			continue
		}
		links = append(links, record.Link{Pid: ppid, Comm: st.Comm})
		if s := r.sessionAt(ppid, st.Start, procUID(ppid)); s != nil {
			return links, s
		}
		ppid = st.Ppid
	}
	return links, nil
}

func procUID(pid int) int {
	fi, err := os.Stat("/proc/" + strconv.Itoa(pid))
	if err != nil { //nolint:silent-error-handling // процесс ушёл: uid неизвестен (-1), и sessionAt сессию по нему не ищет
		return -1
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid)
	}
	return -1
}

func (r *Resolver) home(uid int) string {
	if uid < 0 {
		return ""
	}
	if h, ok := r.homes[uid]; ok {
		return h
	}
	h := ""
	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil {
		h = u.HomeDir
	}
	r.homes[uid] = h
	return h
}

// sessionAt — сессия, если pid с этим временем старта есть в реестре его учётки.
func (r *Resolver) sessionAt(pid int, start string, uid int) *record.Session {
	h := r.home(uid)
	if h == "" {
		return nil
	}
	in, err := session.ForHome(h).Get(pid)
	if err != nil {
		r.log.Warn("tmpguard-track: сессия не проверена", "pid", pid, "err", err)
		return nil //nolint:log-and-return-zero // нечитаемая запись реестра для цепочки — то же, что «не сессия»: идём к следующему предку и к cgroup
	}
	if in == nil || in.ProcStart != start {
		return nil
	}
	return &record.Session{Pid: in.Pid, Name: in.Name, Cwd: in.Cwd, ID: in.SessionID, ProcStart: in.ProcStart, Via: "ppid"}
}

// byCgroup — запасной путь, когда цепочка оборвалась (процесс отцепился от
// родителя): единственная живая сессия в той же cgroup-scope (вкладка Konsole).
func (r *Resolver) byCgroup(cg string, uid int) *record.Session {
	if cg == "" || !strings.HasSuffix(cg, ".scope") || strings.HasSuffix(cg, "init.scope") {
		return nil
	}
	if h, ok := r.cgSess[cg]; ok && time.Since(h.at) < 30*time.Second {
		return h.s
	}
	var found *record.Session
	n := 0
	if home := r.home(uid); home != "" {
		reg := session.ForHome(home)
		all, err := reg.List()
		if err != nil { //nolint:fallback-return // List отдаёт прочитанную часть реестра вместе с ошибкой о нечитаемых записях
			r.log.Warn("tmpguard-track: реестр сессий, проверяю прочитанную часть", "err", err)
		}
		for _, in := range all {
			if !reg.IsAlive(in.Pid, in.ProcStart) {
				continue
			}
			c, err := readCgroup(in.Pid)
			if err != nil { //nolint:silent-error-handling // сессия завершилась между проверками: в счёт не идёт
				continue
			}
			if c != cg {
				continue
			}
			n++
			found = &record.Session{Pid: in.Pid, Name: in.Name, Cwd: in.Cwd, ID: in.SessionID, ProcStart: in.ProcStart, Via: "cgroup"}
		}
	}
	if n != 1 {
		found = nil
	}
	if len(r.cgSess) > 1000 {
		r.cgSess = map[string]cgHit{}
	}
	r.cgSess[cg] = cgHit{s: found, at: time.Now()}
	return found
}

// cgPath переводит id cgroup2 (номер inode её папки) в путь.
func (r *Resolver) cgPath(id uint64) string {
	if id == 0 {
		return ""
	}
	if p, ok := r.cgPaths[id]; ok {
		return p
	}
	found := ""
	err := filepath.WalkDir("/sys/fs/cgroup", func(path string, d os.DirEntry, err error) error {
		if err != nil { //nolint:silent-error-handling // cgroup удалили во время обхода: пропускаем её
			return filepath.SkipDir
		}
		if !d.IsDir() {
			return nil
		}
		var st unix.Stat_t
		if unix.Stat(path, &st) != nil {
			return nil
		}
		p := strings.TrimPrefix(path, "/sys/fs/cgroup")
		if p == "" {
			p = "/"
		}
		if len(r.cgPaths) < 50000 {
			r.cgPaths[st.Ino] = p
		}
		if st.Ino == id {
			found = p
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		r.log.Warn("tmpguard-track: обход /sys/fs/cgroup", "id", id, "err", err)
	}
	return found
}

const (
	pidfdInfoCreds    = 1 << 1
	pidfdInfoCgroupID = 1 << 2
	pidfdInfoExit     = 1 << 3
)

type pinfo struct {
	mask, cgroupid uint64
	ppid, ruid     uint32
}

// pidfdInfo — ioctl PIDFD_GET_INFO (Linux 6.13+, данные вышедшего процесса — 6.15+).
// Раскладка struct pidfd_info: mask@0, cgroupid@8, pid@16, tgid@20, ppid@24, ruid@28.
func pidfdInfo(pidfd int) (pinfo, error) {
	var buf [96]byte
	binary.LittleEndian.PutUint64(buf[0:], pidfdInfoCgroupID|pidfdInfoExit|pidfdInfoCreds)
	cmd := uintptr(3<<30 | uintptr(len(buf))<<16 | 0xFF<<8 | 11) // _IOWR(PIDFS_IOCTL_MAGIC, 11, ...)
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(pidfd), cmd, uintptr(unsafe.Pointer(&buf[0]))); e != 0 {
		return pinfo{}, e
	}
	return pinfo{
		mask:     binary.LittleEndian.Uint64(buf[0:]),
		cgroupid: binary.LittleEndian.Uint64(buf[8:]),
		ppid:     binary.LittleEndian.Uint32(buf[24:]),
		ruid:     binary.LittleEndian.Uint32(buf[28:]),
	}, nil
}
