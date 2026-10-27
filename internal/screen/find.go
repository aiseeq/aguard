package screen

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aiseeq/akit/proc"
)

// Terminal — где читать экран: Konsole по D-Bus или tmux по его сокету.
type Terminal interface {
	// Konsole — строки экрана вкладки konsole процесса konsolePID, в которой ввод — терминал tty.
	Konsole(konsolePID int, tty string) ([]string, error)
	// Tmux — строки экрана панели сервера tmux с сокетом socket, в которой терминал tty.
	Tmux(socket, tty string) ([]string, error)
}

// Reader находит терминал процесса и читает его экран.
type Reader struct {
	Proc string // /proc; в тестах — временная папка
	Term Terminal
}

// NewReader — живая машина: /proc, сеансовая шина D-Bus и tmux из PATH.
func NewReader() *Reader { return &Reader{Proc: "/proc", Term: &live{proc: "/proc"}} }

// ErrNoTerminal — терминал процесса не Konsole и не tmux: экрана не прочитать.
var ErrNoTerminal = errors.New("терминал не Konsole и не tmux")

// ancestors — сколько родителей проходим в поисках konsole: claude ← cl ← оболочка ← konsole, с запасом.
const ancestors = 16

// Prompt — поле ввода Claude Code процесса pid по экрану его терминала.
func (r *Reader) Prompt(pid int) (Prompt, error) {
	lines, err := r.Lines(pid)
	if err != nil {
		return Unknown, err
	}
	return Parse(lines), nil
}

// Lines — строки экрана терминала процесса pid. Внутри tmux — его панель (tmux мог быть открыт и в Konsole),
// иначе — вкладка konsole из предков процесса.
func (r *Reader) Lines(pid int) ([]string, error) {
	dir := filepath.Join(r.Proc, strconv.Itoa(pid))
	tty, err := os.Readlink(filepath.Join(dir, "fd", "0"))
	if err != nil {
		return nil, fmt.Errorf("терминал процесса: %w", err)
	}
	if !strings.HasPrefix(tty, "/dev/pts/") {
		return nil, fmt.Errorf("ввод процесса не терминал: %s", tty)
	}
	if tty, err = r.outerTTY(pid, tty); err != nil {
		return nil, err
	}
	env, err := os.ReadFile(filepath.Join(dir, "environ"))
	if err != nil {
		return nil, fmt.Errorf("окружение процесса: %w", err)
	}
	if socket := tmuxSocket(env); socket != "" {
		return r.Term.Tmux(socket, tty)
	}
	for p, i := pid, 0; p > 1 && i < ancestors; i++ {
		st, err := proc.ReadStat(r.Proc, p)
		if err != nil {
			return nil, fmt.Errorf("предки процесса %d: %w", pid, err)
		}
		if st.Comm == "konsole" {
			return r.Term.Konsole(p, tty)
		}
		p = st.Ppid
	}
	return nil, ErrNoTerminal
}

// tmuxSocket — сокет сервера из TMUX=<сокет>,<pid сервера>,<сессия> в окружении процесса.
func tmuxSocket(environ []byte) string {
	for v := range bytes.SplitSeq(environ, []byte{0}) {
		if s, ok := bytes.CutPrefix(v, []byte("TMUX=")); ok {
			socket, _, _ := strings.Cut(string(s), ",")
			return socket
		}
	}
	return ""
}

// outerTTY — терминал вкладки или панели: claude под pty cl читает внутренний pts, а вкладка показывает внешний,
// с которого читает cl. Подъём по предкам, пока их ввод — терминал; у konsole и сервера tmux ввод не терминал.
// Родителя нет или он чужой (sshd от root) — подъём кончился.
func (r *Reader) outerTTY(pid int, tty string) (string, error) {
	for p, i := pid, 0; i < ancestors; i++ {
		st, err := proc.ReadStat(r.Proc, p)
		if err != nil {
			return "", fmt.Errorf("предки процесса %d: %w", pid, err)
		}
		if st.Ppid <= 1 {
			break
		}
		up, err := ttyOf(r.Proc, st.Ppid)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("терминал предка %d: %w", st.Ppid, err)
		}
		if !strings.HasPrefix(up, "/dev/pts/") {
			break
		}
		p, tty = st.Ppid, up
	}
	return tty, nil
}

// ttyOf — терминал ввода процесса: для сверки вкладки Konsole по процессу её оболочки.
func ttyOf(procRoot string, pid int) (string, error) {
	return os.Readlink(filepath.Join(procRoot, strconv.Itoa(pid), "fd", "0"))
}

func splitLines(out []byte) []string {
	return strings.Split(strings.TrimRight(string(out), "\n"), "\n")
}

// tmuxPane — панель с терминалом tty по списку «<tty> <id>» от list-panes.
func tmuxPane(list []byte, tty string) (string, bool) {
	for _, l := range splitLines(list) {
		if t, id, ok := strings.Cut(l, " "); ok && t == tty {
			return id, true
		}
	}
	return "", false
}

func tmuxRun(socket string, args ...string) ([]byte, error) {
	cmd := exec.Command("tmux", append([]string{"-S", socket}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("tmux %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
