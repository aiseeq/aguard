package track

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// meta — сведения о записи верхнего уровня из statx.
type meta struct {
	dir  bool
	ino  uint64
	uid  int
	born time.Time // btime, если файловая система его знает, иначе mtime
}

// statMeta — statx записи; ошибка (обычно ENOENT) — записи нет.
func statMeta(path string) (meta, error) {
	var sx unix.Statx_t
	err := unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW,
		unix.STATX_TYPE|unix.STATX_INO|unix.STATX_UID|unix.STATX_BTIME|unix.STATX_MTIME, &sx)
	if err != nil {
		return meta{}, err
	}
	m := meta{dir: sx.Mode&unix.S_IFMT == unix.S_IFDIR, ino: sx.Ino, uid: int(sx.Uid)}
	if sx.Mask&unix.STATX_BTIME != 0 && sx.Btime.Sec > 0 {
		m.born = time.Unix(sx.Btime.Sec, int64(sx.Btime.Nsec))
	} else {
		m.born = time.Unix(sx.Mtime.Sec, int64(sx.Mtime.Nsec))
	}
	return m, nil
}

// exists — запись есть (любая ошибка statx кроме ENOENT тоже считается «есть»,
// чтобы не закрыть живую запись из-за сбоя).
func exists(path string) bool {
	_, err := statMeta(path)
	return !errors.Is(err, unix.ENOENT)
}

// usage — занятое место записи (блоки, то есть реальная память tmpfs) и число файлов.
type usage struct {
	path  string
	ino   uint64
	ok    bool
	bytes int64
	files int64
}

type inoKey struct {
	dev uint64
	ino uint64
}

// measure считает du записи: не выходит за пределы файловой системы,
// жёсткие ссылки внутри записи считает один раз.
func measure(path string, ino uint64) usage {
	u := usage{path: path, ino: ino}
	var st unix.Stat_t
	if unix.Lstat(path, &st) != nil || (ino != 0 && st.Ino != ino) {
		return u
	}
	u.ok = true
	seen := map[inoKey]bool{}
	add := func(s *unix.Stat_t) {
		if s.Nlink > 1 && s.Mode&unix.S_IFMT != unix.S_IFDIR {
			k := inoKey{s.Dev, s.Ino}
			if seen[k] {
				return
			}
			seen[k] = true
		}
		u.bytes += s.Blocks * 512
		if s.Mode&unix.S_IFMT != unix.S_IFDIR {
			u.files++
		}
	}
	add(&st)
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		walk(path, st.Dev, add)
	}
	return u
}

func walk(dir string, dev uint64, add func(*unix.Stat_t)) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		var st unix.Stat_t
		if unix.Lstat(p, &st) != nil || st.Dev != dev {
			continue
		}
		add(&st)
		if st.Mode&unix.S_IFMT == unix.S_IFDIR {
			walk(p, dev, add)
		}
	}
}

// listDir — имена прямых потомков папки; ошибка, если папки нет.
func listDir(dir string) ([]string, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return names, err
	}
	return names, nil
}
