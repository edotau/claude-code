package launch

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/edotau/claude-code/internal/settings"
)

// update rewrites docs/templates/examples/settings/settings.windows.json (same convention as
// internal/settings's TestRenderGolden): `go test ./internal/launch -run TestDocsTemplateExampleWindows -update`.
var update = flag.Bool("update", false, "rewrite docs/templates/examples/settings.windows.json")

// TestDocsTemplateExampleWindows renders the Windows subset of the live settings render (via
// stripWindowsOnly) and keeps docs/templates/examples/settings/settings.windows.json in sync with it.
func TestDocsTemplateExampleWindows(t *testing.T) {
	rendered, err := settings.Render()
	if err != nil {
		t.Fatal(err)
	}
	winRendered, err := stripWindowsOnly(rendered)
	if err != nil {
		t.Fatal(err)
	}
	example := filepath.Join("..", "..", "docs", "templates", "examples", "settings", "settings.windows.json")
	if *update {
		if err := os.MkdirAll(filepath.Dir(example), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(example, winRendered, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(example)
	if err != nil {
		t.Fatal(err)
	}
	if string(winRendered) != string(want) {
		t.Errorf("docs/templates/examples/settings/settings.windows.json drifted from stripWindowsOnly(settings.Render()) " +
			"(go test ./internal/launch -run TestDocsTemplateExampleWindows -update)")
	}
}
