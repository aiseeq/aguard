package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/aiseeq/akit/session"
)

// world — подменённые реестр Claude Code, /proc и claude из PATH в t.TempDir: claude — ссылка на versions/2.1.310,
// процесс теста становится сессией s1 «x-master» через link (папка /proc/<pid> и запись реестра — ссылки на
// образец). Решение о сессии даёт поддельный aguard (fakeAguard). Ни настоящей сессии, ни claude тест не
// трогает.
type world struct {
	dir      string
	claude   string
	registry session.Registry
}

func newWorld(t *testing.T) world {
	t.Helper()
	dir := t.TempDir()
	w := world{dir: dir, claude: filepath.Join(dir, "bin", "claude"),
		registry: session.Registry{Dir: filepath.Join(dir, "sessions"), ProcRoot: filepath.Join(dir, "proc")}}
	write(t, filepath.Join(dir, "versions", "2.1.310"), "#!/bin/sh\n")
	symlink(t, filepath.Join(dir, "versions", "2.1.310"), w.claude)
	write(t, filepath.Join(dir, "session.json"), `{"pid":`+strconv.Itoa(os.Getpid())+`,"sessionId":"s1","name":"x-master","kind":"interactive","procStart":"500"}`)
	tmpl := filepath.Join(dir, "proc-tmpl")
	write(t, filepath.Join(tmpl, "stat"), "1 (claude) S 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 500 0 0\n")
	write(t, filepath.Join(tmpl, "cmdline"), "/home/u/.local/bin/claude\x00--dangerously-skip-permissions\x00-n\x00x-master\x00")
	for _, d := range []string{w.registry.Dir, w.registry.ProcRoot} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return w
}

// link делает процесс pid (тест: свой) сессией s1 этого мира.
func (w world) link(t *testing.T, pid int) {
	t.Helper()
	symlink(t, filepath.Join(w.dir, "proc-tmpl"), filepath.Join(w.registry.ProcRoot, strconv.Itoa(pid)))
	symlink(t, filepath.Join(w.dir, "session.json"), filepath.Join(w.registry.Dir, strconv.Itoa(pid)+".json"))
}

func write(t *testing.T, file, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
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
