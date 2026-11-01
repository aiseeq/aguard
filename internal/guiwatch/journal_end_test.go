package guiwatch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Мера тикета self-io-reads. Журнал среза (journalctl по kwin, plasmashell и ядру) собирается не на каждое обновление
// среза, а раз-два на срез: на машине каждый вызов journalctl --user поднимал с диска до ~21 МБ, и за 55 мин дочерний
// journalctl прочитал ~1,2 ГБ (bpftrace 01.11). Находки независимой проверки 487bcbd: журнал собирается до конца эпизода при
// любом его окончании — остановке и сне демона (finishAll), уходе цели (forget), разрыве опроса (recordSamplerGap),
// а не только при закрытии по покою; и в режиме --file фильтр пользовательских юнитов — --user-unit (с --file
// ключ -u ищет системные юниты: на машине 0 строк kwin за сутки против 13 у --user -u).

type journalRun struct {
	calls string
	end   time.Time
}

// runJournalEpisode — эпизод D на 10 с с поддельным journalctl, затем finish заканчивает его (sample — дальнейшие
// опросы с шагом 50 мс); ждёт сборы.
func runJournalEpisode(t *testing.T, finish func(w *Watcher, pid int, at time.Time, sample func(string, int)) error) journalRun {
	t.Helper()
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
	journalDir := t.TempDir()
	userFile := filepath.Join(journalDir, "user-1000.journal")
	if err := os.WriteFile(userFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(userFile, at.Add(time.Hour), at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	w.JournalDir = journalDir
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
	sample("D", 200)
	end := at.Add(time.Duration(i-1) * step)
	if err := finish(w, pid, at.Add(time.Duration(i)*step), sample); err != nil {
		t.Fatal(err)
	}
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
	return journalRun{calls: string(b), end: end}
}

func userCollections(calls string) []string {
	var user []string
	for l := range strings.Lines(calls) {
		if strings.Contains(l, "--user") || strings.Contains(l, "--file") {
			user = append(user, strings.TrimSpace(l))
		}
	}
	return user
}

func checkJournalCoversEnd(t *testing.T, r journalRun) {
	t.Helper()
	user := userCollections(r.calls)
	if len(user) == 0 || len(user) > 2 {
		t.Fatalf("user journal collections = %d, want 1..2 per slice:\n%s", len(user), r.calls)
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
	if until.Before(r.end.Truncate(time.Second)) {
		t.Fatalf("last collection until %v is before episode end %v", until, r.end)
	}
}

func finishStop(w *Watcher, _ int, at time.Time, _ func(string, int)) error {
	return w.finishAll(at, false)
}

// TestJournalCollectedOncePerSlice — эпизод D на 10 с, затем 8 с покоя (закрытие с контекстом 5 с).
func TestJournalCollectedOncePerSlice(t *testing.T) {
	checkJournalCoversEnd(t, runJournalEpisode(t, func(_ *Watcher, _ int, _ time.Time, sample func(string, int)) error {
		sample("S", 160)
		return nil
	}))
}

func TestJournalEndStop(t *testing.T) {
	checkJournalCoversEnd(t, runJournalEpisode(t, finishStop))
}

func TestJournalEndSamplerGap(t *testing.T) {
	checkJournalCoversEnd(t, runJournalEpisode(t, func(w *Watcher, _ int, at time.Time, _ func(string, int)) error {
		return w.recordSamplerGap(at, 2*time.Second)
	}))
}

func TestJournalEndForget(t *testing.T) {
	checkJournalCoversEnd(t, runJournalEpisode(t, func(w *Watcher, pid int, _ time.Time, _ func(string, int)) error {
		return w.forget(pid)
	}))
}

func TestJournalFileUsesUserUnit(t *testing.T) {
	r := runJournalEpisode(t, finishStop)
	files := 0
	for _, l := range userCollections(r.calls) {
		if !strings.Contains(l, "--file") {
			continue
		}
		files++
		if strings.Contains(" "+l+" ", " -u ") || !strings.Contains(l, "--user-unit") {
			t.Fatalf("--file collection must filter user units by --user-unit, not -u: %s", l)
		}
	}
	if files == 0 {
		t.Fatalf("no --file collection with a fresh user journal file:\n%s", r.calls)
	}
}
