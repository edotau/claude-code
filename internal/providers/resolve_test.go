package providers

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestParseTargetCanonicalAndOneM(t *testing.T) {
	reg := load(t)
	def := &Provider{Name: "plain"}
	other := reg.Providers["openai"]
	if other == nil {
		t.Fatal("openai provider missing from test registry")
	}
	cases := []struct {
		spec  string
		prov  *Provider
		model string
		oneM  bool
	}{
		{"claude-opus-5-5[1m]", def, "claude-opus-5-5", false},
		{"claude-opus-4-8[1m]", def, "claude-opus-4-8", true},
		{"claude-opus-5-5", def, "claude-opus-5-5", false},
		{"databricks-claude-opus-5-5", def, "claude-opus-5-5", false},
		{"opus-4-8[1m]", def, "claude-opus-4-8", true},
		{"openai:gpt-5-nano", other, "gpt-5-nano", false},
		{"openai:databricks-claude-opus-5[1m]", other, "claude-opus-5", false},
		{"qwen3:30b", def, "qwen3:30b", false},
	}
	for _, c := range cases {
		got := ParseTarget(reg, c.spec, def)
		if got.Provider != c.prov || got.Model != c.model || got.OneM != c.oneM {
			t.Errorf("ParseTarget(%q) = %s/%q/%v; want %s/%q/%v",
				c.spec, got.Provider.Name, got.Model, got.OneM, c.prov.Name, c.model, c.oneM)
		}
	}
}

func TestClientModelKeepsTypedOneM(t *testing.T) {
	plain := &Provider{Name: "plain"}
	other := &Provider{Name: "other"}
	sel := Selection{Provider: plain, Aliases: map[string]Target{
		"opus":   {Provider: plain, Model: "claude-opus-4-8", OneM: true},
		"sonnet": {Provider: plain, Model: "claude-sonnet-5"},
		"haiku":  {Provider: other, Model: "claude-haiku-4-5", OneM: true},
		"fable":  {Provider: other, Model: "gpt-5"},
	}}
	want := map[string]string{
		"opus":   "claude-opus-4-8[1m]",
		"sonnet": "claude-sonnet-5",
		"haiku":  "claude-haiku-4-5",
		"fable":  "gpt-5",
	}
	for alias, w := range want {
		if got := sel.ClientModel(alias); got != w {
			t.Errorf("ClientModel(%s) = %q; want %q", alias, got, w)
		}
	}
	if got, ok := sel.AliasTarget("gpt-5"); !ok || got.Provider != other {
		t.Errorf("AliasTarget(gpt-5) = %+v %v; want the cross-provider pin", got, ok)
	}
	if _, ok := sel.AliasTarget("claude-sonnet-5"); ok {
		t.Error("AliasTarget(same-provider id) found a cross-provider pin")
	}
}

// A prefixed provider gets its wire id whichever spelling is typed; first-party keeps the canonical id.
func TestParseTargetWirePrefix(t *testing.T) {
	reg := load(t)
	qa := &Provider{Name: "databricks-qa", Provider: "databricks", ModelPrefix: "databricks-"}
	sub := &Provider{Name: "ultra", Provider: "anthropic"}
	cases := []struct {
		spec  string
		prov  *Provider
		model string
		oneM  bool
	}{
		{"opus-5-5", qa, "databricks-claude-opus-5-5", false},
		{"claude-opus-5-5", qa, "databricks-claude-opus-5-5", false},
		{"databricks-claude-opus-5-5", qa, "databricks-claude-opus-5-5", false},
		{"opus-4-8[1m]", qa, "databricks-claude-opus-4-8", true},
		{"opus-5-5[1m]", qa, "databricks-claude-opus-5-5", false},
		{"opus-5-5", sub, "claude-opus-5-5", false},
		{"opus-4-8[1m]", sub, "claude-opus-4-8", true},
	}
	for _, c := range cases {
		got := ParseTarget(reg, c.spec, c.prov)
		if got.Model != c.model || got.OneM != c.oneM {
			t.Errorf("%s ParseTarget(%q) = %q/%v; want %q/%v", c.prov.Name, c.spec, got.Model, got.OneM, c.model, c.oneM)
		}
	}
}

func TestParseTargetAliasOnDatabricksRow(t *testing.T) {
	dev := &Provider{Name: "dev", Preset: PresetDatabricks}
	reg := &Registry{Providers: map[string]*Provider{"dev": dev}}
	for spec, want := range map[string]string{
		"dev:opus":                       "databricks-claude-opus-5-5",
		"dev:databricks-gpt-6-1-sol":     "databricks-gpt-6-1-sol",
		"dev:databricks-claude-opus-5-5": "databricks-claude-opus-5-5",
	} {
		if got := ParseTarget(reg, spec, dev); got.Provider != dev || got.Model != want {
			t.Errorf("ParseTarget(%q) = %s/%q; want dev/%q", spec, got.Provider.Name, got.Model, want)
		}
	}
}

// wireRows are the fixtures WireFor and AnthropicRoute are proved over.
func wireRows(t *testing.T) map[string]*Provider {
	t.Helper()
	return map[string]*Provider{
		"serving": wireProvider(t, ``),
		"anthropic": {Name: "ant", Protocol: ProtocolAnthropic, BaseURL: "https://a.example.com",
			Auth: Auth{Type: AuthKey}},
		"openai": {Name: "oai", Protocol: ProtocolOpenAI, BaseURL: "https://o.example.com/v1",
			Auth: Auth{Type: AuthKey}},
	}
}

func TestWireForTable(t *testing.T) {
	rows := wireRows(t)
	pre := func(row, id string) string { return rows[row].WireModel(id) }
	const (
		a, o, r = ProtocolAnthropic, ProtocolOpenAI, ProtocolResponses
		none    = ""
	)
	cases := []struct {
		row, model, want string
	}{
		{"serving", "claude-opus-5-5", a}, {"serving", "gpt-5-5", r}, {"serving", "gpt-6-1-sol", r},
		{"serving", "gpt-oss-120b", o}, {"serving", "gemini-3-7-flash", o}, {"serving", "kimi-k3", o},
		{"anthropic", "claude-opus-5-5", a}, {"anthropic", "gpt-5-5", none}, {"anthropic", "gpt-oss-120b", none},
		{"anthropic", "gemini-3-7-flash", none}, {"anthropic", "kimi-k3", none},
		{"openai", "claude-opus-5-5", o}, {"openai", "gpt-5-5", none}, {"openai", "gpt-6-1-sol", none},
		{"openai", "gpt-oss-120b", o}, {"openai", "gemini-3-7-flash", o}, {"openai", "kimi-k3", o},
	}
	for _, c := range cases {
		wire, ok, why := WireFor(rows[c.row], pre(c.row, c.model))
		if c.want == none {
			if ok || wire != "" || why == "" {
				t.Errorf("%s/%s: WireFor = %q,%v,%q, want unservable with a reason", c.row, c.model, wire, ok, why)
			}
			continue
		}
		if !ok || wire != c.want || why != "" {
			t.Errorf("%s/%s: WireFor = %q,%v,%q, want %q", c.row, c.model, wire, ok, why, c.want)
		}
	}
}

func TestWireForReasonNamesModelAndRow(t *testing.T) {
	rows := wireRows(t)
	_, _, why := WireFor(rows["openai"], "gpt-5-5")
	if !strings.Contains(why, "gpt-5-5") || !strings.Contains(why, "Responses-only") || !strings.Contains(why, "oai") {
		t.Errorf("responses-only reason = %q", why)
	}
}

func TestAnthropicRoute(t *testing.T) {
	rows := wireRows(t)
	cases := []struct {
		row, model string
		want       bool
	}{
		{"serving", "claude-opus-5-5", true},
		{"serving", "kimi-k3", false},
		{"serving", "gpt-5-5", false},
		{"anthropic", "claude-opus-5-5", true},
		{"anthropic", "kimi-k3", true},
		{"anthropic", "gpt-5-5", true},
		{"openai", "claude-opus-5-5", false},
	}
	for _, c := range cases {
		route, ok := AnthropicRoute(rows[c.row], rows[c.row].WireModel(c.model))
		if ok != c.want {
			t.Errorf("%s/%s: AnthropicRoute ok = %v, want %v", c.row, c.model, ok, c.want)
		}
		if want, _ := rows[c.row].Route(ProtocolAnthropic); ok && route != want {
			t.Errorf("%s/%s: route = %q, want %q", c.row, c.model, route, want)
		}
	}
}

func TestClaudeCodeError(t *testing.T) {
	rows := wireRows(t)
	err := ClaudeCodeError(rows["serving"], "databricks-gpt-5-5")
	if err == nil || !strings.Contains(err.Error(), "databricks-gpt-5-5") || !strings.Contains(err.Error(), "codex or copilot") {
		t.Errorf("Responses-only err = %v", err)
	}
	for _, c := range []struct{ row, model string }{
		{"serving", "databricks-kimi-k3"}, {"serving", "databricks-claude-opus-5-5"},
		{"openai", "gpt-5-5"},
	} {
		if err := ClaudeCodeError(rows[c.row], c.model); err != nil {
			t.Errorf("%s/%s: err = %v, want nil", c.row, c.model, err)
		}
	}
}

func presetSel(t *testing.T, opus, sonnet string) (*Registry, Selection) {
	t.Helper()
	reg := mustParse(t, `{"providers": {"databricks": {"dbx": {"preset": "databricks", "host": "https://adb-1.azuredatabricks.net", "cliProfile": "w"}}}}`)
	p := reg.Providers["dbx"]
	return reg, Selection{Provider: p, Aliases: map[string]Target{
		"opus": {Provider: p, Model: opus}, "sonnet": {Provider: p, Model: sonnet}}}
}

func TestNeedsRouterNonClaudeSlotOnDualRouteProvider(t *testing.T) {
	reg, sel := presetSel(t, "databricks-gpt-5-5", "databricks-claude-sonnet-5-5")
	if !sel.NeedsRouter(reg) {
		t.Error("non-Claude opus on a provider with an openai route must need the router")
	}
	reg, sel = presetSel(t, "databricks-claude-opus-5-5", "databricks-claude-sonnet-5-5")
	if !sel.NeedsRouter(reg) {
		t.Error("all-Claude aliases on the preset row must need the router: it renames Claude Code's built-in ids")
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
		{"claude on anthropic-only", Selection{Provider: tw, Aliases: map[string]Target{"opus": {Provider: tw, Model: "databricks-claude-opus-5-5"}}}, false},
		{"non-claude on anthropic-only (no openai route)", Selection{Provider: tw, Aliases: map[string]Target{"opus": {Provider: tw, Model: "databricks-gpt-5-5"}}}, false},
		{"cross-provider alias to the twin", Selection{Provider: tw, Aliases: map[string]Target{"opus": {Provider: oai, Model: "databricks-gpt-5-5"}}}, true},
		{"openai-only session provider", Selection{Provider: oai, Aliases: map[string]Target{"opus": {Provider: oai, Model: "databricks-gpt-5-5"}}}, true},
	}
	for _, c := range cases {
		if got := c.sel.NeedsRouter(reg); got != c.want {
			t.Errorf("%s: NeedsRouter = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestFailoverTargetKeepsExactClaudeVersion(t *testing.T) {
	from := &Provider{Name: "dev", Preset: PresetDatabricks, Models: map[string]string{"opus": "databricks-claude-opus-4-8"}}
	next := &Provider{Name: "dev2", Preset: PresetDatabricks, Models: map[string]string{"opus": "databricks-claude-opus-4-8[1m]"}}
	for model, want := range map[string]string{
		"claude-opus-5":            "databricks-claude-opus-5",
		"databricks-claude-opus-5": "databricks-claude-opus-5",
	} {
		if got, ok := FailoverTarget(from, model, next); !ok || got.Model != want || got.OneM {
			t.Errorf("FailoverTarget(%s) = %+v %v, want exact %s", model, got, ok, want)
		}
	}
	got, ok := FailoverTarget(from, "databricks-claude-opus-4-8", next)
	if !ok || got.Model != "databricks-claude-opus-4-8" || !got.OneM {
		t.Errorf("slot failover = %+v %v, want the slot id with [1m] moved to OneM", got, ok)
	}
}

// An explicit menu flag beats the catalog, chat-only ids never serve Responses, and any spelling finds the entry.
func TestServesResponsesMenuOverridesCatalog(t *testing.T) {
	vaulttest.Use(t)
	p := &Provider{Name: "dev", Preset: PresetDatabricks, ModelPrefix: "databricks-"}
	if err := SetOpencodeMenu([]OpencodeModel{{ID: "gpt-6-1-sol", Responses: false}, {ID: "gpt-oss-120b", Responses: true}}); err != nil {
		t.Fatal(err)
	}
	for _, spelled := range []string{"databricks-gpt-6-1-sol", "gpt-6-1-sol"} {
		if ServesResponses(p, spelled) {
			t.Errorf("menu Responses:false on a catalog Responses-only id must win for %q", spelled)
		}
	}
	if ResponsesID("gpt-6-1-sol") {
		t.Error("ResponsesID: menu Responses:false must win")
	}
	if ServesResponses(p, "databricks-gpt-oss-120b") || ResponsesID("gpt-oss-120b") || ResponsesServable(p, "databricks-gpt-oss-120b") {
		t.Error("chat-only gpt-oss-120b must not serve Responses whatever the menu says")
	}
	if !ServesResponses(p, "gpt-5-5") || !ResponsesID("gpt-5-5") {
		t.Error("unlisted catalog Responses-only id must serve Responses")
	}
}

// A Responses=false GPT row stays on chat for opencode yet is Responses-servable for codex.
func TestResponsesFalseGPTRowKeepsOpencodeChatRoute(t *testing.T) {
	vaulttest.Use(t)
	p := &Provider{Name: "dev", Preset: PresetDatabricks, ModelPrefix: "databricks-"}
	luna := []OpencodeModel{{ID: "gpt-6-luna"}}
	p.Routes = map[string]string{ProtocolResponses: "/responses", ProtocolOpenAI: "/chat"}
	if ServesResponsesIn(luna, p, "databricks-gpt-6-luna") || !ResponsesServable(p, "databricks-gpt-6-luna") {
		t.Error("a Responses=false GPT row stays chat for opencode (ServesResponses) yet is Responses-servable for codex")
	}
	if wire, _, _ := WireForIn(luna, p, "databricks-gpt-6-luna"); wire == ProtocolResponses {
		t.Errorf("opencode wire for the Responses=false row = %q, want %s", wire, ProtocolOpenAI)
	}
}

// Given a Grok row, Then opencode keeps its chat route and Grok stays out of Responses (Databricks does not stream it).
func TestGrokKeepsOpencodeChatRoute(t *testing.T) {
	vaulttest.Use(t)
	p := &Provider{Name: "dev", Preset: PresetDatabricks, ModelPrefix: "databricks-"}
	p.Routes = map[string]string{ProtocolResponses: "/responses", ProtocolOpenAI: "/chat"}
	grok := []OpencodeModel{{ID: "grok-4-7"}}
	if ResponsesServable(p, "databricks-grok-4-7") || ServesResponsesIn(grok, p, "databricks-grok-4-7") {
		t.Error("Grok must stay outside ResponsesServable and ServesResponses: codex's rule lives in internal/codex")
	}
	if wire, _, _ := WireForIn(grok, p, "databricks-grok-4-7"); wire == ProtocolResponses {
		t.Errorf("opencode wire for Grok = %q, want %s", wire, ProtocolOpenAI)
	}
}

func listingFacts(t *testing.T, facts map[string]EndpointFacts) {
	t.Helper()
	old := ListedFacts
	t.Cleanup(func() { ListedFacts = old })
	ListedFacts = func(_ *Provider, wire string) (EndpointFacts, bool) { f, ok := facts[wire]; return f, ok }
}

// Given a fresh listing, Then api_types decide ResponsesServable for a listed id, the old rule an unlisted one,
// and opencode's Responses-only routing is unchanged.
func TestResponsesServableFollowsListingAPITypes(t *testing.T) {
	vaulttest.Use(t)
	listingFacts(t, map[string]EndpointFacts{
		"databricks-gpt-6-luna": {APITypes: []string{"mlflow/v1/chat/completions"}},
		"databricks-kimi-k3":    {APITypes: []string{"mlflow/v1/chat/completions", "openai/v1/responses"}},
		"databricks-grok-4-7":   {APITypes: []string{"mlflow/v1/chat/completions", "openai/v1/responses"}},
		"databricks-gpt-5-4":    {Context: 400000},
	})
	p := &Provider{Name: "dev", Preset: PresetDatabricks, ModelPrefix: "databricks-"}
	p.Routes = map[string]string{ProtocolResponses: "/responses", ProtocolOpenAI: "/chat"}
	for wire, want := range map[string]bool{"databricks-gpt-6-luna": false, "databricks-kimi-k3": true,
		"databricks-grok-4-7": true, "databricks-gpt-5-4": true, "databricks-gpt-6-sol": true, "databricks-claude-opus-5-5": false} {
		if got := ResponsesServable(p, wire); got != want {
			t.Errorf("ResponsesServable(%s) = %v, want %v", wire, got, want)
		}
	}
	if ServesResponsesIn([]OpencodeModel{{ID: "kimi-k3"}}, p, "databricks-kimi-k3") {
		t.Error("opencode's ServesResponses must not follow api_types")
	}
}

// Given catalog limits and a listing, Then the catalog wins per field and the listing fills the rest.
func TestLimitsForPrefersCatalogThenListing(t *testing.T) {
	listingFacts(t, map[string]EndpointFacts{"databricks-gpt-5-4": {Context: 400000, Output: 128000},
		"databricks-gemini-4-argon": {Context: 5, Output: 7}})
	p := &Provider{Name: "dev", Preset: PresetDatabricks}
	if l, ok := LimitsFor(p, "databricks-gpt-5-4"); !ok || l.Context != 400000 || l.Output != 128000 {
		t.Errorf("listed-only model limits = %+v, %v", l, ok)
	}
	want, _ := models.Limits("databricks-gemini-4-argon")
	if l, _ := LimitsFor(p, "databricks-gemini-4-argon"); want.Context == 0 || l.Context != want.Context {
		t.Errorf("catalog window %d must beat the listing's 5, got %+v", want.Context, l)
	}
	if _, ok := LimitsFor(p, "databricks-unknown-x"); ok {
		t.Error("unknown model must stay unknown")
	}
}
