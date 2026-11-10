package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// An opencode record pinned to opencode-go (valid via PinIDs, no registry row) resolves to its own plan, not an error.
func TestPinnedProfileOpencodeGo(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.SetRecord(t, "opencode", "profile", "opencode-go")
	got, err := PinnedProfile("opencode")
	if err != nil || got != "" {
		t.Errorf("pinnedProfile(opencode) = %q, %v; want empty profile and no error", got, err)
	}
	vaulttest.SetRecord(t, "opencode", "profile", "no-such-profile")
	if _, err := PinnedProfile("opencode"); err == nil || !strings.Contains(err.Error(), `unknown profile "no-such-profile"`) {
		t.Errorf("an invalid pin must still be an error with the ValidAgentPin cause, got %v", err)
	}
}

func stubRouter(t *testing.T, ensure func(context.Context) (string, error)) {
	t.Helper()
	oe, os_ := EnsureRouter, RouterSecret
	t.Cleanup(func() { EnsureRouter, RouterSecret = oe, os_ })
	if ensure != nil {
		EnsureRouter = ensure
	}
}

func TestEnsureRouterErrorSurfaces(t *testing.T) {
	stubRouter(t, func(context.Context) (string, error) { return "", errors.New("boom") })
	_, _, err := SchemaFor("codex").Configure(context.Background(), target(t, "openai", "gpt-5"), "")
	if err == nil || !strings.Contains(err.Error(), "router") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}

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
	_, _, err := SchemaFor("gemini").configureRoute(t.Context(), providers.Target{Provider: p, Model: "gemini-x"}, nil)
	if err == nil || !strings.Contains(err.Error(), "agy has no custom base URL") {
		t.Errorf("err = %v, want the agy no-custom-base-URL refusal", err)
	}
}

// configureRoute surfaces Serves' reason verbatim, prefixed by the agent.
func TestConfigureRouteUsesServesReason(t *testing.T) {
	vaulttest.Use(t)
	ant := &providers.Provider{Name: "ant", Protocol: providers.ProtocolAnthropic, BaseURL: "https://a", Auth: providers.Auth{Type: providers.AuthKey}}
	cases := []struct {
		agent string
		p     *providers.Provider
	}{
		{"gemini", ant},
		{"codex", ant},
		{"copilot", &providers.Provider{Name: "sub", Protocol: providers.ProtocolAnthropic, BaseURL: "https://s", Auth: providers.Auth{Type: providers.AuthCLI}}},
	}
	for _, c := range cases {
		_, why := Serves(c.agent, c.p, "")
		if why == "" {
			t.Fatalf("%s/%s: control: Serves has no reason", c.agent, c.p.Name)
		}
		_, _, err := SchemaFor(c.agent).configureRoute(t.Context(), providers.Target{Provider: c.p, Model: "m"}, nil)
		if err == nil || err.Error() != c.agent+": "+why {
			t.Errorf("%s/%s: err = %v, want %q", c.agent, c.p.Name, err, c.agent+": "+why)
		}
	}
}
