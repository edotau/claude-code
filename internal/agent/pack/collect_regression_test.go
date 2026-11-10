package pack

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestCollectRejectsSecretThroughSymlinkedParent(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "state", "credentials.txt"), "private fixture")
	write(t, filepath.Join(dir, "public", "readme.txt"), "public fixture")
	for alias, target := range map[string]string{"alias": "state", "docs": "public"} {
		if err := os.Symlink(filepath.Join(dir, target), filepath.Join(dir, alias)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := Collect(t.Context(), dir, nil, []string{"alias/*.txt", "docs/*.txt"}, 40, 100, testBudget)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Included) != 1 || got.Included[0].Path != "docs/readme.txt" || got.Included[0].Content != "public fixture" {
		t.Fatalf("Included = %+v, want only public symlink target", got.Included)
	}
	if !slices.Contains(got.Skipped, SkippedFile{"alias/credentials.txt", "secret-like"}) {
		t.Fatalf("Skipped = %+v, want alias/credentials.txt rejected as secret-like", got.Skipped)
	}
}

func TestCollectIncludesTrackedSiblingDirectory(t *testing.T) {
	repo := t.TempDir()
	write(t, filepath.Join(repo, "app", "local.go"), "package app")
	write(t, filepath.Join(repo, "lib", "a.go"), "package lib")
	write(t, filepath.Join(repo, "lib", ".gitignore"), "ignored.txt\n.gitignore\n")
	write(t, filepath.Join(repo, "lib", "ignored.txt"), "ignored fixture")
	for _, args := range [][]string{{"init"}, {"add", "lib/a.go"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	got, err := Collect(t.Context(), filepath.Join(repo, "app"), []string{filepath.Join(repo, "lib")}, nil, 40, 100, testBudget)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Included) != 1 || got.Included[0].Path != "../lib/a.go" || got.Included[0].Content != "package lib" {
		t.Fatalf("Included = %+v, want tracked ../lib/a.go", got.Included)
	}
	if !slices.Contains(got.Skipped, SkippedFile{"../lib/ignored.txt", "gitignored"}) {
		t.Fatalf("Skipped = %+v, want ignored sibling file excluded", got.Skipped)
	}
}

type cancelAfterDiscoveryContext struct {
	context.Context
	cancel context.CancelFunc
	marker string
}

func (c cancelAfterDiscoveryContext) Err() error {
	if _, err := os.Stat(c.marker); err == nil {
		c.cancel()
	}
	return c.Context.Err()
}

func TestCollectCancellationAfterGitDiscovery(t *testing.T) {
	for _, mode := range []string{"walk", "glob"} {
		t.Run(mode, func(t *testing.T) {
			dir, bin := t.TempDir(), t.TempDir()
			if mode == "glob" {
				write(t, filepath.Join(dir, "a.txt"), "must not be read")
			}
			marker := filepath.Join(bin, "listed")
			script := "#!/bin/sh\ncase \"$3\" in\nrev-parse) printf 'true\\n';;\n" +
				"ls-files) printf 'a.txt\\000'; : > \"$PACK_TEST_LISTED\";;\nesac\n"
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			t.Setenv("PACK_TEST_LISTED", marker)
			base, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx := cancelAfterDiscoveryContext{base, cancel, marker}
			dirs, patterns := []string{"."}, []string(nil)
			if mode == "glob" {
				dirs, patterns = nil, []string{"a.txt"}
			}
			got, err := Collect(ctx, dir, dirs, patterns, 40, 100, testBudget)
			if _, markerErr := os.Stat(marker); markerErr != nil {
				t.Fatalf("git discovery did not finish: %v", markerErr)
			}
			if !errors.Is(err, context.Canceled) || len(got.Included) != 0 {
				t.Fatalf("Collect = %+v, %v, want cancellation without files after discovery", got, err)
			}
		})
	}
}

// A cwd containing glob metacharacters must still match its own files literally.
func TestGlobPatternsEscapesCwd(t *testing.T) {
	cwd := filepath.Join(t.TempDir(), "w[1]*?")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "a.go"), []byte("package a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	matches := map[string]bool{}
	if skipped := globPattern(cwd, "*.go", matches, nil, false); len(skipped) != 0 {
		t.Fatalf("skipped = %v", skipped)
	}
	if len(matches) != 1 {
		t.Fatalf("matches = %v, want a.go under %q", matches, cwd)
	}
}

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
	for _, tc := range []struct {
		name     string
		dirs     []string
		patterns []string
	}{
		{"walk", []string{"repo", "plain"}, nil},
		{"glob", nil, []string{"repo/*.txt", "plain/*.txt"}},
		{"git-walk-plain-glob", []string{"repo"}, []string{"plain/*.txt"}},
		{"plain-walk-git-glob", []string{"plain"}, []string{"repo/*.txt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Collect(t.Context(), dir, tc.dirs, tc.patterns, 40, 100, testBudget)
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

// A FIFO sitting in a scanned dir must be skipped by its directory-entry type alone (fs.DirEntry's
// Type() comes from the readdir syscall, no open needed), never opened — opening a FIFO with no
// writer blocks forever, so a regression here would hang the whole scan rather than just mis-skip.
func TestWalkDirsSkipsFIFO(t *testing.T) {
	skipUnlessUnix(t)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "real.txt"), "hello")
	fifoPath := filepath.Join(dir, "a.fifo")
	if err := exec.Command("mkfifo", "-m", "600", fifoPath).Run(); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	done := make(chan struct {
		ctx Context
		err error
	}, 1)
	go func() {
		ctx, err := Collect(t.Context(), dir, []string{"."}, nil, 40, 100, testBudget)
		done <- struct {
			ctx Context
			err error
		}{ctx, err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Collect err = %v", r.err)
		}
		checkFIFOContext(t, r.ctx)
	case <-time.After(5 * time.Second):
		t.Fatal("Collect hung — the FIFO was opened for read instead of skipped by dir-entry type")
	}
}

// checkFIFOContext requires the FIFO skipped as non-regular and the sibling regular file still included.
func checkFIFOContext(t *testing.T, ctx Context) {
	t.Helper()
	foundReal := false
	for _, inc := range ctx.Included {
		if inc.Path == "a.fifo" {
			t.Fatalf("FIFO was opened and included: %+v", inc)
		}
		if inc.Path == "real.txt" {
			foundReal = true
		}
	}
	found := false
	for _, s := range ctx.Skipped {
		if s.Path == "a.fifo" {
			found = true
			if s.Reason != "non-regular-file" {
				t.Errorf("skip reason = %q, want %q", s.Reason, "non-regular-file")
			}
		}
	}
	if !found {
		t.Error("a.fifo not reported in Skipped at all")
	}
	if !foundReal {
		t.Error("real.txt (a genuine regular file alongside the FIFO) must still be included")
	}
}
