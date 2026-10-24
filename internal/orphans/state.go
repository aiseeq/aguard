package orphans

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"tmpguard/internal/record"
)

// State — память между проходами (~/.local/state/procguard/state.json).
type State struct {
	Seen   map[string]time.Time `json:"seen"`   // ключ сессии -> когда впервые замечена мёртвой
	Logged map[string]time.Time `json:"logged"` // корень дерева -> когда записан в журнал «снял бы»
}

// forgetAfter — сколько помнить мёртвую сессию, у которой не осталось процессов.
const forgetAfter = 7 * 24 * time.Hour

// LoadState читает состояние; нет файла — пустое.
func LoadState(path string) (*State, error) {
	st := &State{Seen: map[string]time.Time{}, Logged: map[string]time.Time{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, fmt.Errorf("состояние: %w", err)
	}
	if err := json.Unmarshal(b, st); err != nil {
		return st, fmt.Errorf("состояние %s: %w", path, err)
	}
	if st.Seen == nil {
		st.Seen = map[string]time.Time{}
	}
	if st.Logged == nil {
		st.Logged = map[string]time.Time{}
	}
	return st, nil
}

// Save пишет состояние атомарно.
func (st *State) Save(path string) error {
	b, err := json.MarshalIndent(st, "", " ")
	if err != nil {
		return fmt.Errorf("состояние: %w", err)
	}
	if err := record.WriteFileAtomic(path, b, 0o600); err != nil {
		return fmt.Errorf("состояние: %w", err)
	}
	return nil
}

// Prune забывает корни, которых больше нет, и давно пустые мёртвые сессии.
func (st *State) Prune(trees []Tree, now time.Time) {
	roots, sess := map[string]bool{}, map[string]bool{}
	for i := range trees {
		roots[trees[i].RootKey()] = true
		sess[trees[i].Session.String()] = true
	}
	for k := range st.Logged {
		if !roots[k] {
			delete(st.Logged, k)
		}
	}
	for k, t := range st.Seen {
		if !sess[k] && now.Sub(t) > forgetAfter {
			delete(st.Seen, k)
		}
	}
}

// SessionInfo — что известно о мёртвой сессии по её транскрипту.
type SessionInfo struct {
	Cwd        string    // cwd сессии (из транскрипта)
	Transcript string    // путь транскрипта
	LastWrite  time.Time // последняя запись транскрипта: сессия умерла не раньше
}

// maxScanLines — сколько строк транскрипта читать в поисках cwd.
const maxScanLines = 400

var cwdField = regexp.MustCompile(`"cwd":"([^"]+)"`)

// Describe ищет транскрипт сессии id в projectsDir (~/.claude/projects/*/<id>.jsonl).
// Не нашёлся — пустой SessionInfo без ошибки.
func Describe(projectsDir, id string) (SessionInfo, error) {
	var in SessionInfo
	if id == "" {
		return in, nil
	}
	m, err := filepath.Glob(filepath.Join(projectsDir, "*", id+".jsonl"))
	if err != nil || len(m) == 0 {
		return in, err
	}
	in.Transcript = m[0]
	fi, err := os.Stat(in.Transcript)
	if err != nil {
		return in, fmt.Errorf("транскрипт: %w", err)
	}
	in.LastWrite = fi.ModTime()
	f, err := os.Open(in.Transcript)
	if err != nil {
		return in, fmt.Errorf("транскрипт: %w", err)
	}
	defer f.Close()
	in.Cwd, err = firstCwd(f)
	return in, err
}

func firstCwd(r io.Reader) (string, error) {
	const maxLine = 16 << 20
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<16), maxLine)
	for n := 0; n < maxScanLines && sc.Scan(); n++ {
		if m := cwdField.FindSubmatch(sc.Bytes()); m != nil {
			return string(m[1]), nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("транскрипт: %w", err)
	}
	return "", nil
}
