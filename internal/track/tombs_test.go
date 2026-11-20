package track

import (
	"bytes"
	"encoding/binary"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/aiseeq/aguard/internal/record"
)

// Записи без события выхода (выход потерян, процесс в чужом пространстве
// имён) копились сотнями тысяч (тикет tmpguard-track-swap): старше keep и
// без процесса в /proc они получают выход и живут ещё keep, затем
// снимаются; живые, свежие и непроверенные остаются.
func TestTombsExpireOpenWithoutProc(t *testing.T) {
	var logs bytes.Buffer
	tb := newTombs(time.Minute, slog.New(slog.NewTextHandler(&logs, nil)))
	tb.gone = func(pid int) (bool, error) {
		switch pid {
		case 2:
			return false, nil
		case 6:
			return false, errors.New("stat: permission denied")
		}
		return true, nil
	}
	old := time.Now().Add(-2 * time.Minute)
	tb.m[1] = &tomb{seen: old}                                           // нет процесса, старая — выход сейчас
	tb.m[2] = &tomb{seen: old}                                           // процесс жив — без выхода
	tb.m[3] = &tomb{seen: time.Now()}                                    // свежая — без выхода
	tb.m[4] = &tomb{seen: old, exited: time.Now()}                       // вышел недавно — оставить
	tb.m[5] = &tomb{seen: old, exited: time.Now().Add(-2 * time.Minute)} // вышел давно — снять
	tb.m[6] = &tomb{seen: old}                                           // проверка не удалась — без выхода, WARN
	tb.expireOnce()
	for pid, want := range map[int]bool{1: true, 2: true, 3: true, 4: true, 5: false, 6: true} {
		if _, ok := tb.m[pid]; ok != want {
			t.Errorf("pid %d: запись есть=%v, ждали %v", pid, ok, want)
		}
	}
	if tb.m[1].exited.IsZero() {
		t.Errorf("pid 1: выход не поставлен — запоздалое событие не нашло бы создателя после снятия")
	}
	for _, pid := range []int{2, 3, 6} {
		if !tb.m[pid].exited.IsZero() {
			t.Errorf("pid %d: выход поставлен", pid)
		}
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "permission denied") {
		t.Errorf("нет WARN об ошибке проверки: %s", logs.String())
	}
	if total, open := tb.size(); total != 5 || open != 3 {
		t.Errorf("size = %d, %d; ждали 5, 3", total, open)
	}
	tb.m[1].exited = time.Now().Add(-2 * time.Minute)
	tb.expireOnce()
	if _, ok := tb.m[1]; ok {
		t.Errorf("pid 1: запись не снята через keep после выхода")
	}
}

// execMsg — cn_msg(20) + proc_event exec для pid (как шлёт proc connector).
func execMsg(pid int) []byte {
	b := make([]byte, 20+16+24)
	binary.LittleEndian.PutUint32(b[20:], procEvExec)
	binary.LittleEndian.PutUint32(b[20+16:], uint32(pid))
	binary.LittleEndian.PutUint32(b[20+16+4:], uint32(pid))
	return b
}

func forkMsg(parent, child int) []byte {
	b := make([]byte, 20+16+24)
	binary.LittleEndian.PutUint32(b[20:], procEvFork)
	d := b[20+16:]
	binary.LittleEndian.PutUint32(d[4:], uint32(parent))
	binary.LittleEndian.PutUint32(d[8:], uint32(child))
	binary.LittleEndian.PutUint32(d[12:], uint32(child))
	return b
}

func exitMsg(pid, ppid int) []byte {
	b := make([]byte, 20+16+24)
	binary.LittleEndian.PutUint32(b[20:], procEvExit)
	d := b[20+16:]
	binary.LittleEndian.PutUint32(d[0:], uint32(pid))
	binary.LittleEndian.PutUint32(d[4:], uint32(pid))
	binary.LittleEndian.PutUint32(d[20:], uint32(ppid))
	return b
}

func TestTombKeepsExitedCreatorIdentity(t *testing.T) {
	child := startReadProcChild(t, "echo ready; read x")
	want, err := readProc(child.pid)
	if err != nil {
		t.Fatal(err)
	}
	tombs := newTombs(time.Minute, slog.New(slog.DiscardHandler))
	tombs.handle(forkMsg(os.Getpid(), child.pid))
	tombs.handle(execMsg(child.pid))
	if err := child.in.Close(); err != nil {
		t.Fatal(err)
	}
	child.wait()
	tombs.handle(exitMsg(child.pid, os.Getpid()))
	owner := &record.Owner{}
	r := newResolver(tombs, slog.New(slog.DiscardHandler))
	if ppid := r.fromTomb(owner, child.pid); ppid != os.Getpid() {
		t.Fatalf("tomb owner parent = %d; want %d", ppid, os.Getpid())
	}
	if owner.Proc.State != record.ProcReaped || owner.Proc.Comm != want.comm || owner.Proc.Args != want.args || owner.Proc.UID != want.uid {
		t.Fatalf("tomb owner = %+v; want exited creator comm=%q args=%q uid=%d", owner.Proc, want.comm, want.args, want.uid)
	}
	if owner.Proc.Cwd != "" {
		t.Fatalf("tomb owner cwd = %q; dead-owner cwd is not captured", owner.Proc.Cwd)
	}
}

func BenchmarkTombsForkExecExitSelf(b *testing.B) {
	pid := os.Getpid()
	msgs := [][]byte{forkMsg(pid, pid), execMsg(pid), exitMsg(pid, pid)}
	t := newTombs(time.Minute, slog.New(slog.DiscardHandler))
	b.ReportAllocs()
	b.ResetTimer()
	var start, end unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_PROCESS_CPUTIME_ID, &start); err != nil {
		b.Fatal(err)
	}
	for range b.N {
		for _, msg := range msgs {
			t.handle(msg)
		}
	}
	if err := unix.ClockGettime(unix.CLOCK_PROCESS_CPUTIME_ID, &end); err != nil {
		b.Fatal(err)
	}
	cpuNS := end.Nano() - start.Nano()
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(msgs)), "ns/event")
	b.ReportMetric(float64(cpuNS)/float64(b.N*len(msgs)), "cpu-ns/event")
}

// Пока /proc проверяется без мьютекса, pid ушёл новому процессу (exec
// обновил запись, fork потерян): запись не помечается. Постоянная ошибка
// проверки даёт один WARN в час, не на каждом обходе.
func TestTombsExpireRaceAndWarnOnce(t *testing.T) {
	var logs bytes.Buffer
	tb := newTombs(time.Minute, slog.New(slog.NewTextHandler(&logs, nil)))
	self := os.Getpid() // exec читает /proc: нужен живой процесс
	old := time.Now().Add(-2 * time.Minute)
	tb.m[self] = &tomb{seen: old}
	tb.m[2] = &tomb{seen: old}
	tb.gone = func(pid int) (bool, error) {
		if pid == 2 {
			return false, errors.New("stat: permission denied")
		}
		tb.handle(execMsg(self)) // exec нового процесса с тем же pid во время проверки
		return true, nil
	}
	tb.expireOnce()
	if x := tb.m[self]; !x.exited.IsZero() || x.comm == "" {
		t.Errorf("pid %d: выход поставлен записи, которую за время проверки обновил exec (или exec не дошёл): %+v", self, *x)
	}
	tb.m[self].seen = old
	tb.gone = func(pid int) (bool, error) {
		if pid == 2 {
			return false, errors.New("stat: permission denied")
		}
		return false, nil
	}
	tb.expireOnce()
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
		t.Errorf("WARN об ошибке проверки: %d строк, ждали 1 за час:\n%s", n, logs.String())
	}
	tb.lastFailWarn = time.Now().Add(-time.Hour - time.Second)
	tb.expireOnce()
	if n := strings.Count(logs.String(), "level=WARN"); n != 2 {
		t.Errorf("WARN через час: %d строк, ждали 2", n)
	}
}
