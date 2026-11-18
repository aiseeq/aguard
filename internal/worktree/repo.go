// Package worktree resolves Git worktree folders to their main repository.
package worktree

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Repo returns the main repository for a worktree folder by reading its .git
// file. A missing .git, a .git directory, a path that is a file, and permission
// errors (a foreign closed folder such as systemd-private-* in /var/tmp) mean the
// path is not a worktree; other read errors are returned.
func Repo(path string) (string, error) {
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
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(path, dir)
	}
	repo, _, ok := strings.Cut(dir, "/.git/worktrees/")
	if !ok {
		return "", nil
	}
	return repo, nil
}
