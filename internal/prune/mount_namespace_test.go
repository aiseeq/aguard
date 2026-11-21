package prune

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const mountNamespaceTestEnv = "AGUARD_PRUNE_MOUNT_NS"
const procMountNamespaceEnv = "AGUARD_PRUNE_PROC_MOUNT_NS"
const preUnshareFDChildEnv = "AGUARD_PRUNE_PREUNSHARE_FD_CHILD"

func TestOutsideProcCwdInOtherMountNamespace(t *testing.T) {
	if os.Getenv(procMountNamespaceEnv) == "child" {
		source, target := os.Getenv("AGUARD_PRUNE_PROC_SOURCE"), os.Getenv("AGUARD_PRUNE_PROC_TARGET")
		if err := os.MkdirAll(target, 0700); err != nil {
			t.Fatal(err)
		}
		if err := unix.Mount(source, target, "", unix.MS_BIND, ""); err != nil {
			t.Fatal(err)
		}
		if err := os.Chdir(target); err != nil {
			t.Fatal(err)
		}
		inode, err := statxInode(".")
		if err != nil {
			t.Fatal(err)
		}
		ready := fmt.Sprintf("%d %d %d\n", inode.Dev, inode.Ino, inode.MountID)
		if err := os.WriteFile(os.Getenv("AGUARD_PRUNE_PROC_READY"), []byte(ready), 0600); err != nil {
			t.Fatal(err)
		}
		_, _ = os.Stdin.Read(make([]byte, 1))
		return
	}
	unshare, err := exec.LookPath("unshare")
	if err != nil {
		t.Skipf("cwd mount namespace probe requires unshare: %v", err)
	}
	base := t.TempDir()
	entry := filepath.Join(base, "outside")
	if err := os.Mkdir(entry, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entry, "payload"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(base, "other-view")
	readyFile := filepath.Join(base, "ready")
	cmd := exec.Command(unshare, "-rm", "--pid", "--fork", "--mount-proc", "--", os.Args[0], "-test.run=^TestOutsideProcCwdInOtherMountNamespace$")
	cmd.Env = withEnv(os.Environ(), procMountNamespaceEnv+"=child")
	cmd.Env = withEnv(cmd.Env, "AGUARD_PRUNE_PROC_SOURCE="+entry)
	cmd.Env = withEnv(cmd.Env, "AGUARD_PRUNE_PROC_TARGET="+target)
	cmd.Env = withEnv(cmd.Env, "AGUARD_PRUNE_PROC_READY="+readyFile)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var childOutput strings.Builder
	cmd.Stdout = &childOutput
	cmd.Stderr = &childOutput
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var ready string
	for time.Now().Before(deadline) {
		data, readErr := os.ReadFile(readyFile)
		if readErr == nil {
			ready = strings.TrimSpace(string(data))
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if ready == "" {
		_ = stdin.Close()
		waitErr := cmd.Wait()
		output := childOutput.String()
		if unsharePermissionDenied(output) {
			t.Skipf("unshare refused private user/mount namespace: %s", strings.TrimSpace(output))
		}
		t.Fatalf("namespace child did not become ready (wait=%v): %s", waitErr, output)
	}
	fields := strings.Fields(ready)
	if len(fields) != 3 {
		t.Fatalf("bad child inode report %q", ready)
	}
	dev, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	ino, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	mountID, err := strconv.ParseUint(fields[2], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	entryInode, err := statxInode(entry)
	if err != nil {
		t.Fatal(err)
	}
	proc := emptyProcRoot(t)
	baseProc := addTestProc(t, proc, "7917", os.Getuid())
	if err := os.Symlink(entry, filepath.Join(baseProc, "cwd")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseProc, "mountinfo"), fmt.Appendf(nil, "%d 0 %d:%d / / rw - tmpfs tmpfs rw\n", mountID, unix.Major(dev), unix.Minor(dev)), 0600); err != nil {
		t.Fatal(err)
	}
	oldOpenProcTarget := openProcTarget
	openProcTarget = func(path string) (Inode, error) {
		if path == filepath.Join(baseProc, "cwd") {
			return Inode{Ino: ino, MountID: mountID}, nil
		}
		return oldOpenProcTarget(path)
	}
	t.Cleanup(func() { openProcTarget = oldOpenProcTarget })
	targets, err := collectProcTargets(context.Background(), proc, filepath.Dir(entry), os.Getuid(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := measureOutside(entry, targets, nil, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !tree.Busy {
		t.Fatalf("cwd in a different mount namespace was not classified as busy (dev=%d ino=%d mnt_id=%d, local=%d): tree=%+v", dev, ino, mountID, entryInode.MountID, tree)
	}
	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("namespace child: %v: %s", err, childOutput.String())
	}
}

func TestOutsideFDOpenedBeforeUnshareUsesSelfMountinfo(t *testing.T) {
	if os.Getenv(preUnshareFDChildEnv) == "1" {
		f, err := os.Open(os.Getenv("AGUARD_PRUNE_PREUNSHARE_FD_TARGET"))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(os.Getenv("AGUARD_PRUNE_PREUNSHARE_FD_READY"), []byte("ready"), 0600); err != nil {
			t.Fatal(err)
		}
		_, _ = os.Stdin.Read(make([]byte, 1))
		return
	}
	unshare, err := exec.LookPath("unshare")
	if err != nil {
		t.Skipf("fd mount namespace probe requires unshare: %v", err)
	}
	c, now := world(t)
	path := makeOutside(t, c, "fd-before-unshare", now.Add(-10*24*time.Hour))
	target := filepath.Join(path, "data")
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command(unshare, "-rm", "--fork", "--", os.Args[0], "-test.run=^TestOutsideFDOpenedBeforeUnshareUsesSelfMountinfo$")
	cmd.Env = withEnv(os.Environ(), preUnshareFDChildEnv+"=1")
	cmd.Env = withEnv(cmd.Env, "AGUARD_PRUNE_PREUNSHARE_FD_TARGET="+target)
	cmd.Env = withEnv(cmd.Env, "AGUARD_PRUNE_PREUNSHARE_FD_READY="+ready)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		_ = stdin.Close()
		if !waited {
			_ = cmd.Wait()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(ready); err != nil {
		_ = stdin.Close()
		waitErr := cmd.Wait()
		if unsharePermissionDenied(output.String()) {
			t.Skipf("unshare refused private user/mount namespace: %s", strings.TrimSpace(output.String()))
		}
		t.Fatalf("namespace fd holder did not become ready (wait=%v): %s", waitErr, output.String())
	}
	proc := emptyProcRoot(t)
	base := addTestProc(t, proc, "7915", os.Getuid())
	if err := os.Symlink(target, filepath.Join(base, "fd", "3")); err != nil {
		t.Fatal(err)
	}
	addTestFDInfo(t, base, "3", target)
	id, err := statxInode(target)
	if err != nil {
		t.Fatal(err)
	}
	// Model a process mount namespace created after fd 3 was opened: its
	// mountinfo lacks the descriptor's mount ID, while self mountinfo has it.
	if err := os.WriteFile(filepath.Join(base, "mountinfo"), []byte("999999 0 0:99 / / rw - tmpfs tmpfs rw\n"), 0600); err != nil {
		t.Fatal(err)
	}
	targets, err := collectProcTargets(context.Background(), proc, filepath.Dir(c.Root), os.Getuid(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := measureOutside(path, targets, nil, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !tree.Busy {
		t.Fatalf("pre-unshare fd (mnt_id=%d ino=%d) did not protect record: %+v", id.MountID, id.Ino, tree)
	}
	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("namespace fd holder: %v: %s", err, output.String())
	}
	waited = true
}

func TestMountInsideSessionIsPreserved(t *testing.T) {
	if os.Getenv(mountNamespaceTestEnv) != "1" {
		runMountNamespaceChild(t)
		return
	}

	for _, tc := range []struct {
		name        string
		rel         string
		sibling     bool
		checkSameFS bool
	}{
		{name: "bind-on-session", checkSameFS: true},
		{name: "bind-deep-with-sibling", rel: "a/b/mnt", sibling: true, checkSameFS: true},
		{name: "tmpfs-deep", rel: "a/b/mnt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "claude-1000")
			session := filepath.Join(root, "-project", oldID)
			mountpoint := filepath.Join(session, tc.rel)
			if err := os.MkdirAll(mountpoint, 0o700); err != nil {
				t.Fatal(err)
			}
			sibling := filepath.Join(session, "0-first-sibling.txt")
			if tc.sibling {
				if err := os.WriteFile(sibling, []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			victimDir := filepath.Join(base, "victim")
			if err := os.Mkdir(victimDir, 0o700); err != nil {
				t.Fatal(err)
			}
			victim := filepath.Join(victimDir, "precious.txt")
			if err := os.WriteFile(victim, []byte("preserve"), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.checkSameFS {
				if err := unix.Mount(victimDir, mountpoint, "", unix.MS_BIND, ""); err != nil {
					t.Fatalf("bind mount in private namespace: %v", err)
				}
			} else {
				if err := unix.Mount("tmpfs", mountpoint, "tmpfs", 0, ""); err != nil {
					t.Fatalf("tmpfs mount in private namespace: %v", err)
				}
				victim = filepath.Join(mountpoint, "precious.txt")
				if err := os.WriteFile(victim, []byte("preserve"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() { _ = unix.Unmount(mountpoint, unix.MNT_DETACH) })
			if tc.checkSameFS {
				var rootStat, mountStat unix.Stat_t
				if err := unix.Stat(root, &rootStat); err != nil {
					t.Fatal(err)
				}
				if err := unix.Stat(mountpoint, &mountStat); err != nil {
					t.Fatal(err)
				}
				if rootStat.Dev != mountStat.Dev {
					t.Fatalf("probe requires same-device bind mount, root=%d mount=%d", rootStat.Dev, mountStat.Dev)
				}
			}
			noYield := func(context.Context) error { return nil }
			err := removeSession(root, "-project", oldID, noYield, t.Context())
			if !errors.Is(err, ErrMountBoundary) {
				t.Fatalf("mount was not reported as mount boundary: %v", err)
			}
			if _, err := os.Stat(victim); err != nil {
				t.Fatalf("content behind mount was removed: %v", err)
			}
			if tc.sibling {
				if _, err := os.Stat(sibling); err != nil {
					t.Fatalf("sibling was deleted before mount preflight completed: %v", err)
				}
			}
		})
	}

	t.Run("outside-record-mount-boundary", func(t *testing.T) {
		base := t.TempDir()
		parent := filepath.Join(base, "claude-tmp")
		entry := filepath.Join(parent, "outside-agent")
		if err := os.MkdirAll(entry, 0700); err != nil {
			t.Fatal(err)
		}
		if err := unix.Mount("tmpfs", entry, "tmpfs", 0, ""); err != nil {
			t.Fatalf("mount tmpfs in private namespace: %v", err)
		}
		t.Cleanup(func() { _ = unix.Unmount(entry, unix.MNT_DETACH) })
		victim := filepath.Join(entry, "preserve")
		if err := os.WriteFile(victim, []byte("safe"), 0600); err != nil {
			t.Fatal(err)
		}
		err := removeRootEntryChecked(parent, "outside-agent", func(context.Context) error { return nil }, t.Context(), nil)
		if !errors.Is(err, ErrMountBoundary) {
			t.Fatalf("outside mount was not reported: %v", err)
		}
		if _, err := os.Stat(victim); err != nil {
			t.Fatalf("content behind outside mount was removed: %v", err)
		}
	})
}

func runMountNamespaceChild(t *testing.T) {
	t.Helper()
	unshare, err := exec.LookPath("unshare")
	if err != nil {
		t.Skipf("mount namespace probe requires unshare: %v", err)
	}
	cmd := exec.Command(unshare, "-rm", "--", os.Args[0], "-test.run=^TestMountInsideSessionIsPreserved$", "-test.v")
	cmd.Env = withEnv(os.Environ(), mountNamespaceTestEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return
	}
	output := string(out)
	if !strings.Contains(output, "=== RUN") && unsharePermissionDenied(output) {
		t.Skipf("unshare refused private user/mount namespace: %v: %s", err, strings.TrimSpace(output))
	}
	t.Fatalf("mount namespace child failed: %v\n%s", err, output)
}

func unsharePermissionDenied(output string) bool {
	message := strings.ToLower(output)
	for _, denied := range []string{"operation not permitted", "permission denied", "user namespaces are not enabled", "user namespace is not enabled"} {
		if strings.Contains(message, denied) {
			return true
		}
	}
	return false
}

func withEnv(environ []string, assignment string) []string {
	key, _, ok := strings.Cut(assignment, "=")
	if !ok {
		panic(fmt.Sprintf("invalid environment assignment %q", assignment))
	}
	filtered := make([]string, 0, len(environ)+1)
	for _, entry := range environ {
		entryKey, _, _ := strings.Cut(entry, "=")
		if entryKey != key {
			filtered = append(filtered, entry)
		}
	}
	return append(filtered, assignment)
}
