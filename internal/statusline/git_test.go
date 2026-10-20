package statusline

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGitBranchFast(t *testing.T) {
	t.Setenv("GIT_DIR", "")
	for _, tc := range []struct {
		name, gitfile, head, want string
		ok                        bool
	}{
		{name: "directory", head: "ref: refs/heads/feature/statusline\n", want: "feature/statusline", ok: true},
		{name: "relative gitfile", gitfile: "relative", head: "ref: refs/heads/worktree\n", want: "worktree", ok: true},
		{name: "absolute gitfile", gitfile: "absolute", head: "ref: refs/heads/worktree\n", want: "worktree", ok: true},
		{name: "detached", head: "0123456789abcdef\n", want: "HEAD", ok: true},
		{name: "empty HEAD", head: "", want: "HEAD", ok: true},
		{name: "other symbolic ref", head: "ref: refs/tags/v1\n"},
		{name: "missing HEAD", head: "missing"},
		{name: "empty gitfile", gitfile: "empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			gitDir := filepath.Join(dir, ".git")
			if tc.gitfile != "" {
				gitDir = filepath.Join(dir, "metadata")
				target := gitDir
				switch tc.gitfile {
				case "relative":
					target = "metadata"
				case "empty":
					target = ""
				}
				if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: "+target+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Mkdir(gitDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.head != "missing" {
				if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte(tc.head), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cwd := filepath.Join(dir, "nested", "deeper")
			if err := os.MkdirAll(cwd, 0o755); err != nil {
				t.Fatal(err)
			}
			if got, ok := gitBranchFast(cwd); got != tc.want || ok != tc.ok {
				t.Fatalf("gitBranchFast = %q, %v; want %q, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
	t.Setenv("GIT_DIR", t.TempDir())
	if got, ok := gitBranchFast(t.TempDir()); got != "" || ok {
		t.Fatalf("GIT_DIR override = %q, %v; want fallback", got, ok)
	}
}
