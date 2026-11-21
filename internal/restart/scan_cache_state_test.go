package restart

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/statefile"
)

func TestScanCacheContinueMatchesFullScan(t *testing.T) {
	since := mustTime(t, tsNew)
	lines := []string{
		`{"type":"user","timestamp":"` + tsNew + `","message":{"content":"вопрос"}}`,
		use(tsNew, "tool-1", "Bash", `{"command":"sleep 20"}`),
		result(tsNew, "tool-1", "готово", `null`),
		`{"type":"assistant","timestamp":"2026-10-24T10:00:30Z","message":{"usage":{"input_tokens":12,"output_tokens":3},"content":[{"type":"text","text":"ответ"}]}}`,
		`{"type":"system","subtype":"compact_boundary","timestamp":"2026-10-24T10:01:00Z","compactMetadata":{"trigger":"auto"}}`,
		`{"type":"user","timestamp":"2026-10-24T10:02:00Z","message":{"content":"/clear"}}`,
		use(tsNew, "tool-2", "CronCreate", `{"cron":"0 9 * * *","prompt":"утро","recurring":true}`),
		result(tsNew, "tool-2", "ok", `{"id":"cron-1","recurring":true}`),
	}
	data := []byte(stringsJoinLines(lines) + `{"type":"assistant","timestamp":"` + tsNew + `","message":{"content":[`)
	cut := len(stringsJoinLines(lines))
	file := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(file, data[:cut], 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	cache := &cached{used: time.Now()}
	cache.scan.pathSession, cache.scan.path, cache.scan.file = "session", "project", file
	cache.scan.size, cache.scan.mtime, cache.scan.since = info.Size(), info.ModTime(), since
	cache.scan.scan = newScan(since)
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	n, err := cache.scan.scan.feed(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	cache.scan.offset = n
	cache.scan.inode, cache.scan.prefixHash, err = transcriptPrefix(file, n)
	if err != nil {
		t.Fatal(err)
	}

	j := NewJudge(t.TempDir())
	j.caches[77] = cache
	statePath := filepath.Join(t.TempDir(), "restart", "state.json")
	if err := j.LoadScanCache(statePath, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := j.SaveScanCache(time.Now()); err != nil {
		t.Fatal(err)
	}
	restarted := NewJudge(t.TempDir())
	if err := restarted.LoadScanCache(statePath, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(restarted.caches) != 1 {
		t.Fatalf("восстановлено кэшей: %d", len(restarted.caches))
	}
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	restored := restarted.caches[77].scan
	if _, err := restored.read(file, info, since, since.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	want, err := scanTranscript(bytes.NewReader(data), since)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored.scan, want) {
		gotJSON, _ := json.Marshal(restored.scan.state())
		wantJSON, _ := json.Marshal(want.state())
		t.Fatalf("состояние после продолжения отличается от полного разбора (since=%v userAt=%v context=%v maps=%v/%v/%v/%v/%v/%v/%v/%v/%v requests=%v)\ngot: %s\nwant: %s",
			reflect.DeepEqual(restored.scan.since, want.since), reflect.DeepEqual(restored.scan.userAt, want.userAt),
			reflect.DeepEqual(restored.scan.context, want.context), reflect.DeepEqual(restored.scan.uses, want.uses),
			reflect.DeepEqual(restored.scan.pending, want.pending), reflect.DeepEqual(restored.scan.tasks, want.tasks),
			reflect.DeepEqual(restored.scan.agents, want.agents), reflect.DeepEqual(restored.scan.closed, want.closed),
			reflect.DeepEqual(restored.scan.expires, want.expires), reflect.DeepEqual(restored.scan.crons, want.crons),
			reflect.DeepEqual(restored.scan.oldCronDeletes, want.oldCronDeletes), reflect.DeepEqual(restored.scan.fired, want.fired),
			reflect.DeepEqual(restored.scan.requests, want.requests), gotJSON, wantJSON)
	}
}

func TestScanCacheRestoreRejectsUncertainState(t *testing.T) {
	for _, tc := range []string{"format-version", "truncated-file", "older-mtime", "negative-offset", "offset-past-size", "expired"} {
		t.Run(tc, func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, "session.jsonl")
			data := []byte(use(tsNew, "t1", "Bash", `{"command":"sleep 1"}`) + "\n")
			if err := os.WriteFile(file, data, 0o600); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(file)
			if err != nil {
				t.Fatal(err)
			}
			scan := newScan(mustTime(t, tsNew))
			if _, err := scan.feed(bytes.NewReader(data)); err != nil {
				t.Fatal(err)
			}
			inode, prefixHash, err := transcriptPrefix(file, int64(len(data)))
			if err != nil {
				t.Fatal(err)
			}
			j := NewJudge(dir)
			j.caches[91] = &cached{used: time.Now(), scan: scanCache{file: file, size: info.Size(), mtime: info.ModTime(),
				since: scan.since, offset: int64(len(data)), inode: inode, prefixHash: prefixHash, scan: scan}}
			statePath := filepath.Join(dir, "restart", "state.json")
			j.statePath = statePath
			if err := j.SaveScanCache(time.Now()); err != nil {
				t.Fatal(err)
			}
			switch tc {
			case "format-version":
				stateBytes, err := statefile.Load(statePath)
				if err != nil {
					t.Fatal(err)
				}
				var state savedScanCache
				if err := json.Unmarshal(stateBytes, &state); err != nil {
					t.Fatal(err)
				}
				state.Version++
				stateBytes, err = json.Marshal(state)
				if err != nil {
					t.Fatal(err)
				}
				if err := statefile.Save(statePath, stateBytes); err != nil {
					t.Fatal(err)
				}
			case "negative-offset", "offset-past-size":
				stateBytes, err := statefile.Load(statePath)
				if err != nil {
					t.Fatal(err)
				}
				var state savedScanCache
				if err := json.Unmarshal(stateBytes, &state); err != nil {
					t.Fatal(err)
				}
				if tc == "negative-offset" {
					state.Caches[91].Offset = -1
				} else {
					state.Caches[91].Offset = state.Caches[91].Size + 1
				}
				stateBytes, err = json.Marshal(state)
				if err != nil {
					t.Fatal(err)
				}
				if err := statefile.Save(statePath, stateBytes); err != nil {
					t.Fatal(err)
				}
			case "truncated-file":
				if err := os.WriteFile(file, data[:len(data)-2], 0o600); err != nil {
					t.Fatal(err)
				}
			case "older-mtime":
				old := info.ModTime().Add(-time.Minute)
				if err := os.Chtimes(file, old, old); err != nil {
					t.Fatal(err)
				}
			}
			loadAt := time.Now()
			if tc == "expired" {
				loadAt = loadAt.Add(cacheTTL + time.Second)
			}
			restarted := NewJudge(dir)
			if err := restarted.LoadScanCache(statePath, loadAt); err != nil {
				t.Fatal(err)
			}
			if len(restarted.caches) != 0 {
				t.Fatalf("сомнительный кеш восстановлен: %s", tc)
			}
		})
	}
}

func TestScanCacheSaveDropsExpiredEntries(t *testing.T) {
	now := time.Now()
	j := NewJudge(t.TempDir())
	j.statePath = filepath.Join(t.TempDir(), "restart", "state.json")
	j.caches[5] = &cached{used: now.Add(-cacheTTL - time.Second), scan: scanCache{scan: newScan(now)}}
	if err := j.SaveScanCache(now); err != nil {
		t.Fatal(err)
	}
	b, err := statefile.Load(j.statePath)
	if err != nil {
		t.Fatal(err)
	}
	var saved savedScanCache
	if err := json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Caches) != 0 {
		t.Fatalf("просроченные записи остались на диске: %d", len(saved.Caches))
	}
}

func TestScanCacheLoadSkipsDeletedTranscript(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "restart", "state.json")
	judge := NewJudge(dir)
	for pid := 1; pid <= 2; pid++ {
		file := filepath.Join(dir, fmt.Sprintf("s%d.jsonl", pid))
		data := []byte(use(tsNew, fmt.Sprintf("t%d", pid), "Bash", `{"command":"sleep 10"}`) + "\n")
		if err := os.WriteFile(file, data, 0o600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		scan := newScan(mustTime(t, tsNew))
		if _, err := scan.feed(bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		inode, prefixHash, err := transcriptPrefix(file, int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		judge.caches[pid] = &cached{used: time.Now(), scan: scanCache{file: file, size: info.Size(), mtime: info.ModTime(),
			since: scan.since, offset: int64(len(data)), inode: inode, prefixHash: prefixHash, scan: scan}}
	}
	judge.statePath = statePath
	if err := judge.SaveScanCache(time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "s1.jsonl")); err != nil {
		t.Fatal(err)
	}
	restored := NewJudge(dir)
	if err := restored.LoadScanCache(statePath, time.Now()); err != nil {
		t.Fatalf("удалённый транскрипт не должен ломать остальные: %v", err)
	}
	if len(restored.caches) != 1 || restored.caches[2] == nil {
		t.Fatalf("восстановлены не все сохранившиеся записи: %+v", restored.caches)
	}
}

func TestScanCacheCorruptStateResetsForNextSave(t *testing.T) {
	for _, bad := range []struct{ snapshot, journal string }{
		{snapshot: `{"version":`}, {snapshot: `[]`},
		{snapshot: `{"version":3,"caches":{}}`, journal: "not-json\n"},
		{snapshot: `{"version":3,"caches":{}}`, journal: `{"merge":{"version":{"set":{"x":1}}}}` + "\n"},
	} {
		t.Run(bad.snapshot+bad.journal, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "restart", "state.json")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(bad.snapshot), 0o600); err != nil {
				t.Fatal(err)
			}
			if bad.journal != "" {
				if err := os.WriteFile(path+".journal", []byte(bad.journal), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			j := NewJudge(dir)
			if err := j.LoadScanCache(path, time.Now()); err != nil {
				t.Fatalf("битое состояние сбрасывается без ошибки: %v", err)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("битый снимок остался: %v", err)
			}
			if _, err := os.Stat(path + ".journal"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("битый журнал остался: %v", err)
			}
			if err := j.SaveScanCache(time.Now()); err != nil {
				t.Fatalf("после сброса новое состояние не сохраняется: %v", err)
			}
			if _, err := statefile.Load(path); err != nil {
				t.Fatalf("новое состояние не читается: %v", err)
			}
		})
	}
}

func TestScanCacheRestoreKeepsOriginalAge(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	file := filepath.Join(dir, "session.jsonl")
	data := []byte(use(tsNew, "t1", "Bash", `{"command":"sleep 10"}`) + "\n")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	scan := newScan(mustTime(t, tsNew))
	if _, err := scan.feed(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	inode, prefixHash, err := transcriptPrefix(file, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "restart", "state.json")
	seed := NewJudge(dir)
	seed.statePath = statePath
	seed.caches[1] = &cached{used: now.Add(-cacheTTL / 2), scan: scanCache{file: file, size: info.Size(), mtime: info.ModTime(),
		since: scan.since, offset: int64(len(data)), inode: inode, prefixHash: prefixHash, scan: scan}}
	if err := seed.SaveScanCache(now); err != nil {
		t.Fatal(err)
	}
	restarted := NewJudge(dir)
	if err := restarted.LoadScanCache(statePath, now); err != nil {
		t.Fatal(err)
	}
	// Через TTL/2+1s после загрузки запись с исходным возрастом (TTL/2) уже стара, а со сброшенным — ещё нет.
	if err := restarted.SaveScanCache(now.Add(cacheTTL/2 + time.Second)); err != nil {
		t.Fatal(err)
	}
	b, err := statefile.Load(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var saved savedScanCache
	if err := json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Caches) != 0 {
		t.Fatal("при восстановлении возраст кэша сбросился")
	}
}

func TestScanCacheRestoreRejectsInvalidPID(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "session.jsonl")
	data := []byte(use(tsNew, "t1", "Bash", `{"command":"sleep 10"}`) + "\n")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	scan := newScan(mustTime(t, tsNew))
	if _, err := scan.feed(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	inode, prefixHash, err := transcriptPrefix(file, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	j := NewJudge(dir)
	j.statePath = filepath.Join(dir, "restart", "state.json")
	j.caches[0] = &cached{used: time.Now(), scan: scanCache{file: file, size: info.Size(), mtime: info.ModTime(),
		since: scan.since, offset: int64(len(data)), inode: inode, prefixHash: prefixHash, scan: scan}}
	if err := j.SaveScanCache(time.Now()); err != nil {
		t.Fatal(err)
	}
	restarted := NewJudge(dir)
	if err := restarted.LoadScanCache(j.statePath, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(restarted.caches) != 0 {
		t.Fatal("кеш с pid=0 восстановлен")
	}
}

func TestScanCacheIntervalMustBePositive(t *testing.T) {
	cfg := Default(t.TempDir(), t.TempDir())
	cfg.ScanCacheInterval = 0
	if err := cfg.Validate(t.TempDir()); err == nil {
		t.Fatal("нулевой интервал сохранения принят")
	}
}

func stringsJoinLines(lines []string) string {
	return strings.Join(lines, "\n") + "\n"
}

func BenchmarkScanCacheStartup(b *testing.B) {
	const targetSize = 100 << 20
	line := `{"type":"assistant","timestamp":"2026-10-24T10:00:00Z","message":{"usage":{"input_tokens":100,"output_tokens":1},"content":[{"type":"text","text":"ответ"}]}}` + "\n"
	var data []byte
	for len(data)+len(line) <= targetSize {
		data = append(data, line...)
	}
	data = append(data, []byte(stringsJoinLines([]string{use(tsNew, "tail", "Bash", `{"command":"sleep 1"}`)}))...)
	dir := b.TempDir()
	file := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		b.Fatal(err)
	}
	since, err := time.Parse(time.RFC3339Nano, tsNew)
	if err != nil {
		b.Fatal(err)
	}
	mid := (len(data) / 2)
	mid -= mid % len(line)
	seedPath := filepath.Join(dir, "seed", "state.json")
	if err := os.WriteFile(file, data[:mid], 0o600); err != nil {
		b.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil {
		b.Fatal(err)
	}
	seed := NewJudge(dir)
	seed.statePath = seedPath
	entry := &cached{used: time.Now()}
	entry.scan = scanCache{file: file, size: info.Size(), mtime: info.ModTime(), since: since, scan: newScan(since)}
	f, err := os.Open(file)
	if err != nil {
		b.Fatal(err)
	}
	entry.scan.offset, err = entry.scan.scan.feed(f)
	f.Close()
	if err != nil {
		b.Fatal(err)
	}
	entry.scan.inode, entry.scan.prefixHash, err = transcriptPrefix(file, entry.scan.offset)
	if err != nil {
		b.Fatal(err)
	}
	seed.caches[1] = entry
	if err := seed.SaveScanCache(time.Now()); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(file, data, 0o600); err != nil {
		b.Fatal(err)
	}
	info, err = os.Stat(file)
	if err != nil {
		b.Fatal(err)
	}

	b.Run("from-zero", func(b *testing.B) {
		b.SetBytes(int64(len(data)))
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			s, err := scanTranscript(bytes.NewReader(data), since)
			if err != nil {
				b.Fatal(err)
			}
			_ = s.facts(time.Now())
		}
	})
	b.Run("saved-cache", func(b *testing.B) {
		b.SetBytes(int64(len(data) - mid))
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			j := NewJudge(dir)
			if err := j.LoadScanCache(seedPath, time.Now()); err != nil {
				b.Fatal(err)
			}
			s := j.caches[1].scan
			if _, err := s.read(file, info, since, time.Now()); err != nil {
				b.Fatal(err)
			}
			_ = s.scan.facts(time.Now())
		}
	})
}
