package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// agy has no custom base URL, so a gateway provider for the gemini vendor must fail fast with a clear error.
func TestGeminiGateway(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GW_KEY", secret)
	p := &providers.Provider{Name: "gw", Protocol: providers.ProtocolGemini, BaseURL: "https://gw.example/gemini",
		Auth: providers.Auth{Type: providers.AuthKey, Env: "GW_KEY"}, Headers: map[string]string{"X-Team": "a"}}
	_, _, err := schemaFor("gemini").Configure(context.Background(), providers.Target{Provider: p, Model: "m"}, "")
	if err == nil || !strings.Contains(err.Error(), "no custom base URL") {
		t.Fatalf("err = %v, want a clear \"no custom base URL\" error", err)
	}
}

// gemini runs only on agy's cached Google login (AgyLoginMarker under HOME): no marker refuses to
// configure (no GEMINI_API_KEY fallback), a marker on disk lets it through with no key injected.
func TestGeminiLoginPrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if GeminiLoginPresent() {
		t.Fatal("no marker on disk must read as not logged in")
	}
	tg := target(t, "antigravity", "gemini-2.5-pro")
	// target() reset HOME to its own tempdir; point it back at ours for the marker check.
	t.Setenv("HOME", home)

	if _, _, err := schemaFor("gemini").configureHeadless(context.Background(), tg, Headless{Prompt: "hi"}); err == nil || !strings.Contains(err.Error(), "not signed in") {
		t.Fatalf("without a login marker, err = %v, want a not-signed-in error", err)
	}

	marker := AgyLoginMarker(home)
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !GeminiLoginPresent() {
		t.Fatal("marker on disk must read as logged in")
	}

	env, _, err := schemaFor("gemini").configureHeadless(context.Background(), tg, Headless{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := envMap(env)["GEMINI_API_KEY"]; ok {
		t.Errorf("with a login marker, GEMINI_API_KEY must not be set, got %q", got)
	}
}

// D6: agy's model is flag > process env > the GEMINI_MODEL vault pin > agy's default.
func TestAgyModelPrecedence(t *testing.T) {
	vaulttest.Use(t)
	for _, k := range modelEnvKeys {
		t.Setenv(k, "")
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	sel, _ := providers.Select(reg, "")
	if got := geminiAgyModel(reg, sel, reg.Providers["anthropic"], ""); got != AgyDefaultModel {
		t.Errorf("nothing set = %q, want the default %q", got, AgyDefaultModel)
	}
	if err := providers.SetPins(map[string]string{providers.PinGeminiModel: "gemini-pinned"}); err != nil {
		t.Fatal(err)
	}
	if got := geminiAgyModel(reg, sel, reg.Providers["anthropic"], ""); got != "gemini-pinned" {
		t.Errorf("vault pin = %q, want gemini-pinned", got)
	}
	t.Setenv("GEMINI_MODEL", "gemini-env")
	if got := geminiAgyModel(reg, sel, reg.Providers["anthropic"], ""); got != "gemini-env" {
		t.Errorf("process env = %q, want it to beat the pin", got)
	}
	if got := geminiAgyModel(reg, sel, reg.Providers["anthropic"], "gemini-flag"); got != "gemini-flag" {
		t.Errorf("flag = %q, want it to beat env and pin", got)
	}
}

// Regression: no GEMINI_PROFILE pin and no Google login must refuse, never bill the session's Databricks row.
func TestResolveTargetGeminiNoLoginNoPinRefuses(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("HOME", t.TempDir())
	reg := &providers.Registry{Providers: map[string]*providers.Provider{}}
	p := dbxRow("qa", "databricks-")
	reg.Providers[p.Name] = p
	sel := providers.Selection{Provider: p, Slots: map[string]providers.Target{"opus": {Provider: p, Model: "databricks-claude-opus-5-5"}}}
	_, _, err := schemaFor("gemini").ResolveTarget(reg, sel, false)
	if err == nil || !strings.Contains(err.Error(), "GEMINI_PROFILE") {
		t.Fatalf("err = %v, want the Google-login-or-pin refusal", err)
	}
}

// Regression: --direct has no Databricks route for agy (the router swaps the token); refuse, don't route.
func TestGeminiGatewayRefusesDirect(t *testing.T) {
	vaulttest.Use(t)
	StubRouterForTest(t, "http://127.0.0.1:1")
	_, _, err := schemaFor("gemini").configureRoute(WithDirect(context.Background()),
		providers.Target{Provider: dbxRow("qa", "databricks-"), Model: "gemini-3.8-flash"}, &Headless{Prompt: "hi"})
	if err == nil || !strings.Contains(err.Error(), "--direct") {
		t.Fatalf("err = %v, want a --direct refusal", err)
	}
}
