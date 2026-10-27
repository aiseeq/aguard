package cpuwatch

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/akit/proc"
)

// Виды владельца нагрузки.
const (
	KindSession = "сессия" // сама сессия и её дети
	KindWorker  = "боец"   // боец worker: unit worker-* на ноуте, scope worker-* на PC
	KindFarm    = "ферма"  // ферма llmark: scope farm-*
	KindSystem  = "система"
	KindOther   = "прочее"
)

// farmMaster — хозяин фермы llmark (`~/git/llmark/tool/cmd/farm`).
const farmMaster = "llmark-master"

// Owner — чья нагрузка. Master пуст — писать некому (система, рабочий стол, неизвестный unit), только сводка лиду.
type Owner struct {
	Master string
	Kind   string
	Label  string // задание бойца, unit, процесс
}

// Job — задание worker (~/.local/state/worker/<id>/job.json).
type Job struct {
	ID      string    `json:"id"`
	Dir     string    `json:"dir"`
	Task    string    `json:"task"`
	Master  string    `json:"master"`
	Created time.Time `json:"created"`
}

// Jobs — задания worker по папке состояния; читаются по мере надобности.
type Jobs struct {
	Dir  string
	byID map[string]*Job // nil — задания нет
	all  []*Job          // все задания папки, прочитанные ByTree
	read bool
}

func (j *Jobs) get(id string) (*Job, error) {
	if j.byID == nil {
		j.byID = map[string]*Job{}
	}
	if v, ok := j.byID[id]; ok {
		return v, nil
	}
	v, err := readJob(filepath.Join(j.Dir, id, "job.json"))
	if err != nil {
		return nil, err
	}
	j.byID[id] = v
	return v, nil
}

// readJob — задание из job.json; файла нет (папка не задания или задание убрано) — nil.
func readJob(path string) (*Job, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil //nolint:return-nil-error // не задание: папка без job.json или задание уже убрано — владельца нет
	}
	if err != nil {
		return nil, fmt.Errorf("задание worker: %w", err)
	}
	var v Job
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("задание worker %s: %w", path, err)
	}
	if v.Master == "" {
		return nil, nil //nolint:return-nil-error // старое задание без мастера: писать некому
	}
	return &v, nil
}

// ByUnit — задание по unit бойца на ноуте: worker-<id>.service, повтор — worker-<id>-<n>.service.
func (j *Jobs) ByUnit(unit string) (*Job, error) {
	id, ok := strings.CutPrefix(strings.TrimSuffix(unit, ".service"), "worker-")
	if !ok {
		return nil, nil
	}
	if v, err := j.get(id); v != nil || err != nil {
		return v, err
	}
	i := strings.LastIndexByte(id, '-')
	if i <= 0 || !isPid(id[i+1:]) { // хвоста повтора нет
		return nil, nil
	}
	return j.get(id[:i])
}

// pcSpot — scope задания на PC: <префикс>-<имя дерева>.<16 hex>.scope (llmark tool/internal/pcguard.NewSpot).
var pcSpot = regexp.MustCompile(`^(?:worker|farm)-(.+)\.[0-9a-f]{16}\.scope$`)

// unsafeName — то же, что pcguard.Prefix: имя дерева в имени unit.
var unsafeName = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// ByTree — самое новое задание, чьё дерево (база dir) в имени scope на PC совпадает с tree.
func (j *Jobs) ByTree(tree string) (*Job, error) {
	if !j.read {
		ents, err := os.ReadDir(j.Dir)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("задания worker: %w", err)
		}
		j.read = true
		var errs []error
		for _, e := range ents {
			v, err := j.get(e.Name())
			if err != nil {
				errs = append(errs, err)
			}
			if v != nil {
				j.all = append(j.all, v)
			}
		}
		if err := errors.Join(errs...); err != nil {
			return nil, err
		}
	}
	var best *Job
	for _, v := range j.all {
		if unsafeName.ReplaceAllString(filepath.Base(v.Dir), "_") == tree && (best == nil || v.Created.After(best.Created)) {
			best = v
		}
	}
	return best, nil
}

// Resolver находит владельца потока. Один на проход: кеши заданий живут проход. Сбой поиска (job.json не читается,
// реестр сессий) не роняет замер: поток идёт без мастера, сбой копится в Err.
type Resolver struct {
	Jobs     *Jobs
	Sessions func(pid int) (string, error) // имя сессии Claude Code по pid claude, "" — нет такой
	ProcRoot string                        // /proc ноута: ssh-клиент по порту для потоков PC
	errs     []error
}

// Err — сбои поиска владельцев за проход.
func (r *Resolver) Err() error { return errors.Join(r.errs...) }

func (r *Resolver) fail(err error) bool {
	if err != nil {
		r.errs = append(r.errs, err)
	}
	return err != nil
}

// Owner — владелец потока t на ноуте.
func (r *Resolver) Owner(t *Task) Owner {
	if name, _, ok := proc.SessionScope(t.Cgroup); ok {
		return Owner{Master: name, Kind: KindSession}
	}
	if u := proc.ServiceUnit(t.Cgroup); strings.HasPrefix(u, "worker-") {
		v, err := r.Jobs.ByUnit(u)
		if !r.fail(err) && v != nil {
			return Owner{Master: v.Master, Kind: KindWorker, Label: v.Task}
		}
		return Owner{Kind: KindWorker, Label: strings.TrimSuffix(u, ".service")}
	}
	if u := unitOf(t.Cgroup); strings.HasPrefix(u, "farm-") {
		return Owner{Master: farmMaster, Kind: KindFarm}
	}
	if t.Claude > 0 && r.Sessions != nil {
		name, err := r.Sessions(t.Claude)
		if !r.fail(err) && name != "" {
			return Owner{Master: name, Kind: KindSession}
		}
	}
	return other(t)
}

// RemoteOwner — владелец потока t на чужой машине (PC): scope бойца и фермы по имени, сессия ssh — по ssh-клиенту
// на ноуте, чей локальный порт в SSH_CONNECTION.
func (r *Resolver) RemoteOwner(t *Task) Owner {
	u := unitOf(t.Cgroup)
	if m := pcSpot.FindStringSubmatch(u); m != nil {
		if strings.HasPrefix(u, "farm-") {
			return Owner{Master: farmMaster, Kind: KindFarm}
		}
		v, err := r.Jobs.ByTree(m[1])
		if !r.fail(err) && v != nil {
			return Owner{Master: v.Master, Kind: KindWorker, Label: v.Task}
		}
		return Owner{Kind: KindWorker, Label: u}
	}
	if t.SSH != "" {
		c, err := r.sshClient(t.SSH)
		if !r.fail(err) && c != nil {
			if o := r.Owner(c); o.Master != "" {
				return o
			}
		}
	}
	return other(t)
}

// sshClient — процесс ssh на ноуте, чьё соединение — SSH_CONNECTION «адрес порт адрес порт» (порт клиента второй);
// не нашёлся (соединение уже закрыто, ssh не с ноута) — nil.
func (r *Resolver) sshClient(conn string) (*Task, error) {
	f := strings.Fields(conn)
	if len(f) != 4 || r.ProcRoot == "" {
		return nil, nil
	}
	port, err := strconv.Atoi(f[1])
	if err != nil {
		return nil, fmt.Errorf("SSH_CONNECTION %q: %w", conn, err)
	}
	pid, err := socketOwner(r.ProcRoot, port)
	if err != nil || pid == 0 {
		return nil, err
	}
	t := &Task{Pid: pid}
	dir := filepath.Join(r.ProcRoot, strconv.Itoa(pid))
	cg, err := readProc(filepath.Join(dir, "cgroup"))
	if err != nil {
		return nil, err
	}
	t.Cgroup = parseCgroup(string(cg))
	env, err := readProc(filepath.Join(dir, "environ"))
	if err != nil {
		return nil, err
	}
	t.Claude, _, err = parseEnviron(env)
	return t, err
}

// socketOwner — pid процесса ssh, у которого TCP-сокет с локальным портом port; 0 — не нашёлся.
func socketOwner(procRoot string, port int) (int, error) {
	inodes := map[string]bool{}
	for _, f := range []string{"tcp", "tcp6"} {
		ino, err := localPortInodes(filepath.Join(procRoot, "net", f), port)
		if err != nil {
			return 0, err
		}
		for _, i := range ino {
			inodes["socket:["+i+"]"] = true
		}
	}
	if len(inodes) == 0 {
		return 0, nil
	}
	ents, err := os.ReadDir(procRoot)
	if err != nil {
		return 0, fmt.Errorf("процессы: %w", err)
	}
	for _, e := range ents {
		if !isPid(e.Name()) {
			continue
		}
		dir := filepath.Join(procRoot, e.Name())
		comm, err := readProc(filepath.Join(dir, "comm"))
		if err != nil {
			return 0, err
		}
		if strings.TrimSpace(string(comm)) != "ssh" {
			continue
		}
		if holds, err := holdsSocket(filepath.Join(dir, "fd"), inodes); holds || err != nil {
			pid, perr := strconv.Atoi(e.Name())
			return pid, errors.Join(err, perr)
		}
	}
	return 0, nil
}

// holdsSocket — среди открытых файлов процесса (папка fd) есть сокет из inodes. Процесс ушёл — нет.
func holdsSocket(fdDir string, inodes map[string]bool) (bool, error) {
	fds, err := os.ReadDir(fdDir)
	if proc.Gone(err) || errors.Is(err, fs.ErrPermission) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("открытые файлы: %w", err)
	}
	for _, fd := range fds {
		l, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
		if proc.Gone(err) {
			continue // закрылся во время чтения
		}
		if err != nil {
			return false, fmt.Errorf("открытый файл: %w", err)
		}
		if inodes[l] {
			return true, nil
		}
	}
	return false, nil
}

// localPortInodes — inode сокетов из /proc/net/tcp{,6} с локальным портом port; файла нет (нет IPv6) — пусто.
func localPortInodes(path string, port int) ([]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("сокеты: %w", err)
	}
	defer f.Close()
	want := fmt.Sprintf(":%04X", port)
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fl := strings.Fields(sc.Text())
		if len(fl) > 9 && strings.HasSuffix(fl[1], want) {
			out = append(out, fl[9])
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("сокеты %s: %w", path, err)
	}
	return out, nil
}

// unitOf — последний unit (.service или .scope) в пути cgroup.
func unitOf(cgroup string) string {
	parts := strings.Split(cgroup, "/")
	for i := len(parts) - 1; i >= 0; i-- {
		if strings.HasSuffix(parts[i], ".service") || strings.HasSuffix(parts[i], ".scope") {
			return parts[i]
		}
	}
	return ""
}

// other — поток без мастера: процесс пользователя вне сессий и бойцов (рабочий стол, службы) или системы.
func other(t *Task) Owner {
	if !strings.HasPrefix(t.Cgroup, "/user.slice/") {
		return Owner{Kind: KindSystem}
	}
	return Owner{Kind: KindOther, Label: unitOf(t.Cgroup)}
}
