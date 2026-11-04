package providers

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/models"
)

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
	if got := p.Routes[ProtocolResponses]; got != wireHost+"/ai-gateway/codex/v1" {
		t.Errorf("responses route = %q", got)
	}
	if _, ok := p.Routes[ProtocolOpenAI]; ok {
		t.Errorf("gateway must have no openai route: %v", p.Routes)
	}
	if p.ModelPrefix != "system.ai." {
		t.Errorf("ModelPrefix = %q", p.ModelPrefix)
	}
	if p.Headers["x-databricks-use-coding-agent-mode"] != "true" {
		t.Errorf("headers = %v", p.Headers)
	}
	if got := p.Models["sonnet"]; got != "system.ai.claude-sonnet-5-5" {
		t.Errorf("sonnet alias = %q", got)
	}
}

func TestWireServingDefaultUnchanged(t *testing.T) {
	for _, opts := range []string{``, `, "options": {"wire": "serving"}`} {
		p := wireProvider(t, opts)
		if p.BaseURL != wireHost+"/serving-endpoints/anthropic" {
			t.Errorf("%q: BaseURL = %q", opts, p.BaseURL)
		}
		for _, d := range []string{ProtocolOpenAI, ProtocolResponses} {
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

func TestWireNonPresetUntouched(t *testing.T) {
	p := Provider{ModelPrefix: "x."}
	if got := p.WireModel("gpt-5.5"); got != "gpt-5.5" {
		t.Errorf("non-preset WireModel = %q", got)
	}
}

const dbxProv = `"dbx":{"preset":"databricks","host":"https://adb-1.azuredatabricks.net/","cliProfile":"work"}`

func dbxProvider(t *testing.T) Provider {
	t.Helper()
	return *mustParse(t, `{"providers":{"databricks":{`+dbxProv+`}}}`).Providers["dbx"]
}

func TestWireModelTable(t *testing.T) {
	db := dbxProvider(t)
	plain := Provider{}
	cases := []struct {
		name string
		p    Provider
		in   string
		want string
	}{
		{"canonical", db, "claude-opus-5-5", "databricks-claude-opus-5-5"},
		{"already wire", db, "databricks-claude-opus-5-5", "databricks-claude-opus-5-5"},
		{"1m kept", db, "claude-opus-5-5[1m]", "databricks-claude-opus-5-5[1m]"},
		{"wire 1m unchanged", db, "databricks-claude-opus-5-5[1m]", "databricks-claude-opus-5-5[1m]"},
		{"slash qualified", db, "openai/gpt-5", "openai/gpt-5"},
		{"colon qualified", db, "openai:gpt-5", "openai:gpt-5"},
		{"empty", db, "", ""},
		{"dots to dashes", db, "gpt-5.5", "databricks-gpt-5-5"},
		{"dots with 1m", db, "claude-opus-4.5[1m]", "databricks-claude-opus-4-5[1m]"},
		{"custom name unchanged", db, "my-opus", "my-opus"},
		{"custom dotted name unchanged", db, "my.endpoint", "my.endpoint"},
		{"catalog gemini", db, "gemini-3.8-flash", "databricks-gemini-3-8-flash"},
		{"non-catalog passes through", db, "gemini-2.5-pro", "gemini-2.5-pro"},
		{"no prefix provider", plain, "claude-opus-5-5", "claude-opus-5-5"},
		{"no prefix keeps dots", plain, "gpt-5.5", "gpt-5.5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.p.WireModel(c.in)
			if got != c.want {
				t.Fatalf("WireModel(%q) = %q, want %q", c.in, got, c.want)
			}
			if again := c.p.WireModel(got); again != got {
				t.Fatalf("not idempotent: %q -> %q", got, again)
			}
		})
	}
}

func TestLoadDecoratesPresetModels(t *testing.T) {
	reg := mustParse(t, `{"providers":{"databricks":{"dbx":{"preset":"databricks","host":"https://adb-1.azuredatabricks.net/","cliProfile":"work",
	  "models":{"opus":"claude-opus-5-5[1m]","sonnet":"gpt-5.5"}}}}}`)
	m := reg.Providers["dbx"].Models
	want := map[string]string{
		"opus": "databricks-claude-opus-5-5[1m]", "sonnet": "databricks-gpt-5-5", "haiku": "databricks-claude-haiku-5-5",
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("Models[%s] = %q, want %q", k, m[k], v)
		}
	}
	plain := mustParse(t, `{"providers":{"openai":{"p":{"protocol":"anthropic","base_url":"https://p","auth":{"type":"none"},"models":{"opus":"gpt-5.5"}}}}}`)
	if got := plain.Providers["p"].Models["opus"]; got != "gpt-5.5" {
		t.Errorf("plain provider model = %q", got)
	}
}

// The decorated ids live only in memory: the overlay writer works on the stored doc, never the registry.
func TestDecoratedModelsNeverPersisted(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	err := SetProviderFields("dbx", map[string]any{"preset": "databricks", "host": "https://adb-1.azuredatabricks.net/",
		"cliProfile": "work", "models": map[string]string{"opus": "claude-opus-5-5"}})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := reg.Providers["dbx"].Models["opus"]; got != "databricks-claude-opus-5-5" {
		t.Fatalf("loaded opus = %q, want decorated", got)
	}
	if err := SetProviderModel("dbx", "sonnet", "claude-sonnet-5-5"); err != nil {
		t.Fatal(err)
	}
	doc, err := OverlayDoc()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(doc), "databricks-claude") {
		t.Errorf("overlay persisted decorated ids: %s", doc)
	}
	if !strings.Contains(string(doc), `"claude-opus-5-5"`) || !strings.Contains(string(doc), `"claude-sonnet-5-5"`) {
		t.Errorf("overlay lost the canonical ids: %s", doc)
	}
}

// The endpoint type alone decides who serves 1M: serving and first-party yes, ai-gateway and others no.
func TestServes1MByEndpointType(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    *Provider
		want bool
	}{
		{"nil", nil, false},
		{"databricks serving", &Provider{ModelPrefix: models.DatabricksPrefix}, true},
		{"first-party anthropic", &Provider{Provider: "anthropic"}, true},
		{"ai-gateway", &Provider{ModelPrefix: gatewayModelPrefix}, false},
		{"github copilot", &Provider{Provider: "github"}, false},
		{"non-Claude endpoint", &Provider{Provider: "ollama"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.Serves1M(); got != tc.want {
				t.Errorf("Serves1M = %v, want %v", got, tc.want)
			}
		})
	}
}

// ContextWindow: a typed [1m] is always 1M; a bare catalog id is 1M only on a 1M endpoint; a non-Claude id is 200K.
func TestContextWindowByEndpoint(t *testing.T) {
	serving := &Provider{ModelPrefix: models.DatabricksPrefix}
	gateway := &Provider{ModelPrefix: gatewayModelPrefix}
	copilot := &Provider{Provider: "github"}
	for _, tc := range []struct {
		name string
		p    *Provider
		id   string
		want int
	}{
		{"serving 5.5", serving, "claude-opus-5-5", models.ContextWindow1M},
		{"gateway 5.5 bare", gateway, "claude-opus-5-5", models.ContextWindowStd},
		{"gateway typed [1m] kept", gateway, "claude-opus-5-5[1m]", models.ContextWindow1M},
		{"copilot bare", copilot, "claude-opus-5-5", models.ContextWindowStd},
		{"copilot typed [1m]", copilot, "claude-opus-5-5[1m]", models.ContextWindow1M},
		{"serving non-Claude", serving, "gpt-5", models.ContextWindowStd},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ContextWindow(tc.p, tc.id); got != tc.want {
				t.Errorf("ContextWindow(%q) = %d, want %d", tc.id, got, tc.want)
			}
		})
	}
}
