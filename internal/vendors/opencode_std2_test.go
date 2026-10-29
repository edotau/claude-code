package vendors

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// Headless on a Databricks profile renders the same three providers as the interactive launch.
func TestStd2OpencodeHeadlessDatabricksThreeProviders(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	p := dbxRow("qa", "databricks-")
	p.Routes[providers.ProtocolAnthropic] = "https://h.example.com/serving-endpoints/anthropic"
	base := p.Routes[providers.ProtocolOpenAI]
	render := func(model string) ocCfg {
		t.Helper()
		env, argv, err := opencode(p, providers.ProtocolOpenAI, base, model, "tok", false, &Headless{Prompt: "hi", Tools: true})
		if err != nil {
			t.Fatal(err)
		}
		var c ocCfg
		if err := json.Unmarshal([]byte(envMap(env)["OPENCODE_CONFIG_CONTENT"]), &c); err != nil {
			t.Fatal(err)
		}
		if len(argv) < 4 || argv[0] != "run" {
			t.Fatalf("argv = %q, want an opencode run", argv)
		}
		return c
	}
	c := render("databricks-claude-opus-5-5")
	for _, id := range []string{"harness-qa", "harness-qa-gpt", "harness-qa-claude"} {
		if _, ok := c.Provider[id]; !ok {
			t.Errorf("headless config lacks provider %q; has %d providers", id, len(c.Provider))
		}
	}
	g := render("databricks-gpt-6-astra")
	if g.Model != "harness-qa-gpt/databricks-gpt-6-astra" {
		t.Errorf("headless Responses-only model = %q, want the -gpt provider", g.Model)
	}
}

// A copilot-only opencode launch renders github-copilot/<id> models, never an opencode-go id.
func TestStd2OpencodeCopilotOnlyOverlayModels(t *testing.T) {
	data, _ := isolateOpencodeRouteEnv(t)
	vaulttest.Use(t)
	if err := os.MkdirAll(filepath.Join(data, "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "opencode", "auth.json"), []byte(`{"github-copilot":{"type":"oauth"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"github":{"personal":{"protocol":"openai",`+
		`"base_url":"https://api.githubcopilot.com","auth":{"type":"cli","login":"copilot"},`+
		`"models":{"opus":"gpt-6.1-sol","sonnet":"claude-sonnet-5.5","haiku":"mai-code-1.1-flash"}}}}}`)
	if OpencodeOwnLoginPresent() || !OpencodeCopilotLoginPresent() {
		t.Fatal("fixture must be copilot-only")
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	main, small, ok := OpencodeCopilotModelIDs(reg)
	if !ok {
		t.Fatal("no copilot model ids")
	}
	_, body, _, _, err := OpencodeOverlayRendered()
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["model"] != "github-copilot/"+main || doc["small_model"] != "github-copilot/"+small {
		t.Errorf("model=%v small_model=%v, want github-copilot/%s and github-copilot/%s", doc["model"], doc["small_model"], main, small)
	}
	for _, k := range []string{"model", "small_model"} {
		if s, _ := doc[k].(string); strings.HasPrefix(s, "opencode-go/") {
			t.Errorf("%s = %q, an opencode-go id on a copilot-only login", k, s)
		}
	}
}

// Tools-off headless keeps permission deny even when a vendorconfig Launch.Env sets the same variable.
func TestStd2OpencodeHeadlessToolsOffBeatsLaunchEnv(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	vaulttest.Put(t, vault.Overlay, "agents", `{"agents":{"opencode":{"launch":{"env":["OPENCODE_CONFIG_CONTENT={\"permission\":\"allow\"}"]}}}}`)
	check := func(name string, env []string) {
		t.Helper()
		got := envMap(env)["OPENCODE_CONFIG_CONTENT"] // last duplicate wins, as in exec
		if !strings.Contains(got, `"deny"`) || strings.Contains(got, `"allow"`) {
			t.Errorf("%s: final OPENCODE_CONFIG_CONTENT = %q, want permission deny", name, got)
		}
	}
	h := Headless{Prompt: "hi", Tools: false}
	env, _, err := OwnLoginHeadless("opencode", h)
	if err != nil {
		t.Fatal(err)
	}
	check("OwnLoginHeadless", env)
	env, _ = OpencodeRunArgv("opencode-go/x", h)
	check("OpencodeRunArgv", env)
}
