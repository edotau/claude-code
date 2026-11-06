package launch

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSnapshotPreInstallWarnsLoudlyOnFailure: a snapshot that cannot be taken (history dir blocked by a file)
// returns "" and prints a warning that says the binary has no rollback point.
func TestSnapshotPreInstallWarnsLoudlyOnFailure(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if err := os.WriteFile(filepath.Join(cfg, "state"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if got := snapshotPreInstall(&buf, false, true); got != "" {
		t.Fatalf("commit = %q, want empty on failure", got)
	}
	out := buf.String()
	if !strings.Contains(out, "warning  history snapshot failed") || !strings.Contains(out, "no rollback point") {
		t.Fatalf("output lacks loud warning: %q", out)
	}
}
