package track

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestMeasureWalkExpectedTreeAndFDs(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(target, "target")
	if err := os.Link(target, filepath.Join(root, "hard-link")); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "one", "two", "three")
	if err := os.MkdirAll(nested, 0700); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(nested, "leaf"), "leaf")
	if err := os.Symlink("../..", filepath.Join(nested, "up-two")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(nested, "absolute")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing-target", filepath.Join(nested, "broken")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("leaf", filepath.Join(nested, "to-file")); err != nil {
		t.Fatal(err)
	}
	for name, mode := range map[string]os.FileMode{"write-only": 0300, "read-only": 0600, "no-access": 0000} {
		if err := os.Mkdir(filepath.Join(root, name), mode); err != nil {
			t.Fatal(err)
		}
	}

	countFDs := func() int {
		t.Helper()
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatalf("read /proc/self/fd: %v", err)
		}
		return len(entries)
	}
	before := countFDs()
	u, err := measure(root, 0, &pacer{ctx: context.Background()})
	if err != nil {
		t.Fatal(err)
	}
	after := countFDs()
	if before != after {
		t.Fatalf("file descriptor count changed: before=%d after=%d", before, after)
	}
	if !u.ok || u.files != 6 {
		t.Fatalf("measure(%q) = {ok:%v files:%d}, want {ok:true files:6}", root, u.ok, u.files)
	}
	if want := lstatBytes(t, root); u.bytes != want {
		t.Fatalf("measure(%q).bytes = %d, want %d (st_blocks*512, hard links once)", root, u.bytes, want)
	}

	for _, tc := range []struct {
		name  string
		path  string
		ok    bool
		files int64
	}{
		{name: "nonexistent", path: filepath.Join(root, "absent")},
		{name: "not a directory", path: filepath.Join(target, "child")},
		{name: "0300 directory", path: filepath.Join(root, "write-only"), ok: true},
		{name: "0600 directory", path: filepath.Join(root, "read-only"), ok: true},
		{name: "0000 directory", path: filepath.Join(root, "no-access"), ok: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := measure(tc.path, 0, &pacer{ctx: context.Background()})
			if err != nil {
				t.Fatal(err)
			}
			if got.ok != tc.ok || got.files != tc.files {
				t.Fatalf("measure(%q) = {ok:%v files:%d}, want {ok:%v files:%d}", tc.path, got.ok, got.files, tc.ok, tc.files)
			}
		})
	}
}

// lstatBytes — du записи независимым обходом: st_blocks*512 каждой записи, жёсткая ссылка — один раз,
// нечитаемая папка — только сама папка.
func lstatBytes(t *testing.T, root string) int64 {
	t.Helper()
	var total int64
	seen := map[inoKey]bool{}
	err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return nil // нечитаемую папку обход пропускает, её саму уже посчитали
		}
		var st unix.Stat_t
		if err := unix.Lstat(path, &st); err != nil {
			return nil // запись в папке 0600 не stat-ится, как и в measure
		}
		if st.Nlink > 1 && st.Mode&unix.S_IFMT != unix.S_IFDIR {
			if seen[inoKey{st.Dev, st.Ino}] {
				return nil
			}
			seen[inoKey{st.Dev, st.Ino}] = true
		}
		total += st.Blocks * 512
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return total
}

func TestWalkFDSkipsOtherDevice(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "dir"), 0700); err != nil {
		t.Fatal(err)
	}
	var st unix.Stat_t
	if err := unix.Lstat(root, &st); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	added := 0
	if err := walkFD(fd, st.Dev+1, func(*unix.Stat_t) { added++ }, &pacer{ctx: context.Background()}); err != nil {
		t.Fatal(err)
	}
	if added != 0 {
		t.Fatalf("walkFD on another device added %d entries, want 0", added)
	}
}
