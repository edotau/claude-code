package pack

import (
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func TestCollectMixedGitAndNonGitRoots(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	write(t, filepath.Join(repo, ".gitignore"), ".gitignore\nignored.txt\n")
	write(t, filepath.Join(repo, "kept.txt"), "git-root fixture")
	write(t, filepath.Join(repo, "ignored.txt"), "ignored fixture")
	write(t, filepath.Join(dir, "plain", "kept.txt"), "non-git fixture")
	if out, err := exec.Command("git", "-C", repo, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	for _, mode := range []string{"walk", "glob"} {
		t.Run(mode, func(t *testing.T) {
			dirs, patterns := []string{"repo", "plain"}, []string(nil)
			if mode == "glob" {
				dirs, patterns = nil, []string{"repo/*.txt", "plain/*.txt"}
			}
			got, err := Collect(t.Context(), dir, dirs, patterns, 40, 100, testBudget)
			if err != nil {
				t.Fatal(err)
			}
			var paths []string
			for _, file := range got.Included {
				paths = append(paths, file.Path)
			}
			if want := []string{"plain/kept.txt", "repo/kept.txt"}; !slices.Equal(paths, want) {
				t.Fatalf("Included paths = %v, want %v; Skipped = %+v", paths, want, got.Skipped)
			}
			if !slices.Contains(got.Skipped, SkippedFile{"repo/ignored.txt", "gitignored"}) {
				t.Fatalf("Skipped = %+v, want gitignored repo/ignored.txt", got.Skipped)
			}
		})
	}
}
