package worktree

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRepo(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		name, folder, gitFile, want string
		gitDir                      bool
	}{
		{
			name:    "absolute gitdir",
			folder:  filepath.Join(root, "absolute-wt"),
			gitFile: "gitdir: " + filepath.Join(root, "main", ".git", "worktrees", "absolute") + "\n",
			want:    filepath.Join(root, "main"),
		},
		{
			name:    "relative gitdir",
			folder:  filepath.Join(root, "relative-wt"),
			gitFile: "gitdir: ../relative-main/.git/worktrees/relative\n",
			want:    filepath.Join(root, "relative-main"),
		},
		{
			name:    "submodule in regular repository",
			folder:  filepath.Join(root, "main", "modules", "sm"),
			gitFile: "gitdir: ../../.git/modules/sm\n",
		},
		{name: "git directory", folder: filepath.Join(root, "git-dir"), gitDir: true},
		{name: "malformed git file", folder: filepath.Join(root, "malformed"), gitFile: "not a gitdir"},
		{name: "missing git file", folder: filepath.Join(root, "missing")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.MkdirAll(tc.folder, 0o700); err != nil {
				t.Fatal(err)
			}
			if tc.gitDir {
				if err := os.Mkdir(filepath.Join(tc.folder, ".git"), 0o700); err != nil {
					t.Fatal(err)
				}
			} else if tc.gitFile != "" {
				if err := os.WriteFile(filepath.Join(tc.folder, ".git"), []byte(tc.gitFile), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := Repo(tc.folder)
			if err != nil || got != tc.want {
				t.Fatalf("Repo(%q) = %q, %v; want %q, nil", tc.folder, got, err, tc.want)
			}
		})
	}
}
