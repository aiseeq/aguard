package orphans

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aiseeq/aguard/internal/sessions"
)

// clockTicks — USER_HZ: поле starttime в /proc/<pid>/stat идёт в этих тиках.
const clockTicks = 100

// Proc — процесс из /proc. Поля окружения и cwd читаются только у своих процессов.
type Proc struct {
	Pid       int
	Ppid      int
	UID       int
	Comm      string
	Args      string    // командная строка через пробел
	StartRaw  string    // поле 22 /proc/<pid>/stat как есть (сверка при снятии)
	Start     uint64    // оно же числом
	StartAt   time.Time // момент старта
	Cgroup    string    // путь cgroup v2 ("0::" отрезано)
	Cwd       string
	Session   int    // CLAUDE_PID
	SessionID string // CLAUDE_CODE_SESSION_ID
	Partial   string // свой процесс, но окружение или cgroup не прочитались (недампируемый): почему
}

// Snapshot — все процессы на момент Now.
type Snapshot struct {
	Now   time.Time
	Procs map[int]*Proc
}

// ReadSnapshot читает procRoot. Процессы, ушедшие во время чтения, пропускаются;
// прочие ошибки чтения отдельных процессов возвращаются вместе со снимком.
func ReadSnapshot(procRoot string, uid int, now time.Time) (*Snapshot, error) {
	boot, err := bootTime(procRoot)
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, fmt.Errorf("список процессов: %w", err)
	}
	s := &Snapshot{Now: now, Procs: map[int]*Proc{}}
	var errs []error
	for _, e := range ents {
		if !isPid(e.Name()) {
			continue // не процесс: /proc/self, /proc/meminfo и т. п.
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			return nil, fmt.Errorf("pid %q: %w", e.Name(), err)
		}
		p, err := readProc(procRoot, pid, uid, boot)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			continue // процесс завершился между списком и чтением
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		s.Procs[pid] = p
	}
	return s, errors.Join(errs...)
}

// isPid — имя папки /proc из одних цифр.
func isPid(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func bootTime(procRoot string) (time.Time, error) {
	b, err := os.ReadFile(filepath.Join(procRoot, "stat"))
	if err != nil {
		return time.Time{}, fmt.Errorf("время загрузки: %w", err)
	}
	for l := range strings.SplitSeq(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "btime "); ok {
			sec, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return time.Time{}, fmt.Errorf("btime: %w", err)
			}
			return time.Unix(sec, 0), nil
		}
	}
	return time.Time{}, errors.New("в /proc/stat нет btime")
}

func readProc(procRoot string, pid, uid int, boot time.Time) (*Proc, error) {
	dir := filepath.Join(procRoot, strconv.Itoa(pid))
	fi, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("pid %d: %w", pid, err)
	}
	st, err := sessions.ProcStat(procRoot, pid)
	if err != nil {
		return nil, fmt.Errorf("pid %d: %w", pid, err)
	}
	start, err := strconv.ParseUint(st.Start, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("pid %d: starttime: %w", pid, err)
	}
	p := &Proc{
		Pid: pid, Ppid: st.Ppid, UID: -1, Comm: st.Comm, StartRaw: st.Start, Start: start,
		StartAt: boot.Add(time.Duration(start) * time.Second / clockTicks),
	}
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		p.UID = int(sys.Uid)
	}
	if p.UID != uid {
		return p, nil // чужое окружение не прочитать, а снимаем только своё
	}
	if err := p.readOwn(dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			return nil, err
		}
		p.Partial = err.Error() // недампируемый процесс (EACCES): такое дерево не трогаем
	}
	return p, nil
}

// readOwn дочитывает окружение, командную строку, cgroup и cwd своего процесса.
// Поток ядра или зомби даёт пустые файлы — это не ошибка.
func (p *Proc) readOwn(dir string) error {
	env, err := os.ReadFile(filepath.Join(dir, "environ"))
	if err != nil {
		return fmt.Errorf("environ: %w", err)
	}
	for kv := range strings.SplitSeq(string(env), "\x00") {
		if v, ok := strings.CutPrefix(kv, "CLAUDE_PID="); ok {
			if p.Session, err = strconv.Atoi(v); err != nil {
				return fmt.Errorf("CLAUDE_PID=%q: %w", v, err)
			}
		} else if v, ok := strings.CutPrefix(kv, "CLAUDE_CODE_SESSION_ID="); ok {
			p.SessionID = v
		}
	}
	cmd, err := os.ReadFile(filepath.Join(dir, "cmdline"))
	if err != nil {
		return fmt.Errorf("cmdline: %w", err)
	}
	p.Args = strings.TrimSpace(strings.ReplaceAll(string(cmd), "\x00", " "))
	cg, err := os.ReadFile(filepath.Join(dir, "cgroup"))
	if err != nil {
		return fmt.Errorf("cgroup: %w", err)
	}
	p.Cgroup = parseCgroup(string(cg))
	if p.Cwd, err = os.Readlink(filepath.Join(dir, "cwd")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("cwd: %w", err)
	}
	return nil
}

// parseCgroup — путь из строки cgroup v2 "0::/path".
func parseCgroup(s string) string {
	for l := range strings.SplitSeq(s, "\n") {
		if v, ok := strings.CutPrefix(l, "0::"); ok {
			return v
		}
	}
	return ""
}

// serviceUnit — systemd-сервис, под которым идёт процесс (компонент пути
// cgroup с .service, кроме самого user@<uid>.service), или "".
func serviceUnit(cgroup string) string {
	for c := range strings.SplitSeq(cgroup, "/") {
		if strings.HasSuffix(c, ".service") && !strings.HasPrefix(c, "user@") {
			return c
		}
	}
	return ""
}
