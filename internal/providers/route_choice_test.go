package providers

import "testing"

func presetSel(t *testing.T, opus, sonnet string) (*Registry, Selection) {
	t.Helper()
	reg := mustParse(t, `{"providers": {"databricks": {"dbx": {"preset": "databricks", "host": "https://adb-1.azuredatabricks.net", "cliProfile": "w"}}}}`)
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
	reg := mustParse(t, `{"providers": {"databricks": {
"tw": {"protocol":"anthropic","base_url":"https://h/anthropic","auth":{"type":"none"},"models":{"opus":"databricks-claude-opus-5-5"}},
"tw-oai": {"protocol":"openai","base_url":"https://h","auth":{"type":"none"},"models":{"opus":"databricks-gpt-5-5"}}}}}`)
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
