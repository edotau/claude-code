package pack

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestChangedFilesErrorsOutsideRepo: called outside a git work tree, the error mentions git rather
// than masking the failure as an empty diff.
func TestChangedFilesErrorsOutsideRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	_, err := ChangedFiles(t.Context(), dir)
	if err == nil {
		t.Fatal("expected an error outside a git work tree")
	}
	if !strings.Contains(err.Error(), "git") {
		t.Errorf("err = %v, want it to mention git", err)
	}
}

// TestBuildIndexOnlyInlinesNoBodies: IndexOnly packs the index of every path and no <file> body.
func TestBuildIndexOnlyInlinesNoBodies(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.go"), "package a")
	write(t, filepath.Join(dir, "b.go"), "package b")
	got, err := Build(t.Context(), Spec{Dirs: []string{"."}, Index: true, IndexOnly: true}, "t", dir, testBudget)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.Prompt, "<file path=") || !strings.Contains(got.Prompt, "a.go") || !strings.Contains(got.Prompt, "b.go") {
		t.Errorf("want both paths in the index and no file body:\n%s", got.Prompt)
	}
}

// TestBuildDiffInlinesOnlyChanged: Diff packs only files changed vs HEAD and refuses once nothing changed.
func TestBuildDiffInlinesOnlyChanged(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	write(t, filepath.Join(repo, "base.txt"), "base content")
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "T")
	git("add", ".")
	git("commit", "-m", "base")
	write(t, filepath.Join(repo, "new.txt"), "untracked content")

	got, err := Build(t.Context(), Spec{Diff: true, MaxFiles: -1}, "t", repo, testBudget)
	if err != nil {
		t.Fatal(err)
	}
	if p := got.Prompt; !strings.Contains(p, `<file path="new.txt"`) || strings.Contains(p, `<file path="base.txt"`) {
		t.Errorf("want only new.txt inlined:\n%s", p)
	}
	git("add", ".")
	git("commit", "-m", "new")
	if _, err = Build(t.Context(), Spec{Diff: true}, "t", repo, testBudget); err == nil || !strings.Contains(err.Error(), "nothing changed") {
		t.Errorf("err = %v, want 'nothing changed'", err)
	}
}

// TestBuildLargeFileAccepted: a 200 KiB inlined file is packed whole, not capped.
func TestBuildLargeFileAccepted(t *testing.T) {
	const bigFile = 200 << 10
	dir := t.TempDir()
	write(t, filepath.Join(dir, "big.txt"), strings.Repeat("x", bigFile))
	got, err := Build(t.Context(), Spec{Dirs: []string{"."}, MaxFiles: -1, MaxFileBytes: 2 * bigFile}, "t", dir, testBudget)
	if err != nil {
		t.Fatal(err)
	}
	if p := got.Prompt; !strings.Contains(p, `<file path="big.txt"`) || len(p) < bigFile {
		t.Errorf("big.txt not inlined whole (prompt %d bytes)", len(p))
	}
}
