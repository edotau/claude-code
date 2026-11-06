package codex

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCodexFileOverridesLongLine(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	value := `"` + strings.Repeat("x", 80<<10) + `"`
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[model_provider]\ninstructions = "+value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := codexFileOverrides(path)
	if err != nil {
		t.Fatalf("valid config line over 64 KiB: %v", err)
	}
	want := []string{"-c", "model_provider.instructions=" + value}
	if !slices.Equal(got, want) {
		t.Fatalf("long config value did not round-trip as a table-qualified -c override (got %d args)", len(got))
	}
}
