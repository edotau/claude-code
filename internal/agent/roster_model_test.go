package agent

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

// servesRows are the fixtures Serves is proved over: a Databricks serving row, a gateway row and an anthropic row.
func servesRows() map[string]*providers.Provider {
	return map[string]*providers.Provider{
		"serving": dbxRow("dbx", "databricks-"),
		"gateway": gwRow("gw"),
		"anthropic": {Name: "ant", Protocol: providers.ProtocolAnthropic, BaseURL: "https://a.example.com",
			Auth: providers.Auth{Type: providers.AuthKey}},
	}
}

func TestServesReasons(t *testing.T) {
	rows := servesRows()
	cli := &providers.Provider{Name: "sub", Protocol: providers.ProtocolAnthropic, BaseURL: "https://s", Auth: providers.Auth{Type: providers.AuthCLI}}
	login := &providers.Provider{Name: "gh", Protocol: providers.ProtocolOpenAI, BaseURL: "https://g", Auth: providers.Auth{Type: providers.AuthCLI, Login: "copilot"}}
	cases := []struct {
		agent string
		p     *providers.Provider
		model string
		ok    bool
		why   string
	}{
		{"gemini", rows["serving"], "", false, "agy has no custom base URL"},
		{"codex", rows["anthropic"], "", false, "serves no responses route"},
		{"copilot", rows["serving"], "", true, ""},
		{"copilot", cli, "", false, "uses cli (subscription) auth"},
		{"copilot", login, "", true, ""},
		{"codex", login, "", false, "carries a copilot login"},
		{"nope", rows["serving"], "", false, `unknown agent "nope"`},
		{"copilot", rows["gateway"], "system.ai.kimi-k3", false, "no openai route"},
		{"copilot", rows["gateway"], "system.ai.claude-opus-5-5", true, ""},
		{"codex", rows["serving"], "databricks-gpt-5-5", true, ""},
		{"copilot", rows["serving"], "databricks-gpt-5-5", true, ""}, // responses rides the router suffix
	}
	for _, c := range cases {
		ok, why := Serves(c.agent, c.p, c.model)
		if ok != c.ok || (c.why == "") != (why == "") || !strings.Contains(why, c.why) {
			t.Errorf("Serves(%s,%s,%q) = %v,%q, want %v containing %q", c.agent, c.p.Name, c.model, ok, why, c.ok, c.why)
		}
	}
}

// A wire the agent neither speaks nor has a router suffix for is refused; every roster agent has its suffixes.
func TestServesRefusesWireOutsideAgentSuffix(t *testing.T) {
	rows := servesRows()
	ok, why := Serves("codex", rows["serving"], "databricks-kimi-k3") // codex: responses only; kimi rides openai
	if ok || !strings.Contains(why, "does not speak the openai wire") {
		t.Errorf("Serves = %v,%q", ok, why)
	}
}
