package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Сигнал claude доходит до прямого потомка cl и не трогает его потомков (фоновые задачи сессии). Внук отмечает
// полученный SIGTERM в файле got и выходит сам, когда тест создаёт файл stop; тест сигналит только свой sh. Ответ
// внука pong на ping — после выхода sh: ловушка sh срабатывает раньше следующего витка цикла, так что сигнал, если
// он был, к ответу уже записан в got.
func TestSignalChildLeavesGrandchild(t *testing.T) {
	dir := t.TempDir()
	got, stop := filepath.Join(dir, "got"), filepath.Join(dir, "stop")
	ping, pong := filepath.Join(dir, "ping"), filepath.Join(dir, "pong")
	t.Cleanup(func() { _ = os.WriteFile(stop, nil, 0600) })
	grandchild := grandchildScript(dir, stop)
	cmd := exec.Command("sh", "-c", fmt.Sprintf("sh -c %q & echo $!; wait", grandchild))
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	grand, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	if err := signalChild(cmd.Process, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	st, ok := err.(*exec.ExitError)
	if !ok || st.Sys().(syscall.WaitStatus).Signal() != syscall.SIGTERM {
		t.Fatalf("sh не снят SIGTERM: %v", err)
	}
	if err := os.WriteFile(ping, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(pong); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("внук %d не ответил на ping за 10 с", grand)
		}
	}
	if _, err := os.Stat(got); err == nil {
		t.Fatalf("внук %d получил SIGTERM вместе с sh", grand)
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(grand) + "/stat")
	if err != nil {
		t.Fatalf("внук %d снят вместе с sh: %v", grand, err)
	}
	if _, rest, ok := strings.Cut(string(stat), ") "); !ok || strings.HasPrefix(rest, "Z") {
		t.Fatalf("внук %d — зомби: %s", grand, stat)
	}
}

// Скрипт внука: отмечает SIGTERM в dir/got, отвечает dir/pong на dir/ping и выходит по файлу stop или когда
// каталог dir удалён — иначе внук, не успевший увидеть stop до удаления t.TempDir, живёт вечно.
func grandchildScript(dir, stop string) string {
	got, ping, pong := filepath.Join(dir, "got"), filepath.Join(dir, "ping"), filepath.Join(dir, "pong")
	return fmt.Sprintf(`trap 'echo TERM > %q' TERM; while [ -d %q ] && [ ! -e %q ]; do [ -e %q ] && : > %q; sleep 0.05; done`, got, dir, stop, ping, pong)
}

// Внук выходит сам, когда каталог удалён без stop (так идёт удаление t.TempDir после Cleanup или после падения теста).
func TestGrandchildExitsWhenDirectoryIsRemoved(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "grandchild")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	stop := filepath.Join(root, "stop")
	grandchild := exec.Command("sh", "-c", grandchildScript(dir, stop))
	if err := grandchild.Start(); err != nil {
		t.Fatal(err)
	}
	grand := grandchild.Process.Pid
	wait := make(chan error, 1)
	go func() { wait <- grandchild.Wait() }()
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = os.WriteFile(stop, nil, 0600)
			<-wait
		}
	})

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-wait:
		waited = true
		if err != nil {
			t.Fatalf("внук %d завершился с ошибкой: %v", grand, err)
		}
	case <-time.After(time.Second):
		t.Fatalf("внук %d не вышел после удаления каталога за 1 с", grand)
	}
	if err := syscall.Kill(grand, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("kill(%d, 0) = %v, ожидался ESRCH", grand, err)
	}
}

// Вышедший и собранный claude — не ошибка: его выход и так придёт в done.
func TestSignalChildGoneIsNil(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if err := signalChild(cmd.Process, syscall.SIGTERM); err != nil {
		t.Fatalf("сигнал вышедшему: %v", err)
	}
}
