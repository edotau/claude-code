package vendors

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
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

// TestMain stubs the router seams so no test spawns a daemon, and points every test at a throwaway config dir
// and vault key so none can reach the live vault or router secret.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "vendors-test-")
	if err != nil {
		panic(err)
	}
	os.Setenv("CLAUDE_CONFIG_DIR", dir)
	os.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	mintSession = func(context.Context, string, int) (string, error) { return "", errors.New("session mint is stubbed") }
	revokeSession = func(context.Context, string) error { return nil }
	ensureRouter = func(context.Context) (string, error) { return testRouterBase, nil }
	routerSecret = func() (string, error) { return testRouterSecret, nil }
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

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
	env, argv, err := Configure(ctx, vendor, target(t, provider, model), "")
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

func TestRoutedKeyedProviders(t *testing.T) {
	stubRouter(t, nil)
	ctx := context.Background()
	t.Run("codex", func(t *testing.T) {
		env, argv := cfgFor(t, ctx, "codex", "openai", "gpt-5")
		assertNoSecret(t, env, argv)
		if got := envMap(env)[CodexKeyEnv]; got != testRouterSecret {
			t.Errorf("key = %q, want router secret", got)
		}
		j := strings.Join(argv, "\x00")
		want := `base_url = "` + testRouterBase + `/p/openai/responses"`
		if !strings.Contains(j, want) || strings.Contains(j, "api.openai.com") {
			t.Errorf("codex argv not routed (want %s): %q", want, argv)
		}
	})
	t.Run("opencode", func(t *testing.T) {
		env, argv := cfgFor(t, ctx, "opencode", "openai", "gpt-5")
		assertNoSecret(t, env, argv)
		m := envMap(env)
		if m[OpenCodeKeyEnv] != testRouterSecret {
			t.Errorf("key = %q", m[OpenCodeKeyEnv])
		}
		cfg := m["OPENCODE_CONFIG_CONTENT"]
		if !strings.Contains(cfg, testRouterBase+"/p/openai/openai") || strings.Contains(cfg, "api.openai.com") {
			t.Errorf("opencode config not routed: %s", cfg)
		}
	})
	t.Run("opencode anthropic disables toolStreaming", func(t *testing.T) {
		env, _ := cfgFor(t, ctx, "opencode", "anthropic", "claude-opus-5")
		if !strings.Contains(envMap(env)["OPENCODE_CONFIG_CONTENT"], `"toolStreaming":false`) {
			t.Errorf("missing toolStreaming:false: %s", envMap(env)["OPENCODE_CONFIG_CONTENT"])
		}
		denv, _ := cfgFor(t, WithDirect(ctx), "opencode", "anthropic", "claude-opus-5")
		if strings.Contains(envMap(denv)["OPENCODE_CONFIG_CONTENT"], "toolStreaming") {
			t.Error("direct path must not set toolStreaming")
		}
	})
	t.Run("copilot", func(t *testing.T) {
		for _, prov := range []string{"anthropic", "openai"} {
			env, argv := cfgFor(t, ctx, "copilot", prov, "claude-opus-5")
			assertNoSecret(t, env, argv)
			m := envMap(env)
			if m["COPILOT_PROVIDER_API_KEY"] != testRouterSecret && m["COPILOT_PROVIDER_BEARER_TOKEN"] != testRouterSecret {
				t.Errorf("%s: router secret not handed to copilot: %q", prov, env)
			}
			if !strings.HasPrefix(m["COPILOT_PROVIDER_BASE_URL"], testRouterBase+"/p/"+prov) {
				t.Errorf("%s: base = %q", prov, m["COPILOT_PROVIDER_BASE_URL"])
			}
		}
	})
}

func TestWithDirectKeepsOldHandOff(t *testing.T) {
	stubRouter(t, func(context.Context) (string, error) {
		t.Error("router ensured on direct path")
		return "", errors.New("x")
	})
	ctx := WithDirect(context.Background())
	env, argv := cfgFor(t, ctx, "codex", "openai", "gpt-5")
	if envMap(env)[CodexKeyEnv] != secret {
		t.Errorf("direct codex key = %q", envMap(env)[CodexKeyEnv])
	}
	if !strings.Contains(strings.Join(argv, " "), "https://api.openai.com/v1") {
		t.Errorf("direct base missing: %q", argv)
	}
	env, _ = cfgFor(t, ctx, "copilot", "openai", "gpt-5")
	if m := envMap(env); m["COPILOT_PROVIDER_BEARER_TOKEN"] != secret || m["COPILOT_PROVIDER_BASE_URL"] != "https://api.openai.com/v1" {
		t.Errorf("direct copilot env: %q", env)
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
	env, _, err := Configure(context.Background(), "copilot", tg, "")
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
	env, argv, err := Configure(context.Background(), "codex", tg, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(argv, " "), testRouterBase) || strings.Contains(strings.Join(env, " "), testRouterSecret) {
		t.Errorf("AuthNone was routed: %q %q", env, argv)
	}
}

func TestEnsureRouterErrorSurfaces(t *testing.T) {
	stubRouter(t, func(context.Context) (string, error) { return "", errors.New("boom") })
	_, _, err := Configure(context.Background(), "codex", target(t, "openai", "gpt-5"), "")
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

func TestRoutedLegsCarryAgentTag(t *testing.T) {
	stubRouter(t, nil)
	vaulttest.Use(t)
	ctx := context.Background()
	own := map[string]string{"X-Tenant": "t1"}

	t.Run("codex", func(t *testing.T) {
		_, argv, err := Configure(ctx, "codex", withHeaders(target(t, "openai", "gpt-5"), own), "")
		if err != nil {
			t.Fatal(err)
		}
		j := strings.Join(argv, "\x00")
		if !strings.Contains(j, `http_headers = {"`+agentHdr+`" = "codex"}`) && !strings.Contains(j, `"`+agentHdr+`" = "codex"`) {
			t.Errorf("codex http_headers missing tag: %q", argv)
		}
		if strings.Contains(j, "X-Tenant") {
			t.Errorf("routed codex leaks the provider's own headers client-side: %q", argv)
		}
	})
	t.Run("opencode", func(t *testing.T) {
		env, _, err := Configure(ctx, "opencode", withHeaders(target(t, "openai", "gpt-5"), own), "")
		if err != nil {
			t.Fatal(err)
		}
		var c ocCfg
		if err := json.Unmarshal([]byte(envMap(env)["OPENCODE_CONFIG_CONTENT"]), &c); err != nil {
			t.Fatal(err)
		}
		if got := c.Provider["harness-openai"].Options.Headers[agentHdr]; got != "opencode" {
			t.Errorf("options.headers[%s] = %q", agentHdr, got)
		}
	})
	t.Run("opencode databricks", func(t *testing.T) {
		tg := target(t, "openai", "databricks-claude-opus-5-5")
		p := dbxRow("qa", "databricks-")
		p.Auth = tg.Provider.Auth
		tg.Provider = p
		env, _, err := Configure(ctx, "opencode", tg, "")
		if err != nil {
			t.Fatal(err)
		}
		var c ocCfg
		if err := json.Unmarshal([]byte(envMap(env)["OPENCODE_CONFIG_CONTENT"]), &c); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"harness-qa", "harness-qa-gpt"} {
			if got := c.Provider[id].Options.Headers[agentHdr]; got != "opencode" {
				t.Errorf("%s options.headers[%s] = %q", id, agentHdr, got)
			}
		}
	})
	t.Run("copilot", func(t *testing.T) {
		env, _, err := Configure(ctx, "copilot", withHeaders(target(t, "openai", "gpt-5"), own), "")
		if err != nil {
			t.Fatal(err)
		}
		if got := envMap(env)["COPILOT_PROVIDER_HEADERS"]; got != agentHdr+": copilot" {
			t.Errorf("COPILOT_PROVIDER_HEADERS = %q", got)
		}
	})
}

// A direct leg never passes routerRoute, so it carries the provider's own headers and no agent tag.
func TestDirectLegsHaveNoAgentTag(t *testing.T) {
	stubRouter(t, nil)
	vaulttest.Use(t)
	ctx := WithDirect(context.Background())
	own := map[string]string{"X-Tenant": "t1"}

	_, argv, err := Configure(ctx, "codex", withHeaders(target(t, "openai", "gpt-5"), own), "")
	if err != nil {
		t.Fatal(err)
	}
	if j := strings.Join(argv, "\x00"); strings.Contains(j, agentHdr) || !strings.Contains(j, "X-Tenant") {
		t.Errorf("direct codex headers wrong: %q", argv)
	}
	for _, v := range []string{"opencode", "copilot"} {
		env, _, err := Configure(ctx, v, withHeaders(target(t, "openai", "gpt-5"), own), "")
		if err != nil {
			t.Fatal(err)
		}
		if j := strings.Join(env, "\x00"); strings.Contains(j, agentHdr) || !strings.Contains(j, "X-Tenant") {
			t.Errorf("direct %s headers wrong: %q", v, env)
		}
	}
}
