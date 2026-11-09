package pack

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
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
