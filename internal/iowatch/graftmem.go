package iowatch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// GraftSliceMemReader caches the slice glob for one minute and reads each memory.current once per sample.
type GraftSliceMemReader struct {
	root     string
	uid      int
	paths    []string
	nextGlob time.Time
	globbed  bool
}

func NewGraftSliceMemReader(root string, uid int) *GraftSliceMemReader {
	return &GraftSliceMemReader{root: root, uid: uid}
}

// Sample sums current memory for all graft slices. A failed glob is retried on the next call.
func (r *GraftSliceMemReader) Sample(now time.Time) (int64, error) {
	var globErr error
	if !r.globbed || !now.Before(r.nextGlob) {
		base := filepath.Join(r.root, "user.slice", fmt.Sprintf("user-%d.slice", r.uid), fmt.Sprintf("user@%d.service", r.uid))
		paths, err := filepath.Glob(filepath.Join(base, "*", "*-graft.slice", "memory.current"))
		if err != nil {
			globErr = err
			r.nextGlob = now
		} else {
			r.paths = paths
			r.globbed = true
			r.nextGlob = now.Add(time.Minute)
		}
	}
	var total int64
	var errs []error
	if globErr != nil {
		errs = append(errs, globErr)
	}
	for _, path := range r.paths {
		b, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				r.nextGlob = now
				continue
			}
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
			r.nextGlob = now
			continue
		}
		v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
			r.nextGlob = now
			continue
		}
		total += v
	}
	return total, errors.Join(errs...)
}
