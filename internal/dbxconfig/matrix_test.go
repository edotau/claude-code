package dbxconfig

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

var hostLike = regexp.MustCompile(`https?://[A-Za-z0-9.-]+`)

func TestTemplatesOnlyPlaceholderHost(t *testing.T) {
	for _, n := range []string{"claude.json", "codex.toml"} {
		b, err := tmplFS.ReadFile("templates/" + n)
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range hostLike.FindAllString(string(b), -1) {
			if h != PlaceholderHost {
				t.Errorf("%s: unexpected host %q", n, h)
			}
		}
		if strings.Contains(string(b), "eyJ") {
			t.Errorf("%s: jwt-like string", n)
		}
	}
}

// TestMatrix renders every target x wire x auth and checks the invariants each cell must hold.
func TestMatrix(t *testing.T) {
	const h = "https://x.example.com"
	for _, target := range []string{TargetClaude, TargetCodex} {
		for _, wire := range []string{"serving", "gateway"} {
			for _, auth := range []string{AuthHarness, AuthCLI} {
				o := base(t, target, wire)
				o.Auth = auth
				r, err := Render(o)
				name := target + "/" + wire + "/" + auth
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				if strings.Contains(r.Text, "ANTHROPIC_AUTH_TOKEN") || strings.Contains(r.Text, "eyJ") ||
					strings.Contains(r.Text, "your_token") || strings.Contains(r.Text, "example.cloud") {
					t.Errorf("%s: forbidden string in\n%s", name, r.Text)
				}
				wantURL := h + "/serving-endpoints"
				if wire == "gateway" {
					wantURL = h + "/ai-gateway"
				}
				if !strings.Contains(r.Text, wantURL) {
					t.Errorf("%s: missing %s", name, wantURL)
				}
				helper := "/bin/cc token --provider databricks-dev"
				if auth == AuthCLI {
					helper = "databricks auth token --host " + h + " --output json | jq -r '.access_token'"
				}
				if target == TargetClaude {
					var d struct {
						K   string            `json:"apiKeyHelper"`
						Env map[string]string `json:"env"`
					}
					if err := json.Unmarshal([]byte(r.Text), &d); err != nil {
						t.Fatalf("%s: %v", name, err)
					}
					if d.K != helper || d.Env["CLAUDE_CODE_API_KEY_HELPER_TTL_MS"] != "300000" ||
						d.Env["CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS"] != "1" {
						t.Errorf("%s: helper/env %q %v", name, d.K, d.Env)
					}
					if (d.Env["CLAUDE_CODE_USE_GATEWAY"] == "1") != (wire == "gateway") {
						t.Errorf("%s: gateway flag %v", name, d.Env)
					}
					continue
				}
				for _, w := range []string{`wire_api = "responses"`, "[model_providers.databricks.auth]", "refresh_interval_ms = 1800000"} {
					if !strings.Contains(r.Text, w) {
						t.Errorf("%s: missing %q", name, w)
					}
				}
				wantCmd := `command = "/bin/cc"`
				if auth == AuthCLI {
					wantCmd = `command = "sh"`
				}
				if !strings.Contains(r.Text, wantCmd) {
					t.Errorf("%s: missing %q", name, wantCmd)
				}
				checkTOML(t, name, r.Text)
			}
		}
	}
}

// checkTOML is a dependency-free sanity pass: every non-comment line is blank, a [table], or key = value.
func checkTOML(t *testing.T, name, text string) {
	t.Helper()
	for _, l := range strings.Split(text, "\n") {
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
	o := base(t, TargetClaude, "serving")
	o.Set = []string{"ANTHROPIC_MODEL=databricks-claude-haiku-5-5", "CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=0"}
	r, err := Render(o)
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
		if _, err := Render(o); err == nil {
			t.Errorf("credential-shaped %q accepted", k)
		}
	}
	o.Set = []string{"MAX_TOKENS=5"}
	if _, err := Render(o); err != nil {
		t.Errorf("MAX_TOKENS wrongly refused: %v", err)
	}
}

func TestCodexUnsetAndModel(t *testing.T) {
	o := base(t, TargetCodex, "serving")
	o.Unset = []string{"model_provider"}
	o.Model = "gpt-5.5"
	r, err := Render(o)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.Text, "model_provider =") || !strings.Contains(r.Text, `model = "databricks-gpt-5-5"`) {
		t.Errorf("unset/model:\n%s", r.Text)
	}
}
