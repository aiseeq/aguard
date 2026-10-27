package disk

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"slices"
	"syscall"
	"unsafe"
)

// Занятое место на btrfs — по экстентам (FIEMAP), а не по st_blocks: reflink-копии и снимки делят экстенты, du
// считает их в каждой копии (замер llmark-master 26.10 12:38 CET по compsize: farm-hard и worker вместе 11 ГБ, du —
// 32). compsize точнее (знает сжатый размер), но читает дерево экстентов только с root; FIEMAP доступен владельцу
// файла и отдаёт длины до сжатия.

// fsIocFiemap — FS_IOC_FIEMAP: _IOWR('f', 11, struct fiemap).
const fsIocFiemap = 0xC020660B

// Флаги экстента (linux/fiemap.h).
const (
	extentLast     = 0x0001
	extentDelalloc = 0x0004 // ещё не записан: места на диске нет, адреса нет
	extentInline   = 0x0200 // данные в метаданных
	extentShared   = 0x2000 // экстент делят reflink-копии или снимки
)

// fiemapHead — struct fiemap: Length заполняется для ядра, флаги запроса и резерв — нули.
type fiemapHead struct {
	Start  uint64
	Length uint64 // читает ядро: сколько файла отдать
	_      uint32
	Mapped uint32
	Count  uint32
	_      uint32
}

// fiemapExtent — struct fiemap_extent.
type fiemapExtent struct {
	Logical, Physical, Length uint64
	_                         [2]uint64
	Flags                     uint32
	_                         [3]uint32
}

// extentBatch — экстентов за один вызов.
const extentBatch = 64

type fiemapBuf struct {
	Head    fiemapHead
	Extents [extentBatch]fiemapExtent
}

// span — общий экстент: адрес на диске и длина (у сжатого — длина до сжатия).
type span struct{ at, n uint64 }

// errNoFiemap — файловая система не отдаёт экстенты: место — по st_blocks.
var errNoFiemap = errors.New("FIEMAP не поддержан")

// extents — место файла: своё (экстенты без флага shared, их никто не делит) и общие экстенты, дописанные в shared.
func extents(path string, buf *fiemapBuf, shared []span) (int64, []span, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return 0, shared, fmt.Errorf("экстенты: %w", err)
	}
	defer f.Close()
	var own int64
	start := uint64(0)
	for {
		buf.Head = fiemapHead{Start: start, Length: ^uint64(0), Count: extentBatch}
		if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), fsIocFiemap, uintptr(unsafe.Pointer(buf))); e != 0 {
			if e == syscall.EOPNOTSUPP || e == syscall.ENOTTY {
				return 0, shared, errNoFiemap
			}
			return 0, shared, fmt.Errorf("экстенты %s: %w", path, e)
		}
		n := int(buf.Head.Mapped)
		for i := range n {
			x := &buf.Extents[i]
			switch {
			case x.Flags&extentDelalloc != 0 || x.Flags&extentInline != 0 || x.Physical == 0:
				own += int64(x.Length)
			case x.Flags&extentShared != 0:
				shared = append(shared, span{x.Physical, x.Length})
			default:
				own += int64(x.Length)
			}
		}
		if n < extentBatch || buf.Extents[n-1].Flags&extentLast != 0 {
			return own, shared, nil
		}
		start = buf.Extents[n-1].Logical + buf.Extents[n-1].Length
	}
}

// spanBytes — место общих экстентов, каждый участок диска один раз: копии одного экстента и части одного сжатого
// экстента (у сжатого адрес — начало экстента) сливаются.
func spanBytes(s []span) int64 {
	slices.SortFunc(s, func(a, b span) int { return cmp.Compare(a.at, b.at) })
	var total int64
	var end uint64
	for _, x := range s {
		if x.at >= end {
			total += int64(x.n)
			end = x.at + x.n
		} else if x.at+x.n > end {
			total += int64(x.at + x.n - end)
			end = x.at + x.n
		}
	}
	return total
}
