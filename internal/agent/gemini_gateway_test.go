package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// Regression: no GEMINI_PROFILE pin and no Google login must refuse, never bill the session's Databricks row.
func TestResolveTargetGeminiNoLoginNoPinRefuses(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("HOME", t.TempDir())
	reg := &providers.Registry{Providers: map[string]*providers.Provider{}}
	p := dbxRow("qa", "databricks-")
	reg.Providers[p.Name] = p
	sel := providers.Selection{Provider: p, Slots: map[string]providers.Target{"opus": {Provider: p, Model: "databricks-claude-opus-5-5"}}}
	_, _, err := ResolveVendorTarget(reg, "gemini", sel, false)
	if err == nil || !strings.Contains(err.Error(), "GEMINI_PROFILE") {
		t.Fatalf("err = %v, want the Google-login-or-pin refusal", err)
	}
}

// Regression: --direct has no Databricks route for agy (the router swaps the token); refuse, don't route.
func TestGeminiGatewayRefusesDirect(t *testing.T) {
	vaulttest.Use(t)
	StubRouterForTest(t, "http://127.0.0.1:1")
	_, _, err := configureRoute(WithDirect(context.Background()), "gemini",
		providers.Target{Provider: dbxRow("qa", "databricks-"), Model: "gemini-3.8-flash"}, &Headless{Prompt: "hi"})
	if err == nil || !strings.Contains(err.Error(), "--direct") {
		t.Fatalf("err = %v, want a --direct refusal", err)
	}
}
