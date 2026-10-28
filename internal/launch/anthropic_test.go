package launch

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

const fakeToken = "sk-ant-oat01-FAKEFAKEFAKEFAKEFAKEFAKE0123456789"

func TestParseAnthropicBareFlagTakesNoValue(t *testing.T) {
	for _, flag := range []string{"--oauth", "--x-api-key"} {
		o, rest, err := ParseAnthropicArgs([]string{flag, "-p", "hi"})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(rest, []string{"-p", "hi"}) {
			t.Errorf("%s swallowed a claude arg: rest %q", flag, rest)
		}
		want := map[string][2]string{"--oauth": {AuthOAuth, DefaultOAuthName}, "--x-api-key": {AuthAPIKey, DefaultAPIKeyName}}[flag]
		if o.Mode != want[0] || o.Name != want[1] {
			t.Errorf("%s: mode %q name %q", flag, o.Mode, o.Name)
		}
	}
}

func TestParseAnthropicNoFlagIsLogin(t *testing.T) {
	o, rest, err := ParseAnthropicArgs([]string{"--resume"})
	if err != nil || o.Mode != AuthLogin || o.Name != "" || !slices.Equal(rest, []string{"--resume"}) {
		t.Errorf("%+v %q %v", o, rest, err)
	}
}

func TestParseAnthropicNamedFlag(t *testing.T) {
	o, _, err := ParseAnthropicArgs([]string{"--oauth=MY_TOKEN"})
	if err != nil || o.Mode != AuthOAuth || o.Name != "MY_TOKEN" {
		t.Errorf("%+v %v", o, err)
	}
	o, _, err = ParseAnthropicArgs([]string{"--x-api-key=WORK_KEY"})
	if err != nil || o.Mode != AuthAPIKey || o.Name != "WORK_KEY" {
		t.Errorf("%+v %v", o, err)
	}
}

func TestParseAnthropicRefusesSecretShapedName(t *testing.T) {
	for _, flag := range []string{"--oauth", "--x-api-key"} {
		_, _, err := ParseAnthropicArgs([]string{flag + "=" + fakeToken})
		if err == nil {
			t.Fatalf("%s accepted a secret-shaped NAME", flag)
		}
		if strings.Contains(err.Error(), fakeToken) || strings.Contains(err.Error(), "FAKEFAKE") {
			t.Errorf("error echoes the value: %v", err)
		}
	}
	// A non-credential-shaped but non-env-shaped name is refused too.
	for _, bad := range []string{"has space", "9LEAD", "a-b", ""} {
		if _, _, err := ParseAnthropicArgs([]string{"--oauth=" + bad}); err == nil {
			t.Errorf("--oauth=%q accepted", bad)
		}
	}
}

func TestParseAnthropicRefusesConflicts(t *testing.T) {
	if _, _, err := ParseAnthropicArgs([]string{"--oauth", "--x-api-key"}); err == nil {
		t.Error("--oauth with --x-api-key accepted")
	}
	if _, _, err := ParseAnthropicArgs([]string{"--x-api-key=A", "--oauth=B"}); err == nil {
		t.Error("named --x-api-key with --oauth accepted")
	}
	if _, _, err := ParseAnthropicArgs([]string{"--profile", "anthropic"}); err == nil {
		t.Error("--profile accepted by the anthropic verb")
	}
}

func TestParseAnthropicDoubleDashEndsFlags(t *testing.T) {
	o, rest, err := ParseAnthropicArgs([]string{"--", "--oauth"})
	if err != nil || o.Mode != AuthLogin || !slices.Equal(rest, []string{"--oauth"}) {
		t.Errorf("%+v %q %v", o, rest, err)
	}
}

func TestAnthropicLoginPlan(t *testing.T) {
	vaulttest.Use(t)
	hermetic(t, "")
	t.Setenv(oauthKey, "")
	o, rest, err := ParseAnthropicArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := Anthropic(context.Background(), o, rest, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Set["CLAUDE_PROFILE"]; got != "max" {
		t.Errorf("CLAUDE_PROFILE = %q, want max", got)
	}
	if v, ok := p.Set[oauthKey]; ok && v != "" {
		t.Errorf("login mode must not set %s", oauthKey)
	}
}

func TestAnthropicOAuthPlanSetsTokenAndNeverPrintsIt(t *testing.T) {
	vaulttest.Use(t)
	hermetic(t, "")
	vaulttest.Secret(t, DefaultOAuthName, fakeToken)
	o, rest, err := ParseAnthropicArgs([]string{"--oauth", "--print-env"})
	if err != nil {
		t.Fatal(err)
	}
	secret := providers.StoredSecret(o.Name)
	if secret != fakeToken {
		t.Fatalf("vault read %q", secret)
	}
	p, err := Anthropic(context.Background(), o, rest, secret)
	if err != nil {
		t.Fatal(err)
	}
	if p.Set[oauthKey] != fakeToken {
		t.Errorf("%s not set from the vault value", oauthKey)
	}
	if slices.ContainsFunc(p.Argv, func(a string) bool { return strings.Contains(a, "FAKEFAKE") }) {
		t.Errorf("token in argv: %q", p.Argv)
	}
	var buf bytes.Buffer
	p.Print(&buf)
	if strings.Contains(buf.String(), "FAKEFAKE") {
		t.Errorf("Plan.Print leaked the token:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), oauthKey+"=<redacted>") {
		t.Errorf("expected a redacted %s line:\n%s", oauthKey, buf.String())
	}
}

func TestAnthropicAPIKeyNamedCarriesKeyDirectlyWithoutMutatingRegistry(t *testing.T) {
	vaulttest.Use(t)
	hermetic(t, "")
	before, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	wantEnv := before.Providers["anthropic"].Auth.Env
	o, rest, err := ParseAnthropicArgs([]string{"--x-api-key=WORK_KEY"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := Anthropic(context.Background(), o, rest, fakeToken)
	if err != nil {
		t.Fatal(err)
	}
	if p.Set["CLAUDE_PROFILE"] != "anthropic" {
		t.Errorf("CLAUDE_PROFILE = %q", p.Set["CLAUDE_PROFILE"])
	}
	if p.Set["ANTHROPIC_API_KEY"] != fakeToken {
		t.Error("ANTHROPIC_API_KEY not carried from NAME's value")
	}
	if len(p.Overlay) != 0 || p.OverlayPath != "" || slices.Contains(p.Argv, "--settings") {
		t.Errorf("named key must drop the apiKeyHelper overlay: %q %s", p.Argv, p.Overlay)
	}
	if _, ok := p.Set["CLAUDE_CODE_API_KEY_HELPER_TTL_MS"]; ok {
		t.Error("helper TTL must be dropped")
	}
	if slices.ContainsFunc(p.Argv, func(a string) bool { return strings.Contains(a, "FAKEFAKE") }) {
		t.Errorf("key in argv: %q", p.Argv)
	}
	var buf bytes.Buffer
	p.Print(&buf)
	if strings.Contains(buf.String(), "FAKEFAKE") {
		t.Errorf("Plan.Print leaked the key:\n%s", buf.String())
	}
	after, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := after.Providers["anthropic"].Auth.Env; got != wantEnv {
		t.Errorf("registry anthropic auth env mutated: %q -> %q", wantEnv, got)
	}
}

func TestAnthropicAPIKeyDefaultUsesHelper(t *testing.T) {
	vaulttest.Use(t)
	hermetic(t, "")
	o, rest, err := ParseAnthropicArgs([]string{"--x-api-key"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := Anthropic(context.Background(), o, rest, fakeToken)
	if err != nil {
		t.Fatal(err)
	}
	if p.Set["CLAUDE_PROFILE"] != "anthropic" {
		t.Errorf("CLAUDE_PROFILE = %q", p.Set["CLAUDE_PROFILE"])
	}
	if !slices.Contains(p.Argv, "--settings") || len(p.Overlay) == 0 {
		t.Errorf("bare --x-api-key should use the apiKeyHelper overlay: %q", p.Argv)
	}
	if strings.Contains(string(p.Overlay), "FAKEFAKE") {
		t.Error("overlay must not embed the key")
	}
}

func TestAnthropicModelPicksMainTier(t *testing.T) {
	vaulttest.Use(t)
	hermetic(t, "")
	cases := map[string]string{"": models.Opus, "sonnet-5": models.Sonnet, "haiku": models.Haiku, "claude-opus-5": models.Opus}
	for model, tier := range cases {
		args := []string{}
		if model != "" {
			args = []string{"--model", model}
		}
		o, rest, err := ParseAnthropicArgs(args)
		if err != nil {
			t.Fatal(err)
		}
		p, err := Anthropic(context.Background(), o, rest, "")
		if err != nil {
			t.Fatalf("%q: %v", model, err)
		}
		got := p.Set["ANTHROPIC_MODEL"]
		if model == "" && got != "" {
			t.Errorf("no --model: ANTHROPIC_MODEL %q, want unset", got)
		}
		if model != "" && !strings.Contains(got, tier) {
			t.Errorf("--model %q: ANTHROPIC_MODEL %q, want family %q", model, got, tier)
		}
	}
}

// No error echoes the rejected NAME, including a mixed-case high-entropy value shapes.Looks misses.
func TestParseAnthropicErrorsNeverEchoName(t *testing.T) {
	const entropy = "q8ZtR3vLx9Kp2wN7mBcY4hJs6dFgA1eW"
	for _, flag := range []string{"--oauth", "--x-api-key"} {
		for _, v := range []string{entropy, fakeToken, "bad name-xyz"} {
			_, _, err := ParseAnthropicArgs([]string{flag + "=" + v})
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
		_, _, err := ParseAnthropicArgs([]string{flag, flag})
		want := flag + " given more than once; pass it a single time"
		if err == nil || err.Error() != want {
			t.Errorf("%s %s: err = %v, want %q", flag, flag, err, want)
		}
	}
}
