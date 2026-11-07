package codex

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCodexOwnModelSkipsMultilineValues(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	raw := "notes = \"\"\"\nmodel = \"fake\"\n\"\"\"\nlist = [\n  \"a\",\n]\nmodel = \"real\"\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := codexOwnModel(); got != "real" {
		t.Errorf("got %q, want real", got)
	}
}
