package agent

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestOpenCodeConfigShape(t *testing.T) {
	tg := target(t, "gateway", "anthropic/claude-opus-5")
	env, argv, err := schemaFor("opencode").configureHeadless(WithDirect(context.Background()), tg, Headless{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Model      string                    `json:"model"`
		Permission any                       `json:"permission"`
		Provider   map[string]map[string]any `json:"provider"`
	}
	if err := json.Unmarshal([]byte(envMap(env)["OPENCODE_CONFIG_CONTENT"]), &cfg); err != nil {
		t.Fatal(err)
	}
	prov := cfg.Provider["harness-gateway"]
	opts := prov["options"].(map[string]any)
	if prov["npm"] != "@ai-sdk/anthropic" || opts["baseURL"] != "https://gateway.example.com/api/v1" {
		t.Errorf("provider block = %v", prov)
	}
	if opts["apiKey"] != "{env:"+OpenCodeKeyEnv+"}" || opts["headers"].(map[string]any)["Authorization"] != "Bearer {env:"+OpenCodeKeyEnv+"}" {
		t.Errorf("auth must reference the env var, got %v", opts)
	}
	if cfg.Permission != "deny" || cfg.Model != "harness-gateway/anthropic/claude-opus-5" {
		t.Errorf("tools-off permission/model wrong: %+v", cfg)
	}
	if strings.Contains(envMap(env)["OPENCODE_CONFIG_CONTENT"], secret) || strings.Contains(strings.Join(argv, " "), secret) {
		t.Error("secret inlined into config or argv")
	}
}

// TestOpenCodeDirArgv covers both argv builders opencode's workdir plumbing goes through: the BYOK/gateway
// builder (opencode(), reached via ConfigureHeadless) and the hub ladder's own (OpencodeRunArgv). Both must
// add --dir only when Headless.WorkDir is set, and never otherwise.
func TestOpenCodeDirArgv(t *testing.T) {
	t.Run("BYOK builder", func(t *testing.T) {
		tg := target(t, "gateway", "anthropic/claude-opus-5")
		_, argv, err := schemaFor("opencode").configureHeadless(context.Background(), tg, Headless{Prompt: "hi", WorkDir: "/work/dir"})
		if err != nil {
			t.Fatal(err)
		}
		if i := slices.Index(argv, "--dir"); i == -1 || argv[i+1] != "/work/dir" {
			t.Errorf("argv = %q, want --dir /work/dir", argv)
		}

		_, argv, err = schemaFor("opencode").configureHeadless(context.Background(), tg, Headless{Prompt: "hi"})
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(argv, "--dir") {
			t.Errorf("argv = %q, must not contain --dir when WorkDir is unset", argv)
		}
	})

	t.Run("hub ladder", func(t *testing.T) {
		_, argv := opencodeRunArgv("github-copilot/claude-opus-5", Headless{Prompt: "hi", WorkDir: "/work/dir"})
		if i := slices.Index(argv, "--dir"); i == -1 || argv[i+1] != "/work/dir" {
			t.Errorf("argv = %q, want --dir /work/dir", argv)
		}

		_, argv = opencodeRunArgv("github-copilot/claude-opus-5", Headless{Prompt: "hi"})
		if slices.Contains(argv, "--dir") {
			t.Errorf("argv = %q, must not contain --dir when WorkDir is unset", argv)
		}
	})
}

// TestOpenCodeOverlayEnvOnEveryRoute: applyLaunch wires OPENCODE_CONFIG/OPENCODE_TUI_CONFIG into every
// opencode launch path — own login (LaunchSpec), the BYOK/gateway route (Configure) and the hub ladder's
// own builder (OpencodeRunArgv) — not just the applyLaunch unit the overlay's own test already covers.
func TestOpenCodeOverlayEnvOnEveryRoute(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	hasOverlayEnv := func(t *testing.T, env []string) {
		t.Helper()
		m := envMap(env)
		if m["OPENCODE_CONFIG"] == "" || m["OPENCODE_TUI_CONFIG"] == "" {
			t.Errorf("env = %v, want both OPENCODE_CONFIG and OPENCODE_TUI_CONFIG set", env)
		}
	}

	t.Run("own login (LaunchSpec)", func(t *testing.T) {
		env, _ := schemaFor("opencode").LaunchSpec()
		hasOverlayEnv(t, env)
	})

	t.Run("BYOK/gateway (Configure)", func(t *testing.T) {
		tg := target(t, "gateway", "anthropic/claude-opus-5")
		env, _, err := schemaFor("opencode").configureHeadless(context.Background(), tg, Headless{Prompt: "hi"})
		if err != nil {
			t.Fatal(err)
		}
		hasOverlayEnv(t, env)
	})

	t.Run("hub ladder (OpencodeRunArgv)", func(t *testing.T) {
		env, _ := opencodeRunArgv("github-copilot/claude-opus-5", Headless{Prompt: "hi"})
		hasOverlayEnv(t, env)
	})
}

// TestOpenCodeOwnLoginHeadlessToolsOffDeny: opencode's own-login headless leg (OwnLoginHeadless) still
// rides OPENCODE_CONFIG_CONTENT={"permission":"deny"} when tools are off, alongside the new overlay env.
func TestOpenCodeOwnLoginHeadlessToolsOffDeny(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	env, _, err := schemaFor("opencode").eng().ownLogin(Headless{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	m := envMap(env)
	if m["OPENCODE_CONFIG_CONTENT"] != `{"permission":"deny"}` {
		t.Errorf("OPENCODE_CONFIG_CONTENT = %q, want the tools-off deny", m["OPENCODE_CONFIG_CONTENT"])
	}
	if m["OPENCODE_CONFIG"] == "" || m["OPENCODE_TUI_CONFIG"] == "" {
		t.Errorf("env = %v, want both overlay paths alongside the deny", env)
	}

	env, _, err = schemaFor("opencode").eng().ownLogin(Headless{Prompt: "hi", Tools: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := envMap(env)["OPENCODE_CONFIG_CONTENT"]; ok {
		t.Errorf("tools=true must carry no OPENCODE_CONFIG_CONTENT deny: %v", env)
	}
}

// opencodeMenu unmarshals OPENCODE_CONFIG_CONTENT and returns just the model menu + the top-level model id.
func opencodeMenu(t *testing.T, env []string, id string) (map[string]any, string) {
	t.Helper()
	var cfg struct {
		Model    string                    `json:"model"`
		Provider map[string]map[string]any `json:"provider"`
	}
	if err := json.Unmarshal([]byte(envMap(env)["OPENCODE_CONFIG_CONTENT"]), &cfg); err != nil {
		t.Fatal(err)
	}
	menu, _ := cfg.Provider[id]["models"].(map[string]any)
	return menu, cfg.Model
}

func TestOpenCodeModelMenu(t *testing.T) {
	t.Run("known tier id", func(t *testing.T) {
		tg := target(t, "anthropic", "claude-sonnet-5-5")
		env, _, err := schemaFor("opencode").Configure(context.Background(), tg, "")
		if err != nil {
			t.Fatal(err)
		}
		menu, _ := opencodeMenu(t, env, "harness-anthropic")
		want := map[string]bool{"claude-opus-5-5": true, "claude-sonnet-5-5": true, "claude-haiku-5-5": true, "claude-fable-5-1": true}
		if len(menu) != len(want) {
			t.Fatalf("menu = %v, want exactly the 4 tier ids", menu)
		}
		for id := range want {
			if _, ok := menu[id]; !ok {
				t.Errorf("menu missing tier id %q: %v", id, menu)
			}
		}
	})

	t.Run("model not in table", func(t *testing.T) {
		tg := target(t, "anthropic", "claude-custom")
		env, _, err := schemaFor("opencode").Configure(context.Background(), tg, "")
		if err != nil {
			t.Fatal(err)
		}
		menu, model := opencodeMenu(t, env, "harness-anthropic")
		if len(menu) != 5 {
			t.Errorf("menu = %v, want 5 keys (4 tiers + the requested model)", menu)
		}
		if model != "harness-anthropic/claude-custom" {
			t.Errorf("model = %q, want harness-anthropic/claude-custom", model)
		}
	})

	t.Run("headless carries no menu", func(t *testing.T) {
		tg := target(t, "anthropic", "claude-sonnet-5-5")
		env, _, err := schemaFor("opencode").configureHeadless(context.Background(), tg, Headless{Prompt: "hi"})
		if err != nil {
			t.Fatal(err)
		}
		menu, _ := opencodeMenu(t, env, "harness-anthropic")
		if len(menu) != 1 {
			t.Errorf("headless menu = %v, want exactly 1 key", menu)
		}
	})
}

func TestMutateOpencodeConfigEnvPolicies(t *testing.T) {
	set := func(c map[string]any) { c["k"] = "v" }
	bad := []string{"A=1", opencodeConfigEnv + "={nope"}
	if _, err := mutateOpencodeConfigEnv(append([]string(nil), bad...), false, set); err == nil {
		t.Error("strict: an unreadable block must be an error")
	}
	got, err := mutateOpencodeConfigEnv(append([]string(nil), bad...), true, set)
	if err != nil || got[1] != opencodeConfigEnv+`={"k":"v"}` || len(got) != 2 {
		t.Errorf("lenient = %v, %v; want the block replaced in place", got, err)
	}
	got, _ = mutateOpencodeConfigEnv([]string{"A=1", opencodeConfigEnv + `={"m":1}`}, false, set)
	if got[1] != opencodeConfigEnv+`={"k":"v","m":1}` {
		t.Errorf("merge = %v", got)
	}
	if got, _ = mutateOpencodeConfigEnv(nil, false, set); len(got) != 1 || got[0] != opencodeConfigEnv+`={"k":"v"}` {
		t.Errorf("absent = %v, want one appended entry", got)
	}
}

// TestOpencodeCredentialFallsThroughWhenOnlyAnotherLoginExists checks that when opencode has a login for
// another provider (openai) but none for its own "opencode" plan, an opencode-go cell (prefix "opencode-go"
// maps to credential id "opencode") falls through to the OPENCODE_API_KEY vault key, erroring when that is
// absent too — precedence never borrows the unrelated openai login.
func TestOpencodeCredentialFallsThroughWhenOnlyAnotherLoginExists(t *testing.T) {
	data, _ := fakeOpencode(t, "1.19.3", "", nil)
	t.Setenv(OpencodeAPIKeyEnv, "")
	writeAuthJSON(t, data, `{"openai":{"type":"oauth"}}`)
	if l := OpencodeLogins(); len(l) != 1 || l[0] != "openai" {
		t.Fatalf("precondition: logins = %v, want only openai", l)
	}
	env, err := opencodeCredential("opencode-go")
	if err == nil {
		t.Fatalf("env = %v, want an error: the openai login must not cover an opencode-go cell", env)
	}
	if !strings.Contains(err.Error(), OpencodeAPIKeyEnv) {
		t.Errorf("err = %v, want it to name %s", err, OpencodeAPIKeyEnv)
	}
}

// TestOpencodeCredentialVaultKeyOutranksMissingOwnLogin checks the same opencode-go cell resolves once the
// OPENCODE_API_KEY vault key is present, still with no "opencode" login stored.
func TestOpencodeCredentialVaultKeyOutranksMissingOwnLogin(t *testing.T) {
	data, _ := fakeOpencode(t, "1.19.3", "", nil)
	writeAuthJSON(t, data, `{"openai":{"type":"oauth"}}`)
	t.Setenv(OpencodeAPIKeyEnv, "sk-test-key")
	env, err := opencodeCredential("opencode-go")
	if err != nil {
		t.Fatalf("err = %v, want the vault/env key to resolve it", err)
	}
	if len(env) != 1 || env[0] != OpencodeAPIKeyEnv+"=sk-test-key" {
		t.Errorf("env = %v, want [%s=sk-test-key]", env, OpencodeAPIKeyEnv)
	}
}
