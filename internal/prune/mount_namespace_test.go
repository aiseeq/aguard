package prune

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

const mountNamespaceTestEnv = "AGUARD_PRUNE_MOUNT_NS"

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
