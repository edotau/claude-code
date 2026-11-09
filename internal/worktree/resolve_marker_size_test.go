package worktree

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestConflictMarkersClearHonorsCustomMarkerSize(t *testing.T) {
	repo := gitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("f.txt conflict-marker-size=9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		contents string
		wantPath string
	}{
		{"opening", "<<<<<<<<< HEAD\nours\n", "f.txt"},
		{"separator", "ours\n=========\ntheirs\n", "f.txt"},
		{"closing", "theirs\n>>>>>>>>> feature\n", "f.txt"},
		{"resolved", "resolved content\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte(tc.contents), 0o600); err != nil {
				t.Fatal(err)
			}
			clean, badPath, err := conflictMarkersClear(context.Background(), repo, []string{"f.txt"})
			if err != nil {
				t.Fatal(err)
			}
			if clean != (tc.wantPath == "") {
				t.Fatalf("clean = %v for %q; want offending path %q", clean, tc.contents, tc.wantPath)
			}
			if badPath != tc.wantPath {
				t.Fatalf("badPath = %q, want %q", badPath, tc.wantPath)
			}
		})
	}
}
