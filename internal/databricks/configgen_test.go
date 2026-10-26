package databricks

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

func row(t *testing.T, wire string) *providers.Provider {
	t.Helper()
	doc := `{"providers":{"databricks-dev":{"preset":"databricks","host":"https://x.example.com","options":{"wire":"` + wire + `"}}}}`
	reg, _ := providers.Parse([]byte("{}"), []byte(doc))
	p := reg.Providers["databricks-dev"]
	if p == nil || p.BaseURL == "" {
		t.Fatal("row did not expand")
	}
	return p
}

func base(t *testing.T, target, wire string) Options {
	return Options{Target: target, Host: "https://x.example.com", P: row(t, wire), Auth: AuthHarness, Self: "/bin/cc"}
}

func TestClaudeServingHarness(t *testing.T) {
	r, err := Render(base(t, TargetClaude, "serving"))
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		APIKeyHelper string            `json:"apiKeyHelper"`
		Env          map[string]string `json:"env"`
	}
	if err := json.Unmarshal([]byte(r.Text), &d); err != nil {
		t.Fatal(err)
	}
	if d.APIKeyHelper != "/bin/cc token --provider databricks-dev" {
		t.Errorf("helper %q", d.APIKeyHelper)
	}
	if _, ok := d.Env["ANTHROPIC_AUTH_TOKEN"]; ok {
		t.Error("token slot present")
	}
	if d.Env["ANTHROPIC_BASE_URL"] != "https://x.example.com/serving-endpoints/anthropic" ||
		d.Env["CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS"] != "1" || d.Env["ANTHROPIC_DEFAULT_HAIKU_MODEL"] != "databricks-claude-haiku-5-5" {
		t.Errorf("env %v", d.Env)
	}
	if r.Note != "" || strings.Contains(r.Text, "example.cloud") {
		t.Error("serving must have no note / placeholder host")
	}
}

func TestClaudeGatewayCLISetUnset(t *testing.T) {
	o := base(t, TargetClaude, "gateway")
	o.Auth, o.Set, o.Unset = AuthCLI, []string{"FOO=1"}, []string{"ENABLE_TOOL_SEARCH"}
	r, err := Render(o)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"https://x.example.com/ai-gateway/anthropic", `"FOO": "1"`, "databricks auth token --host https://x.example.com",
		"system.ai.claude-opus-5-5", "x-databricks-use-coding-agent-mode: true", "CLAUDE_CODE_USE_GATEWAY"} {
		if !strings.Contains(r.Text, w) {
			t.Errorf("missing %q in\n%s", w, r.Text)
		}
	}
	if strings.Contains(r.Text, "ENABLE_TOOL_SEARCH") || r.Note != GatewayNote {
		t.Error("unset/note wrong")
	}
}

func TestCodex(t *testing.T) {
	o := base(t, TargetCodex, "serving")
	o.Set = []string{"model_reasoning_effort=high"}
	r, err := Render(o)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{`base_url = "https://x.example.com/serving-endpoints"`, `model = "databricks-gpt-5-5"`,
		`command = "/bin/cc"`, `args = ["token", "--provider", "databricks-dev"]`, `model_reasoning_effort = "high"`, "timeout_ms = 5000"} {
		if !strings.Contains(r.Text, w) {
			t.Errorf("missing %q in\n%s", w, r.Text)
		}
	}
	r, err = Render(base(t, TargetCodex, "gateway"))
	if err != nil || !strings.HasPrefix(r.Text, "# gateway") || !strings.Contains(r.Text, "https://x.example.com/ai-gateway/codex/v1") {
		t.Errorf("gateway %v:\n%s", err, r.Text)
	}
}

func TestRejects(t *testing.T) {
	for _, kv := range []string{"ANTHROPIC_AUTH_TOKEN=x", "OPENAI_API_KEY=x", "my_secret=1", "noequals"} {
		o := base(t, TargetClaude, "serving")
		o.Set = []string{kv}
		if _, err := Render(o); err == nil {
			t.Errorf("%q accepted", kv)
		}
	}
	o := base(t, TargetClaude, "serving")
	o.Model = "gpt-5.5"
	if _, err := Render(o); err == nil || !strings.Contains(err.Error(), "router") {
		t.Errorf("non-claude model for claude: %v", err)
	}
	c := base(t, TargetCodex, "serving")
	c.Model = "claude-opus-5-5"
	if _, err := Render(c); err == nil {
		t.Error("claude model for codex accepted")
	}
	o = base(t, TargetClaude, "serving")
	o.Host = "http://bad"
	if _, err := Render(o); err == nil {
		t.Error("bad host accepted")
	}
}

// TestCodexModelAndNameFollowWire: either wire's id is re-prefixed for the row's wire; the name says which wire.
func TestCodexModelAndNameFollowWire(t *testing.T) {
	cases := []struct{ wire, model, wantModel, wantName string }{
		{"serving", "system.ai.gpt-6-1-sol", `model = "databricks-gpt-6-1-sol"`, `name = "Databricks serving"`},
		{"gateway", "databricks-gpt-6-1-sol", `model = "system.ai.gpt-6-1-sol"`, `name = "Databricks AI Gateway"`},
	}
	for _, c := range cases {
		t.Run(c.wire, func(t *testing.T) {
			o := base(t, TargetCodex, c.wire)
			o.Model = c.model
			r, err := Render(o)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(r.Text, c.wantModel) || !strings.Contains(r.Text, c.wantName) {
				t.Errorf("want %s and %s in:\n%s", c.wantModel, c.wantName, r.Text)
			}
		})
	}
}
