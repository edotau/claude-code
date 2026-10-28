package vendors

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// Non-Claude models on a routed Databricks row ride the router's openai|responses passthrough; Claude stays anthropic.
func TestCopilotNonClaudeWire(t *testing.T) {
	vaulttest.Use(t)
	if err := providers.SetOpencodeMenu([]providers.OpencodeModel{{ID: "gpt-6-1-sol", Responses: true}}); err != nil {
		t.Fatal(err)
	}
	const base = "http://r/p/dev"
	cases := []struct {
		name, model, typ, base, wire, modelID string
		p                                     *providers.Provider
		routed                                bool
	}{
		{"claude", "databricks-claude-sonnet-5-5", "anthropic", base, "", "", dbxRow("dev", "databricks-"), true},
		{"chat", "databricks-kimi-k3", "openai", base + "/openai", "", "kimi-k3", dbxRow("dev", "databricks-"), true},
		{"responses", "databricks-gpt-6-1-sol", "openai", base + "/responses", "responses", "gpt-6-1-sol", dbxRow("dev", "databricks-"), true},
		{"unrouted", "databricks-kimi-k3", "anthropic", base, "", "", dbxRow("dev", "databricks-"), false},
		{"non-databricks", "kimi-k3", "anthropic", base, "", "", &providers.Provider{Name: "other"}, true},
	}
	for _, c := range cases {
		env, _, err := copilot(c.p, providers.ProtocolAnthropic, base, c.model, 200000, "", c.routed, nil)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		get := func(k string) (string, bool) {
			for _, kv := range env {
				if v, ok := strings.CutPrefix(kv, k+"="); ok {
					return v, true
				}
			}
			return "", false
		}
		if v, _ := get("COPILOT_PROVIDER_TYPE"); v != c.typ {
			t.Errorf("%s: TYPE=%q, want %q", c.name, v, c.typ)
		}
		if v, _ := get("COPILOT_PROVIDER_BASE_URL"); v != c.base {
			t.Errorf("%s: BASE_URL=%q, want %q", c.name, v, c.base)
		}
		if v, _ := get("COPILOT_MODEL"); v != c.model {
			t.Errorf("%s: COPILOT_MODEL=%q, want %q", c.name, v, c.model)
		}
		if v, ok := get("COPILOT_PROVIDER_WIRE_API"); v != c.wire || ok != (c.wire != "") {
			t.Errorf("%s: WIRE_API=%q (set %v), want %q", c.name, v, ok, c.wire)
		}
		if v, ok := get("COPILOT_PROVIDER_MODEL_ID"); v != c.modelID || ok != (c.modelID != "") {
			t.Errorf("%s: MODEL_ID=%q (set %v), want %q", c.name, v, ok, c.modelID)
		}
	}
}
