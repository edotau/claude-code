package agent

import (
	"context"
	"github.com/edotau/claude-code/internal/agent/copilotreg"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestCopilotLogin(t *testing.T) {
	tg := target(t, "personal", "claude-opus-5.5")
	env, argv, err := configureHeadless(context.Background(), "copilot", tg, Headless{Prompt: "hi", Tools: true})
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
	if _, _, err := Configure(context.Background(), "codex", tg, ""); err == nil || !strings.Contains(err.Error(), "copilot sign-in") {
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
	for _, k := range []string{providers.PinCopilotEffort, providers.PinCopilotContext} {
		t.Setenv(k, "")
	}

	_, argv, err := Configure(context.Background(), "copilot", tg, "")
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(argv, "--reasoning-effort") || slices.Contains(argv, "--context") {
		t.Errorf("argv %q with no pins set, want neither flag", argv)
	}

	setPin(t, providers.PinCopilotEffort, "xhigh")
	setPin(t, providers.PinCopilotContext, "long_context")
	_, argv, err = Configure(context.Background(), "copilot", tg, "")
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
	setPin(t, providers.PinCopilotEffort, "low")
	setPin(t, providers.PinCopilotContext, "default")

	_, argv, err := Configure(context.Background(), "copilot", tg, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--reasoning-effort", "low", "--context", "default", "--allow-all"}
	if !slices.Equal(argv, want) {
		t.Errorf("argv = %q, want %q", argv, want)
	}
}

// ConfigureHeadless (h != nil) ignores the pins: copilot always runs headless through copilotRunner's
// own SDK options, never this argv path, so copilotHeadless must stay untouched by the pins.
func TestCopilotHeadlessArgvIgnoresPins(t *testing.T) {
	tg := target(t, "personal", "claude-opus-5.5")
	noCopilotLogin(t)
	setPin(t, providers.PinCopilotEffort, "max")
	setPin(t, providers.PinCopilotContext, "long_context")

	_, argv, err := configureHeadless(context.Background(), "copilot", tg, Headless{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"--prompt", "hi"}; !slices.Equal(argv, want) {
		t.Errorf("argv = %q, want %q (pins must not leak into the headless SDK path)", argv, want)
	}
}

// The registry's "auto" slot must not override copilot's own default model: COPILOT_MODEL goes out blank.
func TestCopilotLoginAutoDefersToCopilotDefault(t *testing.T) {
	env, _, err := Configure(context.Background(), "copilot", target(t, "personal", "auto"), "")
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
	env, _, err := Configure(WithDirect(context.Background()), "copilot", tg, "")
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
		t.Errorf("prompt budget must leave room for %d output tokens, got %s", copilotreg.MaxOutputTokens, v)
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
