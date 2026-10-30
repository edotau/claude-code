package vendors

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// fixtureTableModels swaps the OpencodeTableModels seam (router sets it in production) for fixed models.
func fixtureTableModels(t *testing.T, mainModel, smallModel string, ok bool) {
	t.Helper()
	orig := OpencodeTableModels
	OpencodeTableModels = func() (string, string, bool) { return mainModel, smallModel, ok }
	t.Cleanup(func() { OpencodeTableModels = orig })
}

// TestOpencodeOverlayModelComesFromTable checks the rendered overlay's model/small_model are the seam's table models.
func TestOpencodeOverlayModelComesFromTable(t *testing.T) {
	isolateOpencodeRouteEnv(t)
	vaulttest.Use(t)
	fixtureTableModels(t, "fix/sonnet", "fix/haiku", true)
	_, cfgBody, _, _, err := settings.OpencodeOverlayRendered(Router{})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(cfgBody, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["model"] != "fix/sonnet" || doc["small_model"] != "fix/haiku" {
		t.Errorf("model=%v small_model=%v, want the seam's fix/sonnet and fix/haiku", doc["model"], doc["small_model"])
	}
}

// TestLaunchEnvCarriesOverlayPaths checks every opencode launch (applyLaunch) carries both overlay env vars.
func TestLaunchEnvCarriesOverlayPaths(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	env, _ := applyLaunch("opencode", nil, nil, true)
	var haveCfg, haveTUI bool
	for _, kv := range env {
		switch {
		case len(kv) > len("OPENCODE_CONFIG=") && kv[:len("OPENCODE_CONFIG=")] == "OPENCODE_CONFIG=":
			haveCfg = true
		case len(kv) > len("OPENCODE_TUI_CONFIG=") && kv[:len("OPENCODE_TUI_CONFIG=")] == "OPENCODE_TUI_CONFIG=":
			haveTUI = true
		}
	}
	if !haveCfg || !haveTUI {
		t.Errorf("applyLaunch(opencode) env = %v, want both OPENCODE_CONFIG and OPENCODE_TUI_CONFIG", env)
	}
}

// A copilot-only opencode launch renders github-copilot/<id> models, never an opencode-go id.
func TestOpencodeCopilotOnlyOverlayModels(t *testing.T) {
	data, _ := isolateOpencodeRouteEnv(t)
	vaulttest.Use(t)
	if err := os.MkdirAll(filepath.Join(data, "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "opencode", "auth.json"), []byte(`{"github-copilot":{"type":"oauth"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"github":{"personal":{"protocol":"openai",`+
		`"base_url":"https://api.githubcopilot.com","auth":{"type":"cli","login":"copilot"},`+
		`"models":{"opus":"gpt-6.1-sol","sonnet":"claude-sonnet-5.5","haiku":"mai-code-1.1-flash"}}}}}`)
	if OpencodeOwnLoginPresent() || !OpencodeCopilotLoginPresent() {
		t.Fatal("fixture must be copilot-only")
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	main, small, ok := OpencodeCopilotModelIDs(reg)
	if !ok {
		t.Fatal("no copilot model ids")
	}
	_, body, _, _, err := settings.OpencodeOverlayRendered(Router{})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["model"] != "github-copilot/"+main || doc["small_model"] != "github-copilot/"+small {
		t.Errorf("model=%v small_model=%v, want github-copilot/%s and github-copilot/%s", doc["model"], doc["small_model"], main, small)
	}
	for _, k := range []string{"model", "small_model"} {
		if s, _ := doc[k].(string); strings.HasPrefix(s, "opencode-go/") {
			t.Errorf("%s = %q, an opencode-go id on a copilot-only login", k, s)
		}
	}
}
