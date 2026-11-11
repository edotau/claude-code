package opencode

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/paths"
)

// TestRenderOpenCodeOverlayReturnsReadError: an unreadable overlay file is an error, not "absent" (silent drift).
func TestRenderOpenCodeOverlayReturnsReadError(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := os.MkdirAll(filepath.Join(paths.StateDir(), "opencode", "opencode.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	changed, err := renderOpenCodeOverlay(agentconfig.RenderOptions{Check: true}, io.Discard)
	if err == nil {
		t.Fatalf("a directory in place of opencode.json read as absent: changed=%v, want an error", changed)
	}
}

func TestOpencodeRulesOnUsesPortableCoreWithoutClaudeMD(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	std := filepath.Join(dir, "rules", "standards")
	if err := os.MkdirAll(std, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(dir, "CLAUDE.md"): "# c\n",
		filepath.Join(std, "b.md"):      "# b\n",
		filepath.Join(std, "a.md"):      "# a\n",
		filepath.Join(std, "scoped.md"): "---\npaths: [\"*.go\"]\n---\n# s\n",
	}
	for p, body := range files {
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := map[string]any{}
	opencodeRulesOn(cfg)
	want := []string{filepath.Join(std, "a.md"), filepath.Join(std, "b.md")}
	if got := cfg["instructions"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("instructions = %v, want %v", got, want)
	}
}
