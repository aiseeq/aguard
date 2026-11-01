package guiwatch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestJournalCollectedOncePerSlice — мера тикета self-io-reads: журнал среза (journalctl по kwin, plasmashell и ядру)
// собирается не на каждое обновление идущего и закрывающегося среза, а раз-два на срез: на машине каждый вызов
// journalctl --user поднимал с диска до ~21 МБ, и за 55 мин дочерний journalctl прочитал ~1,2 ГБ (bpftrace 01.11).
// Эпизод D на 10 с, затем 8 с покоя (закрытие с контекстом 5 с) — не больше двух сборов журнала, и последний сбор
// покрывает конец эпизода.
func TestJournalCollectedOncePerSlice(t *testing.T) {
	root, dir, pid, at := fixture(t)
	w, s := setupState(t, root, dir, pid)
	defer w.closeAll()
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + calls + "\n"
	if err := os.WriteFile(filepath.Join(bin, "journalctl"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	statPath := filepath.Join(root, fmt.Sprint(pid), "task", fmt.Sprint(pid), "stat")
	step := 50 * time.Millisecond
	i := 0
	sample := func(state string, n int) {
		for range n {
			rewrite(t, statPath, statLine(state, 100))
			if err := w.sample(s, at.Add(time.Duration(i)*step)); err != nil {
				t.Fatal(err)
			}
			i++
			time.Sleep(time.Millisecond) // горутина сбора журнала успевает закончить, как на машине между шагами
		}
	}
	sample("S", 1)
	sample("D", 200) // 10 с
	end := at.Add(time.Duration(i-1) * step)
	sample("S", 160) // 8 с покоя: эпизод закрыт и вышел из closing
	deadline := time.Now().Add(10 * time.Second)
	for {
		w.journalMu.Lock()
		busy := len(w.journalPending)
		w.journalMu.Unlock()
		if busy == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	b, err := os.ReadFile(calls)
	if err != nil {
		t.Fatalf("journalctl never called: %v", err)
	}
	var user []string
	for l := range strings.Lines(string(b)) {
		if strings.Contains(l, "--user") || strings.Contains(l, "--file") {
			user = append(user, strings.TrimSpace(l))
		}
	}
	t.Logf("journalctl calls: %d, user collections: %d", strings.Count(string(b), "\n"), len(user))
	if len(user) == 0 || len(user) > 2 {
		t.Fatalf("user journal collections = %d, want 1..2 per slice:\n%s", len(user), b)
	}
	last := user[len(user)-1]
	_, after, ok := strings.Cut(last, "--until ")
	if !ok {
		t.Fatalf("no --until in last collection: %s", last)
	}
	until, err := time.ParseInLocation("2006-01-02 15:04:05", after[:19], time.Local)
	if err != nil {
		t.Fatal(err)
	}
	if until.Before(end.Truncate(time.Second)) {
		t.Fatalf("last collection until %v is before episode end %v", until, end)
	}
}
