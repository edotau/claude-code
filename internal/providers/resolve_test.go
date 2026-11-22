package providers

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/serving"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestPlainTargetCanonicalAndOneM(t *testing.T) {
	def := &Provider{Name: "plain"}
	anth := &Provider{Name: "anth", Provider: "anthropic"}
	cases := []struct {
		spec  string
		prov  *Provider
		model string
		oneM  bool
	}{
		{"claude-opus-5-5[1m]", def, "claude-opus-5-5", false},
		{"claude-opus-4-8[1m]", anth, "claude-opus-4-8", true},
		{"claude-opus-4-8[1m]", def, "claude-opus-4-8", false},
		{"claude-opus-5-5", def, "claude-opus-5-5", false},
		{"databricks-claude-opus-5-5", def, "claude-opus-5-5", false},
		{"opus-4-8[1m]", anth, "claude-opus-4-8", true},
		{"qwen3:30b", def, "qwen3:30b", false},
	}
	for _, c := range cases {
		got := PlainTarget(c.prov, c.spec)
		if got.Provider != c.prov || got.Model != c.model || models.Has1M(got.ClientModel()) != c.oneM {
			t.Errorf("PlainTarget(%q) = %s/%q/%v; want %s/%q/%v",
				c.spec, got.Provider.Name, got.Model, got.ContextTokens, c.prov.Name, c.model, c.oneM)
		}
	}
}

func TestClientModelKeepsTypedOneM(t *testing.T) {
	plain := &Provider{Name: "plain"}
	other := &Provider{Name: "other"}
	sel := Selection{Provider: plain, Aliases: map[string]Target{
		"opus":   {Provider: plain, Model: "claude-opus-4-8", ContextTokens: models.ContextWindow1M},
		"sonnet": {Provider: plain, Model: "claude-sonnet-5"},
		"haiku":  {Provider: other, Model: "claude-haiku-4-5", ContextTokens: models.ContextWindow1M},
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
}

// A prefixed provider gets its wire id whichever spelling is typed; first-party keeps the canonical id.
func TestPlainTargetWirePrefix(t *testing.T) {
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
		got := PlainTarget(c.prov, c.spec)
		if got.Model != c.model || models.Has1M(got.ClientModel()) != c.oneM {
			t.Errorf("%s PlainTarget(%q) = %q/%v; want %q/%v", c.prov.Name, c.spec, got.Model, got.ContextTokens, c.model, c.oneM)
		}
	}
}

func TestPlainTargetAliasOnDatabricksRow(t *testing.T) {
	dev := &Provider{Name: "dev", Preset: PresetDatabricks}
	for spec, want := range map[string]string{
		"opus":                       "databricks-claude-opus-5-5",
		"databricks-gpt-6-1-sol":     "databricks-gpt-6-1-sol",
		"databricks-claude-opus-5-5": "databricks-claude-opus-5-5",
	} {
		if got := PlainTarget(dev, spec); got.Provider != dev || got.Model != want {
			t.Errorf("PlainTarget(%q) = %s/%q; want dev/%q", spec, got.Provider.Name, got.Model, want)
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
		if got, ok := FailoverTarget(from, model, next); !ok || got.Model != want || models.Has1M(got.ClientModel()) {
			t.Errorf("FailoverTarget(%s) = %+v %v, want exact %s", model, got, ok, want)
		}
	}
	got, ok := FailoverTarget(from, "databricks-claude-opus-4-8", next)
	if !ok || got.Model != "databricks-claude-opus-4-8" || !models.Has1M(got.ClientModel()) {
		t.Errorf("slot failover = %+v %v, want the slot id with [1m] moved to the marked window", got, ok)
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

func listingFacts(t *testing.T, facts map[string]serving.EndpointFacts) {
	t.Helper()
	serving.SetFactsForTest(t, func(_, wire string) (serving.EndpointFacts, bool) { f, ok := facts[wire]; return f, ok })
}

// Given a fresh listing, Then api_types decide ResponsesServable for a listed id, the old rule an unlisted one,
// and opencode's ServesResponses stays menu-driven while WireFor respects live api_types.
func TestResponsesServableFollowsListingAPITypes(t *testing.T) {
	vaulttest.Use(t)
	both := []string{"mlflow/v1/chat/completions", "openai/v1/responses", "codex/v1/responses"}
	listingFacts(t, map[string]serving.EndpointFacts{
		"databricks-gpt-6-luna": {APITypes: []string{"mlflow/v1/chat/completions"}},
		"databricks-kimi-k3":    {APITypes: both},
		"databricks-glm-5-3":    {APITypes: []string{"mlflow/v1/chat/completions", "codex/v1/responses"}},
		"databricks-grok-4-7":   {APITypes: []string{"mlflow/v1/chat/completions", "openai/v1/responses"}},
		"databricks-new-resp":   {APITypes: []string{"openai/v1/responses", "codex/v1/responses"}},
		"databricks-gpt-5-4":    {Context: 400000},
	})
	p := &Provider{Name: "dev", Preset: PresetDatabricks, ModelPrefix: "databricks-"}
	p.Routes = map[string]string{ProtocolResponses: "/responses", ProtocolOpenAI: "/chat"}
	for wire, want := range map[string]bool{
		"databricks-gpt-6-luna": false, "databricks-kimi-k3": true, "databricks-glm-5-3": false,
		"databricks-grok-4-7": false, "databricks-new-resp": true, "databricks-gpt-5-4": true,
		"databricks-gpt-6-sol": true, "databricks-claude-opus-5-5": false,
	} {
		if got := ResponsesServable(p, wire); got != want {
			t.Errorf("ResponsesServable(%s) = %v, want %v", wire, got, want)
		}
	}
	if ServesResponsesIn([]OpencodeModel{{ID: "kimi-k3"}}, p, "databricks-kimi-k3") {
		t.Error("opencode's ServesResponses must not follow api_types")
	}
	if wire, ok, _ := WireForIn([]OpencodeModel{}, p, "databricks-new-resp"); !ok || wire != ProtocolResponses {
		t.Errorf("WireFor(responses-only live endpoint) = %q,%v, want responses", wire, ok)
	}
	if wire, ok, _ := WireForIn([]OpencodeModel{{ID: "grok-4-7", Responses: true}}, p, "databricks-grok-4-7"); !ok || wire != ProtocolOpenAI {
		t.Errorf("WireFor(grok without codex/v1/responses) = %q,%v, want openai", wire, ok)
	}
}

// Given catalog limits and a listing, Then the catalog window wins and a fresh listed output beats the catalog's.
func TestLimitsForPrefersCatalogThenListing(t *testing.T) {
	listingFacts(t, map[string]serving.EndpointFacts{"databricks-gpt-5-4": {Context: 400000, Output: 128000},
		"databricks-gemini-4-argon": {Context: 5, Output: 7}, "databricks-gpt-oss-120b": {Output: 8192}})
	p := &Provider{Name: "dev", Preset: PresetDatabricks}
	if l, ok := LimitsFor(p, "databricks-gpt-5-4"); !ok || l.Context != 400000 || l.Output != 128000 {
		t.Errorf("listed-only model limits = %+v, %v", l, ok)
	}
	want, _ := models.Limits("databricks-gemini-4-argon")
	if l, _ := LimitsFor(p, "databricks-gemini-4-argon"); want.Context == 0 || l.Context != want.Context {
		t.Errorf("catalog window %d must beat the listing's 5, got %+v", want.Context, l)
	}
	if l, _ := LimitsFor(p, "databricks-gpt-oss-120b"); l.Output != 8192 {
		t.Errorf("listed output 8192 must beat the catalog's 25000, got %+v", l)
	}
	if _, ok := LimitsFor(p, "databricks-unknown-x"); ok {
		t.Error("unknown model must stay unknown")
	}
}
