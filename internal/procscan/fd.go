// Package procscan contains small, shared readers for live process entries.
package procscan

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/aiseeq/akit/proc"
)

// ReadFDLinks visits the readable symlink targets in a process fd directory.
// A process that exits or a directory hidden by proc permissions is skipped.
func ReadFDLinks(fdDir string, visit func(string)) error {
	fds, err := os.ReadDir(fdDir)
	if proc.Gone(err) || os.IsPermission(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open fd directory: %w", err)
	}
	for _, fd := range fds {
		link, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
		if proc.Gone(err) || os.IsPermission(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read fd %s: %w", fd.Name(), err)
		}
		visit(link)
	}
	return nil
}
