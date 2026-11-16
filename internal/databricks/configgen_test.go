package databricks

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

func row(t *testing.T) *providers.Provider {
	t.Helper()
	doc := `{"providers":{"databricks":{"databricks-dev":{"preset":"databricks","host":"https://x.example.com"}}}}`
	reg, _ := providers.Parse([]byte("{}"), []byte(doc))
	p := reg.Providers["databricks-dev"]
	if p == nil || p.BaseURL == "" {
		t.Fatal("row did not expand")
	}
	return p
}

func base(t *testing.T, target string) renderOptions {
	return renderOptions{Target: target, Host: "https://x.example.com", P: row(t), Auth: authHarness, Self: "/bin/cc"}
}

func TestClaudeServingHarness(t *testing.T) {
	r, err := renderConfig(base(t, targetClaude))
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
	checkServingEnv(t, d.Env)
	if strings.Contains(r.Text, "example.cloud") {
		t.Error("serving must have no placeholder host")
	}
}

func checkServingEnv(t *testing.T, env map[string]string) {
	t.Helper()
	if _, ok := env["ANTHROPIC_AUTH_TOKEN"]; ok {
		t.Error("token slot present")
	}
	if m, ok := env["ANTHROPIC_MODEL"]; ok {
		t.Errorf("model %q set without --model", m)
	}
	if env["ANTHROPIC_BASE_URL"] != "https://x.example.com/serving-endpoints/anthropic" ||
		env["CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS"] != "1" || env["ANTHROPIC_DEFAULT_HAIKU_MODEL"] != "databricks-claude-haiku-5-5" {
		t.Errorf("env %v", env)
	}
}

func TestClaudeCLISetUnset(t *testing.T) {
	o := base(t, targetClaude)
	o.Auth, o.Set, o.Unset = authCLI, []string{"FOO=1"}, []string{"ENABLE_TOOL_SEARCH"}
	r, err := renderConfig(o)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"https://x.example.com/serving-endpoints/anthropic", `"FOO": "1"`, "databricks auth token --host https://x.example.com",
		"databricks-claude-opus-5-5"} {
		if !strings.Contains(r.Text, w) {
			t.Errorf("missing %q in\n%s", w, r.Text)
		}
	}
	if strings.Contains(r.Text, "ENABLE_TOOL_SEARCH") {
		t.Error("unset ignored")
	}
}

func TestCodex(t *testing.T) {
	o := base(t, targetCodex)
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
}

func TestRejects(t *testing.T) {
	for _, kv := range []string{"OPENAI_API_KEY=x", "noequals"} {
		o := base(t, targetClaude)
		o.Set = []string{kv}
		if _, err := renderConfig(o); err == nil {
			t.Errorf("%q accepted", kv)
		}
	}
	o := base(t, targetClaude)
	o.Model = "gpt-5.5"
	if _, err := renderConfig(o); err == nil || !strings.Contains(err.Error(), "router") {
		t.Errorf("non-claude model for claude: %v", err)
	}
	c := base(t, targetCodex)
	c.Model = "claude-opus-5-5"
	if _, err := renderConfig(c); err == nil {
		t.Error("claude model for codex accepted")
	}
	o = base(t, targetClaude)
	o.Host = "http://bad"
	if _, err := renderConfig(o); err == nil {
		t.Error("bad host accepted")
	}
}

// TestCodexModelAndName: a serving id is kept as the row spells it and the provider name says serving.
func TestCodexModelAndName(t *testing.T) {
	o := base(t, targetCodex)
	o.Model = "databricks-gpt-6-1-sol"
	r, err := renderConfig(o)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{`model = "databricks-gpt-6-1-sol"`, `name = "Databricks serving"`} {
		if !strings.Contains(r.Text, w) {
			t.Errorf("want %s in:\n%s", w, r.Text)
		}
	}
}

// An unlisted id keeps its prefix, codex refuses a chat-only catalog id, and claude seeds every alias from the row.
func TestCodexKeepsCustomIDAndRefusesChatOnly(t *testing.T) {
	o := base(t, targetCodex)
	o.Model = "databricks-my-custom-gpt"
	if r, err := renderConfig(o); err != nil || !strings.Contains(r.Text, `model = "databricks-my-custom-gpt"`) {
		t.Errorf("custom id: err=%v\n%s", err, r.Text)
	}
	o.Model = "databricks-gpt-oss-120b"
	if _, err := renderConfig(o); err == nil || !strings.Contains(err.Error(), "Responses") {
		t.Errorf("gpt-oss for codex: %v", err)
	}
}

func TestClaudeSeedsEveryAliasFromRow(t *testing.T) {
	o := base(t, targetClaude)
	o.P.Models["sonnet"] = "databricks-claude-sonnet-5"
	r, err := renderConfig(o)
	if err != nil {
		t.Fatal(err)
	}
	var d struct{ Env map[string]string }
	if err := json.Unmarshal([]byte(r.Text), &d); err != nil {
		t.Fatal(err)
	}
	if d.Env["ANTHROPIC_DEFAULT_SONNET_MODEL"] != "databricks-claude-sonnet-5" || d.Env["ANTHROPIC_DEFAULT_FABLE_MODEL"] == "" {
		t.Errorf("alias env %v", d.Env)
	}
}

// TestClaudeServingKeeps1M pins that the serving endpoint serves 1M, so CLAUDE_CODE_DISABLE_1M_CONTEXT stays unset.
func TestClaudeServingKeeps1M(t *testing.T) {
	r, err := renderConfig(base(t, targetClaude))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.Text, "CLAUDE_CODE_DISABLE_1M_CONTEXT") {
		t.Errorf("serving row disables 1M:\n%s", r.Text)
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
		if strings.Contains(string(b), "eyJ") {
			t.Errorf("%s: jwt-like string", n)
		}
	}
}

// TestMatrix renders every target x auth and checks the invariants each cell must hold.
func TestMatrix(t *testing.T) {
	for _, target := range []string{targetClaude, targetCodex} {
		for _, auth := range []string{authHarness, authCLI} {
			name := target + "/" + auth
			t.Run(name, func(t *testing.T) {
				o := base(t, target)
				o.Auth = auth
				r, err := renderConfig(o)
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				checkMatrixCommon(t, name, r.Text)
				if target == targetClaude {
					checkMatrixClaude(t, name, r.Text, auth)
					return
				}
				checkMatrixCodex(t, name, r.Text, auth)
			})
		}
	}
}

const matrixHost = "https://x.example.com"

// checkMatrixCommon holds the invariants every cell shares: no token-like text and the serving route.
func checkMatrixCommon(t *testing.T, name, text string) {
	t.Helper()
	if strings.Contains(text, "ANTHROPIC_AUTH_TOKEN") || strings.Contains(text, "eyJ") ||
		strings.Contains(text, "your_token") || strings.Contains(text, "example.cloud") {
		t.Errorf("%s: forbidden string in\n%s", name, text)
	}
	wantURL := matrixHost + "/serving-endpoints"
	if !strings.Contains(text, wantURL) {
		t.Errorf("%s: missing %s", name, wantURL)
	}
}

func checkMatrixClaude(t *testing.T, name, text, auth string) {
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
}

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
	o := base(t, targetClaude)
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
	for _, k := range []string{"ANTHROPIC_AUTH_TOKEN", "anthropic_auth_token", "X-API-KEY", "db.password", "CLIENT_SECRET"} {
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
	o := base(t, targetCodex)
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

// Provider env and headers are refused when a key or value is credential-shaped, as --set is.
func TestClaudeRefusesCredentialProviderEnvAndHeaderValues(t *testing.T) {
	for _, set := range []func(*providers.Provider){
		func(p *providers.Provider) { p.Env = map[string]string{"ANTHROPIC_API_KEY": "sk-abc"} },
		func(p *providers.Provider) { p.Env = map[string]string{"NOTE": "Bearer eyJabc"} },
		func(p *providers.Provider) { p.Headers = map[string]string{"X-Custom": "Bearer eyJabc"} },
	} {
		o := base(t, targetClaude)
		set(o.P)
		if _, err := renderConfig(o); err == nil {
			t.Errorf("credential-shaped provider entry rendered: env=%v headers=%v", o.P.Env, o.P.Headers)
		}
	}
}
