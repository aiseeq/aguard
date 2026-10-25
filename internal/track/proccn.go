package track

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// Netlink proc connector (CONFIG_PROC_EVENTS): fork/exec/exit всех процессов.
// Держим «надгробия» — ppid, comm, argv, cwd, uid — ещё минуту-две после
// выхода, чтобы создатель-однодневка (mktemp, sh -c), умерший до разбора его
// события fanotify, всё равно получил цепочку предков.

type tomb struct {
	ppid            int
	comm, args, cwd string
	uid             int
	exited          time.Time
}

// Tombs — таблица процессов из proc connector.
type Tombs struct {
	mu      sync.Mutex
	m       map[int]*tomb
	keep    time.Duration
	overrun atomic.Int64
	log     *slog.Logger
}

func newTombs(keep time.Duration, lg *slog.Logger) *Tombs {
	return &Tombs{m: map[int]*tomb{}, keep: keep, log: lg}
}

func (t *Tombs) lookup(pid int) (tomb, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	x, ok := t.m[pid]
	if !ok {
		return tomb{}, false
	}
	return *x, true
}

const (
	procEvFork = 0x1
	procEvExec = 0x2
	procEvExit = 0x80000000

	// cnListenLen — подписка на proc connector: nlmsghdr(16) + cn_msg(20) + op(4).
	cnListenLen = 16 + 20 + 4
	// expireEvery — как часто чистить надгробия старше keep.
	expireEvery = 30 * time.Second
)

// listen подписывается на proc connector и разбирает события до ошибки сокета.
// Нужен CAP_NET_ADMIN.
func (t *Tombs) listen() error {
	s, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_DGRAM, unix.NETLINK_CONNECTOR)
	if err != nil {
		return fmt.Errorf("proc connector socket: %w", err)
	}
	pid := uint32(os.Getpid())
	if err := unix.Bind(s, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: 1, Pid: pid}); err != nil {
		return fmt.Errorf("proc connector bind: %w", err)
	}
	msg := make([]byte, cnListenLen)
	binary.LittleEndian.PutUint32(msg[0:], cnListenLen)
	binary.LittleEndian.PutUint16(msg[4:], unix.NLMSG_DONE)
	binary.LittleEndian.PutUint32(msg[12:], pid)
	binary.LittleEndian.PutUint32(msg[16:], 1) // CN_IDX_PROC
	binary.LittleEndian.PutUint32(msg[20:], 1) // CN_VAL_PROC
	binary.LittleEndian.PutUint16(msg[32:], 4)
	binary.LittleEndian.PutUint32(msg[36:], 1) // PROC_CN_MCAST_LISTEN
	if err := unix.Sendto(s, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return fmt.Errorf("proc connector listen: %w", err)
	}
	if err := unix.SetsockoptInt(s, unix.SOL_SOCKET, unix.SO_RCVBUFFORCE, 8<<20); err != nil {
		return fmt.Errorf("proc connector rcvbuf: %w", err)
	}
	go t.expire()
	go t.recvLoop(s)
	return nil
}

func (t *Tombs) recvLoop(s int) {
	buf := make([]byte, 64<<10)
	for {
		t.recvOnce(s, buf)
	}
}

// recvOnce читает и разбирает одну датаграмму; паника в разборе пишется в
// лог, и цикл чтения продолжается.
func (t *Tombs) recvOnce(s int, buf []byte) {
	defer recovered(t.log, "разбор proc connector")
	n, _, err := unix.Recvfrom(s, buf, 0)
	switch {
	case err == nil:
	case errors.Is(err, unix.ENOBUFS):
		t.overrun.Add(1) // события потеряны: число идёт в часовую сводку
		return
	case errors.Is(err, unix.EINTR):
		return
	default:
		t.log.Warn("tmpguard-track: proc connector, чтение", "err", err)
		return
	}
	for b := buf[:n]; len(b) >= 16; {
		l := int(binary.LittleEndian.Uint32(b))
		if l < 16 || l > len(b) {
			break
		}
		t.handle(b[16:l])
		b = b[min((l+3)&^3, len(b)):]
	}
}

func (t *Tombs) expire() {
	for range time.Tick(expireEvery) {
		t.expireOnce()
	}
}

func (t *Tombs) expireOnce() {
	defer recovered(t.log, "чистка надгробий")
	t.mu.Lock()
	defer t.mu.Unlock()
	for pid, x := range t.m {
		if !x.exited.IsZero() && time.Since(x.exited) > t.keep {
			delete(t.m, pid)
		}
	}
}

// recovered — отложенный recover фоновой горутины: паника пишется в лог со
// стеком, а не роняет сторож.
func recovered(lg *slog.Logger, what string) {
	if r := recover(); r != nil {
		lg.Error("tmpguard-track: паника: "+what, "panic", r, "stack", string(debug.Stack()))
	}
}

// handle разбирает cn_msg(20) + proc_event{what u32, cpu u32, ts u64, data}.
func (t *Tombs) handle(cn []byte) {
	if len(cn) < 20+16+24 {
		return
	}
	ev := cn[20:]
	what := binary.LittleEndian.Uint32(ev[0:])
	d := ev[16:]
	u := func(i int) int { return int(binary.LittleEndian.Uint32(d[i*4:])) }
	switch what {
	case procEvFork: // parent_pid, parent_tgid, child_pid, child_tgid
		if u(2) != u(3) { // поток, не процесс
			return
		}
		x := &tomb{ppid: u(1), uid: -1}
		t.mu.Lock()
		if p := t.m[u(1)]; p != nil { // без exec ребёнок остаётся копией родителя
			x.comm, x.args, x.cwd, x.uid = p.comm, p.args, p.cwd, p.uid
		}
		t.m[u(3)] = x
		t.mu.Unlock()
	case procEvExec: // process_pid, process_tgid
		pid := u(1)
		d, err := readProc(pid)
		if err != nil { //nolint:silent-error-handling // процесс вышел раньше, чем прочитали /proc: обычно для однодневок, остаётся унаследованное при fork
			return
		}
		t.mu.Lock()
		x := t.m[pid]
		if x == nil {
			x = &tomb{uid: -1}
			t.m[pid] = x
		}
		x.comm, x.args, x.cwd, x.uid = d.comm, d.args, d.cwd, d.uid
		t.mu.Unlock()
	case procEvExit: // process_pid, process_tgid, exit_code, exit_signal, parent_pid, parent_tgid
		if u(0) != u(1) {
			return
		}
		t.mu.Lock()
		x := t.m[u(1)]
		if x == nil {
			x = &tomb{uid: -1}
			t.m[u(1)] = x
		}
		x.ppid = u(5)
		x.exited = time.Now()
		t.mu.Unlock()
	}
}
