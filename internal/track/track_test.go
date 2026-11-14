package track

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/record"
	"github.com/aiseeq/aguard/internal/trackfd"
	"golang.org/x/sys/unix"
)

func TestApplyUsageIntegral(t *testing.T) {
	born := time.Unix(1000, 0)
	e := &record.Entry{Born: born, Owner: &record.Owner{}}
	applyUsage(e, 100, 1, born.Add(10*time.Second)) // рост от нуля: 100/2*10
	if e.ByteSec != 500 {
		t.Fatalf("первый замер: %v", e.ByteSec)
	}
	applyUsage(e, 300, 2, born.Add(20*time.Second)) // трапеция (100+300)/2*10
	if e.ByteSec != 2500 || e.MaxBytes != 300 || e.Bytes != 300 {
		t.Fatalf("второй замер: %+v", e)
	}
	applyUsage(e, 0, 0, born.Add(30*time.Second))
	if e.MaxBytes != 300 || e.MaxFiles != 2 || e.ByteSec != 4000 {
		t.Fatalf("после удаления содержимого: %+v", e)
	}
	old := &record.Entry{Born: born} // старше сторожа: размер считаем постоянным
	applyUsage(old, 100, 1, born.Add(10*time.Second))
	if old.ByteSec != 1000 {
		t.Fatalf("запись без создателя: %v", old.ByteSec)
	}
}

func TestParseDirName(t *testing.T) {
	h := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	body := make([]byte, 8+8)
	binary.LittleEndian.PutUint32(body[8:], uint32(len(h)))
	binary.LittleEndian.PutUint32(body[12:], 0x81)
	body = append(append(body, h...), []byte("claude-tmp\x00\x00\x00")...)
	dn := parseDirName(body)
	if dn == nil || dn.name != "claude-tmp" || dn.key != "129:0102030405060708" {
		t.Fatalf("parseDirName: %+v", dn)
	}
	if parseDirName(body[:10]) != nil {
		t.Error("короткая запись разобрана")
	}
}

// Удаление должно доходить до журнала и снимка, даже когда замерять нечего
// (все записи старые и уже замерены) — иначе отчёт показывал бы удалённое.
func TestDeleteReachesLogAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	l, err := openEventLog(filepath.Join(dir, record.OwnersFile), 0, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer l.close()
	now := time.Now()
	born := now.Add(-48 * time.Hour)
	tr := &Tracker{
		cfg:     Config{RunDir: dir, StateDir: dir, Young: 15 * time.Minute, Persist: time.Hour},
		owners:  l,
		log:     slog.New(slog.DiscardHandler),
		entries: map[string]*record.Entry{},
		watched: map[string]string{},
	}
	for _, p := range []string{"/tmp/old-a", "/tmp/old-b"} {
		tr.entries[p] = &record.Entry{Path: p, Born: born, Bytes: 100 << 20, MaxBytes: 100 << 20, MeasuredAt: now}
	}
	tr.save(false)
	tr.remove("/tmp/old-a", record.EvDelete, "", now)
	if !tr.dirty {
		t.Fatal("удаление не пометило таблицу изменённой")
	}
	tr.startMeasure(t.Context(), make(chan measureDone, 1), false) // молодых нет: замера не будет
	if tr.measuring {
		t.Fatal("замер запущен без записей")
	}
	st, err := record.ReadState(filepath.Join(dir, record.StateFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Entries) != 1 || st.Entries[0].Path != "/tmp/old-b" {
		t.Fatalf("в снимке осталось удалённое: %+v", st.Entries)
	}
	f, err := os.Open(filepath.Join(dir, record.OwnersFile))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var got []record.Event
	if _, err := record.ParseEvents(f, func(e record.Event) { got = append(got, e) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Ev != record.EvDelete || got[0].LifeS < 47*3600 || got[0].MaxBytes != 100<<20 || !got[0].Measured {
		t.Fatalf("строка удаления: %+v", got)
	}
}

// Потоковый снимок читается в то же, что json.Marshal(record.State).
func TestWriteStateMatchesMarshal(t *testing.T) {
	now := time.Date(2026, 11, 2, 2, 40, 0, 0, time.UTC)
	own := &record.Owner{Proc: record.Proc{Pid: 7, Comm: "node", Args: `node <x> & "y"`, UID: 1000, State: record.ProcOK},
		Chain: []record.Link{{Pid: 6, Comm: "claude"}}, Session: &record.Session{Pid: 6, Via: "ppid"}}
	for _, es := range [][]*record.Entry{
		nil,
		{{Path: "/tmp/a", Ino: 1, UID: 0, Born: now}, {Path: "/tmp/b", Dir: true, Ino: 2, UID: 1000, Born: now, Owner: own, Bytes: 4096, MeasuredAt: now}},
	} {
		s := record.State{Updated: now, Started: now.Add(-time.Hour), Roots: []string{"/tmp"}}
		var buf bytes.Buffer
		if err := writeState(&buf, &s, es); err != nil {
			t.Fatal(err)
		}
		want := s
		want.Entries = make([]record.Entry, 0, len(es))
		for _, e := range es {
			want.Entries = append(want.Entries, *e)
		}
		wb, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		var got, exp any
		if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
			t.Fatalf("снимок не JSON: %v\n%s", err, buf.String())
		}
		if err := json.Unmarshal(wb, &exp); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, exp) {
			t.Fatalf("снимок расходится с json.Marshal:\n%s\n%s", buf.String(), wb)
		}
	}
}

func TestStartInitialMeasure(t *testing.T) {
	dir := t.TempDir()
	watchedExtra := t.TempDir()
	container := filepath.Join(dir, "container")
	if err := os.Mkdir(container, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	cases := []struct {
		name     string
		lastFull time.Time
		full     bool
	}{
		{name: "recent", lastFull: time.Now().Add(-time.Hour), full: false},
		{name: "missing", full: true},
		{name: "expired", lastFull: time.Now().Add(-7 * time.Hour), full: true},
		{name: "future", lastFull: time.Now().Add(time.Hour), full: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := &Tracker{
				cfg:        Config{Full: 6 * time.Hour, Young: 2 * time.Hour},
				lastFull:   tc.lastFull,
				entries:    map[string]*record.Entry{},
				watched:    map[string]string{dir: "root-key", watchedExtra: "extra-key"},
				containers: map[string]bool{container: true},
			}
			tr.entries[filepath.Join(dir, "young")] = &record.Entry{Path: filepath.Join(dir, "young"), Born: time.Now().Add(-time.Hour), MeasuredAt: old}
			tr.entries[filepath.Join(dir, "old-measured")] = &record.Entry{Path: filepath.Join(dir, "old-measured"), Born: time.Now().Add(-24 * time.Hour), MeasuredAt: old}
			tr.entries[filepath.Join(dir, "old-unmeasured")] = &record.Entry{Path: filepath.Join(dir, "old-unmeasured"), Born: time.Now().Add(-24 * time.Hour)}
			done := make(chan measureDone, 1)
			tr.startInitialMeasure(t.Context(), done)
			got := <-done
			wantDirs := map[string]bool{dir: true, watchedExtra: true, container: true}
			gotDirs := make(map[string]bool, len(got.job.dirs))
			for _, d := range got.job.dirs {
				gotDirs[d] = true
			}
			if got.job.full != tc.full || !got.job.list || !reflect.DeepEqual(gotDirs, wantDirs) {
				t.Fatalf("стартовое задание: full=%v list=%v dirs=%v", got.job.full, got.job.list, got.job.dirs)
			}
			wantItems := map[string]bool{
				filepath.Join(dir, "young"):          true,
				filepath.Join(dir, "old-unmeasured"): true,
			}
			if tc.full {
				wantItems[filepath.Join(dir, "old-measured")] = true
			}
			gotItems := make(map[string]bool, len(got.job.items))
			for _, it := range got.job.items {
				gotItems[it.path] = true
			}
			if !reflect.DeepEqual(gotItems, wantItems) {
				t.Fatalf("записи в задании: got %v, want %v", gotItems, wantItems)
			}
		})
	}
}

func TestFinishListingWithoutFullReconciles(t *testing.T) {
	dir := t.TempDir()
	newPath := filepath.Join(dir, "new")
	if err := os.WriteFile(newPath, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(dir, "gone")
	lastFull := time.Now().Add(-time.Hour).Round(0)
	owners, err := openEventLog(filepath.Join(dir, record.OwnersFile), 0, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer owners.close()
	tr := &Tracker{
		cfg:      Config{Name: "track", RunDir: dir, StateDir: dir, Persist: time.Hour},
		log:      slog.New(slog.DiscardHandler),
		owners:   owners,
		entries:  map[string]*record.Entry{oldPath: {Path: oldPath, Born: time.Now().Add(-24 * time.Hour)}},
		lastFull: lastFull,
	}
	tr.finishMeasure(measureDone{
		job:     measureJob{start: time.Now(), list: true},
		listing: map[string][]string{dir: {"new"}},
	})
	if tr.entries[oldPath] != nil {
		t.Fatalf("пропавшая запись осталась: %+v", tr.entries[oldPath])
	}
	added := tr.entries[newPath]
	if added == nil || !added.MeasuredAt.IsZero() {
		t.Fatalf("новая запись отсутствует или уже замерена: %+v", added)
	}
	if len(tr.entries) != 1 {
		t.Fatalf("таблица содержит дубли или лишние записи: %+v", tr.entries)
	}
	if !tr.lastFull.Equal(lastFull) {
		t.Fatalf("листинг без полного изменил lastFull: %v, хотели %v", tr.lastFull, lastFull)
	}
}

func TestRestoreBtimeDirectionOwnerGoneAndNoBtime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "entry")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	disk, err := statMeta(path)
	if err != nil {
		t.Fatal(err)
	}

	owner := &record.Owner{Proc: record.Proc{Pid: 77, Comm: "creator"}, Session: &record.Session{Name: "creator-master"}}
	makeTracker := func(entry record.Entry, override func(string) (meta, error)) (*Tracker, string) {
		t.Helper()
		dir := t.TempDir()
		b, err := json.Marshal(record.State{Updated: time.Now(), Entries: []record.Entry{entry}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, record.StateFile), b, 0o600); err != nil {
			t.Fatal(err)
		}
		owners, err := openEventLog(filepath.Join(dir, record.OwnersFile), 0, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(owners.close)
		tr := &Tracker{cfg: Config{RunDir: dir, StateDir: dir}, entries: map[string]*record.Entry{}, owners: owners,
			log: slog.New(slog.DiscardHandler), statMeta: override}
		return tr, dir
	}

	t.Run("born after btime keeps creator", func(t *testing.T) {
		if !disk.hasBorn {
			t.Skip("файловая система не сообщает STATX_BTIME")
		}
		oldBorn := disk.born.Add(3 * time.Second)
		tr, _ := makeTracker(record.Entry{Path: path, Ino: disk.ino, Born: oldBorn, Owner: owner}, nil)
		tr.restore()
		got := tr.entries[path]
		if got == nil || !got.Born.Equal(oldBorn) || got.Owner == nil || got.Owner.Session == nil || got.Owner.Session.Name != owner.Session.Name {
			t.Fatalf("снимок с Born позже btime должен восстановиться с создателем: %+v", got)
		}
	})

	t.Run("new btime closes old and adopts current", func(t *testing.T) {
		if !disk.hasBorn {
			t.Skip("файловая система не сообщает STATX_BTIME")
		}
		oldBorn := disk.born.Add(-10 * time.Second)
		tr, dir := makeTracker(record.Entry{Path: path, Ino: disk.ino, Born: oldBorn, Owner: owner}, nil)
		tr.restore()
		got := tr.entries[path]
		if got == nil || !got.Born.Equal(disk.born) || got.Owner != nil {
			t.Fatalf("новая запись должна получить disk btime без старого создателя: %+v", got)
		}
		b, err := os.ReadFile(filepath.Join(dir, record.OwnersFile))
		if err != nil {
			t.Fatal(err)
		}
		var foundGone bool
		for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
			var event record.Event
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				t.Fatal(err)
			}
			if event.Ev == record.EvGone && event.Path == path && event.Owner != nil && event.Owner.Session != nil {
				foundGone = true
			}
		}
		if !foundGone {
			t.Fatalf("нет события gone для старой записи с создателем: %s", b)
		}
	})

	t.Run("filesystem without btime restores by inode", func(t *testing.T) {
		oldBorn := disk.born.Add(-20 * time.Second)
		statWithoutBorn := func(string) (meta, error) {
			return meta{ino: disk.ino, born: disk.born.Add(time.Hour), hasBorn: false}, nil
		}
		tr, _ := makeTracker(record.Entry{Path: path, Ino: disk.ino, Born: oldBorn, Owner: owner}, statWithoutBorn)
		tr.restore()
		got := tr.entries[path]
		if got == nil || !got.Born.Equal(oldBorn) || got.Owner == nil {
			t.Fatalf("без btime должны восстановиться born и создатель снимка: %+v", got)
		}
	})
}

func TestLastFullSaveRestore(t *testing.T) {
	dir := t.TempDir()
	lastFull := time.Now().Add(-time.Hour).Round(0)
	tr := &Tracker{cfg: Config{RunDir: dir, StateDir: dir}, lastFull: lastFull,
		entries: map[string]*record.Entry{}, log: slog.New(slog.DiscardHandler)}
	tr.save(true)
	restored := &Tracker{cfg: tr.cfg, entries: map[string]*record.Entry{}, log: slog.New(slog.DiscardHandler)}
	restored.restore()
	if !restored.lastFull.Equal(lastFull) {
		t.Fatalf("lastFull после restore: %v, хотели %v", restored.lastFull, lastFull)
	}
}

// Сохранение снимка не держит его целиком в памяти: на 3 000 записей —
// не больше ~50 выделений на запись (было: копия всех записей и буфер во весь JSON).
func BenchmarkSave(b *testing.B) {
	dir := b.TempDir()
	tr := &Tracker{cfg: Config{RunDir: dir, StateDir: dir}, entries: map[string]*record.Entry{}, log: slog.New(slog.DiscardHandler)}
	own := &record.Owner{Proc: record.Proc{Pid: 7, Comm: "chrome", Args: strings.Repeat("--flag=value ", 40), UID: 1000, State: record.ProcOK},
		Chain: []record.Link{{Pid: 6, Comm: "node"}, {Pid: 5, Comm: "claude"}}}
	for i := range 3000 {
		p := fmt.Sprintf("/tmp/playwright-artifacts-%06d", i)
		tr.entries[p] = &record.Entry{Path: p, Dir: true, Ino: uint64(i), UID: 1000, Born: time.Now(), Owner: own}
	}
	b.ReportAllocs()
	for b.Loop() {
		tr.save(false)
	}
}

// unprivPair — пара без root: fanotify без pidfd (свой pid ядро показывает) и труба вместо proc connector; остальное — как
// на настоящей паре от помощника.
func unprivPair(t *testing.T) trackfd.Pair {
	t.Helper()
	fan, err := unix.FanotifyInit(unix.FAN_CLASS_NOTIF|unix.FAN_CLOEXEC|unix.FAN_REPORT_DFID_NAME_TARGET, unix.O_RDONLY|unix.O_LARGEFILE)
	if err != nil {
		t.Fatalf("fanotify без root: %v", err)
	}
	var p [2]int
	if err := unix.Pipe2(p[:], unix.O_CLOEXEC); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(p[1]) })
	return trackfd.Pair{Fan: fan, Conn: p[0]}
}

func runConfig(t *testing.T, root string) Config {
	t.Helper()
	dir := t.TempDir()
	return Config{Name: "t", Roots: []string{root}, StateDir: filepath.Join(dir, "state"), RunDir: filepath.Join(dir, "run"),
		Full: time.Hour, Young: time.Minute, YoungEvery: 10 * time.Millisecond, Persist: time.Hour, RotateBytes: 1 << 20}
}

// Весь путь внутри демона: запись создана — в снимке; отмена — снимок на диске и Run выходит, горутины чтения не держат
// его; второй экземпляр на те же папки — ошибка, а не затёртый снимок.
func TestRunStopsOnCancelAndSingleWriter(t *testing.T) {
	root := t.TempDir()
	cfg := runConfig(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := make(chan error, 1)
	go func() { res <- Run(ctx, cfg, unprivPair(t), slog.New(slog.DiscardHandler)) }()
	lock := filepath.Join(cfg.RunDir, "writer.lock")
	for end := time.Now().Add(10 * time.Second); ; {
		if _, err := os.Stat(filepath.Join(cfg.StateDir, record.OwnersFile)); err == nil {
			break
		}
		if time.Now().After(end) {
			t.Fatal("сторож не поднялся")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, cfg, unprivPair(t), slog.New(slog.DiscardHandler)); err == nil || !strings.Contains(err.Error(), "другой экземпляр") {
		t.Fatalf("второй писатель: %v", err)
	}
	entry := filepath.Join(root, "created")
	if err := os.WriteFile(entry, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(10 * time.Second); ; {
		b, err := os.ReadFile(filepath.Join(cfg.StateDir, record.OwnersFile))
		if err == nil && strings.Contains(string(b), "created") {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("создание не в журнале: %q, %v", b, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-res:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run не вышел по отмене")
	}
	s, err := record.ReadState(filepath.Join(cfg.StateDir, record.StateFile))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for i := range s.Entries {
		found = found || s.Entries[i].Path == entry
	}
	if !found {
		t.Fatalf("в снимке при остановке нет записи: %+v", s.Entries)
	}
}

// Переименование из непомеченной папки: новая запись заводится, ошибки в обработке нет (open_by_handle_at без root нет).
func TestDirOfUnmarked(t *testing.T) {
	tr := &Tracker{watchKey: map[string]string{"k": "/tmp"}}
	if p, w, err := tr.dirOf(&dirName{key: "k"}); p != "/tmp" || !w || err != nil {
		t.Fatalf("%q %v %v", p, w, err)
	}
	if _, w, err := tr.dirOf(&dirName{key: "другая"}); w || err == nil {
		t.Fatalf("непомеченная: %v %v", w, err)
	}
}

// Чужой процесс (root) cwd не показывает: остальное о нём известно. Проверяется на настоящем pid 1, если он чужой.
// Живой чужой процесс (pid 1): на сигнал 0 ядро отвечает EPERM, это не «процесс ушёл».
func TestLiveForeignRootProcess(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("под root все процессы свои")
	}
	if _, err := os.Readlink("/proc/1/cwd"); !errors.Is(err, fs.ErrPermission) {
		t.Skipf("pid 1 здесь не чужой: %v", err)
	}
	pidfd, err := unix.PidfdOpen(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(pidfd)
	lg := slog.New(slog.DiscardHandler)
	r := newResolver(newTombs(time.Minute, lg), lg)
	o := &record.Owner{}
	if _, err := r.live(o, 1, pidfd); err != nil {
		t.Fatalf("живой чужой процесс принят за ушедший: %v", err)
	}
	if o.Proc.State != record.ProcOK || o.Proc.Comm == "" || o.Proc.Cwd != "" {
		t.Fatalf("живой чужой процесс: %+v", o.Proc)
	}
}

// Удаление незнакомой записи: создатель не нужен, а pidfd события всё равно закрывается.
func TestHandleClosesPidfdWithoutOwner(t *testing.T) {
	var p [2]int
	if err := unix.Pipe2(p[:], 0); err != nil {
		t.Fatal(err)
	}
	defer unix.Close(p[1])
	tr := &Tracker{log: slog.New(slog.DiscardHandler), entries: map[string]*record.Entry{},
		watchKey: map[string]string{"k": "/tmp"}, watched: map[string]string{"/tmp": "k"}, containers: map[string]bool{}}
	ev := fevent{mask: unix.FAN_DELETE, pidfd: p[0], dn: &dirName{key: "k", name: "aguard-review-no-such-entry"}}
	tr.handle(&ev)
	if _, err := unix.FcntlInt(uintptr(p[0]), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
		t.Fatalf("pidfd события не закрыт: %v", err)
	}
}

// Процесс другой учётки: закрытый реестр её сессий не повод для предупреждения на каждое событие.
func TestSessionAtForeignUIDSilent(t *testing.T) {
	logs := &bytes.Buffer{}
	lg := slog.New(slog.NewTextHandler(logs, nil))
	r := newResolver(newTombs(time.Minute, lg), lg)
	foreign := os.Geteuid() + 1
	closed := filepath.Join(t.TempDir(), ".claude", "sessions")
	if err := os.MkdirAll(closed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(closed, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(closed, 0o755); err != nil {
			t.Error(err)
		}
	})
	r.homes[foreign] = filepath.Dir(filepath.Dir(closed))
	if s := r.sessionAt(1, "1", foreign); s != nil {
		t.Fatalf("сессия чужой учётки: %+v", s)
	}
	if logs.Len() != 0 {
		t.Fatalf("предупреждение о чужом реестре: %s", logs.String())
	}
}

func TestReadProcForeignWithoutCwd(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("под root все процессы свои")
	}
	if _, err := os.Readlink("/proc/1/cwd"); !errors.Is(err, fs.ErrPermission) {
		t.Skipf("pid 1 показывает cwd: %v", err)
	}
	d, err := readProc(1)
	if err != nil || d.comm == "" || d.cwd != "" {
		t.Fatalf("%+v, %v", d, err)
	}
}

// Ушедший процесс — ошибка, а не uid-заглушка: вызывающий не примет её за учётку.
func TestProcUID(t *testing.T) {
	uid, err := procUID(os.Getpid())
	if err != nil || uid != os.Geteuid() {
		t.Fatalf("свой процесс: uid %d, err %v, хотим %d", uid, err, os.Geteuid())
	}
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if uid, err := procUID(cmd.Process.Pid); err == nil {
		t.Fatalf("умерший процесс %d: uid %d без ошибки", cmd.Process.Pid, uid)
	}
}
