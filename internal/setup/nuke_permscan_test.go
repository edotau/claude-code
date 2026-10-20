package setup

import (
	"os"
	"path/filepath"
	"testing"
)

// TestKeepMemoryPathsPropagatesWalkError is the GREEN for the audited bug: an unreadable subdir under
// memory/ must surface an error, never a silently truncated file list that a nuke would archive as if
// it were complete.
func TestKeepMemoryPathsPropagatesWalkError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "memory", "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "file.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory", "top.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(locked, 0o755)

	if _, err := keepMemoryPaths(dir); err == nil {
		t.Fatal("keepMemoryPaths = nil error, want a propagated walk error")
	}

	// Plan must refuse to call the archive incomplete: it reports the same error, not a partial plan.
	if _, err := Plan(dir, true); err == nil {
		t.Fatal("Plan(dir, true) = nil error, want it to refuse an incomplete archive plan")
	}
}
