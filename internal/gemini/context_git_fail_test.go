//go:build unix

package gemini

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// fakeGit puts a `git` on PATH that prints stderr and exits with code.
func fakeGit(t *testing.T, code, stderr string) {
	t.Helper()
	bin := t.TempDir()
	write(t, filepath.Join(bin, "git"), "#!/bin/sh\necho '"+stderr+"' >&2\nexit "+code+"\n")
	if err := os.Chmod(filepath.Join(bin, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
}

// A git failure inside a real repo fails closed; only git's "not a git repository" means non-repo.
func TestCollectContextGitFailureFailsClosed(t *testing.T) {
	repo := t.TempDir()
	write(t, filepath.Join(repo, ".git", "HEAD"), "ref: refs/heads/main\n")
	write(t, filepath.Join(repo, "a.txt"), "hello")
	fakeGit(t, "1", "boom: index corrupt")
	got, err := CollectContext(context.Background(), repo, []string{"."}, nil, -1, 0)
	if err == nil || len(got.Included) != 0 {
		t.Fatalf("want error and no files, got err=%v files=%v", err, got.Included)
	}

	plain := t.TempDir()
	write(t, filepath.Join(plain, "a.txt"), "hello")
	fakeGit(t, "128", "fatal: not a git repository")
	got, err = CollectContext(context.Background(), plain, []string{"."}, nil, -1, 0)
	if err != nil || len(got.Included) != 1 {
		t.Fatalf("non-repo: want 1 file and no error, got err=%v files=%v", err, got.Included)
	}
}
