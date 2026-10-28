package providers

import (
	"strings"
	"testing"
)

const wireHost = "https://example.cloud.databricks.com"

func wireProvider(t *testing.T, opts string) *Provider {
	t.Helper()
	reg := mustParse(t, `{"providers": {"databricks": {"dbx": {"preset": "databricks", "host": "`+wireHost+`"`+opts+`}}}}`)
	return reg.Providers["dbx"]
}

func TestWireGatewayDerivation(t *testing.T) {
	p := wireProvider(t, `, "options": {"wire": "ai-gateway"}`)
	if p.BaseURL != wireHost+"/ai-gateway/anthropic" {
		t.Errorf("BaseURL = %q", p.BaseURL)
	}
	if got := p.Routes[DialectResponses]; got != wireHost+"/ai-gateway/codex/v1" {
		t.Errorf("responses route = %q", got)
	}
	if _, ok := p.Routes[DialectOpenAI]; ok {
		t.Errorf("gateway must have no openai route: %v", p.Routes)
	}
	if p.ModelPrefix != "system.ai." {
		t.Errorf("ModelPrefix = %q", p.ModelPrefix)
	}
	if p.Headers["x-databricks-use-coding-agent-mode"] != "true" {
		t.Errorf("headers = %v", p.Headers)
	}
	if got := p.Models["sonnet"]; got != "system.ai.claude-sonnet-5-5" {
		t.Errorf("sonnet slot = %q", got)
	}
}

func TestWireServingDefaultUnchanged(t *testing.T) {
	for _, opts := range []string{``, `, "options": {"wire": "serving"}`} {
		p := wireProvider(t, opts)
		if p.BaseURL != wireHost+"/serving-endpoints/anthropic" {
			t.Errorf("%q: BaseURL = %q", opts, p.BaseURL)
		}
		for _, d := range []string{DialectOpenAI, DialectResponses} {
			if p.Routes[d] != wireHost+"/serving-endpoints" {
				t.Errorf("%q: route %s = %q", opts, d, p.Routes[d])
			}
		}
		if p.ModelPrefix != "databricks-" {
			t.Errorf("%q: prefix = %q", opts, p.ModelPrefix)
		}
		if _, ok := p.Headers["x-databricks-use-coding-agent-mode"]; ok {
			t.Errorf("%q: serving must not set the gateway header", opts)
		}
	}
}

func TestWireUnknownRejected(t *testing.T) {
	_, err := parseOverlay(t, `{"providers": {"databricks": {"dbx": {"preset": "databricks", "host": "`+wireHost+`", "options": {"wire": "bogus"}}}}}`)
	if err == nil || !strings.Contains(err.Error(), "options.wire") {
		t.Fatalf("err = %v, want options.wire rejection", err)
	}
}

func TestWireModelPrefixThenDots(t *testing.T) {
	serving := wireProvider(t, ``)
	gateway := wireProvider(t, `, "options": {"wire": "ai-gateway"}`)
	cases := []struct {
		p        *Provider
		in, want string
	}{
		{serving, "gpt-5.5", "databricks-gpt-5-5"},
		{serving, "claude-opus-5-5[1m]", "databricks-claude-opus-5-5[1m]"},
		{gateway, "gpt-5.5", "system.ai.gpt-5-5"},
		{gateway, "claude-sonnet-5-5", "system.ai.claude-sonnet-5-5"},
		{gateway, "system.ai.gpt-5-5", "system.ai.gpt-5-5"}, // idempotent
	}
	for _, c := range cases {
		if got := c.p.WireModel(c.in); got != c.want {
			t.Errorf("%s WireModel(%q) = %q, want %q", c.p.ModelPrefix, c.in, got, c.want)
		}
	}
}

func TestWireModelsStored(t *testing.T) {
	p := wireProvider(t, `, "options": {"wire": "ai-gateway", "models": {"claude-sonnet-5-5": {"x": 1}}}`)
	if p.Options.Models["claude-sonnet-5-5"]["x"] == nil {
		t.Errorf("Options.Models not stored: %v", p.Options.Models)
	}
}

func TestWireNonPresetUntouched(t *testing.T) {
	p := Provider{ModelPrefix: "x."}
	if got := p.WireModel("gpt-5.5"); got != "gpt-5.5" {
		t.Errorf("non-preset WireModel = %q", got)
	}
}
