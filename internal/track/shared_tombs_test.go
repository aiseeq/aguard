package track

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/aiseeq/aguard/internal/record"
)

// Два клиента получают одну таблицу и оставляют подписку живой до последнего.
func TestSharedTombsConcurrentLifetimeAndRetention(t *testing.T) {
	var starts, stops int
	s := newSharedTombs(time.Minute, slog.New(slog.DiscardHandler))
	s.start = func(*Tombs) (func(), error) { starts++; return func() { stops++ }, nil }
	var wg sync.WaitGroup
	leases := make([]*Tombs, 2)
	for i := range leases {
		wg.Go(func() {
			var err error
			leases[i], err = s.acquire(time.Duration(i+1) * time.Minute)
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if starts != 1 || leases[0] != leases[1] {
		t.Fatalf("starts=%d, tables %p %p", starts, leases[0], leases[1])
	}
	tb := leases[0]
	tb.mu.Lock()
	tb.m[42] = &tomb{seen: time.Now(), exited: time.Now().Add(-90 * time.Second)}
	tb.mu.Unlock()
	s.release()
	tb.expireOnce()
	if _, ok := tb.lookup(42); !ok || stops != 0 {
		t.Fatal("короткий срок или выход первого клиента снял общее состояние")
	}
	s.release()
	if stops != 1 {
		t.Fatalf("остановок %d, ждали одну", stops)
	}
	again, err := s.acquire(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if starts != 2 || again == tb {
		t.Fatal("после последнего выхода нужна новая подписка и таблица")
	}
	s.release()
}

func TestSharedTombsInitialFailureCanRetry(t *testing.T) {
	s := newSharedTombs(time.Minute, slog.New(slog.DiscardHandler))
	starts, stops := 0, 0
	s.start = func(*Tombs) (func(), error) {
		starts++
		if starts == 1 {
			return nil, errors.New("нет подписки")
		}
		return func() { stops++ }, nil
	}
	if _, err := s.acquire(time.Minute); err == nil {
		t.Fatal("принят клиент без подписки")
	}
	if _, err := s.acquire(time.Minute); err != nil {
		t.Fatal(err)
	}
	s.release()
	if starts != 2 || stops != 1 {
		t.Fatalf("starts=%d stops=%d", starts, stops)
	}
}

func TestSharedTombsRetentionOrders(t *testing.T) {
	for _, order := range [][]time.Duration{{time.Minute, 3 * time.Minute}, {3 * time.Minute, time.Minute}} {
		s := newSharedTombs(2*time.Minute, slog.New(slog.DiscardHandler))
		s.start = func(*Tombs) (func(), error) { return func() {}, nil }
		for i, keep := range order {
			tb, err := s.Acquire(keep)
			if err != nil {
				t.Fatal(err)
			}
			want := max(2*time.Minute, order[0])
			if i == 1 {
				want = max(want, order[1])
			}
			if tb.keep != want {
				t.Fatalf("order=%v step=%d keep=%v want=%v", order, i, tb.keep, want)
			}
		}
		s.Release()
		s.Release()
	}
}

// Release закрывает дескриптор, когда reader уже действительно ждёт данные.
func TestSharedTombsReleaseAfterRead(t *testing.T) {
	var writer *os.File
	s := NewSharedTombs(time.Minute, slog.New(slog.DiscardHandler), func() (*os.File, error) {
		r, w, err := os.Pipe()
		writer = w
		return r, err
	})
	tb, err := s.Acquire(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	packet := make([]byte, 16+len(forkMsg(1, 2)))
	binary.LittleEndian.PutUint32(packet, uint32(len(packet)))
	copy(packet[16:], forkMsg(1, 2))
	if _, err := writer.Write(packet); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := tb.lookup(2); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reader не получил событие")
		}
		time.Sleep(time.Millisecond)
	}
	done := make(chan struct{})
	go func() { s.Release(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Release не разбудил reader")
	}
}

func TestSharedTombsRetryWarningDeduplicated(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	var writer *os.File
	attempt := 0
	finished := make(chan struct{})
	open := func() (*os.File, error) {
		attempt++
		if attempt == 1 {
			r, w, err := os.Pipe()
			writer = w
			return r, err
		}
		if attempt == 6 {
			close(finished)
		}
		if attempt < 4 {
			return nil, errors.New("ошибка A")
		}
		return nil, errors.New("ошибка B")
	}
	tb := newTombs(time.Minute, logger)
	stop, err := startSharedConnectorRetry(tb, open, 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		stop()
		t.Fatal("повторы не дошли")
	}
	stop()
	if n := strings.Count(logs.String(), "level=WARN"); n != 3 || tb.connFailures.Load() < 5 {
		t.Fatalf("WARN=%d failures=%d log=%s", n, tb.connFailures.Load(), logs.String())
	}
}

// Те же fork/exec/exit для двух таблиц HEAD и общей таблицы: оба
// независимых Resolver видят одну личность и цепочку умерших предков.
func TestSharedTombsExitedCreatorEquivalent(t *testing.T) {
	child := startReadProcChild(t, "echo ready; read x")
	log := slog.New(slog.DiscardHandler)
	parent := 1000000000
	var legacy []*Tombs
	var writers []*os.File
	done := make(chan struct{})
	defer close(done)
	for range 2 {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		defer w.Close()
		tb := newTombs(time.Minute, log)
		tb.listen(r, done)
		legacy = append(legacy, tb)
		writers = append(writers, w)
	}
	var sharedWriter *os.File
	s := NewSharedTombs(time.Minute, log, func() (*os.File, error) {
		r, w, err := os.Pipe()
		sharedWriter = w
		return r, err
	})
	shared, err := s.Acquire(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Release()
	other, err := s.Acquire(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Release()
	defer sharedWriter.Close()
	writers = append(writers, sharedWriter)
	send := func(msg []byte) {
		packet := make([]byte, 16+len(msg))
		binary.LittleEndian.PutUint32(packet, uint32(len(packet)))
		copy(packet[16:], msg)
		for _, w := range writers {
			if _, err := w.Write(packet); err != nil {
				t.Fatal(err)
			}
		}
	}
	wait := func(exited bool) {
		deadline := time.Now().Add(5 * time.Second)
		for {
			ready := true
			for _, tb := range append(legacy, shared) {
				x, ok := tb.lookup(child.pid)
				if !ok || x.comm == "" || (exited && x.exited.IsZero()) {
					ready = false
				}
			}
			if ready {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("поток не обработан")
			}
			time.Sleep(time.Millisecond)
		}
	}
	send(forkMsg(1, parent))
	send(forkMsg(parent, child.pid))
	send(execMsg(child.pid))
	wait(false)
	if err := child.in.Close(); err != nil {
		t.Fatal(err)
	}
	child.wait()
	send(exitMsg(child.pid, parent))
	send(exitMsg(parent, 1))
	wait(true)
	var wg sync.WaitGroup
	for _, old := range legacy {
		wg.Go(func() {
			want := newResolver(old, log).Owner(child.pid, -1)
			got := newResolver(shared, log).Owner(child.pid, -1)
			if got.Proc.State != record.ProcReaped || got.Proc.Comm == "" || got.Proc != want.Proc || len(got.Chain) != 1 || len(want.Chain) != 1 || got.Chain[0] != want.Chain[0] {
				t.Errorf("owner shared=%+v legacy=%+v", got, want)
			}
		})
	}
	if shared != other {
		t.Fatal("разные таблицы")
	}
	wg.Wait()
}

type scriptedReader struct {
	errs   []error
	packet []byte
}

func (r *scriptedReader) Read(buf []byte) (int, error) {
	if len(r.errs) == 0 {
		return 0, io.EOF
	}
	err := r.errs[0]
	r.errs = r.errs[1:]
	if err == nil {
		return copy(buf, r.packet), nil
	}
	return 0, err
}

// ENOBUFS не обрывает поток; постоянная ошибка сообщает управляющему
// циклу, который переподписывается и сохраняет таблицу для обоих клиентов.
func TestSharedTombsReaderErrors(t *testing.T) {
	var logs bytes.Buffer
	tb := newTombs(time.Minute, slog.New(slog.NewTextHandler(&logs, nil)))
	msg := forkMsg(5, 6)
	packet := make([]byte, 16+len(msg))
	binary.LittleEndian.PutUint32(packet, uint32(len(packet)))
	copy(packet[16:], msg)
	r := &scriptedReader{errs: []error{unix.ENOBUFS, nil, errors.New("socket failed")}, packet: packet}
	_, err := tb.readConnector(r)
	x, ok := tb.lookup(6)
	if err == nil || !strings.Contains(err.Error(), "socket failed") || tb.overrun.Load() != 1 || !ok || x.ppid != 5 {
		t.Fatalf("err=%v, overrun=%d log=%q", err, tb.overrun.Load(), logs.String())
	}
}

func TestSharedTombsConcurrentReadersAndEvents(t *testing.T) {
	tb := newTombs(time.Minute, slog.New(slog.DiscardHandler))
	pid := os.Getpid()
	tb.handle(forkMsg(1, pid))
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			r := newResolver(tb, slog.New(slog.DiscardHandler))
			for range 100 {
				_ = r.fromTomb(&record.Owner{}, pid)
			}
		})
	}
	for range 100 {
		tb.handle(forkMsg(pid, pid))
		tb.handle(execMsg(pid))
		tb.handle(exitMsg(pid, 1))
	}
	wg.Wait()
}

func TestSharedTombsReconnectKeepsBothConsumers(t *testing.T) {
	opened := make(chan *os.File, 2)
	open := func() (*os.File, error) {
		r, w, err := os.Pipe()
		if err == nil {
			opened <- w
		}
		return r, err
	}
	s := NewSharedTombs(time.Minute, slog.New(slog.DiscardHandler), open)
	one, err := s.Acquire(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Release()
	two, err := s.Acquire(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Release()
	one.handle(forkMsg(10, 11))
	first := <-opened
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	var second *os.File
	select {
	case second = <-opened:
	case <-time.After(5 * time.Second):
		t.Fatal("подписка не восстановилась")
	}
	defer second.Close()
	msg := forkMsg(123, 456)
	packet := make([]byte, 16+len(msg))
	binary.LittleEndian.PutUint32(packet, uint32(len(packet)))
	copy(packet[16:], msg)
	if _, err := second.Write(packet); err != nil {
		t.Fatal(err)
	}
	if x, ok := two.lookup(11); !ok || x.ppid != 10 {
		t.Fatalf("надгробия потеряны при переподписке: %+v", x)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if x, ok := one.lookup(456); ok {
			y, ok := two.lookup(456)
			if !ok || x.ppid != 123 || y.ppid != 123 {
				t.Fatalf("после восстановления: %+v %+v", x, y)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("событие после восстановления не дошло")
		}
		time.Sleep(time.Millisecond)
	}
}

func BenchmarkTombsEventParsing(b *testing.B) {
	pid := os.Getpid()
	var packets [][]byte
	for _, msg := range [][]byte{forkMsg(pid, pid), execMsg(pid), exitMsg(pid, pid)} {
		packet := make([]byte, 16+len(msg))
		binary.LittleEndian.PutUint32(packet, uint32(len(packet)))
		copy(packet[16:], msg)
		packets = append(packets, packet)
	}
	for _, tc := range []struct {
		name   string
		tables int
	}{{"two", 2}, {"shared", 1}} {
		b.Run(tc.name, func(b *testing.B) {
			tables := make([]*Tombs, tc.tables)
			for i := range tables {
				tables[i] = newTombs(time.Minute, slog.New(slog.DiscardHandler))
			}
			b.ReportAllocs()
			var start, end unix.Timespec
			if err := unix.ClockGettime(unix.CLOCK_PROCESS_CPUTIME_ID, &start); err != nil {
				b.Fatal(err)
			}
			for b.Loop() {
				for _, msg := range packets {
					for _, tb := range tables {
						tb.sharedMessages(msg)
					}
				}
			}
			if err := unix.ClockGettime(unix.CLOCK_PROCESS_CPUTIME_ID, &end); err != nil {
				b.Fatal(err)
			}
			b.ReportMetric(float64(end.Nano()-start.Nano())/float64(b.N*len(packets)), "cpu-ns/event")
		})
	}
}
