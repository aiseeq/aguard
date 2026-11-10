package restart

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// freeNow — Free свежим судьёй: кэш разбора прошлого вызова не прячет то, что сломано после него.
func (w sessionWorld) freeNow(t *testing.T, after time.Duration, now time.Time) (bool, string) {
	t.Helper()
	j := NewJudge(w.dir)
	j.paths = &w.p
	return j.Free(&Config{}, 4242, after, now)
}

// startedAt переписывает время запуска сессии в реестре.
func (w sessionWorld) startedAt(t *testing.T, was, at time.Time) {
	t.Helper()
	path := filepath.Join(w.dir, "session.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	old := `"startedAt":` + strconv.FormatInt(was.UnixMilli(), 10)
	if !strings.Contains(string(b), old) {
		t.Fatalf("нет %s в %s", old, b)
	}
	write(t, path, strings.Replace(string(b), old, `"startedAt":`+strconv.FormatInt(at.UnixMilli(), 10), 1))
}

// Консоль без транскрипта простаивает с запуска: запущена меньше free_after назад — ещё не свободна.
func TestFreeFreshConsoleNoTranscriptJustStarted(t *testing.T) {
	now := time.Now()
	const after = 2 * time.Minute
	w := newSessionWorld(t, worldTimes{now: now, turnAgo: 10 * time.Minute, ttyAgo: 10 * time.Minute})
	w.link(t, 4242)
	if err := os.Remove(filepath.Join(w.p.projects, "-home-u-git-x", "s1.jsonl")); err != nil {
		t.Fatal(err)
	}
	w.startedAt(t, now.Add(-time.Hour), now.Add(-time.Minute))
	if ok, why := w.freeNow(t, after, now); ok || why != "сессия недавно работала" {
		t.Fatalf("запущена минуту назад: %v %s", ok, why)
	}
	if ok, why := w.freeNow(t, after, now.Add(after)); !ok {
		t.Fatalf("запущена free_after назад: %s", why)
	}
}

// Настоящая ошибка чтения транскрипта — не свобода, и причина отказа — именно она: транскрипт старше free_after.
func TestFreeTranscriptReadErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root читает файлы без прав")
	}
	const after = 2 * time.Minute
	old := func(t *testing.T, path string) {
		t.Helper()
		at := time.Now().Add(-3 * time.Hour)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	locked := func(t *testing.T, path string) {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, info.Mode().Perm()) }) // иначе TempDir не убрать
	}
	cases := []struct {
		name  string
		spoil func(t *testing.T, w sessionWorld, transcript string)
	}{
		{"папка вместо файла", func(t *testing.T, _ sessionWorld, transcript string) {
			if err := os.Remove(transcript); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(transcript, 0o755); err != nil {
				t.Fatal(err)
			}
			old(t, transcript)
		}},
		{"файл без прав на чтение", func(t *testing.T, _ sessionWorld, transcript string) {
			locked(t, transcript)
		}},
		{"папка проекта без прав: stat транскрипта", func(t *testing.T, w sessionWorld, _ string) {
			other := filepath.Join(w.p.projects, "-home-u-other")
			if err := os.Mkdir(other, 0o755); err != nil {
				t.Fatal(err)
			}
			locked(t, other)
		}},
		{"папка проектов без прав: обход", func(t *testing.T, w sessionWorld, _ string) {
			locked(t, w.p.projects)
		}},
		{"два транскрипта сессии", func(t *testing.T, w sessionWorld, _ string) {
			twin := filepath.Join(w.p.projects, "-home-u-other", "s1.jsonl")
			write(t, twin, "\n")
			old(t, twin)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			now := time.Now()
			w := newSessionWorld(t, worldTimes{now: now, turnAgo: 10 * time.Minute, ttyAgo: 10 * time.Minute})
			w.link(t, 4242)
			if ok, why := w.freeNow(t, after, now); !ok {
				t.Fatalf("предусловие: целая сессия свободна: %s", why)
			}
			c.spoil(t, w, filepath.Join(w.p.projects, "-home-u-git-x", "s1.jsonl"))
			if ok, why := w.freeNow(t, after, now); ok || why != "ошибка чтения фактов" {
				t.Fatalf("%v %s", ok, why)
			}
		})
	}
}

// Обычный файл в папке транскриптов (в проде — ~/.claude/projects/subcount.tsv) — не папка проекта: свобода сессии от него не
// зависит.
func TestFreeIgnoresPlainFileInProjects(t *testing.T) {
	now := time.Now()
	const after = 2 * time.Minute
	w := newSessionWorld(t, worldTimes{now: now, turnAgo: 10 * time.Minute, ttyAgo: 10 * time.Minute})
	w.link(t, 4242)
	write(t, filepath.Join(w.p.projects, "subcount.tsv"), "x\t1\n")
	if ok, why := w.freeNow(t, after, now); !ok {
		t.Fatalf("файл в папке транскриптов помешал: %s", why)
	}
}
