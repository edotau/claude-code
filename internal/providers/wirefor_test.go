package providers

import (
	"strings"
	"testing"
)

// wireRows are the fixtures WireFor, AnthropicRoute and Serves are proved over.
func wireRows(t *testing.T) map[string]*Provider {
	t.Helper()
	return map[string]*Provider{
		"serving": wireProvider(t, ``),
		"gateway": wireProvider(t, `, "options": {"wire": "ai-gateway"}`),
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
		none    = "" // no wire: ok false with a reason
	)
	cases := []struct {
		row, model, want string
	}{
		{"serving", "claude-opus-5-5", a}, {"serving", "gpt-5-5", r}, {"serving", "gpt-6-1-sol", r},
		{"serving", "gpt-oss-120b", o}, {"serving", "gemini-3-7-flash", o}, {"serving", "kimi-k3", o},
		{"gateway", "claude-opus-5-5", a}, {"gateway", "gpt-5-5", r}, {"gateway", "gpt-6-1-sol", r},
		{"gateway", "gpt-oss-120b", none}, {"gateway", "gemini-3-7-flash", none}, {"gateway", "kimi-k3", none},
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
	_, _, why = WireFor(rows["gateway"], rows["gateway"].WireModel("kimi-k3"))
	if !strings.Contains(why, "no openai route") || !strings.Contains(why, "anthropic,responses") {
		t.Errorf("gateway chat reason = %q", why)
	}
}

func TestAnthropicRoute(t *testing.T) {
	rows := wireRows(t)
	cases := []struct {
		row, model string
		want       bool
	}{
		{"serving", "claude-opus-5-5", true},
		{"serving", "kimi-k3", false}, // Databricks anthropic is Claude-only
		{"serving", "gpt-5-5", false},
		{"gateway", "claude-opus-5-5", true},
		{"gateway", "kimi-k3", false}, // unservable, but Databricks: still no anthropic fallback
		{"anthropic", "claude-opus-5-5", true},
		{"anthropic", "kimi-k3", true}, // unservable on a third-party host: it serves its own models
		{"anthropic", "gpt-5-5", true},
		{"openai", "claude-opus-5-5", false}, // no anthropic route at all
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
		{"openai", "gpt-5-5"}, // unservable is not a Claude Code refusal; the route check owns it
	} {
		if err := ClaudeCodeError(rows[c.row], c.model); err != nil {
			t.Errorf("%s/%s: err = %v, want nil", c.row, c.model, err)
		}
	}
}

func TestServesReasons(t *testing.T) {
	rows := wireRows(t)
	cli := &Provider{Name: "sub", Protocol: ProtocolAnthropic, BaseURL: "https://s", Auth: Auth{Type: AuthCLI}}
	login := &Provider{Name: "gh", Protocol: ProtocolOpenAI, BaseURL: "https://g", Auth: Auth{Type: AuthCLI, Login: "copilot"}}
	cases := []struct {
		agent string
		p     *Provider
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
	rows := wireRows(t)
	ok, why := Serves("codex", rows["serving"], "databricks-kimi-k3") // codex: responses only; kimi rides openai
	if ok || !strings.Contains(why, "does not speak the openai wire") {
		t.Errorf("Serves = %v,%q", ok, why)
	}
}
