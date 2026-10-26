package cli

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

const cliFakeToken = "sk-ant-oat01-FAKEFAKEFAKEFAKEFAKEFAKE0123456789"

func TestAnthropicSecretMissingOffTTYNamesVaultSet(t *testing.T) {
	useVaultTest(t)
	t.Setenv("MISSING_KEY_FOR_TEST", "")
	for _, mode := range []string{launch.AuthOAuth, launch.AuthAPIKey} {
		o := launch.AnthropicOptions{Mode: mode, Name: "MISSING_KEY_FOR_TEST"}
		_, err := anthropicSecret(o) // go test has no TTY on stdin: must error, never prompt
		if err == nil || !strings.Contains(err.Error(), "claude-code vault set MISSING_KEY_FOR_TEST") {
			t.Errorf("%s: err = %v", mode, err)
		}
	}
	o := launch.AnthropicOptions{Mode: launch.AuthOAuth, Name: "MISSING_KEY_FOR_TEST"}
	o.PrintEnv = true
	if _, err := anthropicSecret(o); err == nil || !strings.Contains(err.Error(), "claude-code vault set MISSING_KEY_FOR_TEST") {
		t.Errorf("--print-env: err = %v", err)
	}
}

func TestAnthropicSecretReadsVault(t *testing.T) {
	useVaultTest(t)
	vaulttest.Secret(t, "WORK_KEY", cliFakeToken)
	o := launch.AnthropicOptions{Mode: launch.AuthOAuth, Name: "WORK_KEY"}
	if v, err := anthropicSecret(o); err != nil || v != cliFakeToken {
		t.Errorf("oauth: %q %v", v, err)
	}
	o.Mode = launch.AuthAPIKey
	if v, err := anthropicSecret(o); err != nil || v != cliFakeToken {
		t.Errorf("api key: %q %v", v, err)
	}
}

func TestCmdAnthropicRefusalsDoNotEchoSecrets(t *testing.T) {
	useVaultTest(t)
	cases := [][]string{
		{"--oauth=" + cliFakeToken},
		{"--x-api-key=" + cliFakeToken},
		{"--oauth", "--x-api-key"},
		{"--provider", "anthropic"},
		{"--oauth=MISSING_KEY_FOR_TEST", "--print-env"},
	}
	for _, args := range cases {
		code, out, errOut := captureOutput(t, func() int { return cmdAnthropic(args) })
		if code == 0 {
			t.Errorf("%q: exit 0", args)
		}
		if strings.Contains(out+errOut, "FAKEFAKE") {
			t.Errorf("%q echoed the secret: %q", args, out+errOut)
		}
	}
}

func TestAnthropicHelpMentionsFlags(t *testing.T) {
	var b strings.Builder
	anthropicHelp(&b)
	for _, want := range []string{"--oauth", "--x-api-key", "--model"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("help lacks %s:\n%s", want, b.String())
		}
	}
}
