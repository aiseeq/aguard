package disk

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

const gb = int64(1) << 30

func testConfig(t *testing.T, home string) *Config {
	t.Helper()
	c := Default()
	c.Roots = []string{"~/.cache"}
	c.Containers = []string{"~/.cache/claude-tmp/claude-*/*"}
	c.Lasting = []string{"~/.cache/go-build"}
	c.Owners = map[string]string{"~/.cache/farm-hard": "llmark-master"}
	if err := c.Validate(home); err != nil {
		t.Fatal(err)
	}
	return &c
}

func mkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestEntries(t *testing.T) {
	home := t.TempDir()
	c := testConfig(t, home)
	pad := filepath.Join(home, ".cache/claude-tmp/claude-1000/-home-u-git-x")
	mkdir(t, filepath.Join(pad, "s1"))
	mkdir(t, filepath.Join(home, ".cache/farm-hard"))
	if err := os.WriteFile(filepath.Join(home, ".cache/claude-tmp/claude-1000/state.json"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(pad, filepath.Join(home, ".cache/link")); err != nil {
		t.Fatal(err)
	}
	got, err := c.Entries()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(pad, "s1"), filepath.Join(home, ".cache/farm-hard")}
	if !slices.Equal(got, want) { // claude-tmp держит контейнер — не запись; файл рядом с контейнером и симлинк — тоже
		t.Errorf("Entries = %v, want %v", got, want)
	}
}

// fakeMeasure — замер по таблице размеров.
func fakeMeasure(sizes map[string]int64, changed time.Time) Measure {
	return func(entries []string) map[string]Usage {
		out := map[string]Usage{}
		for _, e := range entries {
			out[e] = Usage{Bytes: sizes[e], Changed: changed}
		}
		return out
	}
}

func TestPass(t *testing.T) {
	home := "/home/u"
	c := testConfig(t, home)
	farm, build, pad := home+"/.cache/farm-hard", home+"/.cache/go-build", home+"/.cache/claude-tmp/claude-1000/p/s"
	list := func() ([]string, error) { return []string{build, farm, pad}, nil }
	owner := func(p string) (string, string) { return "x", "test" }
	t0 := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	old := t0.Add(-72 * time.Hour)
	sizes := map[string]int64{farm: gb / 2, build: 100 * gb, pad: 3 * gb}
	s := &State{Sizes: map[string][]Point{}, Told: map[string]time.Time{}}

	recs, _, err := s.Pass(c, t0, 300*gb, list, fakeMeasure(sizes, old), owner)
	if err != nil {
		t.Fatal(err)
	}
	// первый обход: роста ещё не видно; старая большая запись — остаток, малая и долгоживущий кэш — нет
	if len(recs) != 1 || recs[0].Kind != KindStale || recs[0].Path != pad || !recs[0].Since.Equal(old) {
		t.Fatalf("первый обход: %+v", recs)
	}

	// через минуту: обход только раз в час
	if recs, _, _ := s.Pass(c, t0.Add(time.Minute), 300*gb, list, fakeMeasure(sizes, old), owner); recs != nil {
		t.Fatalf("обход раньше срока: %+v", recs)
	}

	// за 20 часов farm-hard вырос на 6 ГБ, go-build на 4: рост — только farm-hard; остаток повторно не пишется
	sizes[farm], sizes[build] = gb/2+6*gb, 104*gb
	recs, _, _ = s.Pass(c, t0.Add(20*time.Hour), 290*gb, list, fakeMeasure(sizes, t0.Add(19*time.Hour)), owner)
	if len(recs) != 1 || recs[0].Kind != KindGrew || recs[0].Path != farm || recs[0].Delta != 6*gb || !recs[0].Since.Equal(t0) {
		t.Fatalf("рост: %+v", recs)
	}

	// место тает: 6 ГБ за 2 минуты, обход по тревоге раньше часа; pad вырос на 5 ГБ с прошлого обхода
	at := t0.Add(21 * time.Hour)
	if recs, _, _ := s.Pass(c, at, 290*gb, list, fakeMeasure(sizes, at), owner); len(recs) != 0 {
		t.Fatalf("обычный обход: %+v", recs)
	}
	sizes[pad] = 8 * gb
	recs, _, _ = s.Pass(c, at.Add(3*time.Minute), 284*gb, list, fakeMeasure(sizes, at), owner)
	// и за сутки он вырос на те же 5 ГБ — это и рост
	if len(recs) != 2 || recs[0].Kind != KindFast || recs[0].Path != pad || recs[0].Delta != 5*gb || recs[1].Kind != KindGrew {
		t.Fatalf("быстрое наполнение: %+v", recs)
	}

	// снова тает, но ни одна запись не выросла: fast без пути
	recs, _, _ = s.Pass(c, at.Add(6*time.Minute), 278*gb, list, fakeMeasure(sizes, at), owner)
	if len(recs) != 1 || recs[0].Kind != KindFast || recs[0].Path != "" || recs[0].Delta != 12*gb {
		t.Fatalf("убыль без записи: %+v", recs)
	}

	// запись исчезла — её замеры уходят из состояния
	gone := func() ([]string, error) { return []string{build, farm}, nil }
	if _, _, err := s.Pass(c, at.Add(2*time.Hour), 278*gb, gone, fakeMeasure(sizes, at), owner); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Sizes[pad]; ok {
		t.Error("замеры исчезнувшей записи остались")
	}
}

func TestStateSaveLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Load(path)
	if err != nil || len(s.Sizes) != 0 {
		t.Fatalf("нет файла: %v %v", s, err)
	}
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	s.Sizes["/a"] = []Point{{At: at, Bytes: 5}}
	s.Told["grew /a"] = at
	s.Traces = map[string]Trace{"/t": {Path: "/t", Lifetime: 72 * time.Hour}} // срок следа читается обратно
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	back, err := Load(path)
	if err != nil || back.Sizes["/a"][0].Bytes != 5 || !back.Told["grew /a"].Equal(at) || back.Traces["/t"].Lifetime != 72*time.Hour {
		t.Fatalf("Load = %+v, %v", back, err)
	}
}

func TestOwners(t *testing.T) {
	home := t.TempDir()
	repo := filepath.Join(home, "git/sc2go")
	wt := filepath.Join(home, ".cache/worktrees/sc2go/w-x")
	mkdir(t, wt)
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+repo+"/.git/worktrees/w-x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(home, ".cache/worktrees/hedwig/w-y")
	mkdir(t, other)
	if err := os.WriteFile(filepath.Join(other, ".git"), []byte("gitdir: "+home+"/git/hedwig/.git/worktrees/w-y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pads := filepath.Join(home, ".cache/claude-tmp/claude-1000")
	o := &Owners{
		Table:    map[string]string{filepath.Join(home, ".cache/farm-hard"): "llmark-master"},
		Admins:   map[string]string{filepath.Join(home, "git"): "git-admin"},
		Lead:     "lead",
		Live:     []Session{{Name: "aguard-master", ID: "s1", Cwd: filepath.Join(home, "git/aguard")}, {Name: "sc2go-master", ID: "s2", Cwd: repo}},
		Projects: map[string]string{ProjectKey(home): "lead", ProjectKey(filepath.Join(home, "git/life")): "life-master"},
	}
	cases := []struct{ path, owner, why string }{
		{filepath.Join(home, ".cache/farm-hard/sub"), "llmark-master", "config"},
		{filepath.Join(pads, ProjectKey(filepath.Join(home, "git/aguard")), "s1"), "aguard-master", "session"},
		{filepath.Join(pads, ProjectKey(filepath.Join(home, "git/aguard")), "old"), "aguard-master", "project"},
		{filepath.Join(pads, ProjectKey(filepath.Join(home, "git/life")), "old"), "life-master", "names"},
		{filepath.Join(pads, ProjectKey(home), "old"), "lead", "names"},
		{filepath.Join(pads, ProjectKey(filepath.Join(home, "git/hedwig")), "old"), "git-admin", "admin"},
		{wt, "sc2go-master", "worktree"},
		{other, "git-admin", "admin"},
		{filepath.Join(home, ".cache/gotmp"), "lead", "lead"},
		{"", "lead", "lead"},
	}
	for _, c := range cases {
		if owner, why := o.Of(c.path); owner != c.owner || why != c.why {
			t.Errorf("Of(%s) = %s, %s; want %s, %s", c.path, owner, why, c.owner, c.why)
		}
	}
}

func TestSpanBytes(t *testing.T) {
	// одна копия экстента дважды, часть сжатого экстента с тем же адресом, перекрытие и отдельный экстент
	s := []span{{100, 50}, {100, 50}, {100, 20}, {140, 30}, {500, 10}}
	if got := spanBytes(s); got != 80 {
		t.Errorf("spanBytes = %d, want 80", got)
	}
}

// ficlone — FICLONE: _IOW(0x94, 9, int).
const ficlone = 0x40049409

// TestMeasureReflink — reflink-копия файла внутри записи не удваивает место. Папка — в кэше пользователя: временная
// папка теста бывает на tmpfs, а reflink нужен btrfs или xfs; не умеет — пропуск.
func TestMeasureReflink(t *testing.T) {
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(cache, "aguard-test-")
	if errors.Is(err, os.ErrNotExist) { // песочница без кэша пользователя (go test на PC через graft)
		t.Skipf("кэша пользователя нет: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	data := make([]byte, 1<<20)
	for i := range data {
		data[i] = byte(i * 7)
	}
	src := filepath.Join(dir, "a")
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(filepath.Join(dir, "b"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, out.Fd(), ficlone, in.Fd()); e != 0 {
		t.Skipf("reflink не поддержан: %v", e)
	}
	for _, f := range []*os.File{in, out} { // экстенты получают адрес только после записи на диск
		if err := f.Sync(); err != nil {
			t.Fatal(err)
		}
	}
	exact := measure(t.Context(), dir, map[inode]bool{}, new(fiemapBuf), true)
	blocks := measure(t.Context(), dir, map[inode]bool{}, new(fiemapBuf), false)
	if exact.Files != 3 || blocks.Bytes < 2<<20 || exact.Bytes >= 3<<19 {
		t.Errorf("по экстентам %d, по блокам %d, файлов %d", exact.Bytes, blocks.Bytes, exact.Files)
	}
}

// Остановка демона обрывает обход: отменённый ctx — ни одной записи и ошибка обхода, а не полный замер.
func TestMeasureCanceled(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got := testConfig(t, dir).MeasureAll(ctx, []string{dir}); len(got) != 0 {
		t.Errorf("отменённый замер: %v", got)
	}
	if u := measure(ctx, dir, map[inode]bool{}, new(fiemapBuf), true); !errors.Is(u.Err, context.Canceled) || u.Files != 0 {
		t.Errorf("обход после отмены: %+v", u)
	}
}

// TestLowFree — ниже free_min письмо сразу, дальше раз в сутки; место поднялось — следующее падение снова сразу.
func TestLowFree(t *testing.T) {
	c := Default()
	c.FreeMin = 150 << 30
	s := &State{Sizes: map[string][]Point{
		"/h/.cache/go-build": {{Bytes: 300 << 30}}, "/h/.cache/glint": {{Bytes: 5 << 30}},
		"/h/.local/state/x": {{Bytes: 1 << 30}}, "/h/.cache/small": {{Bytes: 1 << 20}},
	}}
	at := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	low, high := int64(140<<30), int64(160<<30)
	steps := []struct {
		after time.Duration
		free  int64
		want  bool
	}{
		{0, high, false},
		{time.Hour, low, true},
		{2 * time.Hour, low, false},
		{25 * time.Hour, low, true},
		{26 * time.Hour, high, false},
		{27 * time.Hour, low, true},
	}
	for _, st := range steps {
		if got := s.LowFree(&c, at.Add(st.after), st.free); got != st.want {
			t.Fatalf("через %s свободно %d ГиБ: письмо %v, ждал %v", st.after, st.free>>30, got, st.want)
		}
	}
	msg := s.LowFreeMessage(&c, low, "/h")
	for _, want := range []string{"свободно 140", "порога 150", "~/.cache/go-build 300", "~/.cache/glint 5", "~/.local/state/x 1"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("в письме нет %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "small") {
		t.Fatalf("в письме больше трёх записей: %s", msg)
	}
}
