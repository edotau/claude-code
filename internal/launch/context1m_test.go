package launch

import (
	"context"
	"testing"
)

// The endpoint type decides the 1M env: serving and first-party get bare slots and no DISABLE; ai-gateway and a
// non-Claude endpoint get DISABLE=1; a typed [1m] is kept; ANTHROPIC_MODEL only appears with --model.
func TestClaude1MByEndpoint(t *testing.T) {
	hermetic(t, `{"fallback": [], "providers": {"databricks": {
		"dbx": {"preset": "databricks", "host": "https://h.cloud.databricks.com", "auth": {"type":"key", "env": "TEST_GW_KEY"}},
		"gw":  {"preset": "databricks", "host": "https://h.cloud.databricks.com", "auth": {"type":"key", "env": "TEST_GW_KEY"},
			"options": {"wire": "ai-gateway"}},
		"gwm": {"preset": "databricks", "host": "https://h.cloud.databricks.com", "auth": {"type":"key", "env": "TEST_GW_KEY"},
			"options": {"wire": "ai-gateway"}, "models": {"opus": "claude-opus-5-5[1m]"}}},
		"openai": {
		"oll": {"kind": "openai", "base_url": "http://127.0.0.1:11434/v1", "auth": {"type": "none"},
			"models": {"opus": "qwen3-coder", "sonnet": "qwen3-coder", "haiku": "qwen3-coder"}}}}}`)
	t.Setenv("TEST_GW_KEY", "k")
	for _, tc := range []struct {
		name, profile, model string
		disable              bool
		wantModel            string
		wantOpus             string
	}{
		{"databricks serving", "dbx", "", false, "", ""},
		{"first-party", "anthropic", "", false, "", "claude-opus-5-5"},
		{"first-party --model", "anthropic", "claude-opus-5-5", false, "claude-opus-5-5", "claude-opus-5-5"},
		{"ai-gateway", "gw", "", true, "", ""},
		{"ai-gateway typed [1m] kept", "gwm", "", false, "", ""},
		{"non-Claude endpoint", "oll", "", true, "", "qwen3-coder"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := Claude(context.Background(), Options{Provider: tc.profile, Model: tc.model}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := p.Set["CLAUDE_CODE_DISABLE_1M_CONTEXT"] == "1"; got != tc.disable {
				t.Errorf("DISABLE_1M_CONTEXT set = %v, want %v (%v)", got, tc.disable, p.Set)
			}
			if p.Set["ANTHROPIC_MODEL"] != tc.wantModel {
				t.Errorf("ANTHROPIC_MODEL = %q, want %q", p.Set["ANTHROPIC_MODEL"], tc.wantModel)
			}
			if tc.wantOpus != "" && p.Set["ANTHROPIC_DEFAULT_OPUS_MODEL"] != tc.wantOpus {
				t.Errorf("OPUS slot = %q, want %q", p.Set["ANTHROPIC_DEFAULT_OPUS_MODEL"], tc.wantOpus)
			}
		})
	}
}
