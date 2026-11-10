package opencode

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	render "github.com/edotau/claude-code/internal/agentconfig/render"
	"github.com/edotau/claude-code/internal/paths"
)

// TestRenderOpenCodeOverlayReturnsReadError: an unreadable overlay file is an error, not "absent" (silent drift).
func TestRenderOpenCodeOverlayReturnsReadError(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := os.MkdirAll(filepath.Join(paths.StateDir(), "opencode", "opencode.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	changed, err := renderOpenCodeOverlay(render.RenderOptions{Check: true}, io.Discard)
	if err == nil {
		t.Fatalf("a directory in place of opencode.json read as absent: changed=%v, want an error", changed)
	}
}
