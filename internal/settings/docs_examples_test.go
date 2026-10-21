package settings

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDocsTemplateExampleSettings keeps docs/templates/examples/settings/settings.json identical to
// testdata/settings.golden.json — same render, a docs-discoverable location. Rewrite with the package's
// existing -update flag (declared in settings_test.go): `go test ./internal/settings -run
// TestDocsTemplateExampleSettings -update`.
func TestDocsTemplateExampleSettings(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("testdata", "settings.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	example := filepath.Join("..", "..", "docs", "templates", "examples", "settings", "settings.json")
	if *update {
		if err := os.MkdirAll(filepath.Dir(example), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(example, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(example)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("docs/templates/examples/settings/settings.json drifted from testdata/settings.golden.json " +
			"(go test ./internal/settings -run TestDocsTemplateExampleSettings -update)")
	}
}
