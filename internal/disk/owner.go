package disk

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// Session — живая сессия реестра: имя и папка.
type Session struct {
	Name, ID, Cwd string
}

// Owners — кому сигнал о записи: таблица [disk.owners], иначе сессия той же папки (scratchpad — по папке проекта,
// worktree — по его репозиторию): живая, иначе по имени, которое ей дал бы cl; иначе админ пространства, иначе лид.
type Owners struct {
	Table  map[string]string // папка -> хозяин (пути раскрыты)
	Admins map[string]string // префикс папки -> админ (пути раскрыты)
	Lead   string
	Live   []Session
	// Projects — папка проекта в кодировке Claude Code -> имя сессии по таблице cl (internal/names).
	Projects map[string]string
}

// ProjectKey — папка проекта cwd в кодировке Claude Code: так названы папки scratchpad и ~/.claude/projects.
func ProjectKey(cwd string) string { return projectDir(cwd) }

// projectChars — что Claude Code заменяет на «-» в имени папки проекта (~/.claude/projects, claude-tmp).
var projectChars = regexp.MustCompile(`[^A-Za-z0-9]`)

func projectDir(cwd string) string { return projectChars.ReplaceAllString(cwd, "-") }

// Of — хозяин записи path и откуда он известен.
func (o *Owners) Of(path string) (owner, why string) {
	if path == "" {
		return o.Lead, "lead"
	}
	best := ""
	for dir := range o.Table {
		if (path == dir || strings.HasPrefix(path, dir+"/")) && (len(dir) > len(best) || len(dir) == len(best) && dir < best) {
			best = dir
		}
	}
	if best != "" {
		return o.Table[best], "config"
	}
	// scratchpad сессии: …/claude-tmp/claude-<uid>/<папка проекта>/<сессия>
	if parent := filepath.Dir(path); strings.HasPrefix(filepath.Base(filepath.Dir(parent)), "claude-") &&
		filepath.Base(filepath.Dir(filepath.Dir(parent))) == "claude-tmp" {
		return o.scratchpad(filepath.Base(path), filepath.Base(parent))
	}
	repo, err := worktreeRepo(path)
	if err != nil {
		return o.Lead, "lead: " + err.Error()
	}
	if repo != "" {
		return o.worktree(repo)
	}
	return o.Lead, "lead"
}

// scratchpad — хозяин scratchpad сессии id в папке проекта project.
func (o *Owners) scratchpad(id, project string) (string, string) {
	for _, s := range o.Live {
		if s.ID == id {
			return s.Name, "session"
		}
	}
	for _, s := range o.Live {
		if projectDir(s.Cwd) == project {
			return s.Name, "project"
		}
	}
	if name := o.Projects[project]; name != "" {
		return name, "names"
	}
	return o.admin(func(prefix string) bool { return strings.HasPrefix(project, projectDir(prefix)) })
}

// worktree — хозяин worktree репозитория repo.
func (o *Owners) worktree(repo string) (string, string) {
	for _, s := range o.Live {
		if s.Cwd == repo {
			return s.Name, "worktree"
		}
	}
	if name := o.Projects[projectDir(repo)]; name != "" {
		return name, "names"
	}
	return o.admin(func(prefix string) bool { return strings.HasPrefix(repo, prefix+"/") })
}

// admin — админ по самому длинному подходящему префиксу; нет — лид.
func (o *Owners) admin(match func(prefix string) bool) (string, string) {
	best := ""
	for prefix := range o.Admins {
		if match(prefix) && (len(prefix) > len(best) || len(prefix) == len(best) && prefix < best) {
			best = prefix
		}
	}
	if best == "" {
		return o.Lead, "lead"
	}
	return o.Admins[best], "admin"
}

// worktreeRepo — репозиторий worktree по его файлу .git («gitdir: <репо>/.git/worktrees/<имя>»); не worktree (нет
// .git, .git — папка, path — файл или чужая закрытая папка, как systemd-private-* в /var/tmp) — пусто.
func worktreeRepo(path string) (string, error) {
	b, err := os.ReadFile(filepath.Join(path, ".git"))
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EISDIR) ||
		errors.Is(err, syscall.ENOTDIR) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("worktree %s: %w", path, err)
	}
	dir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir: ")
	if !ok {
		return "", nil
	}
	repo, _, ok := strings.Cut(dir, "/.git/worktrees/")
	if !ok {
		return "", nil
	}
	return repo, nil
}
