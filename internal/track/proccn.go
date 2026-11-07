package track

import (
	"cmp"
	"encoding/binary"
	"errors"
	"log/slog"
	"os"
	"runtime/debug"
	"strconv"
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
	seen            time.Time // когда завели запись: без события выхода её снимает проверка /proc
}

// Tombs — таблица процессов из proc connector.
type Tombs struct {
	mu      sync.Mutex
	m       map[int]*tomb
	keep    time.Duration
	overrun atomic.Int64
	log     *slog.Logger
	// gone — процесса нет в /proc; запись без события выхода старше keep без процесса получает выход сейчас
	gone func(pid int) (bool, error)
}

func newTombs(keep time.Duration, lg *slog.Logger) *Tombs {
	return &Tombs{m: map[int]*tomb{}, keep: keep, log: lg, gone: procGone}
}

// procGone — нет ли процесса в /proc; ошибка, кроме «нет такого», — не ответ.
func procGone(pid int) (bool, error) {
	_, err := os.Stat("/proc/" + strconv.Itoa(pid))
	if err == nil {
		return false, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	return false, err
}

// size — записей всего и без события выхода (для часовой сводки).
func (t *Tombs) size() (total, open int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, x := range t.m {
		if x.exited.IsZero() {
			open++
		}
	}
	return len(t.m), open
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

	// expireEvery — как часто чистить надгробия старше keep.
	expireEvery = 30 * time.Second
)

// listen разбирает события уже подписанного сокета (его открывает помощник), пока файл не закрыт.
func (t *Tombs) listen(conn *os.File, done <-chan struct{}) {
	go t.expire(done)
	go t.recvLoop(conn)
}

func (t *Tombs) recvLoop(conn *os.File) {
	buf := make([]byte, 64<<10)
	for {
		if !t.recvOnce(conn, buf) {
			return
		}
	}
}

// recvOnce читает и разбирает одну датаграмму; паника в разборе пишется в
// лог, и цикл чтения продолжается.
func (t *Tombs) recvOnce(conn *os.File, buf []byte) (more bool) {
	more = true
	defer recovered(t.log, "разбор proc connector")
	n, err := conn.Read(buf)
	switch {
	case err == nil:
	case errors.Is(err, os.ErrClosed):
		return false
	case errors.Is(err, unix.ENOBUFS):
		t.overrun.Add(1) // события потеряны: число идёт в часовую сводку
		return true
	default:
		t.log.Warn("aguard слежение: proc connector, чтение", "err", err)
		return true
	}
	t.messages(buf[:n])
	return true
}

func (t *Tombs) expire(done <-chan struct{}) {
	tick := time.NewTicker(expireEvery)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-tick.C:
			t.expireOnce()
		}
	}
}

// expireOnce снимает записи, вышедшие раньше keep. Запись без события выхода
// (процесс в чужом пространстве имён, событие потеряно) старше keep, чьего
// процесса нет в /proc, получает выход сейчас и живёт ещё keep, как
// обычная: запоздалое событие fanotify ещё найдёт создателя. /proc
// проверяется без мьютекса — connector и lookup не ждут обхода.
func (t *Tombs) expireOnce() {
	defer recovered(t.log, "чистка надгробий")
	now := time.Now()
	t.mu.Lock()
	open := map[int]time.Time{}
	for pid, x := range t.m {
		if !x.exited.IsZero() && now.Sub(x.exited) > t.keep {
			delete(t.m, pid)
			continue
		}
		if x.exited.IsZero() && now.Sub(x.seen) > t.keep {
			open[pid] = x.seen
		}
	}
	t.mu.Unlock()
	var gone []int
	var failed int
	var firstErr error
	for pid := range open {
		g, err := t.gone(pid)
		if err != nil {
			failed++
			firstErr = cmp.Or(firstErr, err)
			continue
		}
		if g {
			gone = append(gone, pid)
		}
	}
	if failed > 0 {
		t.log.Warn("aguard слежение: проверка процесса надгробия не удалась, запись оставлена", "записей", failed, "err", firstErr)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, pid := range gone {
		// та же запись: за время проверки pid мог уйти новому процессу
		if x := t.m[pid]; x != nil && x.exited.IsZero() && x.seen.Equal(open[pid]) {
			x.exited = now
		}
	}
}

// recovered — отложенный recover фоновой горутины: паника пишется в лог со
// стеком, а не роняет сторож.
func recovered(lg *slog.Logger, what string) {
	if r := recover(); r != nil {
		lg.Error("aguard слежение: паника: "+what, "panic", r, "stack", string(debug.Stack()))
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
		x := &tomb{ppid: u(1), uid: -1, seen: time.Now()}
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
			x = &tomb{uid: -1, seen: time.Now()}
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
			x = &tomb{uid: -1, seen: time.Now()}
			t.m[u(1)] = x
		}
		x.ppid = u(5)
		x.exited = time.Now()
		t.mu.Unlock()
	}
}
