package agent

import (
	"context"
	"errors"
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

func assertNoSecret(t *testing.T, env, argv []string) {
	t.Helper()
	if s := strings.Join(argv, "\x00"); strings.Contains(s, secret) {
		t.Fatalf("secret in argv: %q", argv)
	}
	for _, kv := range env {
		if strings.Contains(kv, secret) {
			t.Fatalf("secret in env: %s", kv)
		}
	}
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
