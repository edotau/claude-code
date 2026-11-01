package agent

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// D4: gemini (no Dialects) is refused on any non-login row, even one with a gemini route.
func TestGeminiRefusedOnRoutedRow(t *testing.T) {
	vaulttest.Use(t)
	p := &providers.Provider{Name: "gem-key", Models: map[string]string{"opus": "gemini-x"},
		Routes: map[string]string{providers.ProtocolGemini: "https://g.example.com"}}
	if CanServe("gemini", p) {
		t.Error("CanServe(gemini) = true on a keyed row with a gemini route")
	}
	if !CanServe("opencode", p) {
		t.Error("control: opencode speaks gemini and should serve the row")
	}
	_, _, err := schemaFor("gemini").configureRoute(t.Context(), providers.Target{Provider: p, Model: "gemini-x"}, nil)
	if err == nil || !strings.Contains(err.Error(), "agy has no custom base URL") {
		t.Errorf("err = %v, want the agy no-custom-base-URL refusal", err)
	}
}
