package databricks

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

func row(t *testing.T, wire string) *providers.Provider {
	t.Helper()
	doc := `{"providers":{"databricks":{"databricks-dev":{"preset":"databricks","host":"https://x.example.com","options":{"wire":"` + wire + `"}}}}}`
	reg, _ := providers.Parse([]byte("{}"), []byte(doc))
	p := reg.Providers["databricks-dev"]
	if p == nil || p.BaseURL == "" {
		t.Fatal("row did not expand")
	}
	return p
}

func base(t *testing.T, target, wire string) renderOptions {
	return renderOptions{Target: target, Host: "https://x.example.com", P: row(t, wire), Auth: authHarness, Self: "/bin/cc"}
}

func TestClaudeServingHarness(t *testing.T) {
	r, err := renderConfig(base(t, targetClaude, "serving"))
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
	if d.APIKeyHelper != "/bin/cc resolve --profile databricks-dev" {
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
	o := base(t, targetClaude, "ai-gateway")
	o.Auth, o.Set, o.Unset = authCLI, []string{"FOO=1"}, []string{"ENABLE_TOOL_SEARCH"}
	r, err := renderConfig(o)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"https://x.example.com/ai-gateway/anthropic", `"FOO": "1"`, "databricks auth token --host https://x.example.com",
		"system.ai.claude-opus-5-5", "x-databricks-use-coding-agent-mode: true", "CLAUDE_CODE_USE_GATEWAY"} {
		if !strings.Contains(r.Text, w) {
			t.Errorf("missing %q in\n%s", w, r.Text)
		}
	}
	if strings.Contains(r.Text, "ENABLE_TOOL_SEARCH") || r.Note != gatewayNote {
		t.Error("unset/note wrong")
	}
}

func TestCodex(t *testing.T) {
	o := base(t, targetCodex, "serving")
	o.Set = []string{"model_reasoning_effort=high"}
	r, err := renderConfig(o)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{`base_url = "https://x.example.com/serving-endpoints"`, `model = "databricks-gpt-6-1-sol"`,
		`command = "/bin/cc"`, `args = ["resolve", "--profile", "databricks-dev"]`, `model_reasoning_effort = "high"`, "timeout_ms = 5000"} {
		if !strings.Contains(r.Text, w) {
			t.Errorf("missing %q in\n%s", w, r.Text)
		}
	}
	r, err = renderConfig(base(t, targetCodex, "ai-gateway"))
	if err != nil || !strings.HasPrefix(r.Text, "# gateway") || !strings.Contains(r.Text, "https://x.example.com/ai-gateway/codex/v1") {
		t.Errorf("gateway %v:\n%s", err, r.Text)
	}
}

func TestRejects(t *testing.T) {
	for _, kv := range []string{"ANTHROPIC_AUTH_TOKEN=x", "OPENAI_API_KEY=x", "my_secret=1", "noequals"} {
		o := base(t, targetClaude, "serving")
		o.Set = []string{kv}
		if _, err := renderConfig(o); err == nil {
			t.Errorf("%q accepted", kv)
		}
	}
	o := base(t, targetClaude, "serving")
	o.Model = "gpt-5.5"
	if _, err := renderConfig(o); err == nil || !strings.Contains(err.Error(), "router") {
		t.Errorf("non-claude model for claude: %v", err)
	}
	c := base(t, targetCodex, "serving")
	c.Model = "claude-opus-5-5"
	if _, err := renderConfig(c); err == nil {
		t.Error("claude model for codex accepted")
	}
	o = base(t, targetClaude, "serving")
	o.Host = "http://bad"
	if _, err := renderConfig(o); err == nil {
		t.Error("bad host accepted")
	}
}

// TestCodexModelAndNameFollowWire: either wire's id is re-prefixed for the row's wire; the name says which wire.
func TestCodexModelAndNameFollowWire(t *testing.T) {
	cases := []struct{ wire, model, wantModel, wantName string }{
		{"serving", "system.ai.gpt-6-1-sol", `model = "databricks-gpt-6-1-sol"`, `name = "Databricks serving"`},
		{"ai-gateway", "databricks-gpt-6-1-sol", `model = "system.ai.gpt-6-1-sol"`, `name = "Databricks AI Gateway"`},
	}
	for _, c := range cases {
		t.Run(c.wire, func(t *testing.T) {
			o := base(t, targetCodex, c.wire)
			o.Model = c.model
			r, err := renderConfig(o)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(r.Text, c.wantModel) || !strings.Contains(r.Text, c.wantName) {
				t.Errorf("want %s and %s in:\n%s", c.wantModel, c.wantName, r.Text)
			}
		})
	}
}

// TestClaudeDisable1MFollowsEndpoint pins that an endpoint without 1M gets CLAUDE_CODE_DISABLE_1M_CONTEXT and serving does not.
func TestClaudeDisable1MFollowsEndpoint(t *testing.T) {
	for wire, want := range map[string]bool{"ai-gateway": true, "serving": false} {
		r, err := renderConfig(base(t, targetClaude, wire))
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(r.Text, `"CLAUDE_CODE_DISABLE_1M_CONTEXT": "1"`); got != want {
			t.Errorf("%s: DISABLE_1M present=%v, want %v\n%s", wire, got, want, r.Text)
		}
	}
}

var hostLike = regexp.MustCompile(`https?://[A-Za-z0-9.-]+`)

func TestTemplatesOnlyPlaceholderHost(t *testing.T) {
	for _, n := range []string{"claude.json", "codex.toml"} {
		b, err := tmplFS.ReadFile("templates/" + n)
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range hostLike.FindAllString(string(b), -1) {
			if h != placeholderHost {
				t.Errorf("%s: unexpected host %q", n, h)
			}
		}
		if strings.Contains(string(b), "eyJ") || strings.Contains(string(b), "/ai-gateway/") {
			t.Errorf("%s: jwt-like string or an AI Gateway path", n)
		}
	}
}

// TestMatrix renders every target x wire x auth and checks the invariants each cell must hold.
func TestMatrix(t *testing.T) {
	for _, target := range []string{targetClaude, targetCodex} {
		for _, wire := range []string{"serving", "ai-gateway"} {
			for _, auth := range []string{authHarness, authCLI} {
				name := target + "/" + wire + "/" + auth
				t.Run(name, func(t *testing.T) {
					o := base(t, target, wire)
					o.Auth = auth
					r, err := renderConfig(o)
					if err != nil {
						t.Fatalf("%s: %v", name, err)
					}
					checkMatrixCommon(t, name, r.Text, wire)
					if target == targetClaude {
						checkMatrixClaude(t, name, r.Text, wire, auth)
						return
					}
					checkMatrixCodex(t, name, r.Text, auth)
				})
			}
		}
	}
}

const matrixHost = "https://x.example.com"

// checkMatrixCommon holds the invariants every cell shares: no token-like text and the wire's route.
func checkMatrixCommon(t *testing.T, name, text, wire string) {
	t.Helper()
	if strings.Contains(text, "ANTHROPIC_AUTH_TOKEN") || strings.Contains(text, "eyJ") ||
		strings.Contains(text, "your_token") || strings.Contains(text, "example.cloud") {
		t.Errorf("%s: forbidden string in\n%s", name, text)
	}
	wantURL := matrixHost + "/serving-endpoints"
	if wire == "ai-gateway" {
		wantURL = matrixHost + "/ai-gateway"
	} else if strings.Contains(text, "/ai-gateway/") {
		t.Errorf("%s: a serving row renders an AI Gateway path:\n%s", name, text)
	}
	if !strings.Contains(text, wantURL) {
		t.Errorf("%s: missing %s", name, wantURL)
	}
}

// checkMatrixClaude checks the claude cell's key helper and env flags.
func checkMatrixClaude(t *testing.T, name, text, wire, auth string) {
	t.Helper()
	helper := "/bin/cc resolve --profile databricks-dev"
	if auth == authCLI {
		helper = "databricks auth token --host " + matrixHost + " --output json | jq -r '.access_token'"
	}
	var d struct {
		K   string            `json:"apiKeyHelper"`
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal([]byte(text), &d); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if d.K != helper || d.Env["CLAUDE_CODE_API_KEY_HELPER_TTL_MS"] != "300000" ||
		d.Env["CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS"] != "1" {
		t.Errorf("%s: helper/env %q %v", name, d.K, d.Env)
	}
	if (d.Env["CLAUDE_CODE_USE_GATEWAY"] == "1") != (wire == "ai-gateway") {
		t.Errorf("%s: gateway flag %v", name, d.Env)
	}
}

// checkMatrixCodex checks the codex cell's required TOML lines and command.
func checkMatrixCodex(t *testing.T, name, text, auth string) {
	t.Helper()
	for _, w := range []string{`wire_api = "responses"`, "[model_providers.databricks.auth]", "refresh_interval_ms = 1800000"} {
		if !strings.Contains(text, w) {
			t.Errorf("%s: missing %q", name, w)
		}
	}
	wantCmd := `command = "/bin/cc"`
	if auth == authCLI {
		wantCmd = `command = "sh"`
	}
	if !strings.Contains(text, wantCmd) {
		t.Errorf("%s: missing %q", name, wantCmd)
	}
	checkTOML(t, name, text)
}

// checkTOML is a dependency-free sanity pass: every non-comment line is blank, a [table], or key = value.
func checkTOML(t *testing.T, name, text string) {
	t.Helper()
	for l := range strings.SplitSeq(text, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if strings.HasPrefix(l, "[") {
			if !strings.HasSuffix(l, "]") {
				t.Errorf("%s: bad table %q", name, l)
			}
			continue
		}
		if k, v, ok := strings.Cut(l, "="); !ok || strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			t.Errorf("%s: bad line %q", name, l)
		}
	}
}

func TestSetOverridesAndCredentialRefusal(t *testing.T) {
	o := base(t, targetClaude, "serving")
	o.Set = []string{"ANTHROPIC_MODEL=databricks-claude-haiku-5-5", "CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=0"}
	r, err := renderConfig(o)
	if err != nil {
		t.Fatal(err)
	}
	var d struct{ Env map[string]string }
	_ = json.Unmarshal([]byte(r.Text), &d)
	if d.Env["ANTHROPIC_MODEL"] != "databricks-claude-haiku-5-5" || d.Env["CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS"] != "0" {
		t.Errorf("--set not applied: %v", d.Env)
	}
	for _, k := range []string{"ANTHROPIC_AUTH_TOKEN", "anthropic_auth_token", "X-API-KEY", "db.password", "CLIENT_SECRET", "apiKey", "MY_PAT", "ANTHROPIC_BEARER"} {
		o.Set = []string{k + "=v"}
		if _, err := renderConfig(o); err == nil {
			t.Errorf("credential-shaped %q accepted", k)
		}
	}
	o.Set = []string{"MAX_TOKENS=5"}
	if _, err := renderConfig(o); err != nil {
		t.Errorf("MAX_TOKENS wrongly refused: %v", err)
	}
}

func TestCodexUnsetAndModel(t *testing.T) {
	o := base(t, targetCodex, "serving")
	o.Unset = []string{"model_provider"}
	o.Model = "gpt-5.5"
	r, err := renderConfig(o)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.Text, "model_provider =") || !strings.Contains(r.Text, `model = "databricks-gpt-5-5"`) {
		t.Errorf("unset/model:\n%s", r.Text)
	}
}
