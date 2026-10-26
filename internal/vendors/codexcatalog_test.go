package vendors

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestCodexCatalogArgsDatabricks: GPT menu ids become catalog slugs with row labels, no tiers or upgrade prompts.
func TestCodexCatalogArgsDatabricks(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	old := codexDebugModels
	t.Cleanup(func() { codexDebugModels = old })
	codexDebugModels = func() ([]byte, error) {
		return []byte(`{"models":[{"slug":"gpt-6.1-sol","display_name":"x","service_tiers":["priority"],"upgrade":{"id":"y"}},
{"slug":"gpt-5.5","display_name":"z","service_tiers":["priority"]}]}`), nil
	}
	args := codexCatalogArgs(dbxRow("qa", "databricks-"))
	if len(args) != 2 || !strings.HasPrefix(args[1], "model_catalog_json=") {
		t.Fatalf("args = %q", args)
	}
	path := strings.Trim(strings.TrimPrefix(args[1], "model_catalog_json="), `"`)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cat struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(raw, &cat); err != nil {
		t.Fatal(err)
	}
	if len(cat.Models) != 2 {
		t.Fatalf("models = %d, want 2 (gpt-6-astra absent from codex's catalog)", len(cat.Models))
	}
	m := cat.Models[0]
	if m["slug"] != "databricks-gpt-6-1-sol" || m["display_name"] != "GPT-6.1 Sol · qa" || m["upgrade"] != nil ||
		len(m["service_tiers"].([]any)) != 0 {
		t.Errorf("entry = %v", m)
	}
}

// TestCodexCatalogArgsSkipsOtherProviders: a non-Databricks provider gets no catalog flag.
func TestCodexCatalogArgsSkipsOtherProviders(t *testing.T) {
	p := dbxRow("qa", "databricks-")
	p.Preset = ""
	if args := codexCatalogArgs(p); args != nil {
		t.Errorf("args = %q, want nil", args)
	}
}

// TestCodexCatalogArgsIdempotent: a dump of our own catalog (codex reading it back) yields the same catalog.
func TestCodexCatalogArgsIdempotent(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	old := codexDebugModels
	t.Cleanup(func() { codexDebugModels = old })
	codexDebugModels = func() ([]byte, error) {
		return []byte(`{"models":[{"slug":"databricks-gpt-6-1-sol","display_name":"GPT-6.1 Sol · qa","service_tiers":[]}]}`), nil
	}
	if args := codexCatalogArgs(dbxRow("qa", "databricks-")); len(args) != 2 {
		t.Fatalf("args = %q, want the catalog flag when codex already reads our catalog", args)
	}
}
