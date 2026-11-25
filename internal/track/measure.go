package track

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// meta — сведения о записи верхнего уровня из statx.
type meta struct {
	dir     bool
	ino     uint64
	uid     int
	born    time.Time // btime, если файловая система его знает, иначе mtime
	hasBorn bool
}

// statMeta — statx записи; ошибка (обычно ENOENT) — записи нет.
func statMeta(path string) (meta, error) {
	var sx unix.Statx_t
	err := unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW,
		unix.STATX_TYPE|unix.STATX_INO|unix.STATX_UID|unix.STATX_BTIME|unix.STATX_MTIME, &sx)
	if err != nil {
		return meta{}, fmt.Errorf("statx %s: %w", path, err)
	}
	m := meta{dir: sx.Mode&unix.S_IFMT == unix.S_IFDIR, ino: sx.Ino, uid: int(sx.Uid)}
	if sx.Mask&unix.STATX_BTIME != 0 && sx.Btime.Sec > 0 {
		m.hasBorn = true
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

// yieldEvery — через сколько записей каталога обход большой записи спрашивает, не занята ли машина. Вопрос сам по
// себе дёшев: Yielder меряет машину не чаще раза в секунду, между замерами это сравнение времени. N ограничивает
// только чтение после того, как секунда прошла: 256 lstat при холодном кэше метаданных — по оценке (~0,1 мс случайного
// чтения NVMe на lstat, не замер) ~25 мс; у обхода disk 1024 файла: на файл там ещё FIEMAP, вдвое больше вызовов.
const yieldEvery = 256

// pacer — уступка занятой машине внутри одного замера: перед каждой записью и папкой-контейнером и каждые yieldEvery
// записей каталога внутри обхода (счёт — на весь замер); паузы и их время — в итог замера.
type pacer struct {
	ctx     context.Context
	yield   func(context.Context) (bool, error) // пауза и ошибка замера; nil — без уступки
	seen    int
	stopped bool // замер отменён: обход сворачивается на ближайшей записи
	pauses  int
	paused  time.Duration
}

// wait — уступка; ошибка давления останавливает неполный замер и возвращается вызывающему коду.
func (p *pacer) wait() (bool, error) {
	if p.ctx.Err() == nil && p.yield != nil { // уже отменён — не меряем машину и не ждём
		start := time.Now()
		paused, err := p.yield(p.ctx)
		if paused {
			p.pauses++
			p.paused += time.Since(start)
		}
		if err != nil {
			p.stopped = true
			return false, err
		}
	}
	p.stopped = p.ctx.Err() != nil
	return !p.stopped, nil
}

// step — одна запись каталога в обходе: каждая yieldEvery-я — уступка; false — замер отменён (и во вложенной папке:
// родитель сворачивается на своей следующей записи).
func (p *pacer) step() (bool, error) {
	if p.stopped {
		return false, nil
	}
	if p.seen++; p.seen%yieldEvery == 0 {
		return p.wait()
	}
	return true, nil
}

// measure считает du записи: не выходит за пределы файловой системы,
// жёсткие ссылки внутри записи считает один раз. Отмена замера посреди обхода — размер неполон, итог замера
// помечается прерванным и не применяется (runMeasure).
func measure(path string, ino uint64, p *pacer) (usage, error) {
	u := usage{path: path, ino: ino}
	var st unix.Stat_t
	if unix.Lstat(path, &st) != nil || (ino != 0 && st.Ino != ino) {
		return u, nil
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
		fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil { //nolint:silent-error-handling // папку удалили или закрыли права во время замера: в размере — сама папка, как у нечитаемой вложенной
			return u, nil
		}
		if err := walkFD(fd, st.Dev, add, p); err != nil {
			return u, err
		}
	}
	return u, nil
}

func walkFD(fd int, dev uint64, add func(*unix.Stat_t), p *pacer) error {
	dir := os.NewFile(uintptr(fd), "directory")
	defer dir.Close()
	ents, err := dir.Readdirnames(-1)
	if err != nil { //nolint:silent-error-handling // папку удалили во время замера: в размер идёт то, что успели обойти
		return nil
	}
	var st unix.Stat_t
	for _, name := range ents {
		continueWalk, err := p.step()
		if err != nil {
			return err
		}
		if !continueWalk {
			return nil // замер отменён
		}
		if unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW) != nil || st.Dev != dev {
			continue
		}
		add(&st)
		if st.Mode&unix.S_IFMT == unix.S_IFDIR {
			child, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			if err != nil { //nolint:silent-error-handling // вложенную папку удалили или она нечитаема: в размере — сама папка, без содержимого
				continue
			}
			if err := walkFD(child, dev, add, p); err != nil {
				return err
			}
		}
	}
	return nil
}

// listDir — имена прямых потомков папки; ошибка, если папки нет.
func listDir(dir string) ([]string, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, fmt.Errorf("папка %s: %w", dir, err)
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return names, fmt.Errorf("папка %s: %w", dir, err)
	}
	return names, nil
}
