package pack

import (
	"os"
	"path/filepath"
	"testing"
)

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
	if skipped := globPatterns(cwd, []string{"*.go"}, matches, nil, false); len(skipped) != 0 {
		t.Fatalf("skipped = %v", skipped)
	}
	if len(matches) != 1 {
		t.Fatalf("matches = %v, want a.go under %q", matches, cwd)
	}
}
