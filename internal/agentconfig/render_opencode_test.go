package agent

import (
	"encoding/json"
	"os"
	"testing"

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
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"routing":{"cells":{"opencode":{"sonnet":{"model":"github-copilot/fix-sonnet"},"haiku":{"model":"github-copilot/fix-haiku"}}}}}`)
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
	want["model"], want["small_model"] = "github-copilot/fix-sonnet", "github-copilot/fix-haiku"

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

// TestApplyOpencodeTableModelsKeepsUnparsableTemplate checks a body that is not a JSON object comes back unchanged.
func TestApplyOpencodeTableModelsKeepsUnparsableTemplate(t *testing.T) {
	isolateOpencodeRouteEnv(t)
	vaulttest.Use(t)
	bad := []byte("not json")
	if got := applyOpencodeTableModels(bad); string(got) != string(bad) {
		t.Errorf("applyOpencodeTableModels = %s, want the input unchanged", got)
	}
}
