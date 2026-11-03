// Package trackfd — передача пользовательскому демону дескрипторов, которые может открыть только root: группа fanotify
// с FAN_REPORT_PIDFD и подписанный сокет proc connector. Логика слежения — в aguard (internal/track) и обновляется без
// sudo; root-помощник только открывает дескрипторы и отдаёт их, событий не читает и ничего не исполняет.
//
// На каждое подключение — новая пара: закрылись у демона — группа и её очередь освобождены. Удерживать группу на время
// простоя демона нельзя: у вышедших за простой процессов не узнать создателя, время событий сдвигается, а очередь
// заряжается на память помощника (противник замысла, тикет track-in-aguard).
package trackfd

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Socket — путь сокета помощника: папка принадлежит root, подменить его пользователь не может.
const Socket = "/run/aguard-track/fds.sock"

// Pair — группа fanotify и сокет proc connector; закрывает получатель.
type Pair struct{ Fan, Conn int }

// Close закрывает оба.
func (p Pair) Close() {
	_ = unix.Close(p.Fan)
	_ = unix.Close(p.Conn)
}

// OpenFan открывает группу уведомлений. Нужен CAP_SYS_ADMIN; пометки на inode ставит потом сам получатель.
func OpenFan() (int, error) {
	flags := uint(unix.FAN_CLASS_NOTIF | unix.FAN_CLOEXEC | unix.FAN_UNLIMITED_QUEUE |
		unix.FAN_REPORT_DFID_NAME_TARGET | unix.FAN_REPORT_PIDFD)
	fd, err := unix.FanotifyInit(flags, unix.O_RDONLY|unix.O_LARGEFILE)
	if err != nil {
		return -1, fmt.Errorf("fanotify_init: %w", err)
	}
	return fd, nil
}

// cnListenLen — подписка на proc connector: nlmsghdr(16) + cn_msg(20) + op(4).
const cnListenLen = 16 + 20 + 4

// OpenConn открывает сокет proc connector и подписывает его. Нужен CAP_NET_ADMIN: и подписка, и больший буфер.
// Порт netlink выбирает ядро: сокетов может быть несколько, у каждого подключения свой.
func OpenConn() (fd int, err error) {
	s, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.NETLINK_CONNECTOR)
	if err != nil {
		return -1, fmt.Errorf("proc connector socket: %w", err)
	}
	defer func() {
		if err != nil {
			_ = unix.Close(s)
		}
	}()
	if err := unix.Bind(s, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: 1}); err != nil {
		return -1, fmt.Errorf("proc connector bind: %w", err)
	}
	msg := make([]byte, cnListenLen)
	binary.LittleEndian.PutUint32(msg[0:], cnListenLen)
	binary.LittleEndian.PutUint16(msg[4:], unix.NLMSG_DONE)
	binary.LittleEndian.PutUint32(msg[12:], uint32(os.Getpid()))
	binary.LittleEndian.PutUint32(msg[16:], 1) // CN_IDX_PROC
	binary.LittleEndian.PutUint32(msg[20:], 1) // CN_VAL_PROC
	binary.LittleEndian.PutUint16(msg[32:], 4)
	binary.LittleEndian.PutUint32(msg[36:], 1) // PROC_CN_MCAST_LISTEN
	if err := unix.Sendto(s, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return -1, fmt.Errorf("proc connector listen: %w", err)
	}
	if err := unix.SetsockoptInt(s, unix.SOL_SOCKET, unix.SO_RCVBUFFORCE, 8<<20); err != nil {
		return -1, fmt.Errorf("proc connector rcvbuf: %w", err)
	}
	return s, nil
}

// OpenPair — настоящая пара для помощника.
func OpenPair() (Pair, error) {
	fan, err := OpenFan()
	if err != nil {
		return Pair{}, err
	}
	conn, err := OpenConn()
	if err != nil {
		_ = unix.Close(fan)
		return Pair{}, err
	}
	return Pair{Fan: fan, Conn: conn}, nil
}

// peerUID — uid процесса на другом конце unix-сокета по данным ядра.
func peerUID(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return -1, fmt.Errorf("сокет: %w", err)
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) { cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }); err != nil {
		return -1, fmt.Errorf("сокет: %w", err)
	}
	if credErr != nil {
		return -1, fmt.Errorf("SO_PEERCRED: %w", credErr)
	}
	return int(cred.Uid), nil
}

// Serve отдаёт пары процессам заданного uid до отмены ctx; остальным — разрыв и строка журнала.
func Serve(ctx context.Context, path string, uid int, open func() (Pair, error), lg *slog.Logger) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("старый сокет: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("папка сокета: %w", err)
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return fmt.Errorf("сокет %s: %w", path, err)
	}
	defer l.Close()
	if err := os.Chmod(path, 0o666); err != nil { // кому отдавать, решает SO_PEERCRED
		return fmt.Errorf("права сокета: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = l.Close() })
	defer stop()
	lg.Info("помощник слежения: готов", "uid", uid, "socket", path)
	for {
		c, err := l.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		serveOne(c, uid, open, lg)
	}
}

// serveOne — одно подключение: помощник последовательный, запросы редки (старт демона).
func serveOne(c *net.UnixConn, uid int, open func() (Pair, error), lg *slog.Logger) {
	defer c.Close()
	got, err := peerUID(c)
	if err != nil {
		lg.Warn("помощник слежения: подключение", "err", err)
		return
	}
	if got != uid {
		lg.Warn("помощник слежения: чужой uid", "uid", got)
		return
	}
	p, err := open()
	if err != nil {
		lg.Error("помощник слежения: дескрипторы", "err", err)
		return
	}
	defer p.Close()
	if _, _, err := c.WriteMsgUnix([]byte{1}, unix.UnixRights(p.Fan, p.Conn), nil); err != nil {
		lg.Warn("помощник слежения: передача", "err", err)
	}
}

// Fetch получает пару у помощника. serverUID — кем должен быть помощник (root): чужой сокет не источник событий.
func Fetch(path string, serverUID int) (Pair, error) {
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return Pair{}, fmt.Errorf("помощник слежения: %w", err)
	}
	defer c.Close()
	got, err := peerUID(c)
	if err != nil {
		return Pair{}, err
	}
	if got != serverUID {
		return Pair{}, fmt.Errorf("помощник слежения: сокет %s держит uid %d, ждали %d", path, got, serverUID)
	}
	buf, oob := make([]byte, 1), make([]byte, unix.CmsgSpace(2*4))
	_, oobn, _, _, err := c.ReadMsgUnix(buf, oob)
	if errors.Is(err, io.EOF) {
		return Pair{}, fmt.Errorf("помощник слежения отказал: чужой uid или ядро не дало дескрипторов, причина в журнале службы: %w", err)
	}
	if err != nil {
		return Pair{}, fmt.Errorf("помощник слежения: чтение: %w", err)
	}
	return parseRights(oob[:oobn])
}

// parseRights — ровно два дескриптора; лишние не остаются открытыми, CLOEXEC — на случай дочерних ssh и ps демона.
func parseRights(oob []byte) (Pair, error) {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return Pair{}, fmt.Errorf("помощник слежения: ответ: %w", err)
	}
	var fds []int
	for i := range msgs {
		got, err := unix.ParseUnixRights(&msgs[i])
		if err != nil {
			return Pair{}, fmt.Errorf("помощник слежения: дескрипторы: %w", err)
		}
		fds = append(fds, got...)
	}
	for _, fd := range fds {
		unix.CloseOnExec(fd)
	}
	if len(fds) != 2 {
		for _, fd := range fds {
			_ = unix.Close(fd)
		}
		return Pair{}, fmt.Errorf("помощник слежения: отказал: дескрипторов %d, ждали 2", len(fds))
	}
	return Pair{Fan: fds[0], Conn: fds[1]}, nil
}
