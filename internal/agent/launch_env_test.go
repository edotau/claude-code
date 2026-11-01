package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

const (
	testRouterBase   = "http://127.0.0.1:9999"
	testRouterSecret = "rtr-secret-xyz"
)

func stubRouter(t *testing.T, ensure func(context.Context) (string, error)) {
	t.Helper()
	oe, os_ := ensureRouter, routerSecret
	t.Cleanup(func() { ensureRouter, routerSecret = oe, os_ })
	if ensure != nil {
		ensureRouter = ensure
	}
}

func cfgFor(t *testing.T, ctx context.Context, vendor, provider, model string) ([]string, []string) {
	t.Helper()
	env, argv, err := schemaFor(vendor).Configure(ctx, target(t, provider, model), "")
	if err != nil {
		t.Fatal(err)
	}
	return env, argv
}

func TestRoutedStrips1MSuffix(t *testing.T) {
	stubRouter(t, nil)
	env, _ := cfgFor(t, context.Background(), "copilot", "anthropic", "claude-opus-5[1m]")
	if m := envMap(env)["COPILOT_MODEL"]; m != "claude-opus-5" {
		t.Errorf("COPILOT_MODEL = %q", m)
	}
}

func TestLaunchEnvCannotOverrideRouterBase(t *testing.T) {
	stubRouter(t, nil)
	tg := target(t, "openai", "gpt-5")
	vaulttest.Put(t, vault.Overlay, "agents",
		`{"agents":{"copilot":{"launch":{"env":["COPILOT_PROVIDER_BASE_URL=https://evil.example.com","KEEP_ME=1"]}}}}`)
	env, _, err := schemaFor("copilot").Configure(context.Background(), tg, "")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, "COPILOT_PROVIDER_BASE_URL=") {
			n++
			if !strings.HasPrefix(kv, "COPILOT_PROVIDER_BASE_URL="+testRouterBase) {
				t.Errorf("base overridden: %s", kv)
			}
		}
	}
	if n != 1 {
		t.Errorf("base var appears %d times: %q", n, env)
	}
	if envMap(env)["KEEP_ME"] != "1" {
		t.Errorf("unrelated launch.env entry dropped: %q", env)
	}
}

func TestAuthNoneNotRouted(t *testing.T) {
	stubRouter(t, func(context.Context) (string, error) {
		t.Error("router ensured for AuthNone")
		return "", errors.New("x")
	})
	tg := target(t, "openai", "gpt-5")
	p := *tg.Provider
	p.Auth = providers.Auth{Type: providers.AuthNone}
	tg.Provider = &p
	env, argv, err := schemaFor("codex").Configure(context.Background(), tg, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(argv, " "), testRouterBase) || strings.Contains(strings.Join(env, " "), testRouterSecret) {
		t.Errorf("AuthNone was routed: %q %q", env, argv)
	}
}

func TestEnsureRouterErrorSurfaces(t *testing.T) {
	stubRouter(t, func(context.Context) (string, error) { return "", errors.New("boom") })
	_, _, err := schemaFor("codex").Configure(context.Background(), target(t, "openai", "gpt-5"), "")
	if err == nil || !strings.Contains(err.Error(), "router") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}

const agentHdr = "X-Claude-Code-Agent"

// withHeaders returns tg with the provider's own headers set.
func withHeaders(tg providers.Target, h map[string]string) providers.Target {
	p := *tg.Provider
	p.Headers = h
	tg.Provider = &p
	return tg
}

// A direct dispatch never passes routerRoute, so it carries the provider's own headers and no agent tag.
func TestDirectDispatchesHaveNoAgentTag(t *testing.T) {
	stubRouter(t, nil)
	vaulttest.Use(t)
	ctx := WithDirect(context.Background())
	own := map[string]string{"X-Tenant": "t1"}

	_, argv, err := schemaFor("codex").Configure(ctx, withHeaders(target(t, "openai", "gpt-5"), own), "")
	if err != nil {
		t.Fatal(err)
	}
	if j := strings.Join(argv, "\x00"); strings.Contains(j, agentHdr) || !strings.Contains(j, "X-Tenant") {
		t.Errorf("direct codex headers wrong: %q", argv)
	}
	for _, v := range []string{"opencode", "copilot"} {
		env, _, err := schemaFor(v).Configure(ctx, withHeaders(target(t, "openai", "gpt-5"), own), "")
		if err != nil {
			t.Fatal(err)
		}
		if j := strings.Join(env, "\x00"); strings.Contains(j, agentHdr) || !strings.Contains(j, "X-Tenant") {
			t.Errorf("direct %s headers wrong: %q", v, env)
		}
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
	_, _, err := schemaFor("gemini").configureRoute(t.Context(), providers.Target{Provider: p, Model: "gemini-x"}, nil)
	if err == nil || !strings.Contains(err.Error(), "agy has no custom base URL") {
		t.Errorf("err = %v, want the agy no-custom-base-URL refusal", err)
	}
}

// Interactive codex (nil Headless) carries the CODEX_EFFORT pin; headless Effort still wins.
func TestCodexInteractiveCarriesEffortPin(t *testing.T) {
	tg := target(t, "openai", "gpt-5")
	vaulttest.SetRecord(t, "codex", "effort", "medium")
	_, argv, err := schemaFor("codex").Configure(context.Background(), tg, "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(argv, `model_reasoning_effort="medium"`) {
		t.Errorf("interactive argv = %q, want the effort pin", argv)
	}
	_, argv, err = schemaFor("codex").configureHeadless(context.Background(), tg, Headless{Prompt: "hi", Effort: "low"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(argv, `model_reasoning_effort="low"`) || slices.Contains(argv, `model_reasoning_effort="medium"`) {
		t.Errorf("headless argv = %q, want explicit low over the pin", argv)
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
		_, _, err := schemaFor(c.agent).configureRoute(t.Context(), providers.Target{Provider: c.p, Model: "m"}, nil)
		if err == nil || err.Error() != c.agent+": "+why {
			t.Errorf("%s/%s: err = %v, want %q", c.agent, c.p.Name, err, c.agent+": "+why)
		}
	}
}
