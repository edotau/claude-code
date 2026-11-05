package claude

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestAnthropicSecretMissingOffTTYNamesVaultSet(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("MISSING_KEY_FOR_TEST", "")
	for _, mode := range []string{AuthOAuth, AuthAPIKey} {
		o := AnthropicOptions{Mode: mode, Name: "MISSING_KEY_FOR_TEST"}
		_, err := AnthropicSecret(o, failStore) // go test has no TTY on stdin: must error, never prompt
		if err == nil || !strings.Contains(err.Error(), "claude-code vault set MISSING_KEY_FOR_TEST") {
			t.Errorf("%s: err = %v", mode, err)
		}
	}
	o := AnthropicOptions{Mode: AuthOAuth, Name: "MISSING_KEY_FOR_TEST", PrintEnv: true}
	if _, err := AnthropicSecret(o, failStore); err == nil || !strings.Contains(err.Error(), "claude-code vault set MISSING_KEY_FOR_TEST") {
		t.Errorf("--print-env: err = %v", err)
	}
}

func TestAnthropicSecretReadsVault(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Secret(t, "WORK_KEY", fakeToken)
	o := AnthropicOptions{Mode: AuthOAuth, Name: "WORK_KEY"}
	if v, err := AnthropicSecret(o, failStore); err != nil || v != fakeToken {
		t.Errorf("oauth: %q %v", v, err)
	}
	o.Mode = AuthAPIKey
	if v, err := AnthropicSecret(o, failStore); err != nil || v != fakeToken {
		t.Errorf("api key: %q %v", v, err)
	}
}

// A validated env-shaped name may appear in the "not in the vault" error; PrintEnv avoids any prompt.
func TestAnthropicSecretMissingNamesKey(t *testing.T) {
	vaulttest.Use(t)
	const name = "NOPE_ABSENT_KEY_XYZ"
	t.Setenv(name, "")
	o := AnthropicOptions{Mode: AuthOAuth, Name: name, PrintEnv: true}
	_, err := AnthropicSecret(o, failStore)
	if err == nil || !strings.Contains(err.Error(), name+" is not in the vault") {
		t.Fatalf("err = %v", err)
	}
}

// failStore stands in for the vault verb; the tests never reach a real store.
func failStore(string) int { return 1 }
