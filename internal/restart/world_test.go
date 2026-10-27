package restart

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/screen"
)

// sessionWorld — подменённые реестр Claude Code, транскрипты, состояния chooks, /proc, терминал и claude из
// PATH в t.TempDir: одна сессия s1 «x-master» на 2.1.309 при установленной 2.1.310. Процесс с любым pid становится
// этой сессией через link (папка /proc/<pid> и запись реестра — ссылки на общий образец), поэтому её может
// подхватить и подменённый systemd-run, чей pid заранее не известен. Ни настоящей сессии, ни claude тест не трогает.
type sessionWorld struct {
	dir string
	p   paths
}

// worldTimes — когда в подменённой сессии что было, относительно now.
type worldTimes struct {
	now     time.Time
	turnAgo time.Duration // последняя запись транскрипта, состояние chooks и статус реестра
	ttyAgo  time.Duration // последний ввод в терминал (atime)
}

func newSessionWorld(t *testing.T, at worldTimes, transcript ...string) sessionWorld {
	t.Helper()
	dir := t.TempDir()
	w := sessionWorld{dir: dir, p: paths{
		sessions:   filepath.Join(dir, "sessions"),
		projects:   filepath.Join(dir, "projects"),
		hookStates: filepath.Join(dir, "hooks"),
		proc:       filepath.Join(dir, "proc"),
		claude:     filepath.Join(dir, "bin", "claude"),
		prompt:     func(int) (screen.Prompt, error) { return screen.Empty, nil },
		tree:       func(string) ([]string, error) { return nil, nil },
	}}
	turn := at.now.Add(-at.turnAgo)
	write(t, filepath.Join(dir, "versions", "2.1.310"), "#!/bin/sh\n")
	symlink(t, filepath.Join(dir, "versions", "2.1.310"), w.p.claude)
	write(t, filepath.Join(dir, "session.json"), `{"pid":`+strconv.Itoa(os.Getpid())+`,"sessionId":"s1","name":"x-master",`+
		`"kind":"interactive","status":"idle","startedAt":`+strconv.FormatInt(at.now.Add(-time.Hour).UnixMilli(), 10)+
		`,"statusUpdatedAt":`+strconv.FormatInt(turn.UnixMilli(), 10)+`}`)
	if err := os.MkdirAll(w.p.sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	stamped(t, filepath.Join(w.p.hookStates, "s1"), "done\n", turn, turn)
	stamped(t, filepath.Join(w.p.projects, "-home-u-git-x", "s1.jsonl"), strings.Join(transcript, "\n")+"\n", turn, turn)
	tty := filepath.Join(dir, "tty")
	stamped(t, tty, "", at.now.Add(-at.ttyAgo), turn)
	tmpl := filepath.Join(dir, "proc-tmpl")
	write(t, filepath.Join(tmpl, "stat"), "1 (claude) S 1 1 1 0")
	write(t, filepath.Join(tmpl, "cmdline"), "/home/u/.local/bin/claude\x00--dangerously-skip-permissions\x00-n\x00x-master\x00")
	w.setRunning(t, "2.1.309")
	// ttyQuiet берёт только ввод с /dev/pts/ или /dev/tty; «/dev/pts/../..» выводит из devpts в корень и дальше к
	// файлу теста, чей atime тест ставит сам.
	symlink(t, "/dev/pts/../.."+tty, filepath.Join(tmpl, "fd", "0"))
	if err := os.MkdirAll(w.p.proc, 0o755); err != nil {
		t.Fatal(err)
	}
	return w
}

// setRunning — версия бинаря процесса сессии (/proc/<pid>/exe).
func (w sessionWorld) setRunning(t *testing.T, version string) {
	t.Helper()
	exe := filepath.Join(w.dir, "proc-tmpl", "exe")
	if err := os.Remove(exe); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	symlink(t, filepath.Join(w.dir, "versions", version), exe)
}

// link делает процесс pid сессией s1 этого мира.
func (w sessionWorld) link(t *testing.T, pid int) {
	t.Helper()
	symlink(t, filepath.Join(w.dir, "proc-tmpl"), filepath.Join(w.p.proc, strconv.Itoa(pid)))
	symlink(t, filepath.Join(w.dir, "session.json"), filepath.Join(w.p.sessions, strconv.Itoa(pid)+".json"))
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// stamped пишет файл и ставит ему atime и mtime.
func stamped(t *testing.T, file, data string, atime, mtime time.Time) {
	t.Helper()
	write(t, file, data)
	if err := os.Chtimes(file, atime, mtime); err != nil {
		t.Fatal(err)
	}
}

// fakeTool кладёт в dir исполняемый sh-скрипт вместо внешней команды (systemd-run): тест видит, с чем cl её зовёт,
// и не запускает ни scope, ни claude.
func fakeTool(t *testing.T, dir, name, body string) string {
	t.Helper()
	file := filepath.Join(dir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return file
}

// readLines — строки файла, который пишет подменённая команда (аргумент на строку).
func readLines(t *testing.T, file string) []string {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}
