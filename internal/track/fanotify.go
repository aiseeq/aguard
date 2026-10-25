package track

import (
	"encoding/binary"
	"fmt"
	"os"
	"strconv"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Маска событий: только прямые потомки помеченных папок (метка на inode папки,
// не FAN_MARK_FILESYSTEM) — десятки событий в минуту вместо сотен.
const fanMask = uint64(unix.FAN_CREATE | unix.FAN_DELETE | unix.FAN_RENAME | unix.FAN_ONDIR)

// dirName — папка (по file handle) и имя записи в ней.
type dirName struct {
	key    string
	handle unix.FileHandle
	name   string
}

// fevent — разобранное событие fanotify.
type fevent struct {
	mask  uint64
	pid   int
	pidfd int // >=0 — открытый pidfd; FAN_NOPIDFD (-1) — процесс уже ушёл; FAN_EPIDFD (-2) — ошибка
	dn    *dirName
	old   *dirName // FAN_RENAME: откуда
	new   *dirName // FAN_RENAME: куда
	ovf   bool     // переполнение очереди
}

func (e *fevent) isDir() bool { return e.mask&unix.FAN_ONDIR != 0 }

func handleKey(t int32, b []byte) string { return strconv.Itoa(int(t)) + ":" + fmt.Sprintf("%x", b) }

// fanInit открывает группу уведомлений. Нужен CAP_SYS_ADMIN.
func fanInit() (int, error) {
	flags := uint(unix.FAN_CLASS_NOTIF | unix.FAN_CLOEXEC | unix.FAN_UNLIMITED_QUEUE |
		unix.FAN_REPORT_DFID_NAME_TARGET | unix.FAN_REPORT_PIDFD)
	return unix.FanotifyInit(flags, unix.O_RDONLY|unix.O_LARGEFILE)
}

// fanMark ставит метку на папку и возвращает ключ её file handle.
func fanMark(fd int, dir string) (string, error) {
	if err := unix.FanotifyMark(fd, unix.FAN_MARK_ADD, fanMask, unix.AT_FDCWD, dir); err != nil {
		return "", fmt.Errorf("fanotify_mark %s: %w", dir, err)
	}
	h, _, err := unix.NameToHandleAt(unix.AT_FDCWD, dir, 0)
	if err != nil {
		return "", fmt.Errorf("name_to_handle_at %s: %w", dir, err)
	}
	return handleKey(h.Type(), h.Bytes()), nil
}

// parseEvents разбирает буфер read(2) группы fanotify.
func parseEvents(b []byte) []fevent {
	var out []fevent
	mdSize := int(unsafe.Sizeof(unix.FanotifyEventMetadata{}))
	for len(b) >= mdSize {
		md := (*unix.FanotifyEventMetadata)(unsafe.Pointer(&b[0]))
		if int(md.Event_len) < mdSize || int(md.Event_len) > len(b) {
			break
		}
		ev := b[:md.Event_len]
		b = b[md.Event_len:]
		fe := fevent{mask: md.Mask, pid: int(md.Pid), pidfd: -100}
		if md.Mask&unix.FAN_Q_OVERFLOW != 0 {
			fe.ovf = true
			out = append(out, fe)
			continue
		}
		for rec := ev[md.Metadata_len:]; len(rec) >= 4; {
			typ, l := rec[0], int(binary.LittleEndian.Uint16(rec[2:4]))
			if l < 4 || l > len(rec) {
				break
			}
			body := rec[4:l]
			rec = rec[l:]
			switch typ {
			case unix.FAN_EVENT_INFO_TYPE_PIDFD:
				if len(body) >= 4 {
					fe.pidfd = int(int32(binary.LittleEndian.Uint32(body)))
				}
			case unix.FAN_EVENT_INFO_TYPE_DFID_NAME, unix.FAN_EVENT_INFO_TYPE_OLD_DFID_NAME, unix.FAN_EVENT_INFO_TYPE_NEW_DFID_NAME:
				dn := parseDirName(body)
				if dn == nil {
					continue
				}
				switch typ {
				case unix.FAN_EVENT_INFO_TYPE_DFID_NAME:
					fe.dn = dn
				case unix.FAN_EVENT_INFO_TYPE_OLD_DFID_NAME:
					fe.old = dn
				default:
					fe.new = dn
				}
			}
		}
		out = append(out, fe)
	}
	return out
}

// parseDirName: fsid(8) + file_handle{bytes u32, type i32, f_handle[bytes]} + имя\0.
func parseDirName(body []byte) *dirName {
	if len(body) < 16 {
		return nil
	}
	fh := body[8:]
	hb := int(binary.LittleEndian.Uint32(fh[0:4]))
	ht := int32(binary.LittleEndian.Uint32(fh[4:8]))
	if 8+hb > len(fh) {
		return nil
	}
	h := append([]byte(nil), fh[8:8+hb]...)
	name := fh[8+hb:]
	for i, c := range name {
		if c == 0 {
			name = name[:i]
			break
		}
	}
	return &dirName{key: handleKey(ht, h), handle: unix.NewFileHandle(ht, h), name: string(name)}
}

// resolveHandle находит путь папки по handle (для переименований из
// непомеченных папок). mountFD — открытый корень той же файловой системы.
func resolveHandle(mountFD int, h unix.FileHandle) (string, error) {
	fd, err := unix.OpenByHandleAt(mountFD, h, unix.O_PATH)
	if err != nil {
		return "", fmt.Errorf("open_by_handle_at: %w", err)
	}
	defer unix.Close(fd)
	return os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
}
