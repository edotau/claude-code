package generate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// writeOpencodeCopilotLogin drops a github-copilot entry into opencode's 1.x auth.json, under the
// XDG_DATA_HOME (and empty PATH, so no real opencode CLI) scratch() already isolated.
func writeOpencodeCopilotLogin(t *testing.T, home string) {
	t.Helper()
	dir := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(`{"github-copilot":{"type":"oauth"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// With opencode's own github-copilot login present, renderOpenCode writes model/small_model with the
// github-copilot/ prefix and no provider/apiKey wiring, while preserving keys this tool doesn't own.
func TestRenderOpenCodeCopilotWritesModelAndSmallModel(t *testing.T) {
	home := scratch(t)
	// github-copilot's registry defaults are "auto" (Copilot's own router); pin real ids here so this
	// test still proves slot ids pass through unchanged, distinct from the auto -> OpencodeFallback swap.
	t.Setenv("HARNESS_SONNET_MODEL", "gpt-5.3-codex")
	t.Setenv("HARNESS_HAIKU_MODEL", "gpt-5.4-mini")
	writeOpencodeCopilotLogin(t, home)
	ocPath := filepath.Join(home, ".config", "opencode", "opencode.json")
	if err := os.MkdirAll(filepath.Dir(ocPath), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := `{"theme": "custom", "provider": {"harness-openai": {"npm": "stale"}, "myown": {"npm": "keep-me"}}}`
	if err := os.WriteFile(ocPath, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if _, err := Run(Options{Home: home, Vendors: true, Out: &out, Agents: []string{"opencode"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}

	raw, err := os.ReadFile(ocPath)
	if err != nil {
		t.Fatal(err)
	}
	var oc map[string]any
	if err := json.Unmarshal(raw, &oc); err != nil {
		t.Fatalf("opencode.json not valid JSON: %v\n%s", err, raw)
	}

	model, _ := oc["model"].(string)
	small, _ := oc["small_model"].(string)
	if !strings.HasPrefix(model, "github-copilot/") {
		t.Errorf("model = %q, want a github-copilot/ prefix", model)
	}
	if !strings.HasPrefix(small, "github-copilot/") {
		t.Errorf("small_model = %q, want a github-copilot/ prefix", small)
	}
	// small_model is the registry's github-copilot haiku pin (the auto → OpencodeFallback swap is pinned in internal/vendors).
	if small != "github-copilot/gpt-5.4-mini" {
		t.Errorf("small_model = %q, want the github-copilot haiku pin", small)
	}
	if _, ok := oc["apiKey"]; ok {
		t.Errorf("opencode.json has apiKey, want none for the github-copilot route: %s", raw)
	}
	if p, ok := oc["provider"].(map[string]any); ok {
		if _, stale := p["harness-openai"]; stale {
			t.Errorf("stale harness-* provider entry survived: %s", raw)
		}
		if _, keep := p["myown"]; !keep {
			t.Errorf("foreign provider entry %q was dropped: %s", "myown", raw)
		}
	} else {
		t.Error("provider block missing entirely; the foreign \"myown\" entry should have survived")
	}
	if theme, ok := oc["theme"]; !ok || theme != "custom" {
		t.Errorf("foreign top-level key %q was dropped: %s", "theme", raw)
	}
}

// github-copilot's registry default (OpencodeVariant "medium") appends "#medium" to model only;
// small_model never carries it, since opencode variants only apply to the main model slot.
func TestRenderOpenCodeCopilotAppendsVariantToModelOnly(t *testing.T) {
	home := scratch(t)
	t.Setenv("HARNESS_SONNET_MODEL", "claude-opus-5.5")
	t.Setenv("HARNESS_HAIKU_MODEL", "gpt-5.4-mini")
	writeOpencodeCopilotLogin(t, home)

	var out bytes.Buffer
	if _, err := Run(Options{Home: home, Vendors: true, Out: &out, Agents: []string{"opencode"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}

	raw, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	var oc map[string]any
	if err := json.Unmarshal(raw, &oc); err != nil {
		t.Fatalf("opencode.json not valid JSON: %v\n%s", err, raw)
	}
	model, _ := oc["model"].(string)
	small, _ := oc["small_model"].(string)
	if model != "github-copilot/claude-opus-5.5#medium" {
		t.Errorf("model = %q, want the #medium variant suffix", model)
	}
	if strings.Contains(small, "#") {
		t.Errorf("small_model = %q, want no variant suffix", small)
	}
}

// With OpencodeVariant empty, renderOpenCodeCopilot appends no "#" anywhere: the pre-variant behavior.
func TestRenderOpenCodeCopilotNoVariantNoHash(t *testing.T) {
	home := scratch(t)
	t.Setenv("HARNESS_SONNET_MODEL", "claude-opus-5.5")
	t.Setenv("HARNESS_HAIKU_MODEL", "gpt-5.4-mini")
	writeOpencodeCopilotLogin(t, home)
	overlay := `{"providers":{"github-copilot":{"kind":"openai","base_url":"https://api.githubcopilot.com",` +
		`"auth":{"type":"passthrough","login":"copilot"},` +
		`"models":{"opus":"auto","sonnet":"auto","haiku":"auto"},` +
		`"opencode_fallback":"claude-opus-5.5","opencodeID":"github-copilot"}}}`
	vaulttest.Put(t, vault.Overlay, "providers", overlay)

	var out bytes.Buffer
	if _, err := Run(Options{Home: home, Vendors: true, Out: &out, Agents: []string{"opencode"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}

	raw, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	var oc map[string]any
	if err := json.Unmarshal(raw, &oc); err != nil {
		t.Fatalf("opencode.json not valid JSON: %v\n%s", err, raw)
	}
	model, _ := oc["model"].(string)
	small, _ := oc["small_model"].(string)
	if strings.Contains(model, "#") {
		t.Errorf("model = %q, want no variant suffix with OpencodeVariant empty", model)
	}
	if strings.Contains(small, "#") {
		t.Errorf("small_model = %q, want no variant suffix", small)
	}
}

// A stray opencode.jsonc (which opencode prefers over .json) is retired to a timestamped backup, never deleted.
func TestRenderOpenCodeCopilotBacksUpStaleJSONC(t *testing.T) {
	home := scratch(t)
	writeOpencodeCopilotLogin(t, home)
	jsoncDir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(jsoncDir, 0o755); err != nil {
		t.Fatal(err)
	}
	jsoncPath := filepath.Join(jsoncDir, "opencode.jsonc")
	original := []byte(`{"model":"anthropic/claude-sonnet-5"}`)
	if err := os.WriteFile(jsoncPath, original, 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if _, err := Run(Options{Home: home, Vendors: true, Out: &out, Agents: []string{"opencode"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}

	if _, err := os.Stat(jsoncPath); !os.IsNotExist(err) {
		t.Fatalf("opencode.jsonc still present at the original path: err=%v", err)
	}
	entries, err := os.ReadDir(jsoncDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".bak-") {
			t.Errorf("no backup must land beside the original jsonc, got %v", e.Name())
		}
	}
	bakDir := filepath.Join(paths.CacheDir(), "backups")
	var bak string
	_ = filepath.WalkDir(bakDir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasPrefix(d.Name(), "opencode.jsonc.bak-") {
			bak = path
		}
		return nil
	})
	if bak == "" {
		t.Fatalf("no opencode.jsonc.bak-<ts> found under %s", bakDir)
	}
	got, err := os.ReadFile(bak)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Errorf("backup content = %q, want %q", got, original)
	}
}

// With no github-copilot login, the plain keyed-provider opencode route (and gemini/codex alongside
// it) is unaffected: this pins the "other routes unchanged" contract against the shared fixture.
func TestOpenCodeNonCopilotRouteUnchangedByTheCopilotFeature(t *testing.T) {
	home := scratch(t) // no github-copilot login written: openai (the registry default) serves opencode as before
	var out bytes.Buffer
	if _, err := Run(Options{Home: home, Vendors: true, Out: &out, Agents: []string{"opencode", "gemini"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	raw, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	var oc map[string]any
	if err := json.Unmarshal(raw, &oc); err != nil {
		t.Fatalf("opencode.json not valid JSON: %v", err)
	}
	if _, ok := oc["small_model"]; ok {
		t.Errorf("opencode.json has a \"small_model\" key, which only the github-copilot route sets: %s", raw)
	}
	if _, ok := oc["provider"].(map[string]any)["harness-openai"]; !ok {
		t.Errorf("opencode.json missing the harness-openai provider block: %s", raw)
	}
}
