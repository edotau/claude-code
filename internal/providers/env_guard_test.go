package providers

import (
	"strings"
	"testing"
)

func envOverlay(key string) string {
	return `{"providers":{"e":{"kind":"anthropic","base_url":"https://e","auth":{"type":"none"},"env":{"` + key + `":"v"}}}}`
}

func TestValidateRejectsHarnessOwnedEnvKeys(t *testing.T) {
	for _, k := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "HARNESS_PROVIDER",
		"CLAUDE_CODE_API_KEY_HELPER_TTL_MS", "CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING"} {
		if _, err := parseOverlay(t, envOverlay(k)); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("env key %s accepted (err %v)", k, err)
		}
	}
}

func TestValidateAllowsBenignEnvKeys(t *testing.T) {
	for _, k := range []string{"CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS", "GW_REGION", "DATA_PATH"} {
		if _, err := parseOverlay(t, envOverlay(k)); err != nil {
			t.Errorf("env key %s rejected: %v", k, err)
		}
	}
}

func TestValidateRejectsCredentialSegmentEnvKeys(t *testing.T) {
	for _, k := range []string{"FOO_HEADERS", "MY_AUTH", "DB_CREDENTIALS", "DB_CREDENTIAL", "DATABRICKS_PAT", "X_BEARER", "X_AUTH_Y"} {
		if _, err := parseOverlay(t, envOverlay(k)); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("credential-shaped env key %s accepted (err %v)", k, err)
		}
	}
}

const sessionProviders = `"gw":{"kind":"anthropic","base_url":"https://gw.example/anthropic","auth":{"type":"none"},"models":{"opus":"o","sonnet":"s"}},` +
	`"plain":{"kind":"anthropic","base_url":"https://plain.example/anthropic","auth":{"type":"none"},"models":{"opus":"o"}}`

func profileReg(t *testing.T, sets string) *Registry {
	t.Helper()
	return mustParse(t, `{"providers":{`+sessionProviders+`},"profiles":{"active":"w","sets":{"w":`+sets+`}}}`)
}

func TestResolveAgentSessionIgnoresProviderPrefix(t *testing.T) {
	reg := profileReg(t, `{"session":{"account":"gw","models":{"sonnet":"m-son"}}}`)
	b := reg.ResolveAgent(AgentRequest{Agent: "session", Model: "plain:other"})
	if b.Provider != "gw" || b.Slots["sonnet"] != "m-son" || b.Slots["opus"] != "plain:other" || b.Profile != "w" {
		t.Errorf("binding = %+v", b)
	}
}

func TestResolveAgentAskKeepsProviderPrefix(t *testing.T) {
	reg := profileReg(t, `{"copilot":{"account":"gw","model":"row-m"}}`)
	b := reg.ResolveAgent(AgentRequest{Agent: "copilot", Model: "plain:other"})
	if b.Provider != "plain" || b.Model != "plain:other" {
		t.Errorf("binding = %+v", b)
	}
}

func TestResolveAgentAccountlessRowModelApplies(t *testing.T) {
	reg := profileReg(t, `{"copilot":{"model":"vendor-m","effort":"high"}}`)
	b := reg.ResolveAgent(AgentRequest{Agent: "copilot"})
	if b.Model != "vendor-m" || b.Effort != "high" || b.Provider != "" || b.Profile != "w" {
		t.Errorf("binding = %+v", b)
	}
	if b := reg.ResolveAgent(AgentRequest{Agent: "copilot", Provider: "plain"}); b.Model != "" {
		t.Errorf("explicit provider must not take an account-less row model: %+v", b)
	}
}
