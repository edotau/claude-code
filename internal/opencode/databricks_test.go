package opencode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/serving"
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

// A family id newer than the menu and catalog runs on the Responses provider, as its known sibling does.
func TestOpencodeDatabricksRunsUnknownFamilyMember(t *testing.T) {
	p := regressionDatabricksProfile(t, "dev", providers.DatabricksOpencodeMenu)
	body := string(regressionGeneratedMenu(t, p, "gpt-7-sol"))
	if !strings.Contains(body, `"model":"harness-dev-gpt/databricks-gpt-7-sol"`) {
		t.Errorf("gpt-7-sol launch config = %s, want it on the Responses provider", body)
	}
	if _, err := opencodeDatabricks(p, "harness-dev", providers.ProtocolOpenAI, p.BaseURL, "databricks-gpt-7-nova", "", false, nil); err == nil {
		t.Error("an off-menu id outside every family must stay refused")
	}
}

func TestMenuEntriesCarryCatalogLimits(t *testing.T) {
	menu := []providers.OpencodeModel{{ID: "claude-opus-5-5", Label: "Opus"}, {ID: "glm-5-2", Label: "GLM", Responses: true}}
	p := regressionDatabricksProfile(t, "qa-lim", menu)
	body := string(regressionGeneratedMenu(t, p, "glm-5-2"))
	var cfg struct {
		Provider map[string]struct {
			Models map[string]struct {
				Limit *struct{ Context, Output int } `json:"limit"`
			} `json:"models"`
		} `json:"provider"`
	}
	if err := json.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatal(err)
	}
	lim, known := models.Limits("claude-opus-5-5")
	if !known {
		t.Fatal("catalog lacks claude-opus-5-5 limits")
	}
	got := map[string]*struct{ Context, Output int }{}
	for _, prov := range cfg.Provider {
		for id, m := range prov.Models {
			got[id] = m.Limit
		}
	}
	if o := got["databricks-claude-opus-5-5"]; o == nil || o.Context != lim.Context || o.Output != lim.Output {
		t.Errorf("opus limit = %+v, want %+v", o, lim)
	}
	if g, ok := got["databricks-glm-5-2"]; !ok || g != nil {
		t.Errorf("glm entry present=%v limit=%+v, want present without a limit", ok, g)
	}
}

// Given a model the catalog lacks but the listing states both limits, Then the entry gets the limit block.
func TestWithLimitUsesListedLimits(t *testing.T) {
	serving.SetFactsForTest(t, func(_, wire string) (serving.EndpointFacts, bool) {
		return serving.EndpointFacts{Context: 400000, Output: 128000}, wire == "databricks-listed-only"
	})
	p := &providers.Provider{Name: "dbx", Preset: providers.PresetDatabricks}
	got := withLimit(map[string]any{}, p, "databricks-listed-only")
	if want := (map[string]any{"context": 400000, "output": 128000}); !reflect.DeepEqual(got["limit"], want) {
		t.Errorf("limit = %v, want %v", got["limit"], want)
	}
	if _, ok := withLimit(map[string]any{}, p, "databricks-other")["limit"]; ok {
		t.Error("an unknown model must get no limit block")
	}
}
