package vendors

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router/routetable"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// TestOpencodeOverlayTemplatesParse pins the embedded templates as valid JSON.
func TestOpencodeOverlayTemplatesParse(t *testing.T) {
	for _, name := range []string{"template/opencode.json", "template/opencode-tui.json"} {
		body, err := opencodeOverlayFS.ReadFile(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var doc map[string]any
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Fatalf("%s: not valid JSON: %v", name, err)
		}
	}
}

// TestWriteOpencodeOverlayWritesBothFiles checks the two overlay files land at state/harness/opencode
// with mode 0600, and that the cfg file's model/small_model come from the routing table, every other
// field unchanged from the embedded template (the table-driven contract applyOpencodeTableModels keeps).
func TestWriteOpencodeOverlayWritesBothFiles(t *testing.T) {
	isolateOpencodeRouteEnv(t)
	cfg, tui, err := WriteOpencodeOverlay()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{cfg, tui} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", p, fi.Mode().Perm())
		}
	}
	tmpl, err := opencodeOverlayFS.ReadFile("template/opencode.json")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal(tmpl, &want); err != nil {
		t.Fatal(err)
	}
	tb, err := routetable.Load()
	if err != nil {
		t.Fatal(err)
	}
	sonnet, _ := tb.Cell("opencode", "sonnet")
	haiku, _ := tb.Cell("opencode", "haiku")
	want["model"], want["small_model"] = sonnet.Model, haiku.Model

	raw, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("opencode.json: not valid JSON: %v", err)
	}
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("opencode.json = %s, want %s (model/small_model from the table, rest from the template)", gotJSON, wantJSON)
	}
}

// TestOpencodeOverlayModelComesFromTable checks the rendered overlay's model/small_model equal the
// routing table's opencode sonnet/haiku cells, both with no overlay and with a vault route override.
func TestOpencodeOverlayModelComesFromTable(t *testing.T) {
	isolateOpencodeRouteEnv(t)
	vaulttest.Use(t)
	_, cfgBody, _, _, err := OpencodeOverlayRendered()
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(cfgBody, &doc); err != nil {
		t.Fatal(err)
	}
	tb, err := routetable.Load()
	if err != nil {
		t.Fatal(err)
	}
	sonnet, _ := tb.Cell("opencode", "sonnet")
	haiku, _ := tb.Cell("opencode", "haiku")
	if doc["model"] != sonnet.Model {
		t.Errorf("model = %v, want %q (routetable's opencode/sonnet cell)", doc["model"], sonnet.Model)
	}
	if doc["small_model"] != haiku.Model {
		t.Errorf("small_model = %v, want %q (routetable's opencode/haiku cell)", doc["small_model"], haiku.Model)
	}

	// A vault route override on opencode/sonnet must change the rendered model, not just the table's own read.
	// github-copilot/ is a known backend prefix (its billing moves to the copilot account); an unknown
	// prefix like "zed/" is rejected by routetable.LoadFrom and covered in routetable's own tests.
	overlay := `{"routing":{"cells":{"opencode":{"sonnet":{"model":"github-copilot/overridden-model"}}}}}`
	vaulttest.Put(t, vault.Overlay, "providers", overlay)
	_, cfgBody2, _, _, err := OpencodeOverlayRendered()
	if err != nil {
		t.Fatal(err)
	}
	var doc2 map[string]any
	if err := json.Unmarshal(cfgBody2, &doc2); err != nil {
		t.Fatal(err)
	}
	if doc2["model"] != "github-copilot/overridden-model" {
		t.Errorf("overridden model = %v, want %q", doc2["model"], "github-copilot/overridden-model")
	}
	if doc2["small_model"] != haiku.Model {
		t.Errorf("small_model after an opus/sonnet-only override = %v, want unchanged %q", doc2["small_model"], haiku.Model)
	}
}

// TestApplyOpencodeTableModelsFallsBackOnLoadError checks a routetable load failure (an overlay naming an
// unknown cell) leaves the template's own hard-coded model/small_model untouched, the one fallback use.
func TestApplyOpencodeTableModelsFallsBackOnLoadError(t *testing.T) {
	isolateOpencodeRouteEnv(t)
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"routing":{"cells":{"ghost":{"opus":{"model":"m"}}}}}`)
	tmpl, err := opencodeOverlayFS.ReadFile("template/opencode.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := routetable.Load(); err == nil {
		t.Fatal("precondition: the unknown overlay cell must make routetable.Load fail")
	}
	got := applyOpencodeTableModels(tmpl)
	if string(got) != string(tmpl) {
		t.Errorf("applyOpencodeTableModels on a load error = %s, want the template unchanged", got)
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
	_, body, _, _, err := OpencodeOverlayRendered()
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
