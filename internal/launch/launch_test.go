package launch

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func vendorPATHFixture(t *testing.T, name, body string) string {
	t.Helper()
	cfg := os.Getenv("CLAUDE_CONFIG_DIR")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	bin := pathstest.FakeVendor(t, name, body)
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if name == "agy" {
		dst := filepath.Join(filepath.Dir(bin), "agy")
		if err := os.Rename(bin, dst); err != nil {
			t.Fatal(err)
		}
		bin = dst
	}
	return bin
}

// headless is the only launch that carries the setup-token: -p and --print, in any position.
func TestHeadless(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"-p", "hi"}, true},
		{[]string{"hi", "--print"}, true},
		{nil, false},
		{[]string{"-c"}, false},
		{[]string{"--settings", "x"}, false},
	} {
		if got := headless(tc.args); got != tc.want {
			t.Errorf("headless(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

// hermetic isolates config, pins and seams; userProviders is an optional vault overlay/providers document.
func hermetic(t *testing.T, userProviders string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	// Install writes completion and the wl-paste proxy under HOME/XDG; keep them out of the real home.
	home := filepath.Join(dir, "home")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("BASH_COMPLETION_USER_DIR", "")
	for _, k := range []string{providers.PinProvider, providers.PinModel, "HARNESS_OPUS_MODEL", "HARNESS_SONNET_MODEL",
		"HARNESS_HAIKU_MODEL", "HARNESS_FABLE_MODEL", "ANTHROPIC_API_KEY", "TEST_GW_KEY"} {
		t.Setenv(k, "")
	}
	if userProviders != "" {
		vaulttest.Put(t, vault.Overlay, "providers", userProviders)
	}
	userBin := filepath.Join(dir, "localbin")
	oldUserBin := userBinDir
	userBinDir = func() string { return userBin }
	t.Cleanup(func() { userBinDir = oldUserBin })
	oldTmux := tmuxConfPaths
	tmuxConfPaths = func() []string {
		return []string{filepath.Join(dir, ".tmux.conf"), filepath.Join(dir, "xdg", "tmux.conf")}
	}
	t.Cleanup(func() { tmuxConfPaths = oldTmux })
	oldFind, oldSelf, oldEnsure := findClaude, selfBinary, ensureRouter
	findClaude = func() (string, error) { return "/opt/claude", nil }
	selfBinary = func() (string, error) { return "/opt/bin/claude-code", nil }
	ensureRouter = func(context.Context) (string, error) { return "http://127.0.0.1:4000", nil }
	t.Cleanup(func() { findClaude, selfBinary, ensureRouter = oldFind, oldSelf, oldEnsure })
}

func TestParseArgs(t *testing.T) {
	o, rest, err := ParseArgs([]string{"--provider=openai", "-p", "hi", "--model", "gpt-5", "--router", "--print-env", "--", "--model", "x"}, true)
	if err != nil || o.Provider != "openai" || o.Model != "gpt-5" || !o.Router || !o.PrintEnv {
		t.Fatalf("%+v %v", o, err)
	}
	if !slices.Equal(rest, []string{"-p", "hi", "--model", "x"}) {
		t.Errorf("rest %q", rest)
	}
	if _, rest, _ := ParseArgs([]string{"--router"}, false); !slices.Equal(rest, []string{"--router"}) {
		t.Errorf("vendor launch must pass --router through: %q", rest)
	}
	if _, _, err := ParseArgs([]string{"--provider"}, true); err == nil {
		t.Error("missing value accepted")
	}
}

// --keep-env takes a comma list, repeatable, and accepts both the "--name value" and "--name=value" shapes.
func TestParseArgsKeepEnv(t *testing.T) {
	o, _, err := ParseArgs([]string{"--keep-env", "DATABRICKS_HOST,DATABRICKS_TOKEN", "--keep-env=HOME"}, true)
	if err != nil || !slices.Equal(o.KeepEnv, []string{"DATABRICKS_HOST", "DATABRICKS_TOKEN", "HOME"}) {
		t.Fatalf("KeepEnv = %q, err %v", o.KeepEnv, err)
	}
	if _, _, err := ParseArgs([]string{"--keep-env"}, true); err == nil {
		t.Error("missing value accepted")
	}
}

func TestClaudeDirect(t *testing.T) {
	hermetic(t, `{"fallback": [], "providers": {"gw": {"kind": "anthropic", "base_url": "https://gw.example/anthropic",
		"auth": {"type": "bearer", "env": "TEST_GW_KEY"}, "headers": {"X-B": "2", "X-A": "1"},
		"models": {"opus": "claude-opus-4-6", "haiku": "claude-haiku-4-5"}, "one_m": true}}}`)
	t.Setenv("TEST_GW_KEY", "secret-value")
	p, err := Claude(context.Background(), Options{Provider: "gw"}, []string{"-p", "hi"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"ANTHROPIC_BASE_URL":                    "https://gw.example/anthropic",
		"ANTHROPIC_MODEL":                       "claude-opus-4-6[1m]",
		"ANTHROPIC_DEFAULT_OPUS_MODEL":          "claude-opus-4-6[1m]",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":         "claude-haiku-4-5",
		"ANTHROPIC_CUSTOM_HEADERS":              "X-A: 1\nX-B: 2",
		"MAX_THINKING_TOKENS":                   "31999",
		"CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING": "1",
		"CLAUDE_CODE_API_KEY_HELPER_TTL_MS":     "300000",
	}
	for k, v := range want {
		if p.Set[k] != v {
			t.Errorf("%s = %q, want %q", k, p.Set[k], v)
		}
	}
	if !slices.Contains(p.Unset, oauthKey) {
		t.Error("API-key launch must wipe CLAUDE_CODE_OAUTH_TOKEN")
	}
	if !slices.Equal(p.Argv, []string{"--settings", p.OverlayPath, "-p", "hi"}) {
		t.Errorf("argv %q", p.Argv)
	}
	if !strings.Contains(string(p.Overlay), `"/opt/bin/claude-code token --provider gw"`) || strings.Contains(string(p.Overlay), "secret-value") {
		t.Errorf("overlay %s", p.Overlay)
	}
	env := p.Env([]string{"ANTHROPIC_AUTH_TOKEN=stale", "PATH=/bin", "ANTHROPIC_MODEL=old"})
	if slices.Contains(env, "ANTHROPIC_AUTH_TOKEN=stale") || slices.Contains(env, "ANTHROPIC_MODEL=old") || !slices.Contains(env, "PATH=/bin") {
		t.Errorf("env %q", env)
	}
}

func TestClaudeAnthropicDefaultOmitsBaseURL(t *testing.T) {
	hermetic(t, `{"fallback": []}`)
	p, err := Claude(context.Background(), Options{Provider: "anthropic"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Set["ANTHROPIC_BASE_URL"]; ok {
		t.Error("api.anthropic.com should not be pinned as ANTHROPIC_BASE_URL")
	}
	if len(p.Warnings) == 0 {
		t.Error("expected a missing-credential warning")
	}
}

func TestClaudePassthrough(t *testing.T) {
	hermetic(t, "")
	p, err := Claude(context.Background(), Options{Provider: "subscription"}, []string{"--resume"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Overlay) != 0 || slices.Contains(p.Argv, "--settings") {
		t.Errorf("passthrough must not set an apiKeyHelper: %q %s", p.Argv, p.Overlay)
	}
	// Interactive: the setup-token is withheld so Remote Control sees the /login credential.
	if !slices.Contains(p.Unset, oauthKey) || p.Set[oauthKey] != "" {
		t.Error("interactive passthrough must withhold CLAUDE_CODE_OAUTH_TOKEN")
	}
	t.Setenv(oauthKey, "sk-ant-oat-test")
	h, err := Claude(context.Background(), Options{Provider: "subscription"}, []string{"-p", "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(h.Unset, oauthKey) {
		t.Error("headless passthrough must keep CLAUDE_CODE_OAUTH_TOKEN")
	}
	// The built-in fallback does not route a login: a loopback base URL would disable Remote Control.
	if _, ok := p.Set["ANTHROPIC_BASE_URL"]; ok || p.Set["ANTHROPIC_CUSTOM_HEADERS"] != "" {
		t.Errorf("passthrough must launch direct: base %q headers %q", p.Set["ANTHROPIC_BASE_URL"], p.Set["ANTHROPIC_CUSTOM_HEADERS"])
	}
	// --router still forces it: the login stays in Authorization, the secret rides ClientHeader.
	r, err := Claude(context.Background(), Options{Provider: "subscription", Router: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Set["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:4000/p/subscription" || !strings.HasPrefix(r.Set["ANTHROPIC_CUSTOM_HEADERS"], router.ClientHeader+": ") {
		t.Errorf("routed passthrough: base %q headers %q", r.Set["ANTHROPIC_BASE_URL"], r.Set["ANTHROPIC_CUSTOM_HEADERS"])
	}
	if _, err := Claude(context.Background(), Options{Provider: "anthropic", Model: "subscription:claude-opus-5"}, nil); err == nil {
		t.Error("a passthrough slot target through the router must error")
	}
	if _, err := Claude(context.Background(), Options{Provider: "chatgpt"}, nil); err == nil || !strings.Contains(err.Error(), "run codex") {
		t.Errorf("a vendor-login provider must not back Claude Code: %v", err)
	}
}

// Print used to mask only known secret env keys and vendor headers; a static credential sitting inside
// a quoted codex -c TOML value or an opencode JSON blob rode straight through. redactQuoted now masks
// every credential-shaped quoted string in both.
func TestPrintRedactsQuotedSecrets(t *testing.T) {
	p := Plan{
		Binary: "codex",
		Argv:   []string{"-c", `p={ http_headers = { "cf-aig-authorization" = "Bearer sk-livegatewaysecret0123456789abcdefghijklmnop" } }`},
		Set: map[string]string{
			"OPENCODE_CONFIG_CONTENT": `{"provider":{"x":{"options":{"headers":{"h":"sk-abc123"}}}}}`,
			"ANTHROPIC_MODEL":         "gpt-x",
		},
	}
	var buf bytes.Buffer
	p.Print(&buf)
	out := buf.String()
	if strings.Contains(out, "sk-livegatewaysecret0123456789abcdefghijklmnop") {
		t.Errorf("argv secret leaked: %s", out)
	}
	if strings.Contains(out, "sk-abc123") {
		t.Errorf("OPENCODE_CONFIG_CONTENT secret leaked: %s", out)
	}
	if !strings.Contains(out, "<redacted>") {
		t.Errorf("expected a <redacted> marker: %s", out)
	}
	if !strings.Contains(out, "gpt-x") {
		t.Errorf("a non-secret value must survive redaction: %s", out)
	}
}

// redactQuoted must mask a credential-shaped quoted value, including one with an escaped quote inside
// it, and must leave an empty quoted string and a ${ENV}-style reference untouched.
func TestRedactQuotedTable(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"credential with escaped quote", `"sk-ab\"cdefghijklmnopqrstuvwxyz0123456789"`, `"<redacted>"`},
		{"empty quoted string untouched", `""`, `""`},
		{"env reference untouched", `"$HOME"`, `"$HOME"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactQuoted(tc.in); got != tc.want {
				t.Errorf("redactQuoted(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestClaudeRouter(t *testing.T) {
	hermetic(t, "")
	p, err := Claude(context.Background(), Options{Provider: "ollama"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Set["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:4000/p/ollama" || p.Set["ANTHROPIC_MODEL"] != "qwen3-coder" {
		t.Errorf("set %v", p.Set)
	}
	if !strings.Contains(string(p.Overlay), "claude-code token --router") {
		t.Errorf("overlay %s", p.Overlay)
	}
	if _, ok := p.Set["ANTHROPIC_CUSTOM_HEADERS"]; ok {
		t.Error("router launch must not carry provider headers")
	}
	p, err = Claude(context.Background(), Options{Provider: "anthropic", Model: "openai:gpt-5"}, nil)
	if err != nil || p.Set["ANTHROPIC_MODEL"] != "openai:gpt-5" || !strings.HasSuffix(p.Set["ANTHROPIC_BASE_URL"], "/p/anthropic") {
		t.Errorf("cross-provider --model: %v %v", p.Set, err)
	}
}
