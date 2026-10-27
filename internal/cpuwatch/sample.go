// Package cpuwatch — перегрузка процессора и чья она (задание лида 27.10 по словам пользователя в транскрипте лида
// 2dc3ff44: «"Почему aguard молчал. Нагрузку на процессор он не отслеживает." - ну здрасьте. А кто кроме него
// отслеживать то её будет?»). Мера — число готовых к работе задач в момент замера (procs_running), одно чтение:
// «я когда контролировал нагрузку опирался на неусреднённое, моментальное значение задач готовых к работе», «Нам не
// нужно точное число процессов. Нам нужно актуальное». Машина занята — по правилу akit/pressure: готовых задач не
// меньше, чем потоков у машины.
//
// Раз в интервал — замер: число готовых задач и сами готовые потоки (состояние R) с процессом, cgroup и окружением.
// Владелец потока — мастер: сессия по scope cl или CLAUDE_PID, боец worker по своему unit и job.json, ферма llmark
// по scope farm-*, на PC — ещё и по SSH_CONNECTION до ssh на ноуте. Занятых замеров в окне не меньше доли share —
// перегрузка: письмо мастерам, чьих задач больше всего, и сводка лиду; пока длится — не чаще раза в repeat. Ничего не
// снимает и не понижает (пользователь 22.10 про ферму: «зря ты дал задание ослабить задачи. Надо было разобраться
// почему такие задачи вызывают тормоза»).
package cpuwatch

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/akit/pressure"
	"github.com/aiseeq/akit/proc"
)

// Task — готовые к работе потоки одного процесса в замере.
type Task struct {
	Pid    int
	N      int    // потоков в состоянии R
	Comm   string // имя процесса
	Cgroup string // путь cgroup v2
	Claude int    // CLAUDE_PID из окружения: сессия Claude Code, под которой он запущен
	SSH    string // SSH_CONNECTION из окружения: «адрес порт адрес порт» клиента и сервера
}

// Sample — замер машины: вердикт правила akit «машина занята» и готовые потоки.
type Sample struct {
	At      time.Time
	Verdict pressure.Verdict
	Tasks   []Task
}

// Busy — машина занята по процессору: готовых к работе задач не меньше, чем потоков. Занятость по диску и свопу —
// забота memory и disk: готовые потоки её владельца не показывают.
func (s *Sample) Busy() bool { return s.Verdict.Measure == pressure.CPU }

// ReadTasks — готовые к работе потоки всех процессов procRoot. Процесс, ушедший во время чтения, пропускается;
// окружение и cgroup чужих процессов не читаются — у них остаётся то, что прочиталось.
func ReadTasks(procRoot string) ([]Task, error) {
	ents, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, fmt.Errorf("процессы: %w", err)
	}
	var out []Task
	var errs []error
	for _, e := range ents {
		if !isPid(e.Name()) {
			continue
		}
		t, err := readTask(filepath.Join(procRoot, e.Name()))
		if err != nil {
			errs = append(errs, err)
		}
		if t != nil {
			out = append(out, *t)
		}
	}
	return out, errors.Join(errs...)
}

// readTask — готовые потоки процесса dir; нет готовых или процесс ушёл — nil.
func readTask(dir string) (*Task, error) {
	n, err := runnable(filepath.Join(dir, "task"))
	if err != nil || n == 0 {
		return nil, err
	}
	comm, err := readProc(filepath.Join(dir, "comm"))
	if err != nil || comm == nil {
		return nil, err
	}
	pid, err := strconv.Atoi(filepath.Base(dir))
	if err != nil {
		return nil, fmt.Errorf("pid %s: %w", dir, err)
	}
	t := &Task{Pid: pid, N: n, Comm: strings.TrimSpace(string(comm))}
	cg, err := readProc(filepath.Join(dir, "cgroup"))
	if err != nil {
		return t, err
	}
	t.Cgroup = parseCgroup(string(cg))
	env, err := readProc(filepath.Join(dir, "environ"))
	if err != nil {
		return t, err
	}
	t.Claude, t.SSH, err = parseEnviron(env)
	return t, err
}

// readProc — файл процесса. Процесс ушёл или он чужой (окружение чужого uid не читается) — nil без ошибки: в замере
// у него остаётся прочитанное.
func readProc(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if proc.Gone(err) || errors.Is(err, fs.ErrPermission) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("процесс: %w", err)
	}
	return b, nil
}

func isPid(name string) bool {
	if name == "" || name[0] == '0' {
		return false
	}
	for i := range len(name) {
		if name[i] < '0' || name[i] > '9' {
			return false
		}
	}
	return true
}

// runnable — сколько потоков процесса в состоянии R (поле после последней «)» в task/<tid>/stat). Процесс или поток
// ушёл во время чтения — их потоки не считаются.
func runnable(taskDir string) (int, error) {
	ents, err := os.ReadDir(taskDir)
	if proc.Gone(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("потоки: %w", err)
	}
	n := 0
	for _, e := range ents {
		b, err := readProc(filepath.Join(taskDir, e.Name(), "stat"))
		if err != nil {
			return n, err
		}
		if i := bytes.LastIndexByte(b, ')'); i >= 0 && i+2 < len(b) && b[i+2] == 'R' {
			n++
		}
	}
	return n, nil
}

func parseCgroup(s string) string {
	for l := range strings.SplitSeq(s, "\n") {
		if v, ok := strings.CutPrefix(l, "0::"); ok {
			return v
		}
	}
	return ""
}

// parseEnviron — CLAUDE_PID и SSH_CONNECTION из окружения процесса.
func parseEnviron(b []byte) (claude int, ssh string, err error) {
	for kv := range bytes.SplitSeq(b, []byte{0}) {
		if v, ok := bytes.CutPrefix(kv, []byte("CLAUDE_PID=")); ok {
			if claude, err = claudePid(string(v)); err != nil {
				return 0, "", err
			}
		}
		if v, ok := bytes.CutPrefix(kv, []byte("SSH_CONNECTION=")); ok {
			ssh = string(v)
		}
	}
	return claude, ssh, nil
}

// claudePid — значение CLAUDE_PID; пусто — 0.
func claudePid(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("CLAUDE_PID %q: %w", s, err)
	}
	return n, nil
}

// remoteFiles — файлы /proc, которые читает правило akit: их RemoteScript отдаёт как есть.
var remoteFiles = []string{"stat", "pressure/io", "pressure/cpu", "vmstat"}

// RemoteScript — замер чужой машины одним вызовом ssh: файлы правила akit разделами «##<файл>» и готовые потоки
// построчно (число потоков, pid, comm, cgroup, CLAUDE_PID, SSH_CONNECTION через табуляцию). Разбирает ParseRemote.
const RemoteScript = `for f in stat pressure/io pressure/cpu vmstat; do echo "##$f"; cat /proc/$f; done
echo '##tasks'
grep -l ') R ' /proc/[0-9]*/task/[0-9]*/stat 2>/dev/null | cut -d/ -f3 | sort | uniq -c | while read -r n p; do
	c=$(cat /proc/$p/comm 2>/dev/null) || continue
	g=$(cut -d: -f3- /proc/$p/cgroup 2>/dev/null)
	e=$(tr '\0' '\n' </proc/$p/environ 2>/dev/null)
	cl=$(printf '%s\n' "$e" | sed -n 's/^CLAUDE_PID=//p')
	ss=$(printf '%s\n' "$e" | sed -n 's/^SSH_CONNECTION=//p')
	printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$n" "$p" "$c" "$g" "$cl" "$ss"
done
echo '##end'
`

// ParseRemote разбирает вывод RemoteScript: файлы правила кладёт в root (по нему замеряет pressure.Meter), готовые
// потоки возвращает. Нет строки ##end — вывод оборван: ошибка.
func ParseRemote(raw []byte, root string) ([]Task, error) {
	files, rest, ok := bytes.Cut(raw, []byte("##tasks\n"))
	if !ok {
		return nil, errors.New("замер: нет раздела ##tasks")
	}
	tasks, _, ok := bytes.Cut(rest, []byte("##end\n"))
	if !ok {
		return nil, errors.New("замер оборван: нет ##end")
	}
	for i, name := range remoteFiles {
		head := []byte("##" + name + "\n")
		if !bytes.HasPrefix(files, head) {
			return nil, fmt.Errorf("замер: нет раздела ##%s", name)
		}
		body := files[len(head):]
		end := len(body)
		if i+1 < len(remoteFiles) {
			if end = bytes.Index(body, []byte("##"+remoteFiles[i+1]+"\n")); end < 0 {
				return nil, fmt.Errorf("замер: нет раздела ##%s", remoteFiles[i+1])
			}
		}
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return nil, fmt.Errorf("замер: %w", err)
		}
		if err := os.WriteFile(p, body[:end], 0o600); err != nil {
			return nil, fmt.Errorf("замер: %w", err)
		}
		files = body[end:]
	}
	var out []Task
	for l := range strings.SplitSeq(strings.TrimSuffix(string(tasks), "\n"), "\n") {
		if l == "" {
			continue
		}
		f := strings.Split(l, "\t")
		if len(f) != 6 {
			return nil, fmt.Errorf("замер: строка %q", l)
		}
		var t Task
		var err error
		if t.N, err = strconv.Atoi(f[0]); err != nil {
			return nil, fmt.Errorf("замер: %q: %w", l, err)
		}
		if t.Pid, err = strconv.Atoi(f[1]); err != nil {
			return nil, fmt.Errorf("замер: %q: %w", l, err)
		}
		t.Comm, t.Cgroup, t.SSH = f[2], f[3], f[5]
		if t.Claude, err = claudePid(f[4]); err != nil {
			return nil, fmt.Errorf("замер: %q: %w", l, err)
		}
		out = append(out, t)
	}
	slices.SortFunc(out, func(a, b Task) int { return a.Pid - b.Pid })
	return out, nil
}
