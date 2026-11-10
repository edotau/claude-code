package memory

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestDigestInputLiteralBracketConfig(t *testing.T) {
	isolatedConfig(t)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "claude[work]"))
	dir := HomeBankDir()
	write(t, filepath.Join(archiveDir(dir), "2026-01.md"), "first archive fact")
	write(t, filepath.Join(archiveDir(dir), "2026-02.md"), "second archive fact")
	write(t, filepath.Join(archiveDir(dir), DigestFile), "old digest must not be summarized")
	got, used, err := digestInput(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(used, []string{"2026-01.md", "2026-02.md"}) {
		t.Fatalf("archive sources under literal bracket path = %v", used)
	}
	if !strings.Contains(got, "first archive fact") || !strings.Contains(got, "second archive fact") {
		t.Fatalf("digest lost archive contents: %q", got)
	}
	if strings.Contains(got, "old digest") {
		t.Fatalf("digest included itself: %q", got)
	}
}

// The file that pushes the archive past the cap is refused by size, never read (b.md is an unreadable dir).
func TestDigestInputBoundsBeforeReading(t *testing.T) {
	dir := t.TempDir()
	arch := archiveDir(dir)
	if err := os.MkdirAll(filepath.Join(arch, "b.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(arch, "a.md"), bytes.Repeat([]byte("x"), digestMaxInput), 0o600); err != nil {
		t.Fatal(err)
	}
	_, used, err := digestInput(dir)
	if err == nil || !strings.Contains(err.Error(), "needs-scope") || !slices.Equal(used, []string{"a.md"}) {
		t.Errorf("digestInput = %v, %v; want needs-scope before reading b.md, after using a.md", used, err)
	}
}
