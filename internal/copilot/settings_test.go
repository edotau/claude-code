package copilot

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agentconfig/render"
)

// An unreadable settings.json (here a directory) must be skipped, not rewritten from an empty map.
func TestRenderCopilotSettingsUnreadableIsSkipped(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	home := t.TempDir()
	path := filepath.Join(home, ".copilot", "settings.json")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	changed, err := renderCopilotSettings(home, render.RenderOptions{}, &out)
	if err != nil || changed {
		t.Fatalf("changed=%v err=%v, want false,nil", changed, err)
	}
	if !strings.Contains(out.String(), "cannot read") {
		t.Fatalf("output %q lacks the cannot-read skip note", out.String())
	}
	if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
		t.Fatalf("path was replaced: %v", err)
	}
}
