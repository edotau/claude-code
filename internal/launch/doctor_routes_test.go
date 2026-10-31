package launch

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// doctorScratch isolates CLAUDE_CONFIG_DIR, every credential, and opencode's own auth.json/opencode.jsonc
// (XDG_DATA_HOME/XDG_CONFIG_HOME) so DoctorChecks never reads real machine state.
func doctorScratch(t *testing.T) (xdgData string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("PATH", t.TempDir()) // no gh/copilot CLI reachable
	xdgData = filepath.Join(dir, "xdg-data")
	t.Setenv("XDG_DATA_HOME", xdgData)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "xdg-config"))
	t.Setenv("HOME", dir) // never read the host's ~/.config/opencode or ~/.local/share/opencode
	for _, k := range []string{"CLAUDE_PROFILE", "CLAUDE_MODEL", "CLAUDE_OPUS_MODEL", "CLAUDE_SONNET_MODEL",
		"CLAUDE_HAIKU_MODEL", "CLAUDE_FABLE_MODEL", "COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN",
		"COPILOT_MODEL", "COPILOT_REASONING_EFFORT", "COPILOT_CONTEXT_TIER",
		"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY", "OPENROUTER_API_KEY"} {
		t.Setenv(k, "")
	}
	return xdgData
}

// workableCopilotPin pins a github profile whose login is user (a scratch hosts.yml) with 10% of its credits used.
func workableCopilotPin(t *testing.T, user string) {
	t.Helper()
	dir := t.TempDir()
	hosts := "github.com:\n    user: " + user + "\n    users:\n        " + user + ":\n            oauth_token: tok\n"
	if err := os.WriteFile(filepath.Join(dir, "hosts.yml"), []byte(hosts), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_CONFIG_DIR", dir)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"github":{"work":{"protocol":"openai",`+
		`"base_url":"https://api.githubcopilot.com","login":"`+user+`","auth":{"type":"cli","login":"copilot"}}}}}`)
	setPins(t, map[string]string{providers.PinCopilotProfile: "work"})
	prev := agent.CopilotUsedPct
	agent.CopilotUsedPct = func(context.Context, string) (float64, bool) { return 10, true }
	t.Cleanup(func() { agent.CopilotUsedPct = prev })
}

// setPins seeds vault pins (pins never read the process env).
func setPins(t *testing.T, kv map[string]string) {
	t.Helper()
	if err := providers.SetPins(kv); err != nil {
		t.Fatal(err)
	}
}

func findCheck(checks []Check, name string) *Check {
	for i := range checks {
		if checks[i].Name == name {
			return &checks[i]
		}
	}
	return nil
}

// Without opencode's own github-copilot login, the doctor row names the session provider, never a
// model/small_model pair (that's the github-copilot route's own shape).
func TestDoctorOpenCodeRouteWithoutCopilotLogin(t *testing.T) {
	doctorScratch(t)
	checks, _ := DoctorChecks()
	c := findCheck(checks, "opencode route")
	if c == nil {
		t.Fatal(`no "opencode route" check in DoctorChecks output`)
	}
	if strings.Contains(c.Detail, "small_model=") {
		t.Errorf("detail = %q, want no small_model on the non-copilot route", c.Detail)
	}
	if !strings.Contains(c.Detail, "opencode_login=false") {
		t.Errorf("detail = %q, want opencode_login=false", c.Detail)
	}
}

// With opencode's own github-copilot login present, the doctor row names provider=personal and
// both model/small_model: the registry now pins both slots to concrete ids (no "auto" default to
// swap for OpencodeFallback any more — that swap path is covered directly in internal/agent).
func TestDoctorOpenCodeRouteWithCopilotLogin(t *testing.T) {
	xdgData := doctorScratch(t)
	// github-copilot's registry defaults are "auto" (Copilot's own router); pin real ids here so this
	// test still proves slot ids pass through unchanged, distinct from the auto -> OpencodeFallback swap.
	setPins(t, map[string]string{providers.SlotPin("sonnet"): "gpt-5.3-codex", providers.SlotPin("haiku"): "gpt-5.4-mini"})
	authDir := filepath.Join(xdgData, "opencode")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "auth.json"), []byte(`{"github-copilot":{"type":"oauth"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	checks, _ := DoctorChecks()
	c := findCheck(checks, "opencode route")
	if c == nil {
		t.Fatal(`no "opencode route" check in DoctorChecks output`)
	}
	if !strings.Contains(c.Detail, "provider=personal") {
		t.Errorf("detail = %q, want provider=personal", c.Detail)
	}
	if !strings.Contains(c.Detail, "opencode_login=true") {
		t.Errorf("detail = %q, want opencode_login=true", c.Detail)
	}
	if !strings.Contains(c.Detail, "model=github-copilot/gpt-5.3-codex") {
		t.Errorf("detail = %q, want the sonnet slot's own id, unchanged", c.Detail)
	}
	if !strings.Contains(c.Detail, "small_model=github-copilot/gpt-5.4-mini") {
		t.Errorf("detail = %q, want the haiku slot's own id, unchanged", c.Detail)
	}
}

// On the copilot CLI's own-login dispatch, the doctor row names COPILOT_MODEL's resolved source and the
// effort/context pins: unpinned, source is the github-copilot default and both pins read "default".
func TestDoctorCopilotRouteSourceEffortContextUnpinned(t *testing.T) {
	doctorScratch(t)
	workableCopilotPin(t, "alice")
	checks, _ := DoctorChecks()
	c := findCheck(checks, "copilot route")
	if c == nil {
		t.Fatal(`no "copilot route" check in DoctorChecks output`)
	}
	if !strings.Contains(c.Detail, "source=github-copilot default") {
		t.Errorf("detail = %q, want source=github-copilot default", c.Detail)
	}
	if !strings.Contains(c.Detail, "effort=default") || !strings.Contains(c.Detail, "context=default") {
		t.Errorf("detail = %q, want effort=default context=default", c.Detail)
	}
}

// With COPILOT_MODEL pinned and the effort/context pins set, the doctor row names the pin source
// and the pinned values, not the github-copilot provider default.
func TestDoctorCopilotRouteSourceEffortContextPinned(t *testing.T) {
	doctorScratch(t)
	workableCopilotPin(t, "alice")
	setPins(t, map[string]string{providers.PinCopilotModel: "claude-opus-5.5", providers.PinCopilotEffort: "high",
		providers.PinCopilotContext: "long_context"})
	checks, _ := DoctorChecks()
	c := findCheck(checks, "copilot route")
	if c == nil {
		t.Fatal(`no "copilot route" check in DoctorChecks output`)
	}
	if !strings.Contains(c.Detail, "model=claude-opus-5.5") {
		t.Errorf("detail = %q, want model=claude-opus-5.5", c.Detail)
	}
	if !strings.Contains(c.Detail, "source=pin") {
		t.Errorf("detail = %q, want source=pin", c.Detail)
	}
	if !strings.Contains(c.Detail, "effort=high") || !strings.Contains(c.Detail, "context=long_context") {
		t.Errorf("detail = %q, want effort=high context=long_context", c.Detail)
	}
}

// A stale static key in opencode's own auth.json for a provider the vault now covers fires the
// soft "opencode auth.json" check, naming the integration id and the logout command, never a value.
func TestDoctorOpenCodeAuthJSONStaleKeyFires(t *testing.T) {
	xdgData := doctorScratch(t)
	t.Setenv("NVIDIA_API_KEY", "nvapi-doctorFAKEkey1234567890")
	authDir := filepath.Join(xdgData, "opencode")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "auth.json"), []byte(`{"nvidia":{"type":"api","key":"nvapi-realFAKEsecretvalue"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	checks, _ := DoctorChecks()
	c := findCheck(checks, "opencode auth.json")
	if c == nil {
		t.Fatal(`no "opencode auth.json" check in DoctorChecks output`)
	}
	if c.OK {
		t.Errorf("OK = true, want false: a stale vault-covered key must fail the soft check")
	}
	if c.Hard {
		t.Error("the check must be soft, never hard")
	}
	if !strings.Contains(c.Detail, "nvidia key") || !strings.Contains(c.Detail, "opencode auth logout nvidia") {
		t.Errorf("detail = %q, want it to name nvidia and the logout command", c.Detail)
	}
	if strings.Contains(c.Detail, "nvapi-realFAKEsecretvalue") {
		t.Fatalf("detail leaked the stored credential value: %q", c.Detail)
	}
}

// No stale key (auth.json absent, or the provider has no vault credential) reports clean.
func TestDoctorOpenCodeAuthJSONCleanWhenNoStaleKey(t *testing.T) {
	doctorScratch(t)
	checks, _ := DoctorChecks()
	c := findCheck(checks, "opencode auth.json")
	if c == nil {
		t.Fatal(`no "opencode auth.json" check in DoctorChecks output`)
	}
	if !c.OK || c.Detail != "clean" {
		t.Errorf("check = %+v, want OK=true Detail=clean", c)
	}
}

// An oauth connection (not a static "key" method) never counts as stale, even for a vault-covered provider.
func TestDoctorOpenCodeAuthJSONIgnoresOAuthConnections(t *testing.T) {
	xdgData := doctorScratch(t)
	t.Setenv("NVIDIA_API_KEY", "nvapi-doctorFAKEkey1234567890")
	authDir := filepath.Join(xdgData, "opencode")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "auth.json"), []byte(`{"nvidia":{"type":"oauth"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	checks, _ := DoctorChecks()
	c := findCheck(checks, "opencode auth.json")
	if c == nil {
		t.Fatal(`no "opencode auth.json" check in DoctorChecks output`)
	}
	if !c.OK || c.Detail != "clean" {
		t.Errorf("check = %+v, want OK=true Detail=clean for an oauth-only connection", c)
	}
}

// The doctor row never prints a secret: GH_TOKEN's value must not surface, only the env var's presence.
func TestDoctorOpenCodeRouteNamesNoSecret(t *testing.T) {
	doctorScratch(t)
	t.Setenv("GH_TOKEN", "super-secret-gh-token-value")
	checks, _ := DoctorChecks()
	for _, c := range checks {
		if strings.Contains(c.Detail, "super-secret-gh-token-value") {
			t.Fatalf("check %q leaked a credential: %q", c.Name, c.Detail)
		}
	}
}

// A key-auth built-in nothing routes to (not session, default, fallback or pinned) is a soft note, not ✗;
// pinning it for an agent makes the missing key a real ✗ again.
func TestDoctorUnusedProviderMissingKeyIsSoft(t *testing.T) {
	doctorScratch(t)
	checks, _ := DoctorChecks()
	c := findCheck(checks, "credential openai")
	if c == nil || !c.OK || !strings.Contains(c.Detail, "unused") {
		t.Fatalf("unused openai row = %+v, want OK with an unused note", c)
	}
	setPins(t, map[string]string{providers.PinCodexProfile: "openai"})
	checks, _ = DoctorChecks()
	if c := findCheck(checks, "credential openai"); c == nil || c.OK {
		t.Fatalf("pinned openai row = %+v, want ✗ for the missing key", c)
	}
}

// altAuthScratch isolates the machine state altAuth reads: CLAUDE_CONFIG_DIR (keyring/secrets.env),
// HOME (the gemini agy login marker, and ClaudeLoginStatus has nowhere real to run), and a PATH with
// neither `claude` nor `security`/`secret-tool` on it, so the real Keychain and a real claude login
// can never answer for this test by accident.
func altAuthScratch(t *testing.T) (home string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("PATH", t.TempDir())
	home = filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv(providers.OAuthTokenEnv, "")
	return home
}

func TestAltAuthAnthropicHeadlessToken(t *testing.T) {
	altAuthScratch(t)
	t.Setenv(providers.OAuthTokenEnv, "sk-ant-oat-test")
	if got := altAuth(nil, "anthropic"); got != "claude uses the OAuth token instead" {
		t.Errorf("altAuth(anthropic) with a headless token = %q", got)
	}
}

func TestAltAuthAnthropicNone(t *testing.T) {
	altAuthScratch(t) // no headless token, no claude binary to report a login
	if got := altAuth(nil, "anthropic"); got != "" {
		t.Errorf("altAuth(anthropic) with neither = %q, want \"\"", got)
	}
}

// gemini has no altAuth case: GeminiLoginPresent (not altAuth) now reports the agy Google login.
func TestAltAuthGemini(t *testing.T) {
	home := altAuthScratch(t)
	if got := altAuth(nil, "gemini"); got != "" {
		t.Errorf("altAuth(gemini) without the agy marker = %q, want \"\"", got)
	}
	if err := os.MkdirAll(filepath.Join(home, ".gemini"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".gemini", "jetski-standalone-oauth-token"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := altAuth(nil, "gemini"); got != "" {
		t.Errorf("altAuth(gemini) with the agy marker = %q, want \"\" (altAuth has no gemini case)", got)
	}
}

func TestAltAuthUnknownProvider(t *testing.T) {
	altAuthScratch(t)
	if got := altAuth(nil, "openai"); got != "" {
		t.Errorf("altAuth(openai) = %q, want \"\" (no alt auth path for this provider)", got)
	}
}

// RouteDetail is the per-vendor shim `vendoragent setup` calls instead of running the whole doctor
// sweep; its detail string must match the corresponding "<v> route" row in DoctorChecks exactly.
func TestRouteDetailMatchesDoctorChecks(t *testing.T) {
	doctorScratch(t)
	t.Setenv("GH_TOKEN", "ghp_doctorFAKEtoken1234567890")
	setPins(t, map[string]string{providers.SlotPin("sonnet"): "gpt-5.3-codex"})

	checks, _ := DoctorChecks()
	for _, vendor := range []string{"codex", "gemini", "opencode", "copilot"} {
		detail, ok := agent.RouteDetail(vendor)
		if !ok {
			t.Errorf("RouteDetail(%q) ok = false, want true", vendor)
			continue
		}
		c := findCheck(checks, vendor+" route")
		if c == nil {
			t.Fatalf("no %q check in DoctorChecks output", vendor+" route")
		}
		if detail != c.Detail {
			t.Errorf("RouteDetail(%q) = %q, want DoctorChecks' %q", vendor, detail, c.Detail)
		}
	}
}

// An unknown vendor reports ok=false, never a stale/zero detail string.
func TestRouteDetailUnknownVendor(t *testing.T) {
	doctorScratch(t)
	if detail, ok := agent.RouteDetail("not-a-vendor"); ok || detail != "" {
		t.Errorf("RouteDetail(unknown) = (%q, %v), want (\"\", false)", detail, ok)
	}
}

// TestPrintChecksGoldenBytes pins printChecks' byte format (color off, the case for every test and
// any non-tty run): "<mark> <name>: <detail>\n", mark = ✓ when OK else ✗. Doctor and VaultDoctor both
// share this function now; a drift here changes what both commands print.
func TestPrintChecksGoldenBytes(t *testing.T) {
	checks := []Check{
		{Name: "sample", OK: true, Detail: "all good"},
		{Name: "sample2", OK: false, Detail: "broken: reason"},
	}
	var buf bytes.Buffer
	PrintChecks(&buf, checks)
	want := "✓ sample: all good\n✗ sample2: broken: reason\n"
	if got := buf.String(); got != want {
		t.Errorf("printChecks =\n%q\nwant\n%q", got, want)
	}
}
