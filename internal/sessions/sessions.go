// Package sessions читает реестр сессий Claude Code (~/.claude/sessions/<pid>.json)
// и проверяет, жива ли сессия: pid существует и время его старта совпадает
// с procStart из реестра (иначе pid переиспользован другим процессом).
package sessions

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Info — запись реестра сессии.
type Info struct {
	Pid        int    `json:"pid"`
	SessionID  string `json:"sessionId"`
	Cwd        string `json:"cwd"`
	ProcStart  string `json:"procStart"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	StartedAt  int64  `json:"startedAt"`
	SocketPath string `json:"messagingSocketPath"`
}

// Registry — папка реестра одной учётки и корень /proc (подменяется в тестах).
type Registry struct {
	Dir      string
	ProcRoot string
}

// ForHome — реестр в домашней папке пользователя.
func ForHome(home string) Registry {
	return Registry{Dir: filepath.Join(home, ".claude", "sessions"), ProcRoot: "/proc"}
}

// Stat — нужные поля /proc/<pid>/stat.
type Stat struct {
	Comm  string
	Ppid  int
	Start string // поле 22, время старта в тиках с загрузки
}

// ProcStat читает /proc/<pid>/stat. Ошибка — процесса нет (или он недоступен).
func ProcStat(procRoot string, pid int) (Stat, error) {
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return Stat{}, err
	}
	return ParseStat(string(b))
}

// ParseStat разбирает строку /proc/<pid>/stat. comm может содержать пробелы
// и скобки, поэтому режем по последней ')'.
func ParseStat(s string) (Stat, error) {
	l, r := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if l < 0 || r < l || r+2 > len(s) {
		return Stat{}, fmt.Errorf("stat: нет comm в %.40q", s)
	}
	st := Stat{Comm: s[l+1 : r]}
	f := strings.Fields(s[r+2:])
	// после comm идут поля 3.. ; поле 22 (starttime) — f[19]
	if len(f) < 20 {
		return st, fmt.Errorf("stat: полей %d, нужно 20", len(f))
	}
	ppid, err := strconv.Atoi(f[1])
	if err != nil {
		return st, fmt.Errorf("stat: ppid: %w", err)
	}
	st.Ppid, st.Start = ppid, f[19]
	return st, nil
}

// Get читает запись реестра по pid; нет файла — (nil, nil).
func (r Registry) Get(pid int) (*Info, error) {
	b, err := os.ReadFile(filepath.Join(r.Dir, strconv.Itoa(pid)+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var in Info
	if err := json.Unmarshal(b, &in); err != nil {
		return nil, err
	}
	return &in, nil
}

// List читает все записи реестра (живые и мёртвые). Нечитаемые записи
// пропускаются и возвращаются ошибкой вместе с прочитанными.
func (r Registry) List() ([]Info, error) {
	ents, err := os.ReadDir(r.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Info
	var errs []error
	for _, e := range ents {
		n := e.Name()
		if !strings.HasSuffix(n, ".json") {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSuffix(n, ".json"))
		if err != nil {
			continue
		}
		in, err := r.Get(pid)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", n, err)) // файл мог писаться прямо сейчас
			continue
		}
		if in != nil {
			out = append(out, *in)
		}
	}
	return out, errors.Join(errs...)
}

// IsAlive — процесс pid жив и стартовал в procStart.
func (r Registry) IsAlive(pid int, procStart string) bool {
	if pid <= 0 || procStart == "" {
		return false
	}
	st, err := ProcStat(r.ProcRoot, pid)
	return err == nil && st.Start == procStart
}

// Alive — живые сессии по имени; при совпадении имён берётся самая свежая.
// Ошибка чтения части реестра не мешает вернуть остальное.
func (r Registry) Alive() (map[string]Info, error) {
	all, err := r.List() // при ошибке — что прочиталось, ошибка уходит вызывающему
	out := map[string]Info{}
	for _, in := range all {
		if in.Name == "" || !r.IsAlive(in.Pid, in.ProcStart) {
			continue
		}
		if old, ok := out[in.Name]; ok && old.StartedAt > in.StartedAt {
			continue
		}
		out[in.Name] = in
	}
	return out, err
}

// Token — peerToken из <pid>.<sha256>.key; нет файла — пустая строка без ошибки.
func (r Registry) Token(pid int) (string, error) {
	m, err := filepath.Glob(filepath.Join(r.Dir, strconv.Itoa(pid)+".*.key"))
	if err != nil {
		return "", err
	}
	for _, p := range m {
		b, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		var k struct {
			PeerToken string `json:"peerToken"`
		}
		if err := json.Unmarshal(b, &k); err != nil {
			return "", fmt.Errorf("%s: %w", p, err)
		}
		if k.PeerToken != "" {
			return k.PeerToken, nil
		}
	}
	return "", nil
}
