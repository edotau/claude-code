package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestCopilotLogin(t *testing.T) {
	tg := target(t, "personal", "claude-opus-5.5")
	env, argv, err := schemaFor("copilot").configureHeadless(context.Background(), tg, Headless{Prompt: "hi", Tools: true})
	if err != nil {
		t.Fatal(err)
	}
	got := envMap(env)
	for k, v := range map[string]string{"COPILOT_MODEL": "claude-opus-5.5", "COPILOT_PROVIDER_BASE_URL": "", "COPILOT_OFFLINE": ""} {
		if w, ok := got[k]; !ok || w != v {
			t.Errorf("env %s = %q (set %v), want %q", k, w, ok, v)
		}
	}
	if want := []string{"--prompt", "hi", "--allow-all-tools"}; !slices.Equal(argv, want) {
		t.Errorf("argv %q, want %q", argv, want)
	}
	if _, _, err := schemaFor("codex").Configure(context.Background(), tg, ""); err == nil || !strings.Contains(err.Error(), "copilot sign-in") {
		t.Errorf("codex must refuse the copilot login: %v", err)
	}
}

// copilotPinArgv isolation: no gh/token env so CopilotLaunchToken (called unconditionally inside
// copilotLogin) resolves to "" instead of touching a real GitHub session.
func noCopilotLogin(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	t.Setenv("COPILOT_HOME", t.TempDir())
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
	for _, k := range []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		t.Setenv(k, "")
	}
}

// Interactive copilot (h == nil, own GitHub-login route): the configure page's pins ride argv as
// --reasoning-effort/--context when set, and neither flag appears when both pins are unset.
func TestCopilotLoginArgvCarriesPinsWhenSetOmitsWhenUnset(t *testing.T) {
	tg := target(t, "personal", "claude-opus-5.5")
	noCopilotLogin(t)
	for _, k := range []string{"COPILOT_EFFORT", "COPILOT_CONTEXT"} {
		t.Setenv(k, "")
	}

	_, argv, err := schemaFor("copilot").Configure(context.Background(), tg, "")
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(argv, "--reasoning-effort") || slices.Contains(argv, "--context") {
		t.Errorf("argv %q with no pins set, want neither flag", argv)
	}

	vaulttest.SetRecord(t, "copilot", "effort", "xhigh")
	vaulttest.SetRecord(t, "copilot", "context", "long_context")
	_, argv, err = schemaFor("copilot").Configure(context.Background(), tg, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--reasoning-effort", "xhigh", "--context", "long_context", "--allow-all"}
	if !slices.Equal(argv, want) {
		t.Errorf("argv = %q, want %q", argv, want)
	}
}

// Interactive copilot (h == nil, BYOK/gateway route): same pin-to-argv wiring as the own-login route.
func TestCopilotBYOKArgvCarriesPinsWhenSet(t *testing.T) {
	tg := target(t, "openai", "gpt-5")
	vaulttest.SetRecord(t, "copilot", "effort", "low")
	vaulttest.SetRecord(t, "copilot", "context", "default")

	_, argv, err := schemaFor("copilot").Configure(context.Background(), tg, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--reasoning-effort", "low", "--context", "default", "--allow-all"}
	if !slices.Equal(argv, want) {
		t.Errorf("argv = %q, want %q", argv, want)
	}
}

// ConfigureHeadless (h != nil) ignores the pins: copilot always runs headless through copilotEngine.Run's
// own SDK options, never this argv path, so copilotHeadless must stay untouched by the pins.
func TestCopilotHeadlessArgvIgnoresPins(t *testing.T) {
	tg := target(t, "personal", "claude-opus-5.5")
	noCopilotLogin(t)
	vaulttest.SetRecord(t, "copilot", "effort", "max")
	vaulttest.SetRecord(t, "copilot", "context", "long_context")

	_, argv, err := schemaFor("copilot").configureHeadless(context.Background(), tg, Headless{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"--prompt", "hi"}; !slices.Equal(argv, want) {
		t.Errorf("argv = %q, want %q (pins must not leak into the headless SDK path)", argv, want)
	}
}

// The registry's "auto" slot must not override copilot's own default model: COPILOT_MODEL goes out blank.
func TestCopilotLoginAutoDefersToCopilotDefault(t *testing.T) {
	env, _, err := schemaFor("copilot").Configure(context.Background(), target(t, "personal", "auto"), "")
	if err != nil {
		t.Fatal(err)
	}
	if w, ok := envMap(env)["COPILOT_MODEL"]; !ok || w != "" {
		t.Errorf("COPILOT_MODEL = %q (set %v), want blank", w, ok)
	}
}

// copilot's own token precedence: COPILOT_GITHUB_TOKEN, GH_TOKEN, GITHUB_TOKEN, then an authenticated gh.
func TestCopilotLoginPresentHonoursEveryTokenVar(t *testing.T) {
	t.Setenv("PATH", t.TempDir())              // no gh on PATH
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // CLAUDE_CONFIG_DIR isolated so nothing real leaks in
	t.Setenv("COPILOT_HOME", t.TempDir())      // no real copilot config.json: CopilotStoredLogin reads nothing
	t.Setenv("GH_CONFIG_DIR", t.TempDir())     // no real gh hosts.yml: GHActiveAccount reads nothing
	for _, k := range []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		t.Setenv(k, "")
	}
	if CopilotLoginPresent() {
		t.Fatal("no token and no gh must read as not logged in")
	}
	for _, k := range []string{"COPILOT_GITHUB_TOKEN", "GITHUB_TOKEN"} {
		t.Setenv(k, "tok")
		if !CopilotLoginPresent() {
			t.Errorf("%s must count as a copilot login", k)
		}
		t.Setenv(k, "")
	}
}

// BYOK on copilot: headers ride COPILOT_PROVIDER_HEADERS, a bearer route sets only the bearer var, and the
// prompt budget leaves room for the output cap.
func TestCopilotBYOKHeadersBearerAndBudget(t *testing.T) {
	tg := target(t, "openai", "gpt-5")
	tg.Provider.Headers = map[string]string{"X-Tenant": "t1", "Authorization-Extra": "z"}
	env, _, err := schemaFor("copilot").Configure(WithDirect(context.Background()), tg, "")
	if err != nil {
		t.Fatal(err)
	}
	got := envMap(env)
	if got["COPILOT_PROVIDER_HEADERS"] != "Authorization-Extra: z\nX-Tenant: t1" {
		t.Errorf("headers = %q", got["COPILOT_PROVIDER_HEADERS"])
	}
	if got["COPILOT_PROVIDER_API_KEY"] != "" || got["COPILOT_PROVIDER_BEARER_TOKEN"] == "" {
		t.Errorf("bearer route must set only the bearer var: key=%q bearer=%q", got["COPILOT_PROVIDER_API_KEY"], got["COPILOT_PROVIDER_BEARER_TOKEN"])
	}
	if v, ok := got["COPILOT_PROVIDER_MAX_PROMPT_TOKENS"]; ok && v == strconv.Itoa(models.ContextWindow("gpt-5")) {
		t.Errorf("prompt budget must leave room for %d output tokens, got %s", copilotMaxOutputTokens, v)
	}
}

// ValidCopilotToken: every documented GitHub token prefix passes at length, a short or wrongly-prefixed
// or whitespace-mangled value does not.
func TestValidCopilotToken(t *testing.T) {
	cases := []struct {
		name string
		tok  string
		want bool
	}{
		{"gho_ prefix", "gh" + "o_abcdefghijklmnopqrstuvwxyz", true},
		{"ghp_ prefix", "gh" + "p_abcdefghijklmnopqrstuvwxyz", true},
		{"ghu_ prefix", "gh" + "u_abcdefghijklmnopqrstuvwxyz", true},
		{"github_pat_ prefix", "github_" + "pat_abcdefghijklmnop", true},
		{"too short with valid prefix", "ghp_short", false},
		{"wrong prefix", "sk-abcdefghijklmnopqrstuvwxyz", false},
		{"no prefix at all", "abcdefghijklmnopqrstuvwxyz1234", false},
		{"empty", "", false},
		{"leading whitespace defeats the prefix check", " ghp_abcdefghijklmnopqrstuvwxyz", false},
		{"trailing whitespace still counts toward length but prefix matches", "gh" + "p_abcdefghijklmnopqrstuvwxyz ", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ValidCopilotToken(c.tok); got != c.want {
				t.Errorf("ValidCopilotToken(%q) = %v, want %v", c.tok, got, c.want)
			}
		})
	}
}

// copilotAuthEnv isolates config/home/gh/keyring so copilotAuthSource reads nothing real; the fake `gh` on
// PATH reports an active account for hosts.yml but is never `security`/`secret-tool`, and the vault key comes
// from HARNESS_VAULT_KEY with an in-memory keyring behind it.
func copilotAuthEnv(t *testing.T) {
	t.Helper()
	vaulttest.Use(t)
	vaulttest.Keyring(t)
	t.Setenv("COPILOT_HOME", t.TempDir())
	for _, k := range []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		t.Setenv(k, "")
	}
	ghDir := t.TempDir()
	t.Setenv("GH_CONFIG_DIR", ghDir)
	hosts := "github.com:\n    user: alice\n    users:\n        alice:\n            oauth_token: tok\n"
	if err := os.WriteFile(filepath.Join(ghDir, "hosts.yml"), []byte(hosts), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	gh := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(gh), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin) // gh present, but no `security`/`secret-tool`: vault.KeyringAvailable() is false
}

// A stored COPILOT_GITHUB_TOKEN (vault) outranks gh's active account.
func TestCopilotAuthSourceStoredTokenOutranksActiveAccount(t *testing.T) {
	copilotAuthEnv(t)
	if _, _, ok := secrets.Get(CopilotTokenEnv); ok {
		t.Fatal("precondition: no stored token yet")
	}
	a, ok := copilotAuthSource()
	if !ok || a.Source != srcActive || a.Account != "alice" {
		t.Fatalf("with no stored token: got %+v, want gh's active account alice", a)
	}
	if err := secrets.Store(CopilotTokenEnv, "stored-tok"); err != nil {
		t.Fatal(err)
	}
	if v, src, ok := secrets.Get(CopilotTokenEnv); !ok || src != "vault" || v != "stored-tok" {
		t.Fatalf("secrets.Get(%s) = (%q, %q, %v), want (stored-tok, vault, true)", CopilotTokenEnv, v, src, ok)
	}
	a, ok = copilotAuthSource()
	if !ok || a.Source != SrcStoredToken || a.token != "stored-tok" {
		t.Fatalf("with a stored token: got %+v, want Source=%s token=stored-tok", a, SrcStoredToken)
	}
}

// A pinned gh user (COPILOT_PROFILE's login) still outranks a stored token.
func TestCopilotAuthSourcePinOutranksStoredToken(t *testing.T) {
	copilotAuthEnv(t)
	if err := secrets.Store(CopilotTokenEnv, "stored-tok"); err != nil {
		t.Fatal(err)
	}
	pinCopilotLogin(t, "alice")
	a, ok := copilotAuthSource()
	if !ok || a.Source != srcPinned || a.Account != "alice" {
		t.Fatalf("a pin must outrank a stored token: got %+v", a)
	}
}

// The same token present only as a process env var (never stored) must not take the stored-token path:
// it falls through to copilot's own env-precedence order instead.
func TestCopilotAuthSourceEnvOnlyTokenIsNotStored(t *testing.T) {
	copilotAuthEnv(t)
	t.Setenv(CopilotTokenEnv, "env-tok")
	if _, src, ok := secrets.Get(CopilotTokenEnv); !ok || src != "env" {
		t.Fatalf("secrets.Get(%s) = (_, %q, %v), want source env", CopilotTokenEnv, src, ok)
	}
	a, ok := copilotAuthSource()
	if !ok || a.Source == SrcStoredToken {
		t.Fatalf("an env-only token must not resolve as %s: got %+v", SrcStoredToken, a)
	}
	// gh's active account still outranks copilot's own env-var order (srcActive beats CopilotTokenEnv here).
	if a.Source != srcActive {
		t.Fatalf("got %+v, want gh's active account to win over an env-only token", a)
	}
}

// Catalog limits only lower the gated window and the output cap; unknown models keep today's values.
func TestCapsLowerOnly(t *testing.T) {
	for _, tc := range []struct {
		id             string
		window         int
		wantCtx, wantO int
	}{
		{"databricks-claude-sonnet-5-5", 200_000, 200_000, copilotMaxOutputTokens},
		{"claude-opus-5-5", 2_000_000, 1_000_000, copilotMaxOutputTokens},
		{"databricks-gpt-5", 200_000, 200_000, copilotMaxOutputTokens},
		{"not-a-model", 50_000, 50_000, copilotMaxOutputTokens},
	} {
		if ctx, out := copilotCaps(tc.window, tc.id); ctx != tc.wantCtx || out != tc.wantO {
			t.Errorf("copilotCaps(%d, %q) = %d, %d; want %d, %d", tc.window, tc.id, ctx, out, tc.wantCtx, tc.wantO)
		}
	}
}

// A pinned gh user that is not logged in fails closed: no stored token, no active account, a named fix.
func TestCopilotPinMissingFailsClosed(t *testing.T) {
	copilotAuthEnv(t)
	if err := secrets.Store(CopilotTokenEnv, "stored-tok"); err != nil {
		t.Fatal(err)
	}
	pinCopilotLogin(t, "ghost")
	if a, ok := copilotAuthSource(); ok {
		t.Fatalf("a missing pinned user must not resolve another credential: got %+v", a)
	}
	tok, err := CopilotLaunchToken()
	if err == nil || tok != "" || !strings.Contains(err.Error(), "claude-code configure github") || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("CopilotLaunchToken = (%q, %v), want an error naming ghost and the fix", tok, err)
	}
	if _, _, err := copilotLogin("auto", nil); err == nil {
		t.Error("the launch env must fail too, not run copilot on its own stored login")
	}
}

// Unpinned behaviour is unchanged: gh's active account resolves and the launch token has no error.
func TestCopilotUnpinnedUnchanged(t *testing.T) {
	copilotAuthEnv(t)
	if err := copilotPinError(); err != nil {
		t.Fatalf("no pin must mean no pin error: %v", err)
	}
	if a, ok := copilotAuthSource(); !ok || a.Source != srcActive || a.Account != "alice" {
		t.Fatalf("unpinned: got %+v, want gh's active account alice", a)
	}
}

// Non-Claude models on a routed Databricks row ride the router's openai|responses passthrough; Claude stays anthropic.
func TestCopilotNonClaudeWire(t *testing.T) {
	vaulttest.Use(t)
	if err := providers.SetOpencodeMenu([]providers.OpencodeModel{{ID: "gpt-6-1-sol", Responses: true}}); err != nil {
		t.Fatal(err)
	}
	const base = "http://r/p/dev"
	cases := []struct {
		name, model, typ, base, wire, modelID string
		p                                     *providers.Provider
		routed                                bool
	}{
		{"claude", "databricks-claude-sonnet-5-5", "anthropic", base, "", "", dbxRow("dev", "databricks-"), true},
		{"chat", "databricks-kimi-k3", "openai", base + "/openai", "", "kimi-k3", dbxRow("dev", "databricks-"), true},
		{"responses", "databricks-gpt-6-1-sol", "openai", base + "/responses", "responses", "gpt-6-1-sol", dbxRow("dev", "databricks-"), true},
		{"unrouted", "databricks-kimi-k3", "anthropic", base, "", "", dbxRow("dev", "databricks-"), false},
		{"non-databricks", "kimi-k3", "anthropic", base, "", "", &providers.Provider{Name: "other"}, true},
	}
	for _, c := range cases {
		env, _, err := copilotWire(c.p, providers.ProtocolAnthropic, base, c.model, 200000, "", c.routed, nil)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		get := func(k string) (string, bool) {
			for _, kv := range env {
				if v, ok := strings.CutPrefix(kv, k+"="); ok {
					return v, true
				}
			}
			return "", false
		}
		if v, _ := get("COPILOT_PROVIDER_TYPE"); v != c.typ {
			t.Errorf("%s: TYPE=%q, want %q", c.name, v, c.typ)
		}
		if v, _ := get("COPILOT_PROVIDER_BASE_URL"); v != c.base {
			t.Errorf("%s: BASE_URL=%q, want %q", c.name, v, c.base)
		}
		if v, _ := get("COPILOT_MODEL"); v != c.model {
			t.Errorf("%s: COPILOT_MODEL=%q, want %q", c.name, v, c.model)
		}
		if v, ok := get("COPILOT_PROVIDER_WIRE_API"); v != c.wire || ok != (c.wire != "") {
			t.Errorf("%s: WIRE_API=%q (set %v), want %q", c.name, v, ok, c.wire)
		}
		if v, ok := get("COPILOT_PROVIDER_MODEL_ID"); v != c.modelID || ok != (c.modelID != "") {
			t.Errorf("%s: MODEL_ID=%q (set %v), want %q", c.name, v, ok, c.modelID)
		}
	}
}

const ghProfileRow = `"protocol":"openai","base_url":"https://api.githubcopilot.com","auth":{"type":"cli","login":"copilot"},` +
	`"models":{"opus":"gpt-6.1-sol","sonnet":"claude-sonnet-5.5","haiku":"auto"}`

// ghArgsOnPin runs the launch-token path with COPILOT_PROFILE=pin on a migrated hermetic vault and returns gh's argv.
func ghArgsOnPin(t *testing.T, pin string) string {
	t.Helper()
	copilotAuthEnv(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"github":{`+
		`"personal":{`+ghProfileRow+`,"login":"edotau"},"bare":{`+ghProfileRow+`}}}}`)
	ghDir := os.Getenv("GH_CONFIG_DIR")
	hosts := "github.com:\n    user: alice\n    users:\n        alice:\n            oauth_token: t\n" +
		"        edotau:\n            oauth_token: t\n        octocat:\n            oauth_token: t\n"
	if err := os.WriteFile(filepath.Join(ghDir, "hosts.yml"), []byte(hosts), 0o600); err != nil {
		t.Fatal(err)
	}
	bin, log := t.TempDir(), filepath.Join(t.TempDir(), "args")
	script := "#!/bin/sh\necho \"$@\" >> " + log + "\necho tok\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if pin != "" {
		vaulttest.SetRecord(t, "copilot", "profile", pin)
	}
	if _, err := CopilotLaunchToken(); err != nil {
		t.Fatalf("CopilotLaunchToken(pin %q): %v", pin, err)
	}
	raw, _ := os.ReadFile(log)
	return strings.TrimSpace(string(raw))
}

func TestCopilotProfilePinPassesLoginToGh(t *testing.T) {
	for _, tc := range []struct{ name, pin, want string }{
		{"profile pin passes its login", "personal", "auth token -u edotau"},
		{"unset pin passes no user", "", "auth token"},
		{"profile with no login passes no user", "bare", "auth token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ghArgsOnPin(t, tc.pin); got != tc.want {
				t.Errorf("gh argv = %q, want %q", got, tc.want)
			}
		})
	}
}

// The shared private home: one COPILOT_HOME for launch and SDK, the login guard fires, dry touches nothing.
func TestPrepareCopilotHome(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("COPILOT_HOME", "")
	t.Setenv("HOME", t.TempDir())
	home := filepath.Join(cfg, "state", "copilot", "registry-home")

	if _, err := PrepareCopilotHome(true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(home); err == nil {
		t.Fatal("dry run created the home")
	}
	ph, err := PrepareCopilotHome(false)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(home); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("home = %v, %v; want 0700 dir", fi, err)
	}
	for _, want := range []string{"COPILOT_HOME=" + home, "DBUS_SESSION_BUS_ADDRESS="} {
		if !slices.Contains(ph.Env, want) {
			t.Errorf("env %v lacks %q", ph.Env, want)
		}
	}
	for _, kv := range ph.Env {
		if strings.HasPrefix(kv, "HOME=") {
			t.Errorf("env overrides HOME: %q", kv)
		}
	}
	if !slices.Contains(ph.Unset, "GITHUB_ENTERPRISE_TOKEN") || !slices.Contains(ph.Unset, "COPILOT_SDK_AUTH_TOKEN") {
		t.Errorf("unset %v misses the union list", ph.Unset)
	}
	login := `{"loggedInUsers":[{"login":"x"}]}`
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(login), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareCopilotHome(false); err == nil {
		t.Fatal("login guard did not refuse a home holding a login")
	}
}

// Regression: legacy Claude ids the catalog does not list stay Claude (no OpenAI preference, anthropic wire).
func TestLegacyClaudeStaysAnthropic(t *testing.T) {
	vaulttest.Use(t)
	host := "https://h.example.com"
	p := &providers.Provider{
		Name: "dev", Preset: providers.PresetDatabricks, ModelPrefix: "databricks-",
		Routes: map[string]string{providers.ProtocolAnthropic: host, providers.ProtocolOpenAI: host + "/serving-endpoints"},
	}
	for _, id := range []string{"claude-3-7-sonnet", "databricks-claude-3-7-sonnet", "us.anthropic.claude-3-5-sonnet-20241022-v2:0"} {
		if _, ok := providers.AnthropicRoute(p, id); !ok {
			t.Errorf("AnthropicRoute(%q) not ok, want the anthropic route", id)
		}
		if w, _, ok, _ := copilotEndpointWire(p, id); !ok || w != providers.ProtocolAnthropic {
			t.Errorf("copilotEndpointWire(%q) = %q,%v, want anthropic wire", id, w, ok)
		}
	}
	if _, ok := providers.AnthropicRoute(p, "databricks-kimi-k3"); ok {
		t.Error("AnthropicRoute(kimi) ok, want false")
	}
}

const servingBody = `{"endpoints":[
  {"name":"databricks-claude-sonnet","task":"llm/v1/chat","state":{"ready":"READY"}},
  {"name":"embed-small","task":"llm/v1/embeddings","state":{"ready":"READY"}},
  {"name":"chat-down","task":"llm/v1/chat","state":{"ready":"NOT_READY"}},
  {"name":"gpt-x","task":"llm/v1/chat","state":{"ready":"READY"}}]}`

func TestCopilotModelsServingRows(t *testing.T) {
	vaulttest.Use(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/2.0/serving-endpoints" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(servingBody))
	}))
	t.Cleanup(srv.Close)
	prev := http.DefaultClient.Transport
	http.DefaultClient.Transport = srv.Client().Transport
	t.Cleanup(func() { http.DefaultClient.Transport = prev })
	t.Setenv("DBX_COPILOT_MODELS_TOKEN", "tok")

	reg := &providers.Registry{Providers: map[string]*providers.Provider{
		"dbx": {Name: "dbx", Preset: providers.PresetDatabricks, Host: srv.URL,
			Auth: providers.Auth{Type: providers.AuthKey, Env: "DBX_COPILOT_MODELS_TOKEN"}},
		"other": {Name: "other"},
	}}

	// Databricks row: embedding and NOT_READY rows dropped, order kept, prefixed id matches the unprefixed menu id.
	got, err := CopilotModelSource(context.Background(), reg, "dbx")
	if err != nil {
		t.Fatalf("CopilotModelSource(dbx): %v", err)
	}
	if !got.Databricks || got.Source != "dbx serving endpoints" || got.Profile != "dbx" {
		t.Errorf("catalog = %+v, want Databricks dbx serving endpoints", got)
	}
	if len(got.Rows) != 2 || got.Rows[0].ID != "databricks-claude-sonnet" || got.Rows[1].ID != "gpt-x" {
		t.Fatalf("rows = %+v, want [databricks-claude-sonnet gpt-x]", got.Rows)
	}
	menu := []providers.OpencodeModel{{ID: "claude-sonnet"}}
	rows := copilotServingChatRows([]providers.ModelInfo{
		{ID: "databricks-claude-sonnet", Task: "llm/v1/chat", Ready: "READY"},
		{ID: "gpt-x", Task: "llm/v1/chat", Ready: "READY"},
	}, menu)
	if len(rows) != 2 || !rows[0].Tested || rows[1].Tested {
		t.Errorf("Tested = %+v, want [true false] (prefix stripped vs menu)", rows)
	}

	// Non-Databricks row: GitHub catalog, no network call, no rows.
	gh, err := CopilotModelSource(context.Background(), reg, "other")
	if err != nil {
		t.Fatalf("CopilotModelSource(other): %v", err)
	}
	if gh.Databricks || gh.Source != "GitHub Copilot catalog" || len(gh.Rows) != 0 {
		t.Errorf("catalog = %+v, want GitHub Copilot catalog, no rows", gh)
	}
}
