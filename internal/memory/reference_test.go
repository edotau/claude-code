package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReferenceMissingFileErrors: a missing skills/reflection/references/<verb>.md is an error —
// a background job must fail loudly rather than run on a silently empty prompt.
func TestReferenceMissingFileErrors(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if _, err := Reference("dream"); err == nil {
		t.Fatal("Reference(\"dream\") with no file on disk = nil error, want one")
	}
}

// TestReferenceEmptyFileErrors: a present-but-empty (or whitespace-only) reference is also refused —
// it is just as silent as a missing file.
func TestReferenceEmptyFileErrors(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	refDir := filepath.Join(cfg, "skills", "reflection", "references")
	if err := os.MkdirAll(refDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(refDir, "promote.md"), []byte("   \n\n  "), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Reference("promote"); err == nil {
		t.Fatal("Reference(\"promote\") on a whitespace-only file = nil error, want one")
	}
}

// TestReferencePresentReturnsContent: a present, non-empty reference returns its trimmed content verbatim.
func TestReferencePresentReturnsContent(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	refDir := filepath.Join(cfg, "skills", "reflection", "references")
	if err := os.MkdirAll(refDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "\n\n# Review procedure\n\nstep one\nstep two\n\n"
	if err := os.WriteFile(filepath.Join(refDir, "review.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Reference("review")
	if err != nil {
		t.Fatalf("Reference(\"review\"): %v", err)
	}
	if got != strings.TrimSpace(body) {
		t.Errorf("Reference(\"review\") = %q, want %q", got, strings.TrimSpace(body))
	}
}
