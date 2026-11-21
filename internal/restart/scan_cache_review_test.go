package restart

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/statefile"
)

func TestScanCacheReviewRoundTripAtEveryLineCut(t *testing.T) {
	since := mustTime(t, tsNew)
	data := reviewTranscript()
	full, err := scanTranscript(bytes.NewReader(data), since)
	if err != nil {
		t.Fatal(err)
	}
	assertReviewFields(t, full)
	wantFacts := full.facts(since.Add(2 * time.Hour))
	wantState, err := json.Marshal(full.state())
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	file := filepath.Join(root, "session.jsonl")
	statePath := filepath.Join(root, "restart", "state.json")
	saver := NewJudge(root)
	if err := saver.LoadScanCache(statePath, time.Now()); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Hour)
	cuts := reviewCuts(data)
	for _, loc := range []*time.Location{time.UTC, time.Local} {
		for _, cut := range cuts {
			t.Run(fmt.Sprintf("cut-%d-%s", cut, loc), func(t *testing.T) {
				prefixTime := since.In(loc)
				if err := os.WriteFile(file, data[:cut], 0o600); err != nil {
					t.Fatal(err)
				}
				mt := base.Add(time.Duration(cut) * time.Millisecond)
				if err := os.Chtimes(file, mt, mt); err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(file)
				if err != nil {
					t.Fatal(err)
				}
				c := &cached{used: time.Now()}
				if _, err := c.scan.read(file, info, prefixTime, since.Add(2*time.Hour)); err != nil {
					t.Fatal(err)
				}
				saver.caches = map[int]*cached{1: c}
				if err := saver.SaveScanCache(time.Now()); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, data, 0o600); err != nil {
					t.Fatal(err)
				}
				mt = mt.Add(time.Minute)
				if err := os.Chtimes(file, mt, mt); err != nil {
					t.Fatal(err)
				}
				info, err = os.Stat(file)
				if err != nil {
					t.Fatal(err)
				}
				restarted := NewJudge(root)
				if err := restarted.LoadScanCache(statePath, time.Now()); err != nil {
					t.Fatal(err)
				}
				r := restarted.caches[1]
				if r == nil {
					t.Fatal("сохранённый разбор не восстановлен")
				}
				if r.scan.offset != int64(lastCompleteLine(data[:cut])) {
					t.Fatalf("восстановлен offset=%d, want %d: дочитывать надо с сохранённого места", r.scan.offset, lastCompleteLine(data[:cut]))
				}
				gotFacts, err := r.scan.read(file, info, prefixTime, since.Add(2*time.Hour))
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(gotFacts, wantFacts) {
					t.Fatalf("facts отличаются от полного разбора\ngot: %+v\nwant: %+v", gotFacts, wantFacts)
				}
				r.scan.scan.since = r.scan.scan.since.In(full.since.Location())
				if !reflect.DeepEqual(r.scan.scan, full) {
					got, _ := json.Marshal(r.scan.scan.state())
					t.Fatalf("transcriptScan отличается от полного разбора\ngot: %s\nwant: %s", got, wantState)
				}
				if r.scan.offset != int64(lastCompleteLine(data)) {
					t.Fatalf("offset=%d, want %d", r.scan.offset, lastCompleteLine(data))
				}
			})
		}
	}
}

func TestScanCacheSaveDoesNotHoldJudgeLockWhileWaitingForSession(t *testing.T) {
	j := NewJudge(t.TempDir())
	j.statePath = filepath.Join(t.TempDir(), "restart", "state.json")
	busy := j.cache(1, time.Now())
	busy.mu.Lock()
	idle := j.cache(2, time.Now())
	idle.scan.scan = newScan(time.Now())
	saveDone := make(chan struct{})
	go func() {
		_ = j.SaveScanCache(time.Now())
		close(saveDone)
	}()
	saved := false
	select {
	case <-saveDone:
	case <-time.After(300 * time.Millisecond):
		saved = true
	}
	done := make(chan struct{})
	go func() { j.cache(2, time.Now()); close(done) }()
	select {
	case <-done:
	case <-time.After(300 * time.Millisecond):
		t.Error("cache() другой сессии задержан блокировкой сессии, которую сохраняют")
	}
	busy.mu.Unlock()
	if saved {
		select {
		case <-saveDone:
		case <-time.After(time.Second):
		}
		t.Fatal("сохранение ждёт блокированную сессию")
	}
	b, err := statefile.Load(j.statePath)
	if err != nil {
		t.Fatal(err)
	}
	var state savedScanCache
	if err := json.Unmarshal(b, &state); err != nil {
		t.Fatal(err)
	}
	if state.Caches[2] == nil {
		t.Fatal("сохранение пропустило незанятую сессию")
	}
}

func TestScanCacheReplacementIsRejected(t *testing.T) {
	for _, mode := range []string{"review-rename", "inode-with-matching-tail", "same-inode-changed-tail"} {
		t.Run(mode, func(t *testing.T) {
			since := mustTime(t, tsOld)
			dir := t.TempDir()
			file := filepath.Join(dir, "session.jsonl")
			original, replacement := rewriteFixture(mode)
			if err := os.WriteFile(file, original, 0o600); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(file)
			if err != nil {
				t.Fatal(err)
			}
			c := &cached{used: time.Now()}
			if _, err := c.scan.read(file, info, since, since.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			j := NewJudge(dir)
			j.caches[1] = c
			j.statePath = filepath.Join(dir, "restart", "state.json")
			if err := j.SaveScanCache(time.Now()); err != nil {
				t.Fatal(err)
			}
			target := file
			if mode == "same-inode-changed-tail" {
				if err := os.WriteFile(file, replacement, 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				target = file + ".new"
				if err := os.WriteFile(target, replacement, 0o600); err != nil {
					t.Fatal(err)
				}
				if mode == "inode-with-matching-tail" {
					later := info.ModTime().Add(time.Second)
					if err := os.Chtimes(target, later, later); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Rename(target, file); err != nil {
					t.Fatal(err)
				}
			}
			later := info.ModTime().Add(2 * time.Second)
			if err := os.Chtimes(file, later, later); err != nil {
				t.Fatal(err)
			}
			restarted := NewJudge(dir)
			if err := restarted.LoadScanCache(j.statePath, time.Now()); err != nil {
				t.Fatal(err)
			}
			if len(restarted.caches) != 0 {
				t.Fatal("кеш изменённого транскрипта восстановлен")
			}
		})
	}
}

func rewriteFixture(mode string) ([]byte, []byte) {
	if mode == "review-rename" {
		original := []byte(use(tsNew, "t1", "Bash", `{"command":"sleep 100"}`) + "\n")
		replacement := []byte(`{"type":"progress","timestamp":"` + tsNew + `","x":"` + strings.Repeat("y", len(original)) + `"}` + "\n" +
			`{"type":"user","timestamp":"` + tsNew + `","message":{"content":"новая реплика"}}` + "\n")
		return original, replacement
	}
	original := append(bytes.Repeat([]byte{'a'}, 8192), '\n')
	if mode == "inode-with-matching-tail" {
		replacement := append(bytes.Repeat([]byte{'b'}, 4096), bytes.Repeat([]byte{'a'}, 4096)...)
		return original, append(replacement, '\n')
	}
	replacement := append(bytes.Repeat([]byte{'a'}, 4096), bytes.Repeat([]byte{'b'}, 4096)...)
	return original, append(replacement, '\n')
}

func reviewTranscript() []byte {
	lines := []string{
		use(tsOld, "delete-before-start", "CronDelete", `{"id":"foreign"}`),
		`{"type":"attachment","timestamp":"` + tsNew + `","attachment":{"type":"session_cron_carry","tasks":[{"id":"carried","cron":"0 9 * * *","prompt":"own","recurring":true}],"otherArmed":[{"id":"foreign","cron":"0 10 * * *","prompt":"inherited","recurring":true},{"id":"fired","cron":"0 11 * * *","prompt":"one-shot","recurring":false}]}}`,
		`{"type":"user","timestamp":"` + tsNew + `","message":{"content":"человек"},"origin":{"kind":"human"},"promptSource":"typed"}`,
		use(tsNew, "waiting", "Bash", `{"command":"sleep 30","description":"wait command"}`),
		use(tsNew, "plain", "Bash", `{"command":"ls","description":"no wait"}`),
		use(tsNew, "agent-launch", "Agent", `{"description":"subagent"}`),
		result(tsNew, "agent-launch", "launched", `{"isAsync":true,"agentId":"agent-1"}`),
		notice(tsNew, "user", `<task-id>agent-1</task-id>\n<status>completed</status>\n<summary>Agent is waiting on its own background work</summary>`),
		use(tsNew, "background", "Bash", `{"command":"make","run_in_background":true}`),
		result(tsNew, "background", "running", `{"backgroundTaskId":"task-1"}`),
		notice(tsNew, "user", `<task-id>task-1</task-id>\n<status>completed</status>`),
		use(tsNew, "monitor", "Monitor", `{"description":"monitor job"}`),
		result(tsNew, "monitor", "Monitor started (task monitor-1, expires in 30m)", `null`),
		use(tsNew, "create-cron", "CronCreate", `{"cron":"0 12 * * *","prompt":"created","recurring":true}`),
		result(tsNew, "create-cron", "created", `{"id":"created","recurring":true}`),
		`{"type":"system","subtype":"scheduled_task_fire","timestamp":"` + tsNew + `","taskId":"fired"}`,
		use(tsNew, "wakeup", "ScheduleWakeup", `{"delaySeconds":14400}`),
		result(tsNew, "wakeup", "scheduled", `null`),
		`{"type":"assistant","timestamp":"` + tsNew + `","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":4,"output_tokens":3},"content":[{"type":"text","text":"ответ"}]}}`,
		`{"type":"system","subtype":"compact_boundary","timestamp":"2026-10-24T10:01:00Z","compactMetadata":{"trigger":"auto"}}`,
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

func reviewCuts(data []byte) []int {
	cuts, seen := []int{0, len(data)}, map[int]bool{0: true, len(data): true}
	start := 0
	for i, b := range data {
		if b != '\n' {
			continue
		}
		for _, cut := range []int{i, i + 1, (start + i) / 2, start + 1} {
			if !seen[cut] {
				seen[cut] = true
				cuts = append(cuts, cut)
			}
		}
		start = i + 1
	}
	if start < len(data) {
		cut := (start + len(data)) / 2
		if !seen[cut] {
			cuts = append(cuts, cut)
		}
	}
	return cuts
}

func lastCompleteLine(data []byte) int {
	if i := bytes.LastIndexByte(data, '\n'); i >= 0 {
		return i + 1
	}
	return 0
}

func assertReviewFields(t *testing.T, s *transcriptScan) {
	t.Helper()
	checks := map[string]bool{
		"uses.input": len(s.uses) > 0,
		"uses.wait": func() bool {
			for _, u := range s.uses {
				if u.wait && len(u.input) > 0 {
					return true
				}
			}
			return false
		}(),
		"uses.Bash без ожидания": func() bool {
			for _, u := range s.uses {
				if u.name == "Bash" && !u.wait {
					return true
				}
			}
			return false
		}(),
		"tasks": len(s.tasks) > 0, "agents": len(s.agents) > 0, "closed": len(s.closed) > 0,
		"expires": len(s.expires) > 0, "crons": len(s.crons) >= 3, "oldCronDeletes": len(s.oldCronDeletes) > 0,
		"fired": len(s.fired) > 0, "wakeup": !s.wakeup.IsZero(), "Compacted": !s.context.Compacted.IsZero(),
		"Human": s.context.Human != "", "requests": len(s.requests) > 0,
	}
	for field, present := range checks {
		if !present {
			t.Errorf("fixture не заполняет %s", field)
		}
	}
	var own, inherited bool
	for _, cron := range s.crons {
		own = own || cron.Own
		inherited = inherited || !cron.Own
	}
	if !own || !inherited {
		t.Errorf("fixture должен содержать свои и унаследованные crons: %+v", s.crons)
	}
}

// TestScanCacheReadRejectsReplacementInMemory — тот же отказ без сохранения: кеш в памяти не дочитывает файл,
// подменённый rename (хвост перед offset совпал, inode другой), переписанный на месте (inode тот же, хвост другой)
// или переписанный на месте раньше хвоста с mtime раньше разобранного (inode и хвост те же).
func TestScanCacheReadRejectsReplacementInMemory(t *testing.T) {
	pad := func(c string) string {
		return `{"type":"progress","timestamp":"` + tsNew + `","x":"` + strings.Repeat(c, 5000) + `"}`
	}
	original := []byte(use(tsNew, "t1", "Bash", `{"command":"sleep 100"}`) + "\n" + pad("a") + "\n")
	for mode, replacement := range map[string][]byte{
		"rename-matching-tail":    []byte(use(tsNew, "t2", "Bash", `{"command":"sleep 200"}`) + "\n" + pad("a") + "\n"),
		"same-inode-changed-tail": []byte(use(tsNew, "t2", "Bash", `{"command":"sleep 200"}`) + "\n" + pad("b") + "\n"),
		"same-inode-older-mtime":  []byte(use(tsNew, "t2", "Bash", `{"command":"sleep 200"}`) + "\n" + pad("a") + "\n"),
	} {
		t.Run(mode, func(t *testing.T) {
			since := mustTime(t, tsOld)
			file := filepath.Join(t.TempDir(), "session.jsonl")
			if err := os.WriteFile(file, original, 0o600); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(file)
			if err != nil {
				t.Fatal(err)
			}
			var c scanCache
			if _, err := c.read(file, info, since, since.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if mode != "rename-matching-tail" {
				if err := os.WriteFile(file, replacement, 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(file+".new", replacement, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(file+".new", file); err != nil {
					t.Fatal(err)
				}
			}
			later := info.ModTime().Add(time.Second)
			if mode == "same-inode-older-mtime" {
				later = info.ModTime().Add(-time.Second)
			}
			if err := os.Chtimes(file, later, later); err != nil {
				t.Fatal(err)
			}
			if info, err = os.Stat(file); err != nil {
				t.Fatal(err)
			}
			if _, err := c.read(file, info, since, since.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			want, err := scanTranscript(bytes.NewReader(replacement), since)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(c.scan, want) {
				t.Fatalf("кеш дочитал подменённый файл со старого смещения: pending %v, want %v", c.scan.pending, want.pending)
			}
		})
	}
}
