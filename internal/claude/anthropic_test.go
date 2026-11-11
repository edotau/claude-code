package claude

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

const fakeToken = "sk-ant-oat01-FAKEFAKEFAKEFAKEFAKEFAKE0123456789"

func TestParseAnthropicBareFlagTakesNoValue(t *testing.T) {
	for _, flag := range []string{"--oauth", "--x-api-key"} {
		o, rest, err := parseAnthropicArgs([]string{flag, "-p", "hi"})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(rest, []string{"-p", "hi"}) {
			t.Errorf("%s swallowed a claude arg: rest %q", flag, rest)
		}
		want := map[string][2]string{"--oauth": {authOAuth, oauthKey}, "--x-api-key": {authAPIKey, defaultAPIKeyName}}[flag]
		if o.Mode != want[0] || o.Name != want[1] {
			t.Errorf("%s: mode %q name %q", flag, o.Mode, o.Name)
		}
	}
}

func TestParseAnthropicNoFlagIsLogin(t *testing.T) {
	o, rest, err := parseAnthropicArgs([]string{"--resume"})
	if err != nil || o.Mode != authLogin || o.Name != "" || !slices.Equal(rest, []string{"--resume"}) {
		t.Errorf("%+v %q %v", o, rest, err)
	}
}

func TestParseAnthropicNamedFlag(t *testing.T) {
	o, _, err := parseAnthropicArgs([]string{"--oauth=MY_TOKEN"})
	if err != nil || o.Mode != authOAuth || o.Name != "MY_TOKEN" {
		t.Errorf("%+v %v", o, err)
	}
	o, _, err = parseAnthropicArgs([]string{"--x-api-key=WORK_KEY"})
	if err != nil || o.Mode != authAPIKey || o.Name != "WORK_KEY" {
		t.Errorf("%+v %v", o, err)
	}
}

func TestParseAnthropicRefusesSecretShapedName(t *testing.T) {
	for _, flag := range []string{"--oauth", "--x-api-key"} {
		_, _, err := parseAnthropicArgs([]string{flag + "=" + fakeToken})
		if err == nil {
			t.Fatalf("%s accepted a secret-shaped NAME", flag)
		}
		if strings.Contains(err.Error(), fakeToken) || strings.Contains(err.Error(), "FAKEFAKE") {
			t.Errorf("error echoes the value: %v", err)
		}
	}
	// A non-credential-shaped but non-env-shaped name is refused too.
	for _, bad := range []string{"has space", "9LEAD", "a-b", ""} {
		if _, _, err := parseAnthropicArgs([]string{"--oauth=" + bad}); err == nil {
			t.Errorf("--oauth=%q accepted", bad)
		}
	}
}

func TestParseAnthropicRefusesConflicts(t *testing.T) {
	if _, _, err := parseAnthropicArgs([]string{"--oauth", "--x-api-key"}); err == nil {
		t.Error("--oauth with --x-api-key accepted")
	}
	if _, _, err := parseAnthropicArgs([]string{"--x-api-key=A", "--oauth=B"}); err == nil {
		t.Error("named --x-api-key with --oauth accepted")
	}
	if _, _, err := parseAnthropicArgs([]string{"--oauth", "--profile", "anthropic"}); err == nil {
		t.Error("--oauth with --profile accepted")
	}
}

func TestParseAnthropicDoubleDashEndsFlags(t *testing.T) {
	o, rest, err := parseAnthropicArgs([]string{"--", "--oauth"})
	if err != nil || o.Mode != authLogin || !slices.Equal(rest, []string{"--oauth"}) {
		t.Errorf("%+v %q %v", o, rest, err)
	}
}

func TestAnthropicLoginPlan(t *testing.T) {
	vaulttest.Use(t)
	hermetic(t, "")
	t.Setenv(oauthKey, "")
	o, rest, err := parseAnthropicArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := anthropicPlan(context.Background(), o, rest, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Set["CLAUDE_PROFILE"]; got != "ultra" {
		t.Errorf("CLAUDE_PROFILE = %q, want ultra", got)
	}
	if v, ok := p.Set[oauthKey]; ok && v != "" {
		t.Errorf("login mode must not set %s", oauthKey)
	}
}

// planFor parses args as the anthropic verb does and resolves the plan with secret.
func planFor(t *testing.T, secret string, args ...string) agent.Plan {
	t.Helper()
	o, rest, err := parseAnthropicArgs(args)
	if err != nil {
		t.Fatal(err)
	}
	p, err := anthropicPlan(context.Background(), o, rest, secret)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// printed is Plan.Print plus argv and overlay: every place a secret could surface.
func printed(p agent.Plan) string {
	var buf bytes.Buffer
	p.Print(&buf)
	return buf.String() + string(overlayFile(p).Data) + strings.Join(p.Argv, " ")
}

// --oauth: the vault token rides CLAUDE_CODE_OAUTH_TOKEN, no apiKeyHelper, and Print masks it.
func TestAnthropicOAuthLaunchPlan(t *testing.T) {
	hermetic(t, "")
	vaulttest.Secret(t, oauthKey, fakeToken)
	if got := providers.StoredSecret(oauthKey); got != fakeToken {
		t.Fatalf("vault read %q", got)
	}
	p := planFor(t, fakeToken, "--oauth", "--print-env")
	if p.Set[oauthKey] != fakeToken || p.Set[providers.EnvSessionProfile] != "ultra" {
		t.Errorf("set %v", p.Set)
	}
	if len(p.Files) != 0 || slices.Contains(p.Argv, "--settings") {
		t.Errorf("a login profile must not set an apiKeyHelper: %q", p.Argv)
	}
	out := printed(p)
	if strings.Contains(out, "FAKEFAKE") || !strings.Contains(out, oauthKey+"=<redacted>") {
		t.Errorf("token not masked:\n%s", out)
	}
}

// --x-api-key=NAME: the named key is carried directly as ANTHROPIC_API_KEY; the registry entry stays untouched.
func TestAnthropicNamedAPIKeyLaunchPlan(t *testing.T) {
	hermetic(t, "")
	before, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	p := planFor(t, fakeToken, "--x-api-key=WORK_KEY")
	if p.Set["ANTHROPIC_API_KEY"] != fakeToken || p.Set[providers.EnvSessionProfile] != "anthropic" {
		t.Errorf("set %v", p.Set)
	}
	if len(p.Files) != 0 || slices.Contains(p.Argv, "--settings") {
		t.Errorf("a named key drops the apiKeyHelper overlay: %q %s", p.Argv, overlayFile(p).Data)
	}
	if _, ok := p.Set["CLAUDE_CODE_API_KEY_HELPER_TTL_MS"]; ok {
		t.Error("helper TTL must be dropped with the helper")
	}
	if out := printed(p); strings.Contains(out, "FAKEFAKE") {
		t.Errorf("Print leaked the key:\n%s", out)
	}
	after, err := providers.Load()
	if err != nil || after.Providers["anthropic"].Auth.Env != before.Providers["anthropic"].Auth.Env {
		t.Errorf("registry anthropic auth env mutated: %v", err)
	}
}

// Bare --x-api-key keeps the registry's ANTHROPIC_API_KEY apiKeyHelper overlay and never embeds the key in it.
func TestAnthropicDefaultAPIKeyLaunchPlan(t *testing.T) {
	hermetic(t, "")
	p := planFor(t, fakeToken, "--x-api-key")
	if p.Set[providers.EnvSessionProfile] != "anthropic" || p.Set["ANTHROPIC_API_KEY"] != "" {
		t.Errorf("set %v", p.Set)
	}
	if !slices.Contains(p.Argv, "--settings") || !strings.Contains(string(overlayFile(p).Data), "apiKeyHelper") {
		t.Errorf("bare --x-api-key should use the apiKeyHelper overlay: %q", p.Argv)
	}
	if out := printed(p); strings.Contains(out, "FAKEFAKE") {
		t.Errorf("overlay or Print embeds the key:\n%s", out)
	}
}

func TestAnthropicModelPicksMainTier(t *testing.T) {
	vaulttest.Use(t)
	hermetic(t, "")
	cases := map[string]string{"": models.Opus, "sonnet-5": models.Sonnet, "haiku": models.Haiku, "claude-opus-5": models.Opus}
	for model, alias := range cases {
		args := []string{}
		if model != "" {
			args = []string{"--model", model}
		}
		o, rest, err := parseAnthropicArgs(args)
		if err != nil {
			t.Fatal(err)
		}
		p, err := anthropicPlan(context.Background(), o, rest, "")
		if err != nil {
			t.Fatalf("%q: %v", model, err)
		}
		got := p.Set["ANTHROPIC_MODEL"]
		if model == "" && got != "" {
			t.Errorf("no --model: ANTHROPIC_MODEL %q, want unset", got)
		}
		if model != "" && !strings.Contains(got, alias) {
			t.Errorf("--model %q: ANTHROPIC_MODEL %q, want family %q", model, got, alias)
		}
	}
}

// No error echoes the rejected NAME, including a mixed-case high-entropy value shapes.Looks misses.
func TestParseAnthropicErrorsNeverEchoName(t *testing.T) {
	const entropy = "q8ZtR3vLx9Kp2wN7mBcY4hJs6dFgA1eW"
	for _, flag := range []string{"--oauth", "--x-api-key"} {
		for _, v := range []string{entropy, fakeToken, "bad name-xyz"} {
			_, _, err := parseAnthropicArgs([]string{flag + "=" + v})
			if err == nil {
				t.Fatalf("%s=%q accepted", flag, v)
			}
			if strings.Contains(err.Error(), v) {
				t.Errorf("%s error echoes %q: %v", flag, v, err)
			}
		}
	}
}

func TestParseAnthropicRefusesRepeatedFlag(t *testing.T) {
	for _, flag := range []string{"--oauth", "--x-api-key"} {
		_, _, err := parseAnthropicArgs([]string{flag, flag})
		want := flag + " given more than once; pass it a single time"
		if err == nil || err.Error() != want {
			t.Errorf("%s %s: err = %v, want %q", flag, flag, err, want)
		}
	}
}

func TestAnthropicSecretMissingOffTTYNamesVaultSet(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("MISSING_KEY_FOR_TEST", "")
	for _, mode := range []string{authOAuth, authAPIKey} {
		o := anthropicOptions{Mode: mode, Name: "MISSING_KEY_FOR_TEST"}
		_, err := anthropicSecret(o, failStore) // go test has no TTY on stdin: must error, never prompt
		if err == nil || !strings.Contains(err.Error(), "claude-code vault set MISSING_KEY_FOR_TEST") {
			t.Errorf("%s: err = %v", mode, err)
		}
	}
	o := anthropicOptions{Mode: authOAuth, Name: "MISSING_KEY_FOR_TEST", PrintEnv: true}
	if _, err := anthropicSecret(o, failStore); err == nil || !strings.Contains(err.Error(), "claude-code vault set MISSING_KEY_FOR_TEST") {
		t.Errorf("--print-env: err = %v", err)
	}
}

func TestAnthropicSecretReadsVault(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Secret(t, "WORK_KEY", fakeToken)
	o := anthropicOptions{Mode: authOAuth, Name: "WORK_KEY"}
	if v, err := anthropicSecret(o, failStore); err != nil || v != fakeToken {
		t.Errorf("oauth: %q %v", v, err)
	}
	o.Mode = authAPIKey
	if v, err := anthropicSecret(o, failStore); err != nil || v != fakeToken {
		t.Errorf("api key: %q %v", v, err)
	}
}

// A validated env-shaped name may appear in the "not in the vault" error; PrintEnv avoids any prompt.
func TestAnthropicSecretMissingNamesKey(t *testing.T) {
	vaulttest.Use(t)
	const name = "NOPE_ABSENT_KEY_XYZ"
	t.Setenv(name, "")
	o := anthropicOptions{Mode: authOAuth, Name: name, PrintEnv: true}
	_, err := anthropicSecret(o, failStore)
	if err == nil || !strings.Contains(err.Error(), name+" is not in the vault") {
		t.Fatalf("err = %v", err)
	}
}

// failStore stands in for the vault verb; the tests never reach a real store.
func failStore(string) int { return 1 }

func TestRunSetupTokenScrubsVaultKey(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", "setup-secret")
	t.Setenv("DISPLAY", ":7")
	out := filepath.Join(t.TempDir(), "env")
	pathstest.FakeVendor(t, "claude", `echo "key=$HARNESS_VAULT_KEY display=$DISPLAY" > `+out)
	runSetupToken()
	got, err := os.ReadFile(out)
	if err != nil || strings.Contains(string(got), "setup-secret") || !strings.Contains(string(got), "display=:7") {
		t.Fatalf("setup-token env (err=%v): %q; want no vault key, DISPLAY kept", err, got)
	}
}
