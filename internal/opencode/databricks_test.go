package opencode

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func regressionDatabricksProfile(t *testing.T, name string, menu []providers.OpencodeModel) *providers.Provider {
	t.Helper()
	vaulttest.Use(t)
	overlay := fmt.Sprintf(`{"providers":{"databricks":{%q:{"preset":"databricks","host":"https://workspace.example.test"}}}}`, name)
	vaulttest.Put(t, vault.Overlay, "providers", overlay)
	if err := providers.SetOpencodeMenu(menu); err != nil {
		t.Fatal(err)
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	p := reg.Row(name)
	if p == nil {
		t.Fatalf("missing test profile %q", name)
	}
	return p
}

func regressionGeneratedMenu(t *testing.T, p *providers.Provider, model string) []byte {
	t.Helper()
	env, err := opencodeDatabricks(p, "harness-"+p.Name, providers.ProtocolOpenAI, p.BaseURL, agentconfig.MenuWire(p, model), "", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range env {
		if body, ok := strings.CutPrefix(kv, opencodeConfigEnv+"="); ok {
			return []byte(body)
		}
	}
	t.Fatal("generated launch has no opencode config")
	return nil
}

// importGenerated feeds a generated launch config back through the menu importer.
func importGenerated(t *testing.T, body []byte) []providers.OpencodeModel {
	t.Helper()
	file := filepath.Join(t.TempDir(), "opencode.json")
	if err := os.WriteFile(file, body, 0o600); err != nil {
		t.Fatal(err)
	}
	got, _, err := catalog.MenuImportDiff(file)
	if err != nil {
		t.Fatalf("import generated config: %v", err)
	}
	return got
}

func TestMenuImportHyphenatedProfile(t *testing.T) {
	want := []providers.OpencodeModel{{ID: "glm-5-2", Label: "GLM 5.2", Responses: true}}
	p := regressionDatabricksProfile(t, "qa-prod", want)
	body := regressionGeneratedMenu(t, p, "glm-5-2")
	if !strings.Contains(string(body), `"harness-qa-prod-gpt"`) {
		t.Fatalf("fixture lacks hyphenated provider ID: %s", body)
	}
	if got := importGenerated(t, body); !reflect.DeepEqual(got, want) {
		t.Fatalf("imported menu = %+v, want %+v", got, want)
	}
}

func TestMenuImportPreservesExplicitRouting(t *testing.T) {
	menu := []providers.OpencodeModel{
		{ID: "glm-5-2", Label: "GLM 5.2", Responses: true},
		{ID: "gpt-6-1-sol", Label: "GPT 6.1 Sol", Responses: false},
	}
	p := regressionDatabricksProfile(t, "databricks-test", menu)
	want := []providers.OpencodeModel{menu[1], menu[0]}
	if got := importGenerated(t, regressionGeneratedMenu(t, p, "glm-5-2")); !reflect.DeepEqual(got, want) {
		t.Fatalf("import must retain explicit Responses and chat overrides: got %+v, want %+v", got, want)
	}
}
