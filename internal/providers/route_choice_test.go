package providers

import "testing"

func presetSel(t *testing.T, opus, sonnet string) (*Registry, Selection) {
	t.Helper()
	reg := mustParse(t, `{"providers": {"dbx": {"preset": "databricks", "host": "https://adb-1.azuredatabricks.net", "cliProfile": "w"}}}`)
	p := reg.Providers["dbx"]
	return reg, Selection{Provider: p, Slots: map[string]Target{
		"opus": {Provider: p, Model: opus}, "sonnet": {Provider: p, Model: sonnet}}}
}

func TestNeedsRouterNonClaudeSlotOnDualRouteProvider(t *testing.T) {
	reg, sel := presetSel(t, "databricks-gpt-5-5", "databricks-claude-sonnet-5-5")
	if !sel.NeedsRouter(reg) {
		t.Error("non-Claude opus on a provider with an openai route must need the router")
	}
	reg, sel = presetSel(t, "databricks-claude-opus-5-5", "databricks-claude-sonnet-5-5")
	if sel.NeedsRouter(reg) {
		t.Error("all-Claude slots on the preset row must stay direct")
	}
}

func TestNeedsRouterTwinShapedUnchanged(t *testing.T) {
	reg := mustParse(t, `{"providers": {
"tw": {"kind":"anthropic","base_url":"https://h/anthropic","auth":{"type":"none"},"models":{"opus":"databricks-claude-opus-5-5"}},
"tw-oai": {"kind":"openai","base_url":"https://h","auth":{"type":"none"},"models":{"opus":"databricks-gpt-5-5"}}}}`)
	tw, oai := reg.Providers["tw"], reg.Providers["tw-oai"]
	cases := []struct {
		name string
		sel  Selection
		want bool
	}{
		{"claude on anthropic-only", Selection{Provider: tw, Slots: map[string]Target{"opus": {Provider: tw, Model: "databricks-claude-opus-5-5"}}}, false},
		{"non-claude on anthropic-only (no openai route)", Selection{Provider: tw, Slots: map[string]Target{"opus": {Provider: tw, Model: "databricks-gpt-5-5"}}}, false},
		{"cross-provider slot to the twin", Selection{Provider: tw, Slots: map[string]Target{"opus": {Provider: oai, Model: "databricks-gpt-5-5"}}}, true},
		{"openai-only session provider", Selection{Provider: oai, Slots: map[string]Target{"opus": {Provider: oai, Model: "databricks-gpt-5-5"}}}, true},
	}
	for _, c := range cases {
		if got := c.sel.NeedsRouter(reg); got != c.want {
			t.Errorf("%s: NeedsRouter = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestResolveAgentTable(t *testing.T) {
	reg := &Registry{
		Providers: map[string]*Provider{"gw": {Name: "gw"}, "other": {Name: "other"}},
		Profiles: Profiles{Active: "w", Sets: map[string]Profile{"w": {
			"codex":   {Account: "gw", Model: "row-m", Effort: "high"},
			"session": {Account: "gw", Models: map[string]string{"opus": "m-opus", "sonnet": "m-son"}},
		}}},
	}
	none := &Registry{Providers: reg.Providers}
	cases := []struct {
		name string
		reg  *Registry
		req  AgentRequest
		want AgentBinding
	}{
		{"no profile passes the request through", none, AgentRequest{Agent: "codex", Model: "x", Effort: "low"},
			AgentBinding{Model: "x", Effort: "low"}},
		{"row fills everything", reg, AgentRequest{Agent: "codex"},
			AgentBinding{Provider: "gw", Model: "row-m", Effort: "high", Profile: "w"}},
		{"explicit wins over row", reg, AgentRequest{Agent: "codex", Provider: "other", Model: "cli-m", Effort: "low"},
			AgentBinding{Provider: "other", Model: "cli-m", Effort: "low"}},
		{"provider:model prefix beats row account", reg, AgentRequest{Agent: "codex", Model: "other:foo"},
			AgentBinding{Provider: "other", Model: "other:foo", Effort: "high", Profile: "w"}},
		{"agent without a row", reg, AgentRequest{Agent: "gemini", Model: "g"}, AgentBinding{Model: "g"}},
	}
	for _, c := range cases {
		got := c.reg.ResolveAgent(c.req)
		if got.Provider != c.want.Provider || got.Model != c.want.Model || got.Effort != c.want.Effort ||
			got.Profile != c.want.Profile || len(got.Slots) != len(c.want.Slots) {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
	got := reg.ResolveAgent(AgentRequest{Agent: "session", Model: "cli-opus"})
	if got.Provider != "gw" || got.Slots["opus"] != "cli-opus" || got.Slots["sonnet"] != "m-son" {
		t.Errorf("session slots: %+v", got)
	}
}
